package server

// OpenAI Responses API 兼容层（HTTP 部分）。
//
// 上游只提供 Chat Completions，因此这里做的是
// 「Responses 请求 → Chat Completions 请求」与
// 「Chat Completions 响应/SSE → response 对象/事件流」的映射。
//
// 纯数据转换在 internal/protocol（responses.go）；本文件只负责
// HTTP 传输、SSE 编码、响应存储与错误映射。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"codearts2api/internal/apierr"
	"codearts2api/internal/codearts"
	"codearts2api/internal/protocol"
	"codearts2api/internal/registry"
)

// handleResponses 是 POST /v1/responses 的入口。
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
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
	var req protocol.ResponsesRequest
	parseErr := json.Unmarshal(body, &req)
	// 采样在任何改写之前（见 dump.go）：重放要复现客户端的真实请求。
	s.dumps.Record(requestID(r.Context()), r.URL.Path, req.Model, req.Stream, body)
	if parseErr != nil {
		writeRequestErr(w, http.StatusBadRequest, "请求体解析失败: "+parseErr.Error(),
			"invalid_request_error", apierr.CodeInvalidValue, "")
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

	// previous_response_id：从内存存储取历史消息。
	var prefix []map[string]any
	if id := strings.TrimSpace(req.PreviousResponseID); id != "" {
		entry, found := s.responses().get(id)
		if !found {
			err := &apierr.ResponseNotFound{ID: id, Param: "previous_response_id"}
			s.record(requestID(r.Context()), extractKey(r), nil, model.ID, req.Stream,
				apierr.From(err).Status, start, 0, 0, 0, err)
			writeAPIErr(w, errOpenAI, err)
			return
		}
		prefix = entry.Messages
	}

	// 联网搜索（Responses 的 {"type":"web_search"}）：网关代执行。
	search, serr := s.applyResponsesWebSearch(r.Context(), &req)
	if serr != nil {
		s.record(requestID(r.Context()), extractKey(r), nil, model.ID, req.Stream,
			apierr.From(serr).Status, start, 0, 0, 0, serr)
		writeAPIErr(w, errOpenAI, serr)
		return
	}

	upReq, dropped, err := protocol.ToChatRequest(&req, model.ID, prefix)
	if err != nil {
		err = responsesParamErr(err)
		s.record(requestID(r.Context()), extractKey(r), nil, model.ID, req.Stream,
			apierr.From(err).Status, start, 0, 0, 0, err)
		writeAPIErr(w, errOpenAI, err)
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
		writeRequestErr(w, http.StatusInternalServerError, "请求体序列化失败",
			"server_error", apierr.CodeInternal, "")
		return
	}

	// 复用 Chat Completions 的前置校验与角色规范化：翻译出来的 messages
	// 也必须过同一套规则，否则「Responses 能过、Chat 不能过」这种不一致
	// 会变成又一个只在线上才发现的坑。
	upBody, moreDropped, err := validateChatRequest(upBody)
	dropped = append(dropped, moreDropped...)
	announceUnsupported(w, dropped)
	if err != nil {
		s.record(requestID(r.Context()), extractKey(r), nil, model.ID, req.Stream,
			apierr.From(err).Status, start, 0, 0, 0, err)
		writeAPIErr(w, errOpenAI, err)
		return
	}
	upBody, err = normalizeRoles(upBody)
	if err != nil {
		s.record(requestID(r.Context()), extractKey(r), nil, model.ID, req.Stream,
			apierr.From(err).Status, start, 0, 0, 0, err)
		writeAPIErr(w, errOpenAI, err)
		return
	}
	upBody, err = rewriteModel(upBody, model.ID, req.Stream)
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
	promptLen := protocol.EstimatePromptLen(upBody)

	if req.Stream {
		s.streamResponses(w, r, upBody, &req, model, key, timeout, start, promptLen)
		return
	}
	s.blockingResponses(w, r, upBody, &req, model, key, timeout, start, promptLen)
}

// responsesParamErr 把转换层的 ParamError 翻译成规范错误。
func responsesParamErr(err error) error {
	var pe *protocol.ParamError
	if errors.As(err, &pe) {
		code := apierr.CodeInvalidValue
		if pe.Unsupported {
			code = apierr.CodeUnsupportedParam
		}
		return &apierr.InvalidParamError{Param: pe.Param, Reason: pe.Reason, Code: code}
	}
	return err
}

