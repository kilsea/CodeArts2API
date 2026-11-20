package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"codearts2api/internal/apierr"
	"codearts2api/internal/codearts"
	"codearts2api/internal/protocol"
	"codearts2api/internal/registry"
	"codearts2api/internal/store"
)

// chatRequest 是 OpenAI Chat Completions 的请求体（仅取网关需要处理的字段）。
//
// 其余字段（tools / response_format / temperature / …）不解析，
// 而是原样转发给上游 —— 上游本身就是 OpenAI 兼容协议，无需转换。
type chatRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// handleChatCompletions 是 /v1/chat/completions 的主入口。
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeRequestErr(w, http.StatusMethodNotAllowed, "仅支持 POST", "invalid_request_error", "", "")
		return
	}
	start := time.Now()
	r = withRequestIDValue(w, r)

	body, ok := readBody(w, r, errOpenAI)
	if !ok {
		return
	}

	var req chatRequest
	parseErr := json.Unmarshal(body, &req)
	// 采样必须在任何改写之前：重放要复现客户端的**真实**请求（含非法 role /
	// 越界参数），存网关改写后的体等于让重放永远成功，失去意义。
	s.dumps.Record(requestID(r.Context()), r.URL.Path, req.Model, req.Stream, body)
	if parseErr != nil {
		writeRequestErr(w, http.StatusBadRequest, "请求体解析失败: "+parseErr.Error(),
			"invalid_request_error", apierr.CodeInvalidValue, "")
		return
	}
	if len(req.Messages) == 0 {
		writeRequestErr(w, http.StatusBadRequest, "messages 不能为空",
			"invalid_request_error", apierr.CodeInvalidValue, "messages")
		return
	}

	// 请求体前置校验：参数越界 / 不支持的字段在触达上游之前处理掉。
	// 未支持字段的声明必须写进响应头，所以这里先记下来，等开始写响应时再补。
	body, dropped, err := validateChatRequest(body)
	if err != nil {
		s.record(requestID(r.Context()), extractKey(r), nil, req.Model, req.Stream,
			apierr.From(err).Status, start, 0, 0, 0, err)
		writeAPIErr(w, errOpenAI, err)
		return
	}
	announceUnsupported(w, dropped)

	// 角色规范化必须在 rewriteModel 之前：上游只认 system/user/assistant/tool，
	// 其余写法会换来一句会误导人的「Message role cannot empty」。
	body, err = normalizeRoles(body)
	if err != nil {
		s.record(requestID(r.Context()), extractKey(r), nil, req.Model, req.Stream, apierr.From(err).Status, start, 0, 0, 0, err)
		writeAPIErr(w, errOpenAI, err)
		return
	}

	settings := s.Store.Settings()
	model, fallback, err := s.resolveModel(settings, req.Model)
	if err != nil {
		// 请求侧拒绝也要进日志：否则客户端贴来一个 X-Request-Id，网关里搜不到任何东西。
		s.record(requestID(r.Context()), extractKey(r), nil, req.Model, req.Stream,
			apierr.From(err).Status, start, 0, 0, 0, err)
		writeAPIErr(w, errOpenAI, err)
		return
	}
	if fallback {
		announceFallback(w, strings.TrimSpace(req.Model), model.ID)
	}
	if !settings.AllowCreditModels {
		msg := "模型 " + model.ID + " 会真实消耗积分，当前已在控制台关闭。如需使用请开启「允许计费模型」。"
		s.record(requestID(r.Context()), extractKey(r), nil, model.ID, req.Stream,
			http.StatusForbidden, start, 0, 0, 0, errors.New(msg))
		writeRequestErr(w, http.StatusForbidden, msg,
			"invalid_request_error", apierr.CodeCreditDisabled, "model")
		return
	}

	// 联网搜索：客户端声明 type=web_search 时由网关代执行（上游不支持内置工具）。
	// 放在模型与计费校验之后、估算 promptLen 之前：非法模型不该白白触发一次外部检索，
	// 而日志里的长度必须是真实发给上游的长度。
	searchRes, serr := s.applyChatWebSearch(r.Context(), body)
	if serr != nil {
		s.record(requestID(r.Context()), extractKey(r), nil, model.ID, req.Stream,
			apierr.From(serr).Status, start, 0, 0, 0, serr)
		writeAPIErr(w, errOpenAI, serr)
		return
	}
	if searchRes != nil {
		body = searchRes.body
		w.Header().Set("X-Gateway-Web-Search", searchRes.info)
	}

	upBody, err := rewriteModel(body, model.ID, req.Stream)
	if err != nil {
		writeRequestErr(w, http.StatusBadRequest, "请求体改写失败: "+err.Error(),
			"invalid_request_error", apierr.CodeInvalidValue, "")
		return
	}

	key := extractKey(r)
	timeout := time.Duration(settings.RequestTimeout) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	promptLen := protocol.EstimatePromptLen(body)
	// 上游按 user-msg-id 记账，反查真实积分时用同一个 id（见 settleCredits）。
	msgID := newMsgID()

	if req.Stream {
		s.streamChat(w, r, upBody, model, key, timeout, start, promptLen, msgID)
		return
	}
	s.blockingChat(w, r, upBody, model, key, timeout, start, promptLen, msgID)
}

