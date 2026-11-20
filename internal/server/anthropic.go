package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"net/http"
	"strconv"
	"strings"
	"time"

	"codearts2api/internal/apierr"
	"codearts2api/internal/codearts"
	"codearts2api/internal/protocol"
	"codearts2api/internal/registry"
)

// Anthropic Messages API 兼容层（HTTP 部分）。
//
// 上游（CodeArts inferhub）本身是 OpenAI 兼容协议，因此这里做的是
// 「Anthropic 请求 → OpenAI 请求」与「OpenAI 响应/SSE → Anthropic 响应/SSE」
// 的双向映射，覆盖 text / thinking / image / tool_use / tool_result。
//
// 纯数据转换在 internal/protocol；本文件只负责 HTTP 传输与 SSE 编码。

// handleAnthropicMessages 是 POST /v1/messages 的入口。
func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "仅支持 POST")
		return
	}
	start := time.Now()
	r = withRequestIDValue(w, r)

	body, ok := readBody(w, r, errAnthropic)
	if !ok {
		return
	}
	var req protocol.Request
	parseErr := json.Unmarshal(body, &req)
	// 采样在任何改写之前（见 dump.go）：重放要复现客户端的真实请求。
	s.dumps.Record(requestID(r.Context()), r.URL.Path, req.Model, req.Stream, body)
	if parseErr != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "请求体解析失败: "+parseErr.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "messages 不能为空")
		return
	}
	if len(req.Messages) > maxMessages {
		writeAnthropicErr(w, http.StatusRequestEntityTooLarge, "invalid_request_error",
			"messages 条数超过上限（"+strconv.Itoa(maxMessages)+" 条）")
		return
	}

	settings := s.Store.Settings()
	model, fallback, err := s.resolveModel(settings, req.Model)
	if err != nil {
		// 请求侧拒绝也要进日志：否则客户端贴来一个 X-Request-Id，网关里搜不到任何东西。
		s.record(requestID(r.Context()), extractKey(r), nil, req.Model, req.Stream,
			apierr.From(err).Status, start, 0, 0, 0, err)
		writeAPIErr(w, errAnthropic, err)
		return
	}
	if fallback {
		announceFallback(w, strings.TrimSpace(req.Model), model.ID)
	}
	if !settings.AllowCreditModels {
		msg := "模型 " + model.ID + " 会真实消耗积分，当前已在控制台关闭。如需使用请开启「允许计费模型」。"
		s.record(requestID(r.Context()), extractKey(r), nil, model.ID, req.Stream,
			http.StatusForbidden, start, 0, 0, 0, errors.New(msg))
		writeAnthropicErr(w, http.StatusForbidden, "invalid_request_error", msg)
		return
	}
	// 联网搜索 / 服务端工具：web_search 由网关代执行，其余服务端工具显式拒绝。
	search, serr := s.applyAnthropicWebSearch(r.Context(), &req)
	if serr != nil {
		s.record(requestID(r.Context()), extractKey(r), nil, model.ID, req.Stream,
			apierr.From(serr).Status, start, 0, 0, 0, serr)
		writeAPIErr(w, errAnthropic, serr)
		return
	}

	upReq, err := protocol.ToOpenAI(&req, model.ID)
	if err != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if search != nil {
		if msgs, ok := upReq["messages"].([]map[string]any); ok {
			upReq["messages"] = prependSystemMap(msgs, search.block)
			w.Header().Set("X-Gateway-Web-Search", search.info)
		}
	}
	upBody, err := json.Marshal(upReq)
	if err != nil {
		writeAnthropicErr(w, http.StatusInternalServerError, "api_error", "请求体序列化失败")
		return
	}

	key := extractKey(r)
	timeout := time.Duration(settings.RequestTimeout) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	promptLen := protocol.EstimatePromptLen(upBody)

	if req.Stream {
		s.streamAnthropic(w, r, upBody, model, key, timeout, start, promptLen)
		return
	}
	s.blockingAnthropic(w, r, upBody, model, key, timeout, start, promptLen)
}

// blockingAnthropic 收集完整回复后按 Anthropic message 结构返回。
func (s *Server) blockingAnthropic(w http.ResponseWriter, r *http.Request, body []byte, model registry.Model, key string, timeout time.Duration, start time.Time, promptLen int) {
	var (
		raw    []byte
		outLen int
	)
	acct, tried, err := s.withAccountRetry(r.Context(), timeout, func(ctx context.Context, acct *codearts.Account) error {
		res, err := s.chatOnce(ctx, acct, model, body)
		if err != nil {
			return err
		}
		raw = res
		return nil
	})
	if err != nil {
		e := apierr.From(err)
		s.record(requestID(r.Context()), key, acct, model.ID, false, e.Status, start, promptLen, 0, tried, err)
		writeAPIErr(w, errAnthropic, err)
		return
	}

	out := protocol.FromOpenAI(raw, model.ID)
	outLen = len([]rune(out.Text)) + len([]rune(out.Thinking))
	s.record(requestID(r.Context()), key, acct, model.ID, false, http.StatusOK, start, promptLen, outLen, tried, nil)
	writeJSON(w, http.StatusOK, out.Payload)
}