// blockingResponses 收集完整回复后按 response 对象返回。
func (s *Server) blockingResponses(w http.ResponseWriter, r *http.Request, body []byte, req *protocol.ResponsesRequest, model registry.Model, key string, timeout time.Duration, start time.Time, promptLen int) {
	var raw []byte
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
		writeAPIErr(w, errOpenAI, err)
		return
	}

	out := protocol.FromChatCompletion(raw, req, model.ID, promptLen)
	outLen := len([]rune(out.Text)) + len([]rune(out.Thinking))
	s.saveResponse(req, out.Payload, body, out.ChatMessage)
	s.record(requestID(r.Context()), key, acct, model.ID, false, http.StatusOK, start, promptLen, outLen, tried, nil)
	writeJSON(w, http.StatusOK, out.Payload)
}

// streamResponses 把上游 SSE 转换为 Responses API 事件流。
func (s *Server) streamResponses(w http.ResponseWriter, r *http.Request, body []byte, req *protocol.ResponsesRequest, model registry.Model, key string, timeout time.Duration, start time.Time, promptLen int) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeRequestErr(w, http.StatusInternalServerError, "服务端不支持流式响应",
			"server_error", apierr.CodeInternal, "stream")
		return
	}

	st := protocol.NewResponsesStream(req, model.ID, promptLen)
	var (
		started   bool
		outLen    int
		streamErr error
	)

	writeEvents := func(evs []protocol.StreamEvent) error {
		if len(evs) == 0 {
			return nil
		}
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
		for _, ev := range evs {
			raw, err := json.Marshal(ev.Data)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, raw); err != nil {
				return err
			}
			flusher.Flush()
		}
		return nil
	}

	acct, tried, err := s.withAccountRetry(r.Context(), timeout, func(ctx context.Context, acct *codearts.Account) error {
		return s.streamOnce(ctx, acct, model, body, func(chunk []byte) error {
			evs, ferr := st.Feed(chunk)
			if ferr != nil {
				return ferr
			}
			outLen = st.OutputRunes()
			return writeEvents(evs)
		})
	})
	streamErr = err

	if !started {
		// 上游连一次都没成功：按普通 HTTP 错误返回，客户端能拿到明确状态码。
		if err == nil {
			err = codearts.UpstreamBusinessError(http.StatusBadGateway, "上游没有返回任何分片")
		}
		e := apierr.From(err)
		s.record(requestID(r.Context()), key, acct, model.ID, true, e.Status, start, promptLen, outLen, tried, err)
		writeAPIErr(w, errOpenAI, err)
		return
	}

	if streamErr != nil {
		e := apierr.From(streamErr)
		_ = writeEvents(st.Fail(streamErr.Error(), e.Code))
	} else {
		_ = writeEvents(st.Finish())
		// 只有成功的响应才值得保存：失败的结果续写没有意义，
		// 存下来只会让 previous_response_id 指向一段残缺上下文。
		s.saveResponse(req, st.Response(), body, st.ChatMessage())
	}
	s.record(requestID(r.Context()), key, acct, model.ID, true,
		apierr.From(streamErr).Status, start, promptLen, outLen, tried, streamErr)
}

// saveResponse 把一次成功的响应存进内存（store=false 时跳过）。
//
// messages 快照取自**实际发给上游**的请求体（含 previous_response_id 带进来的
// 历史），再补上本轮 assistant 回复 —— 下一次续写直接当历史消息用。
func (s *Server) saveResponse(req *protocol.ResponsesRequest, payload map[string]any, upBody []byte, assistant map[string]any) {
	if req == nil || !req.StoreEnabled() || payload == nil {
		return
	}
	id, _ := payload["id"].(string)
	if id == "" {
		return
	}
	var parsed struct {
		Messages []map[string]any `json:"messages"`
	}
	_ = json.Unmarshal(upBody, &parsed)
	msgs := make([]map[string]any, 0, len(parsed.Messages)+1)
	msgs = append(msgs, parsed.Messages...)
	if assistant != nil {
		msgs = append(msgs, assistant)
	}
	s.responses().put(&storedResponse{
		ID:        id,
		CreatedAt: time.Now().Unix(),
		Response:  payload,
		Messages:  msgs,
		Input:     responseInputItems(req.Input),
	})
}