// blockingChat 收集完整回复后一次性返回。
func (s *Server) blockingChat(w http.ResponseWriter, r *http.Request, body []byte, model registry.Model, key string, timeout time.Duration, start time.Time, promptLen int, msgID string) {
	var (
		raw    []byte
		outLen int
	)

	acct, tried, err := s.withAccountRetry(r.Context(), timeout, func(ctx context.Context, acct *codearts.Account) error {
		res, err := s.chatOnce(ctx, acct, model, body, msgID)
		if err != nil {
			return err
		}
		raw = res
		return nil
	})
	if err != nil {
		e := apierr.From(err)
		s.record(requestID(r.Context()), key, acct, model.ID, false, e.Status, start, promptLen, 0, tried, err)
		writeAPIErr(w, errOpenAI, err)
		return
	}

	u, _ := usageFromJSON(raw)
	outLen = completionLen(raw)
	// 响应体里的 model 必须是**实际执行**的模型：客户端靠它判断自己到底在用什么，
	// 回填请求里的名字（尤其别名 / 兼容别名命中时）等于骗人。
	raw = withExecutedModel(raw, model.ID)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)

	// 先把响应交给客户端，再反查上游账单：查询耗时（通常几百毫秒）不应该
	// 变成用户等待的一部分。
	elapsed := time.Since(start)
	s.record(requestID(r.Context()), key, acct, model.ID, false, http.StatusOK, start, promptLen, outLen, tried, nil,
		withElapsed(elapsed), withCredit(s.settleCredits(r.Context(), acct, model, msgID, u)))
}

// streamChat 以 OpenAI SSE 格式流式返回（上游本身就是 SSE，做透明转发 + 错误识别）。
func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, body []byte, model registry.Model, key string, timeout time.Duration, start time.Time, promptLen int, msgID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeRequestErr(w, http.StatusInternalServerError, "服务端不支持流式响应",
			"server_error", apierr.CodeInternal, "stream")
		return
	}

	// 先尝试建立上游连接；失败时还能以普通 HTTP 错误返回。
	var (
		started   bool
		outLen    int
		streamErr error
		u         tokenUsage
	)

	acct, tried, err := s.withAccountRetry(r.Context(), timeout, func(ctx context.Context, acct *codearts.Account) error {
		return s.streamOnce(ctx, acct, model, body, msgID, func(chunk []byte) error {
			if bytes.Contains(chunk, []byte(`"usage"`)) {
				if nu, ok := usageFromJSON(chunk); ok {
					u = nu
				}
			}
			chunk = withExecutedModel(chunk, model.ID)
			if !started {
				h := w.Header()
				h.Set("Content-Type", "text/event-stream; charset=utf-8")
				h.Set("Cache-Control", "no-cache")
				h.Set("Connection", "keep-alive")
				h.Set("X-Accel-Buffering", "no")
				h.Set("X-Content-Type-Options", "nosniff")
				w.WriteHeader(http.StatusOK)
				started = true
			}
			outLen += deltaLen(chunk)
			if _, err := fmt.Fprintf(w, "data: %s\n\n", chunk); err != nil {
				return err
			}
			flusher.Flush()
			return nil
		})
	})
	streamErr = err

	if !started {
		// 上游连一次都没成功：按普通错误返回，客户端能拿到明确的状态码。
		// err == nil 但一个分片都没有，说明上游空响应，归为上游故障（502）。
		if err == nil {
			err = codearts.UpstreamBusinessError(http.StatusBadGateway, "上游没有返回任何分片")
		}
		e := apierr.From(err)
		s.record(requestID(r.Context()), key, acct, model.ID, true, e.Status, start, promptLen, outLen, tried, err)
		writeAPIErr(w, errOpenAI, err)
		return
	}

	if streamErr != nil {
		// 已经在流里了，只能以 SSE 错误块收尾（OpenAI 客户端只看 error 对象）
		e := apierr.From(streamErr)
		body := map[string]any{"message": streamErr.Error(), "type": e.OpenAI}
		if e.Code != "" {
			body["code"] = e.Code
		}
		errChunk, _ := json.Marshal(map[string]any{"error": body})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", errChunk)
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()

	elapsed := time.Since(start)
	s.record(requestID(r.Context()), key, acct, model.ID, true, apierr.From(streamErr).Status, start, promptLen, outLen, tried, streamErr,
		withElapsed(elapsed), withCredit(s.settleCredits(r.Context(), acct, model, msgID, u)))
}