// streamAnthropic 把上游 OpenAI SSE 转换为 Anthropic SSE 事件流。
func (s *Server) streamAnthropic(w http.ResponseWriter, r *http.Request, body []byte, model registry.Model, key string, timeout time.Duration, start time.Time, promptLen int) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicErr(w, http.StatusInternalServerError, "api_error", "服务端不支持流式响应")
		return
	}

	msgID := "msg_" + codearts.RandomHex(12)
	var (
		started   bool
		outLen    int
		streamErr error
	)

	acct, tried, err := s.withAccountRetry(r.Context(), timeout, func(ctx context.Context, acct *codearts.Account) error {
		st := newAnthropicStream(w, flusher, msgID, model.ID, promptLen)
		err := s.streamOnce(ctx, acct, model, body, func(chunk []byte) error {
			if !started {
				h := w.Header()
				h.Set("Content-Type", "text/event-stream; charset=utf-8")
				h.Set("Cache-Control", "no-cache")
				h.Set("Connection", "keep-alive")
				h.Set("X-Accel-Buffering", "no")
				h.Set("X-Content-Type-Options", "nosniff")
				w.WriteHeader(http.StatusOK)
				started = true
				st.start()
			}
			n, err := st.consume(chunk)
			outLen += n
			return err
		})
		if err == nil {
			st.finish("")
		}
		return err
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
		writeAPIErr(w, errAnthropic, err)
		return
	}
	if streamErr != nil {
		e := apierr.From(streamErr)
		body := map[string]any{"type": e.Anthropic, "message": streamErr.Error()}
		if e.Code != "" {
			body["code"] = e.Code
		}
		writeSSE(w, flusher, "error", map[string]any{"type": "error", "error": body})
	}
	s.record(requestID(r.Context()), key, acct, model.ID, true, apierr.From(streamErr).Status, start, promptLen, outLen, tried, streamErr)
}

// --- OpenAI SSE → Anthropic SSE ---

// anthropicStream 把上游 OpenAI SSE 分片转换为 Anthropic SSE 事件。
type anthropicStream struct {
	w       http.ResponseWriter
	flusher http.Flusher
	msgID   string
	model   string
	inTok   int

	index    int
	kind     string // "" / "thinking" / "text" / "tool_use"
	toolArgs strings.Builder
	toolID   string
	toolName string
	outTok   int
	// thinkBuf 累积当前 thinking 块的正文，收块时据此派生 signature。
	// 必须在块内累积：signature 是对整块内容做 HMAC，不是逐片。
	thinkBuf strings.Builder
	// sawToolUse 记录是否出现过工具调用，用于决定 stop_reason
	sawToolUse bool
}

func newAnthropicStream(w http.ResponseWriter, f http.Flusher, msgID, model string, promptLen int) *anthropicStream {
	return &anthropicStream{w: w, flusher: f, msgID: msgID, model: model, inTok: promptLen, index: -1}
}

// start 发送 message_start 事件。
func (a *anthropicStream) start() {
	a.send("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": a.msgID, "type": "message", "role": "assistant", "model": a.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": a.inTok, "output_tokens": 0},
		},
	})
}

// openBlock 在需要时切换内容块。
func (a *anthropicStream) openBlock(kind string) {
	if a.kind == kind {
		return
	}
	a.closeBlock()
	a.index++
	a.kind = kind
	a.thinkBuf.Reset()
	var block map[string]any
	switch kind {
	case "thinking":
		block = map[string]any{"type": "thinking", "thinking": ""}
	case "tool_use":
		block = map[string]any{"type": "tool_use", "id": a.toolID, "name": a.toolName, "input": map[string]any{}}
	default:
		block = map[string]any{"type": "text", "text": ""}
	}
	a.send("content_block_start", map[string]any{
		"type": "content_block_start", "index": a.index, "content_block": block,
	})
}

// closeBlock 结束当前内容块。
//
// thinking 块必须在此刻补一个 signature_delta：Anthropic 官方 SDK / Claude Code
// 在多轮回传 assistant 内容时会校验 thinking 块的 signature，缺失会导致
// 后续请求 400 —— 典型表现是「第一轮能用、第二轮崩」。
func (a *anthropicStream) closeBlock() {
	if a.kind == "" {
		return
	}
	if a.kind == "thinking" {
		if sig := protocol.ThinkingSignature(a.thinkBuf.String()); sig != "" {
			a.send("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": a.index,
				"delta": map[string]any{"type": "signature_delta", "signature": sig},
			})
		}
	}
	a.send("content_block_stop", map[string]any{"type": "content_block_stop", "index": a.index})
	a.kind = ""
}