// responseInputItems 把原始 input 归一成条目数组（input_items 接口用）。
func responseInputItems(raw json.RawMessage) []any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []any{map[string]any{
			"type": "message", "role": "user", "status": "completed",
			"content": []any{map[string]any{"type": "input_text", "text": s}},
		}}
	}
	var items []any
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	return items
}

// handleResponsesInputTokens 是 POST /v1/responses/input_tokens。
//
// 只做本地估算，不打上游：官方语义也是「预估」。因此不扣积分、不需要账号。
func (s *Server) handleResponsesInputTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeRequestErr(w, http.StatusMethodNotAllowed, "仅支持 POST", "invalid_request_error", "", "")
		return
	}
	r = withRequestIDValue(w, r)
	body, ok := readBody(w, r, errOpenAI)
	if !ok {
		return
	}
	var req protocol.ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRequestErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error(),
			"invalid_request_error", apierr.CodeInvalidValue, "")
		return
	}
	upReq, _, err := protocol.ToChatRequest(&req, req.Model, nil)
	if err != nil {
		writeAPIErr(w, errOpenAI, responsesParamErr(err))
		return
	}
	raw, err := json.Marshal(upReq)
	if err != nil {
		writeRequestErr(w, http.StatusInternalServerError, "请求体序列化失败",
			"server_error", apierr.CodeInternal, "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":       "response.input_tokens",
		"input_tokens": protocol.EstimatePromptLen(raw),
	})
}

// handleResponsesByID 处理 GET / DELETE /v1/responses/{id}。
func (s *Server) handleResponsesByID(w http.ResponseWriter, r *http.Request) {
	r = withRequestIDValue(w, r)
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeRequestErr(w, http.StatusNotFound, "未指定响应 id", "invalid_request_error", apierr.CodeResponseNotFound, "id")
		return
	}
	switch r.Method {
	case http.MethodGet:
		entry, ok := s.responses().get(id)
		if !ok {
			writeAPIErr(w, errOpenAI, &apierr.ResponseNotFound{ID: id})
			return
		}
		writeJSON(w, http.StatusOK, entry.Response)
	case http.MethodDelete:
		if !s.responses().delete(id) {
			writeAPIErr(w, errOpenAI, &apierr.ResponseNotFound{ID: id})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "object": "response.deleted", "deleted": true,
		})
	default:
		writeRequestErr(w, http.StatusMethodNotAllowed, "仅支持 GET / DELETE",
			"invalid_request_error", "", "")
	}
}

// handleResponsesInputItems 处理 GET /v1/responses/{id}/input_items。
func (s *Server) handleResponsesInputItems(w http.ResponseWriter, r *http.Request) {
	r = withRequestIDValue(w, r)
	if r.Method != http.MethodGet {
		writeRequestErr(w, http.StatusMethodNotAllowed, "仅支持 GET", "invalid_request_error", "", "")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	entry, ok := s.responses().get(id)
	if !ok {
		writeAPIErr(w, errOpenAI, &apierr.ResponseNotFound{ID: id})
		return
	}
	data := entry.Input
	if data == nil {
		data = []any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":   "list",
		"data":     data,
		"first_id": firstItemID(data),
		"last_id":  lastItemID(data),
		"has_more": false,
	})
}

func firstItemID(items []any) any {
	if len(items) == 0 {
		return nil
	}
	return itemID(items[0])
}

func lastItemID(items []any) any {
	if len(items) == 0 {
		return nil
	}
	return itemID(items[len(items)-1])
}

func itemID(v any) any {
	if m, ok := v.(map[string]any); ok {
		if id, ok := m["id"]; ok {
			return id
		}
	}
	return nil
}

// responses 返回响应存储（首次使用时惰性创建）。
//
// newTestServer 直接构造 &Server{…} 而不走 New，所以这里不能假设字段已初始化。
func (s *Server) responses() *responseStore {
	s.respOnce.Do(func() {
		s.respStore = newResponseStore()
	})
	return s.respStore
}