// chatOnce 用指定账号发一次非流式请求。
//
// 上游对个别模型（实测 glm-5.3）的非流式接口只回一个空 body 的 400，
// 这里检测到该特征后自动改用「流式 + 本地聚合」，对客户端保持非流式语义。
func (s *Server) chatOnce(ctx context.Context, acct *codearts.Account, model registry.Model, body []byte, msgID string) ([]byte, error) {
	sess, err := s.Cred.Session(ctx, acct, s.resolveOptions())
	if err != nil {
		return nil, err
	}

	if model.StreamOnly {
		return s.aggregateViaStream(ctx, sess, model, body, msgID)
	}

	raw, err := s.Up.Chat(ctx, sess, model.ID, body, codearts.WithMsgID(msgID))
	if err != nil {
		if codearts.IsEmptyRejection(err) {
			s.Reg.MarkStreamOnly(model.ID)
			if agg, aerr := s.aggregateViaStream(ctx, sess, model, body, msgID); aerr == nil {
				return agg, nil
			} else {
				return nil, aerr
			}
		}
		return nil, err
	}
	if msg, status, bad := codearts.IsUpstreamBusinessError(raw); bad {
		return nil, codearts.UpstreamBusinessError(status, msg)
	}
	return raw, nil
}

// aggregateViaStream 用流式请求取回分片，再在本地聚合成一次性响应。
func (s *Server) aggregateViaStream(ctx context.Context, sess *codearts.Session, model registry.Model, body []byte, msgID string) ([]byte, error) {
	streamBody, err := rewriteStream(body, true)
	if err != nil {
		return nil, err
	}
	var chunks [][]byte
	err = s.Up.ChatStream(ctx, sess, model.ID, streamBody, func(chunk []byte) error {
		if msg, status, bad := codearts.IsUpstreamBusinessError(chunk); bad {
			return codearts.UpstreamBusinessError(status, msg)
		}
		chunks = append(chunks, append([]byte(nil), chunk...))
		return nil
	}, codearts.WithMsgID(msgID))
	if err != nil {
		return nil, err
	}
	return codearts.AggregateSSE(chunks)
}

// streamOnce 用指定账号发一次流式请求，逐块回调。
func (s *Server) streamOnce(ctx context.Context, acct *codearts.Account, model registry.Model, body []byte, msgID string, onChunk func([]byte) error) error {
	sess, err := s.Cred.Session(ctx, acct, s.resolveOptions())
	if err != nil {
		return err
	}
	return s.Up.ChatStream(ctx, sess, model.ID, body, func(chunk []byte) error {
		if msg, status, bad := codearts.IsUpstreamBusinessError(chunk); bad {
			return codearts.UpstreamBusinessError(status, msg)
		}
		return onChunk(chunk)
	}, codearts.WithMsgID(msgID))
}