// consume 处理一个上游分片，返回新增的输出字符数。
func (a *anthropicStream) consume(chunk []byte) (int, error) {
	var c struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(chunk, &c); err != nil {
		return 0, nil
	}
	n := 0
	for _, ch := range c.Choices {
		if ch.Delta.ReasoningContent != "" {
			a.openBlock("thinking")
			n += len([]rune(ch.Delta.ReasoningContent))
			a.outTok += len([]rune(ch.Delta.ReasoningContent))
			a.thinkBuf.WriteString(ch.Delta.ReasoningContent)
			a.send("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": a.index,
				"delta": map[string]any{"type": "thinking_delta", "thinking": ch.Delta.ReasoningContent},
			})
		}
		if ch.Delta.Content != "" {
			a.openBlock("text")
			n += len([]rune(ch.Delta.Content))
			a.outTok += len([]rune(ch.Delta.Content))
			a.send("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": a.index,
				"delta": map[string]any{"type": "text_delta", "text": ch.Delta.Content},
			})
		}
		for _, tc := range ch.Delta.ToolCalls {
			if a.kind != "tool_use" || a.toolID != tc.ID {
				if tc.ID != "" {
					a.toolID = tc.ID
					a.toolName = tc.Function.Name
					a.toolArgs.Reset()
					a.openBlock("tool_use")
				}
			}
			if tc.Function.Arguments != "" {
				a.sawToolUse = true
				a.toolArgs.WriteString(tc.Function.Arguments)
				a.send("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": a.index,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
				})
			}
		}
	}
	return n, nil
}

// finish 发送收尾事件。stopReason 为空时按是否出现工具调用自动判定。
func (a *anthropicStream) finish(stopReason string) {
	if stopReason == "" {
		if a.sawToolUse {
			stopReason = "tool_use"
		} else {
			stopReason = "end_turn"
		}
	}
	if a.kind != "" {
		a.closeBlock()
	}
	a.send("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": a.outTok},
	})
	a.send("message_stop", map[string]any{"type": "message_stop"})
}

func (a *anthropicStream) send(event string, payload map[string]any) {
	writeSSE(a.w, a.flusher, event, payload)
}

// writeSSE 输出一个 Anthropic SSE 事件。
func writeSSE(w http.ResponseWriter, f http.Flusher, event string, payload map[string]any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
	f.Flush()
}

// writeAnthropicErr 按 Anthropic 的错误结构返回（不带规范码的简易形式）。
func writeAnthropicErr(w http.ResponseWriter, code int, typ, msg string) {
	writeAnthropicAPIErr(w, apierr.Error{Status: code, Anthropic: typ}, msg)
}

// handleAnthropicCountTokens 是 POST /v1/messages/count_tokens 的入口。
//
// 为什么需要它：Claude Code 与部分 Anthropic SDK 在发请求前会先问「这段上下文
// 有多少 token」，拿不到就退化成自己猜，或者直接报错。以前这个路径是 404。
//
// 实现是**本地估算**（不走上游、不消耗积分）：把请求按 /v1/messages 同一套规则
// 翻译成 Chat 体，再用与日志里 prompt_len 相同的估算函数 —— 两处数字必须一致，
// 否则控制台看到的长度和客户端拿到的 token 数会对不上。
func (s *Server) handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "仅支持 POST")
		return
	}
	r = withRequestIDValue(w, r)
	body, ok := readBody(w, r, errAnthropic)
	if !ok {
		return
	}
	var req protocol.Request
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "请求体解析失败: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "messages 不能为空")
		return
	}
	if len(req.Messages) > maxMessages {
		writeAnthropicErr(w, http.StatusRequestEntityTooLarge, "invalid_request_error",
			"messages 条数超过上限（"+strconv.Itoa(maxMessages)+" 条）")
		return
	}

	// count_tokens 与具体模型无关，但仍按 /v1/messages 的规则解析模型名：
	// 客户端用同一个名字发对话，两边结论必须一致（未知模型照旧 404）。
	model, _, err := s.resolveModel(s.Store.Settings(), req.Model)
	if err != nil {
		writeAPIErr(w, errAnthropic, err)
		return
	}
	up, err := protocol.ToOpenAI(&req, model.ID)
	if err != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	raw, err := json.Marshal(up)
	if err != nil {
		writeAnthropicErr(w, http.StatusInternalServerError, "api_error", "请求体序列化失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": protocol.EstimatePromptLen(raw)})
}