// 重试相关常量。
//
// 尝试次数与账号数量**解耦**：上游最常见的失败是「每账号 3 个并发会话」打满
// （TM.00001041），此时账号本身是健康的，只要退避重试即可 —— 如果只有 1 个账号
// 就退化成「不重试」，客户端会直接拿到 502。
const (
	// defaultMaxAttempts 是设置里未配置时的尝试次数。
	defaultMaxAttempts = 3
	// maxAttemptsCap 是尝试次数上限，防止设置被填成很大时长时间占用连接。
	maxAttemptsCap = 8
	// transientBaseWait 是临时性故障的退避基数（第 n 次退避 = n * base）。
	// 上游会话释放需要时间，太短（曾经是 300ms）基本等于不退避。
	transientBaseWait = time.Second
	// transientMaxWait 是单次退避上限。
	transientMaxWait = 3 * time.Second
	// queueWaitTimeout 是「等账号并发槽位」的最长排队时长。
	//
	// 上游每账号硬上限 3 个并发会话。以前槽位满了也照打，换回一个
	// TM.00001041（临时性错误）后白白消耗一次重试；现在改成先在网关里排队。
	// 20 秒的取舍：一次对话动辄几十秒，排 20 秒仍比「被上游拒绝 + 退避重试」
	// 快，也不会让客户端觉得卡死。
	queueWaitTimeout = 20 * time.Second
	// queueRetryAfter 是排队超时后给客户端的建议重试间隔。
	queueRetryAfter = 5 * time.Second
)

// transientBackoff 返回第 n 次临时性故障后的退避时长（n 从 1 开始）：
// base、2*base、…，封顶 max。抽成纯函数是为了让「退避序列」能被单测钉住 ——
// 之前这里只有一条 `n * base` 的内联表达式，退避写错（比如恒为 0）不会有任何
// 测试失败，只会在线上表现为「重试等于没退避」。
func transientBackoff(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	return min(time.Duration(n)*transientBaseWait, transientMaxWait)
}

// waitRetry 是重试前的等待。测试会替换它以避免真的睡满 1~3 秒；
// 生产路径始终是「可被 ctx 打断的定时等待」。
var waitRetry = func(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// withAccountRetry 选账号并在失败时重试。
//
// 返回本次实际使用的账号（可能为 nil）、实际尝试的账号数（供日志记录）、错误。
// tried 让控制台能看出「一次成功」与「重试 N 次后失败」的区别 —— 请求侧错误恒为 1。
func (s *Server) withAccountRetry(ctx context.Context, timeout time.Duration, fn func(context.Context, *codearts.Account) error) (*codearts.Account, int, error) {
	accts := s.Store.Accounts()
	if len(accts) == 0 {
		return nil, 0, apierr.NoAccount
	}
	attempts := s.Store.Settings().MaxRetries
	if attempts <= 0 {
		attempts = defaultMaxAttempts
	}
	if attempts > maxAttemptsCap {
		attempts = maxAttemptsCap
	}

	var lastErr error
	var used *codearts.Account
	transientWaits := 0
	tried := 0
	for i := 0; i < attempts; i++ {
		acct := s.Store.NextAccount()
		if acct == nil {
			break
		}
		tried++
		used = acct
		// 每账号并发闸门：上游每账号硬上限 3 个并发会话，超限时上游回
		// TM.00001041（临时性错误）。在这里排队，把「打到上游被拒 + 退避重试」
		// 换成「在网关里等一个槽位」。
		//
		// 排队超时与客户端断开必须分开处理：前者该回 429 + Retry-After，
		// 后者什么都不该回（客户端已经不在了）。
		qctx, qcancel := context.WithTimeout(ctx, queueWaitTimeout)
		slotErr := s.Store.AcquireSlot(qctx, acct.ID)
		qcancel()
		if slotErr != nil {
			if ctx.Err() != nil {
				break
			}
			lastErr = &apierr.AccountBusy{RetryAfter: queueRetryAfter}
			break
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		err := fn(cctx, acct)
		cancel()
		s.Store.ReleaseSlot(acct.ID)

		if err == nil {
			s.Store.MarkAccountResult(acct.ID, true, "")
			return acct, tried, nil
		}
		lastErr = err
		// 客户端主动断开（curl/浏览器提前关连接）不是账号故障，不应计入失败
		if ctx.Err() != nil {
			break
		}
		// 临时性故障（并发上限 / 429 / 5xx）：账号本身没问题，退避后重试
		if codearts.IsTransientUpstream(err) {
			if i == attempts-1 {
				break
			}
			transientWaits++
			if err := waitRetry(ctx, transientBackoff(transientWaits)); err != nil {
				return used, tried, lastErr
			}
			continue
		}
		// 网络层抖动（连接中断 `unexpected EOF` / 单次请求超时 `context deadline
		// exceeded`）：账号本身没坏，退避后换号重试。
		//
		// 2026-11-03 线上事故：这两类错误此前落到下面的 MarkAccountResult(false)，
		// 一次抖动就把账号标成 error，而 NextAccount 又只挑 Failures==0 的账号，
		// 等于永久踢出轮转。它们既不是上游业务错误，也不是请求非法，必须单独判。
		if codearts.IsTransientNetwork(err) {
			if i == attempts-1 {
				break
			}
			transientWaits++
			if err := waitRetry(ctx, transientBackoff(transientWaits)); err != nil {
				return used, tried, lastErr
			}
			continue
		}
		// 请求本身非法（参数越界 / 模型未注册 / tools 超长）：换哪个账号都一样，
		// 不是账号故障。计入失败会让任意客户端发错一次参数就把健康账号标成 error，
		// 还会累加 failures 触发熔断。直接返回，让客户端去改请求。
		if codearts.IsClientRequestError(err) {
			return used, tried, lastErr
		}
		s.Store.MarkAccountResult(acct.ID, false, err.Error())
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("全部账号均不可用")
	}
	return used, tried, lastErr
}

// rewriteModel 把请求体里的 model 换成解析后的上游模型 ID。
//
// 其余字段原样保留，避免丢失 tools / response_format 等上游已支持的能力。
func rewriteModel(body []byte, model string, stream bool) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	mb, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	m["model"] = mb
	sb, err := json.Marshal(stream)
	if err != nil {
		return nil, err
	}
	m["stream"] = sb
	return json.Marshal(m)
}

// rewriteStream 只改写请求体里的 stream 字段。
func rewriteStream(body []byte, stream bool) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	sb, err := json.Marshal(stream)
	if err != nil {
		return nil, err
	}
	m["stream"] = sb
	return json.Marshal(m)
}

// record 记录一次请求结果。
// attempts 是本次请求实际尝试的账号数（withAccountRetry 的返回值），
// 让日志能区分「一次成功」与「重试 N 次后失败」；ErrClass 由 apierr.Classify 推导，
// 控制台据此按「审核 / 参数 / 登录态 / 临时 / 上游」筛选，不必读错误原文。
// rid 是响应头 X-Request-Id 的值：客户端贴来一个 id，就能在日志里定位到这一条。
func (s *Server) record(rid, key string, acct *codearts.Account, model string, stream bool, status int, start time.Time, inLen, outLen, attempts int, err error, opts ...recordOpt) {
	e := store.LogEntry{
		Time:      time.Now().Unix(),
		RequestID: rid,
		Key:       codearts.Mask(key),
		Model:     model,
		Stream:    stream,
		Status:    status,
		LatencyMS: time.Since(start).Milliseconds(),
		PromptLen: inLen,
		OutputLen: outLen,
	}
	for _, fn := range opts {
		fn(&e)
	}
	if acct != nil {
		e.Account = acct.DisplayName()
		e.AccountID = acct.ID
	}
	if err != nil {
		e.Err = err.Error()
	}
	e.ErrClass = apierr.Classify(err)
	e.Attempts = attempts
	s.Store.Record(e, outLen)
}

// completionLen 统计非流式响应里的输出字符数。
func completionLen(raw []byte) int {
	var resp struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return 0
	}
	n := 0
	for _, c := range resp.Choices {
		n += len([]rune(c.Message.Content)) + len([]rune(c.Message.ReasoningContent))
	}
	return n
}

// deltaLen 统计一个流式分片里的输出字符数。
func deltaLen(chunk []byte) int {
	var c struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(chunk, &c); err != nil {
		return 0
	}
	n := 0
	for _, ch := range c.Choices {
		n += len([]rune(ch.Delta.Content)) + len([]rune(ch.Delta.ReasoningContent))
	}
	return n
}
