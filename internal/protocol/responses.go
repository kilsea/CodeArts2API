package protocol

// OpenAI Responses API 与 Chat Completions 之间的**纯数据转换**。
//
// 上游（CodeArts inferhub）只提供 Chat Completions，因此 Responses 面只能
// 翻译：请求把 input / instructions / tools 变成 messages，响应把
// chat.completion / chat.completion.chunk 变成 response 对象与事件流。
//
// 与 anthropic.go 一样，本文件不碰 HTTP、不碰 context、不依赖任何内部包：
// 输入字节 / 结构体，输出结构体。流式转换做成一只有状态的状态机
// （ResponsesStream.Feed / Finish），这样 40+ 条事件的顺序与载荷可以脱离
// 网络被完整单测 —— 见 adr-0003。

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// ResponsesRequest 是 OpenAI Responses API 请求体中网关关心的字段。
//
// 不认识的字段被 json.Unmarshal 静默忽略，这是刻意的：Responses 的规范还在
// 演进，硬拒所有未知字段会让新 SDK 直接不可用。真正**会影响语义**的字段
// （background / conversation / truncation …）下面单独列出来显式拒绝。
type ResponsesRequest struct {
	Model              string              `json:"model"`
	Input              json.RawMessage     `json:"input"`
	Instructions       json.RawMessage     `json:"instructions"`
	Stream             bool                `json:"stream"`
	MaxOutputTokens    *int                `json:"max_output_tokens"`
	Temperature        *float64            `json:"temperature"`
	TopP               *float64            `json:"top_p"`
	Tools              []ResponsesTool     `json:"tools"`
	ToolChoice         json.RawMessage     `json:"tool_choice"`
	ParallelToolCalls  *bool               `json:"parallel_tool_calls"`
	Text               *ResponsesText      `json:"text"`
	Reasoning          *ResponsesReasoning `json:"reasoning"`
	Store              *bool               `json:"store"`
	PreviousResponseID string              `json:"previous_response_id"`
	Metadata           map[string]any      `json:"metadata"`
	User               string              `json:"user"`

	// 以下字段网关**不支持**，出现即 400（二选一原则，绝不静默降级）。
	Background       *bool           `json:"background"`
	Conversation     json.RawMessage `json:"conversation"`
	Prompt           json.RawMessage `json:"prompt"`
	Truncation       string          `json:"truncation"`
	Include          []string        `json:"include"`
	MaxToolCalls     *int            `json:"max_tool_calls"`
	Logprobs         *bool           `json:"logprobs"`
	TopLogprobs      *int            `json:"top_logprobs"`
	SafetyIdentifier string          `json:"safety_identifier"`
	PromptCacheKey   string          `json:"prompt_cache_key"`
	ServiceTier      string          `json:"service_tier"`
}

// ResponsesTool 是 Responses API 的工具声明（扁平结构）。
type ResponsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict"`
}

// ResponsesText 是 text 参数（输出格式控制）。
type ResponsesText struct {
	Format    *ResponsesFormat `json:"format"`
	Verbosity string           `json:"verbosity"`
}

// ResponsesFormat 是 text.format。
type ResponsesFormat struct {
	Type   string          `json:"type"`
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	Strict *bool           `json:"strict"`
}

// ResponsesReasoning 是 reasoning 参数（推理强度 / 摘要）。
type ResponsesReasoning struct {
	Effort  string `json:"effort"`
	Summary string `json:"summary"`
}

// ParamError 是转换层发现的「参数不合法 / 不支持」。
//
// 不直接用 internal/apierr 是因为 protocol 不依赖内部包（见 SPEC）；
// server 层把它翻译成 apierr.InvalidParamError，状态码与错误码由 apierr 决定。
type ParamError struct {
	// Param 是请求体里的字段路径（如 "input[2].content[0]"）。
	Param string
	// Reason 是一句能直接展示给用户的话。
	Reason string
	// Unsupported 为真表示「字段合法但网关不支持」，对外码是
	// unsupported_parameter；为假表示取值/类型不合法。
	Unsupported bool
}

// Error 实现 error。
func (e *ParamError) Error() string { return e.Reason }

// StoreEnabled 报告本次请求是否要求网关保存响应（Responses 规范默认 true）。
func (r *ResponsesRequest) StoreEnabled() bool { return r.Store == nil || *r.Store }

// InstructionsText 提取 instructions（字符串或文本分段数组）。
func InstructionsText(raw json.RawMessage) string { return SystemText(raw) }

// ToChatRequest 把 Responses 请求翻译成 Chat Completions 请求体。
//
// prefix 是 previous_response_id 指向的历史消息（由调用方从响应存储取出），
// 会被放在本次 input 之前；instructions 非空时覆盖历史里的 system 提示。
//
// 返回被摘掉的字段名（写 X-Gateway-Unsupported）与错误。
func ToChatRequest(req *ResponsesRequest, model string, prefix []map[string]any) (map[string]any, []string, error) {
	if err := checkResponsesUnsupported(req); err != nil {
		return nil, nil, err
	}

	var dropped []string
	out := map[string]any{"model": model, "stream": req.Stream}

	if req.MaxOutputTokens != nil {
		if *req.MaxOutputTokens <= 0 {
			return nil, nil, &ParamError{Param: "max_output_tokens", Reason: "max_output_tokens 必须是正整数"}
		}
		out["max_tokens"] = *req.MaxOutputTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if req.ParallelToolCalls != nil {
		out["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	if req.User != "" {
		out["user"] = req.User
	}

	// text.format：text / json_object 能翻译，json_schema 上游不认。
	if req.Text != nil {
		if req.Text.Verbosity != "" {
			dropped = append(dropped, "text.verbosity")
		}
		if f := req.Text.Format; f != nil {
			switch f.Type {
			case "", "text":
			case "json_object":
				out["response_format"] = map[string]any{"type": "json_object"}
			default:
				return nil, nil, &ParamError{
					Param:       "text.format.type",
					Unsupported: true,
					Reason:      "text.format.type=" + f.Type + " 暂不支持（只支持 text / json_object）",
				}
			}
		}
	}

	// 这些字段上游没有对应能力，摘掉并在响应头声明。
	if req.Reasoning != nil {
		if req.Reasoning.Effort != "" {
			dropped = append(dropped, "reasoning.effort")
		}
		if req.Reasoning.Summary != "" {
			dropped = append(dropped, "reasoning.summary")
		}
	}
	if len(req.Include) > 0 {
		dropped = append(dropped, "include")
	}
	if req.MaxToolCalls != nil {
		dropped = append(dropped, "max_tool_calls")
	}
	if req.SafetyIdentifier != "" {
		dropped = append(dropped, "safety_identifier")
	}
	if req.PromptCacheKey != "" {
		dropped = append(dropped, "prompt_cache_key")
	}
	if req.ServiceTier != "" {
		dropped = append(dropped, "service_tier")
	}
	if req.Logprobs != nil && *req.Logprobs {
		dropped = append(dropped, "logprobs")
	}
	if req.TopLogprobs != nil {
		dropped = append(dropped, "top_logprobs")
	}

	// messages = 历史（previous_response_id）+ instructions + 本次 input
	msgs := make([]map[string]any, 0, len(prefix)+4)
	msgs = append(msgs, prefix...)
	if sys := InstructionsText(req.Instructions); sys != "" {
		// 新的 instructions 覆盖历史里的 system 提示：官方语义是「本次请求的
		// instructions 生效」，把旧的留在前面等于让两段提示互相打架。
		for len(msgs) > 0 {
			role := Str(msgs[0]["role"])
			if role != "system" && role != "developer" {
				break
			}
			msgs = msgs[1:]
		}
		msgs = append([]map[string]any{{"role": "system", "content": sys}}, msgs...)
	}
	inputMsgs, err := ResponsesInputMessages(req.Input)
	if err != nil {
		return nil, nil, err
	}
	msgs = append(msgs, inputMsgs...)
	if len(msgs) == 0 {
		return nil, nil, &ParamError{Param: "input", Reason: "input 不能为空"}
	}
	out["messages"] = msgs

	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for i, t := range req.Tools {
			if t.Type != "" && t.Type != "function" {
				return nil, nil, &ParamError{
					Param:       "tools[" + strconv.Itoa(i) + "].type",
					Unsupported: true,
					Reason:      "工具类型 " + t.Type + " 暂不支持（只支持 function）",
				}
			}
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if len(t.Parameters) > 0 {
				fn["parameters"] = json.RawMessage(t.Parameters)
			} else {
				fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		out["tools"] = tools
		if len(req.ToolChoice) > 0 {
			tc, err := ResponsesToolChoice(req.ToolChoice)
			if err != nil {
				return nil, nil, err
			}
			out["tool_choice"] = tc
		}
	}

	// 流式必须显式要 usage：上游默认不发，缺了 response.completed.usage 就是假的。
	if req.Stream {
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	return out, dropped, nil
}

// checkResponsesUnsupported 拦截网关明确不支持的字段。
func checkResponsesUnsupported(req *ResponsesRequest) error {
	if req.Background != nil && *req.Background {
		return &ParamError{
			Param:       "background",
			Unsupported: true,
			Reason:      "background 暂不支持：网关不提供异步作业与进程重启后的结果恢复",
		}
	}
	if !isEmptyJSON(req.Conversation) {
		return &ParamError{Param: "conversation", Unsupported: true, Reason: "conversation 暂不支持，请改用 previous_response_id"}
	}
	if !isEmptyJSON(req.Prompt) {
		return &ParamError{Param: "prompt", Unsupported: true, Reason: "prompt 模板暂不支持"}
	}
	switch req.Truncation {
	case "", "disabled":
	default:
		return &ParamError{
			Param:       "truncation",
			Unsupported: true,
			Reason:      "truncation=" + req.Truncation + " 暂不支持：网关不做上下文自动裁剪，请自行缩短 input",
		}
	}
	return nil
}

// isEmptyJSON 报告字段是否缺失 / null / 空对象 / 空数组。
func isEmptyJSON(raw json.RawMessage) bool {
	t := strings.TrimSpace(string(raw))
	return t == "" || t == "null" || t == "{}" || t == "[]"
}

// ResponsesToolChoice 把 Responses 的 tool_choice 映射为 OpenAI 形态。
func ResponsesToolChoice(raw json.RawMessage) (any, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto", "none", "required":
			return s, nil
		}
		return nil, &ParamError{Param: "tool_choice", Reason: "tool_choice=" + s + " 不是合法取值"}
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, &ParamError{Param: "tool_choice", Reason: "tool_choice 必须是字符串或对象"}
	}
	switch rawStr(obj["type"]) {
	case "function":
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": rawStr(obj["name"])},
		}, nil
	case "allowed_tools":
		return nil, &ParamError{Param: "tool_choice", Unsupported: true, Reason: "tool_choice.type=allowed_tools 暂不支持"}
	default:
		return nil, &ParamError{Param: "tool_choice.type", Reason: "tool_choice.type=" + rawStr(obj["type"]) + " 不是合法取值"}
	}
}

// ResponsesInputMessages 把 input（字符串或条目数组）翻译成 OpenAI messages。
func ResponsesInputMessages(raw json.RawMessage) ([]map[string]any, error) {
	if isEmptyJSON(raw) {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []map[string]any{{"role": "user", "content": s}}, nil
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, &ParamError{Param: "input", Reason: "input 必须是字符串或条目数组"}
	}

	out := make([]map[string]any, 0, len(items))
	for i, it := range items {
		param := "input[" + strconv.Itoa(i) + "]"
		switch typ := rawStr(it["type"]); typ {
		case "", "message":
			msg, err := responsesMessage(it, param)
			if err != nil {
				return nil, err
			}
			out = append(out, msg)
		case "function_call":
			call := map[string]any{
				"id":   rawStr(it["call_id"]),
				"type": "function",
				"function": map[string]any{
					"name":      rawStr(it["name"]),
					"arguments": rawStr(it["arguments"]),
				},
			}
			// 连续的 function_call 必须合并进同一条 assistant 消息：上游按
			// 「一条 assistant 的 tool_calls ↔ 后续 tool 消息」配对，
			// 拆成多条会让 tool 结果找不到归属（ModelArts.81001）。
			if n := len(out); n > 0 {
				last := out[n-1]
				if Str(last["role"]) == "assistant" && Str(last["content"]) == "" {
					if calls, ok := last["tool_calls"].([]any); ok {
						last["tool_calls"] = append(calls, call)
						continue
					}
				}
			}
			out = append(out, map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{call}})
		case "function_call_output":
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": rawStr(it["call_id"]),
				"content":      rawStr(it["output"]),
			})
		case "reasoning":
			// 不回流思维链：上游不需要，回传只是白占上下文。
		case "item_reference":
			return nil, &ParamError{Param: param, Unsupported: true, Reason: "item_reference 暂不支持：请直接回传条目内容"}
		default:
			return nil, &ParamError{Param: param + ".type", Unsupported: true, Reason: "输入条目类型 " + typ + " 暂不支持"}
		}
	}
	return out, nil
}

// responsesMessage 翻译一条 message 条目。
func responsesMessage(it map[string]json.RawMessage, param string) (map[string]any, error) {
	role := rawStr(it["role"])
	if role == "" {
		role = "user"
	}
	msg := map[string]any{"role": role}
	content := it["content"]

	var s string
	if json.Unmarshal(content, &s) == nil {
		msg["content"] = s
		return msg, nil
	}
	if isEmptyJSON(content) {
		msg["content"] = ""
		return msg, nil
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(content, &parts); err != nil {
		return nil, &ParamError{Param: param + ".content", Reason: "content 必须是字符串或分段数组"}
	}

	converted := make([]any, 0, len(parts))
	for j, p := range parts {
		pparam := param + ".content[" + strconv.Itoa(j) + "]"
		switch typ := rawStr(p["type"]); typ {
		case "input_text", "output_text", "text", "refusal":
			converted = append(converted, map[string]any{"type": "text", "text": rawStr(p["text"])})
		case "input_image":
			url := rawStr(p["image_url"])
			if url == "" {
				return nil, &ParamError{Param: pparam, Unsupported: true, Reason: "input_image 只支持 image_url（file_id 暂不支持）"}
			}
			img := map[string]any{"url": url}
			if d := rawStr(p["detail"]); d != "" {
				img["detail"] = d
			}
			converted = append(converted, map[string]any{"type": "image_url", "image_url": img})
		case "input_file":
			return nil, &ParamError{Param: pparam, Unsupported: true, Reason: "input_file 暂不支持：请把文件内容作为文本传入"}
		default:
			return nil, &ParamError{Param: pparam + ".type", Unsupported: true, Reason: "内容分段类型 " + typ + " 暂不支持"}
		}
	}
	// 单段纯文本摊平成字符串，与 OpenAI 的常见写法一致。
	if len(converted) == 1 {
		if p, ok := converted[0].(map[string]any); ok && p["type"] == "text" {
			msg["content"] = p["text"]
			return msg, nil
		}
	}
	msg["content"] = converted
	return msg, nil
}

// --- Chat Completions → Responses ---

// ResponsesReply 是转换后的响应对象及统计信息。
type ResponsesReply struct {
	// Payload 是最终对外响应体（response 对象）。
	Payload map[string]any
	// Text / Thinking 供调用方统计输出长度（写日志用）。
	Text     string
	Thinking string
	// ChatMessage 是本轮 assistant 在 **Chat Completions** 里的形态，
	// 存进响应存储后用 previous_response_id 续写时直接当历史消息用。
	ChatMessage map[string]any
}

// respUsage 是上游 usage 的归一形态。
type respUsage struct {
	Prompt     int
	Completion int
	Cached     int
	Reasoning  int
}

// responsesUsage 输出 Responses 规范的 usage 对象。
func responsesUsage(u respUsage) map[string]any {
	if u.Prompt == 0 && u.Completion == 0 {
		return nil
	}
	return map[string]any{
		"input_tokens":          u.Prompt,
		"input_tokens_details":  map[string]any{"cached_tokens": u.Cached},
		"output_tokens":         u.Completion,
		"output_tokens_details": map[string]any{"reasoning_tokens": u.Reasoning},
		"total_tokens":          u.Prompt + u.Completion,
	}
}

// responseStatus 由 finish_reason 推导状态与 incomplete_details。
func responseStatus(finish string) (string, any) {
	switch finish {
	case "length":
		return "incomplete", map[string]any{"reason": "max_output_tokens"}
	case "content_filter":
		return "incomplete", map[string]any{"reason": "content_filter"}
	}
	return "completed", nil
}

// FromChatCompletion 把一次非流式 Chat Completions 响应翻译成 Responses 响应。
func FromChatCompletion(raw []byte, req *ResponsesRequest, model string, promptLen int) ResponsesReply {
	var resp struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(raw, &resp)

	var (
		text, thinking string
		finish         string
		calls          []respToolCall
	)
	if len(resp.Choices) > 0 {
		ch := resp.Choices[0]
		text = ch.Message.Content
		thinking = ch.Message.ReasoningContent
		finish = ch.FinishReason
		for _, tc := range ch.Message.ToolCalls {
			calls = append(calls, respToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
		}
	}

	usage := respUsage{
		Prompt:     resp.Usage.PromptTokens,
		Completion: resp.Usage.CompletionTokens,
		Cached:     resp.Usage.PromptTokensDetails.CachedTokens,
		Reasoning:  resp.Usage.CompletionTokensDetails.ReasoningTokens,
	}
	if usage.Prompt == 0 {
		usage.Prompt = promptLen
	}
	if usage.Completion == 0 {
		usage.Completion = len([]rune(text)) + len([]rune(thinking))
	}
	if usage.Reasoning == 0 {
		usage.Reasoning = len([]rune(thinking))
	}

	items, msg := buildOutputItems(text, thinking, calls)
	status, incomplete := responseStatus(finish)

	return ResponsesReply{
		Text:        text,
		Thinking:    thinking,
		ChatMessage: msg,
		Payload:     responseObject(req, model, status, incomplete, items, usage),
	}
}

// respToolCall 是上游一次工具调用的归一形态。
type respToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// buildOutputItems 组装 output 数组，并返回本轮 assistant 的 Chat 形态消息。
//
// 顺序固定为 reasoning → message → function_call：客户端与官方 SDK 都按
// output_index 递增读取，乱序会让它把函数调用当成「消息之前的内容」。
func buildOutputItems(text, thinking string, calls []respToolCall) ([]any, map[string]any) {
	items := make([]any, 0, 2+len(calls))
	msg := map[string]any{"role": "assistant"}

	if thinking != "" {
		items = append(items, map[string]any{
			"type": "reasoning",
			"id":   "rs_" + randomHex(12),
			"summary": []any{
				map[string]any{"type": "summary_text", "text": thinking},
			},
		})
	}
	if text != "" || len(calls) == 0 {
		items = append(items, map[string]any{
			"type":   "message",
			"id":     "msg_" + randomHex(12),
			"status": "completed",
			"role":   "assistant",
			"content": []any{
				map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
			},
		})
	}
	msg["content"] = text

	chatCalls := make([]any, 0, len(calls))
	for _, c := range calls {
		id := c.ID
		if id == "" {
			id = "call_" + randomHex(12)
		}
		items = append(items, map[string]any{
			"type":      "function_call",
			"id":        "fc_" + randomHex(12),
			"call_id":   id,
			"name":      c.Name,
			"arguments": c.Arguments,
			"status":    "completed",
		})
		chatCalls = append(chatCalls, map[string]any{
			"id":       id,
			"type":     "function",
			"function": map[string]any{"name": c.Name, "arguments": c.Arguments},
		})
	}
	if len(chatCalls) > 0 {
		msg["tool_calls"] = chatCalls
		msg["content"] = nil
	}
	return items, msg
}

// responseObject 组装完整的 response 对象。
func responseObject(req *ResponsesRequest, model, status string, incomplete any, items []any, usage respUsage) map[string]any {
	if items == nil {
		items = []any{}
	}
	out := map[string]any{
		"id":                   "resp_" + randomHex(24),
		"object":               "response",
		"created_at":           time.Now().Unix(),
		"status":               status,
		"background":           false,
		"error":                nil,
		"incomplete_details":   incomplete,
		"instructions":         nil,
		"max_output_tokens":    nil,
		"model":                model,
		"output":               items,
		"parallel_tool_calls":  true,
		"previous_response_id": nil,
		"reasoning":            map[string]any{"effort": nil, "summary": nil},
		"store":                req.StoreEnabled(),
		"temperature":          nil,
		"text":                 map[string]any{"format": map[string]any{"type": "text"}},
		"tool_choice":          "auto",
		"tools":                []any{},
		"top_p":                nil,
		"truncation":           "disabled",
		"usage":                responsesUsage(usage),
		"metadata":             map[string]any{},
		"user":                 nil,
	}
	if sys := InstructionsText(req.Instructions); sys != "" {
		out["instructions"] = sys
	}
	if req.MaxOutputTokens != nil {
		out["max_output_tokens"] = *req.MaxOutputTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if req.ParallelToolCalls != nil {
		out["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	if req.PreviousResponseID != "" {
		out["previous_response_id"] = req.PreviousResponseID
	}
	if req.User != "" {
		out["user"] = req.User
	}
	if len(req.Metadata) > 0 {
		out["metadata"] = req.Metadata
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			params := any(map[string]any{"type": "object", "properties": map[string]any{}})
			if len(t.Parameters) > 0 {
				params = json.RawMessage(t.Parameters)
			}
			item := map[string]any{
				"type":        "function",
				"name":        t.Name,
				"description": t.Description,
				"parameters":  params,
			}
			if t.Strict != nil {
				item["strict"] = *t.Strict
			}
			tools = append(tools, item)
		}
		out["tools"] = tools
	}
	// 回显本次实际生效的输出格式（json_object 时不能还说 text）。
	if req.Text != nil && req.Text.Format != nil && req.Text.Format.Type != "" {
		out["text"] = map[string]any{"format": map[string]any{"type": req.Text.Format.Type}}
	}
	return out
}

// --- 流式：Chat Completions 分片 → Responses 事件 ---

// StreamEvent 是一条 Responses API 流式事件（server 层拼成 event:/data:）。
type StreamEvent struct {
	Type string
	Data map[string]any
}

// ResponsesStream 把上游 Chat Completions 分片转换为 Responses 事件流。
//
// 用法：每收到一个分片调用 Feed，流正常结束时调用 Finish，出错时调用 Fail。
// 三个方法都会返回「本次应写出的事件」，调用方按序写 SSE 即可。
type ResponsesStream struct {
	req       *ResponsesRequest
	model     string
	promptLen int

	id        string
	createdAt int64
	seq       int

	started   bool
	output    []any
	nextIndex int

	// 当前打开的输出块
	kind     string // "" / "reasoning" / "message" / "function_call"
	curIndex int
	itemID   string
	callID   string
	name     string

	reasonBuf strings.Builder
	textBuf   strings.Builder
	argBuf    strings.Builder

	// totalText / totalReason 是整条流的累计字符数（usage 兜底用）：
	// 块切换会重置 reasonBuf / textBuf，不能拿它们算总量。
	totalText   int
	totalReason int

	finishReason string
	usage        respUsage
	sawUsage     bool
	// chatCalls 按上游 tool_calls 的 index 归档，用于组装 assistant 消息。
	chatCalls map[int]map[string]any
	callOrder []int
}

// NewResponsesStream 构造一个流式转换器。promptLen 仅在 upstream 不回 usage 时兜底。
func NewResponsesStream(req *ResponsesRequest, model string, promptLen int) *ResponsesStream {
	return &ResponsesStream{
		req:       req,
		model:     model,
		promptLen: promptLen,
		id:        "resp_" + randomHex(24),
		createdAt: time.Now().Unix(),
		chatCalls: map[int]map[string]any{},
	}
}

// Feed 处理一个上游分片，返回本次应写出的事件。
func (s *ResponsesStream) Feed(chunk []byte) ([]StreamEvent, error) {
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
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(chunk, &c); err != nil {
		// 分片解析不了就当空片跳过：上游偶尔会发心跳 / 非标准行，
		// 为此中断整条流比丢掉一个空片更糟。
		return nil, nil
	}

	var evs []StreamEvent
	if !s.started {
		s.started = true
		evs = append(evs, s.openStream()...)
	}

	for _, ch := range c.Choices {
		if ch.Delta.ReasoningContent != "" {
			evs = append(evs, s.appendReasoning(ch.Delta.ReasoningContent)...)
		}
		if ch.Delta.Content != "" {
			evs = append(evs, s.appendText(ch.Delta.Content)...)
		}
		for _, tc := range ch.Delta.ToolCalls {
			evs = append(evs, s.appendToolCall(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments)...)
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			s.finishReason = *ch.FinishReason
		}
	}
	if c.Usage != nil {
		s.usage = respUsage{
			Prompt:     c.Usage.PromptTokens,
			Completion: c.Usage.CompletionTokens,
			Cached:     c.Usage.PromptTokensDetails.CachedTokens,
			Reasoning:  c.Usage.CompletionTokensDetails.ReasoningTokens,
		}
		s.sawUsage = true
	}
	return evs, nil
}

// TokenUsage 返回上游 usage 的 token 数（供网关记积分用）。
//
// ok=false 表示整条流里都没有 usage（上游没回 / 提前断开），调用方应退回
// 字符数或本地估算，而不是把 0 当成「没消耗」。
func (s *ResponsesStream) TokenUsage() (in, cached, out int, ok bool) {
	if !s.sawUsage {
		return 0, 0, 0, false
	}
	return s.usage.Prompt, s.usage.Cached, s.usage.Completion, true
}

// Finish 收尾：关闭当前块并发出 response.completed。
func (s *ResponsesStream) Finish() []StreamEvent {
	evs := s.closeBlock()
	evs = append(evs, s.event("response.completed", map[string]any{"response": s.object()}))
	return evs
}

// Fail 以 response.failed 收尾（流已经开始后唯一的错误出口）。
func (s *ResponsesStream) Fail(msg, code string) []StreamEvent {
	evs := s.closeBlock()
	errObj := map[string]any{"code": code, "message": msg}
	if code == "" {
		errObj["code"] = nil
	}
	obj := s.object()
	obj["status"] = "failed"
	obj["error"] = errObj
	evs = append(evs, s.event("response.failed", map[string]any{"response": obj}))
	return evs
}

// ChatMessage 返回本轮 assistant 在 Chat Completions 里的形态（写响应存储用）。
func (s *ResponsesStream) ChatMessage() map[string]any {
	msg := map[string]any{"role": "assistant", "content": s.textBuf.String()}
	if len(s.callOrder) == 0 {
		return msg
	}
	calls := make([]any, 0, len(s.callOrder))
	for _, idx := range s.callOrder {
		calls = append(calls, s.chatCalls[idx])
	}
	msg["tool_calls"] = calls
	msg["content"] = nil
	return msg
}

// OutputItems 返回已完成（含当前块收尾后）的 output 数组。
//
// 必须在 Finish 之后调用：Finish 会把最后一个块落进 output。
func (s *ResponsesStream) OutputItems() []any { return s.output }

// Response 返回最终的 response 对象；Finish 之后调用与 response.completed
// 里的对象一致（写响应存储用）。
func (s *ResponsesStream) Response() map[string]any { return s.object() }

// OutputRunes 返回累计输出字符数（思维链 + 正文，写日志用）。
func (s *ResponsesStream) OutputRunes() int { return s.totalText + s.totalReason }

// openStream 发出 response.created / response.in_progress。
func (s *ResponsesStream) openStream() []StreamEvent {
	obj := s.object()
	obj["status"] = "in_progress"
	// 尚未产生任何 token，usage 必须为 null：报一组 0 会被客户端当成真实计数。
	obj["usage"] = nil
	return []StreamEvent{
		s.event("response.created", map[string]any{"response": obj}),
		s.event("response.in_progress", map[string]any{"response": obj}),
	}
}

// appendReasoning 累积思维链，必要时打开 reasoning 块。
func (s *ResponsesStream) appendReasoning(delta string) []StreamEvent {
	var evs []StreamEvent
	if s.kind != "reasoning" {
		evs = append(evs, s.closeBlock()...)
		evs = append(evs, s.openReasoning()...)
	}
	s.reasonBuf.WriteString(delta)
	s.totalReason += len([]rune(delta))
	evs = append(evs, s.event("response.reasoning_summary_text.delta", map[string]any{
		"item_id": s.itemID, "output_index": s.curIndex, "summary_index": 0, "delta": delta,
	}))
	return evs
}

// appendText 累积正文，必要时打开 message 块。
func (s *ResponsesStream) appendText(delta string) []StreamEvent {
	var evs []StreamEvent
	if s.kind != "message" {
		evs = append(evs, s.closeBlock()...)
		evs = append(evs, s.openMessage()...)
	}
	s.textBuf.WriteString(delta)
	s.totalText += len([]rune(delta))
	evs = append(evs, s.event("response.output_text.delta", map[string]any{
		"item_id": s.itemID, "output_index": s.curIndex, "content_index": 0, "delta": delta, "logprobs": []any{},
	}))
	return evs
}

// appendToolCall 累积函数调用参数，必要时切换 function_call 块。
//
// 上游只有**首个**分片带 id / name，后续分片只带 arguments —— 所以 id 必须
// 从已归档的调用里回填，否则每个 arguments 分片都会被当成一次新调用。
func (s *ResponsesStream) appendToolCall(idx int, id, name, args string) []StreamEvent {
	var evs []StreamEvent
	call, seen := s.chatCalls[idx]
	if !seen {
		if id == "" {
			id = "call_" + randomHex(12)
		}
		call = map[string]any{
			"id":       id,
			"type":     "function",
			"function": map[string]any{"name": name, "arguments": ""},
		}
		s.chatCalls[idx] = call
		s.callOrder = append(s.callOrder, idx)
	} else {
		if id == "" {
			id = Str(call["id"])
		}
		if name == "" {
			if fn, ok := call["function"].(map[string]any); ok {
				name = Str(fn["name"])
			}
		}
	}
	if !seen || s.kind != "function_call" || s.callID != id {
		evs = append(evs, s.closeBlock()...)
		evs = append(evs, s.openFunctionCall(id, name)...)
	}
	if args != "" {
		s.argBuf.WriteString(args)
		fn, _ := call["function"].(map[string]any)
		fn["arguments"] = s.argBuf.String()
		evs = append(evs, s.event("response.function_call_arguments.delta", map[string]any{
			"item_id": s.itemID, "output_index": s.curIndex, "delta": args,
		}))
	}
	return evs
}

func (s *ResponsesStream) openReasoning() []StreamEvent {
	s.kind = "reasoning"
	s.curIndex = s.nextIndex
	s.nextIndex++
	s.itemID = "rs_" + randomHex(12)
	s.reasonBuf.Reset()
	return []StreamEvent{
		s.event("response.output_item.added", map[string]any{
			"output_index": s.curIndex,
			"item": map[string]any{
				"type": "reasoning", "id": s.itemID,
				"summary": []any{map[string]any{"type": "summary_text", "text": ""}},
			},
		}),
		s.event("response.reasoning_summary_part.added", map[string]any{
			"item_id": s.itemID, "output_index": s.curIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""},
		}),
	}
}

func (s *ResponsesStream) openMessage() []StreamEvent {
	s.kind = "message"
	s.curIndex = s.nextIndex
	s.nextIndex++
	s.itemID = "msg_" + randomHex(12)
	s.textBuf.Reset()
	return []StreamEvent{
		s.event("response.output_item.added", map[string]any{
			"output_index": s.curIndex,
			"item": map[string]any{
				"type": "message", "id": s.itemID, "status": "in_progress",
				"role": "assistant", "content": []any{},
			},
		}),
		s.event("response.content_part.added", map[string]any{
			"item_id": s.itemID, "output_index": s.curIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		}),
	}
}

func (s *ResponsesStream) openFunctionCall(callID, name string) []StreamEvent {
	s.kind = "function_call"
	s.curIndex = s.nextIndex
	s.nextIndex++
	s.itemID = "fc_" + randomHex(12)
	s.callID = callID
	s.name = name
	s.argBuf.Reset()
	return []StreamEvent{
		s.event("response.output_item.added", map[string]any{
			"output_index": s.curIndex,
			"item": map[string]any{
				"type": "function_call", "id": s.itemID, "call_id": callID,
				"name": name, "arguments": "", "status": "in_progress",
			},
		}),
	}
}

// closeBlock 结束当前输出块，发出 done 事件并把条目落进 output。
func (s *ResponsesStream) closeBlock() []StreamEvent {
	if s.kind == "" {
		return nil
	}
	var evs []StreamEvent
	switch s.kind {
	case "reasoning":
		text := s.reasonBuf.String()
		evs = append(evs,
			s.event("response.reasoning_summary_text.done", map[string]any{
				"item_id": s.itemID, "output_index": s.curIndex, "summary_index": 0, "text": text,
			}),
			s.event("response.reasoning_summary_part.done", map[string]any{
				"item_id": s.itemID, "output_index": s.curIndex, "summary_index": 0,
				"part": map[string]any{"type": "summary_text", "text": text},
			}),
		)
		item := map[string]any{
			"type": "reasoning", "id": s.itemID,
			"summary": []any{map[string]any{"type": "summary_text", "text": text}},
		}
		s.output = append(s.output, item)
		evs = append(evs, s.event("response.output_item.done", map[string]any{
			"output_index": s.curIndex, "item": item,
		}))
	case "message":
		text := s.textBuf.String()
		part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
		evs = append(evs,
			s.event("response.output_text.done", map[string]any{
				"item_id": s.itemID, "output_index": s.curIndex, "content_index": 0, "text": text, "logprobs": []any{},
			}),
			s.event("response.content_part.done", map[string]any{
				"item_id": s.itemID, "output_index": s.curIndex, "content_index": 0, "part": part,
			}),
		)
		item := map[string]any{
			"type": "message", "id": s.itemID, "status": "completed",
			"role": "assistant", "content": []any{part},
		}
		s.output = append(s.output, item)
		evs = append(evs, s.event("response.output_item.done", map[string]any{
			"output_index": s.curIndex, "item": item,
		}))
	case "function_call":
		args := s.argBuf.String()
		evs = append(evs, s.event("response.function_call_arguments.done", map[string]any{
			"item_id": s.itemID, "output_index": s.curIndex, "arguments": args,
		}))
		item := map[string]any{
			"type": "function_call", "id": s.itemID, "call_id": s.callID,
			"name": s.name, "arguments": args, "status": "completed",
		}
		s.output = append(s.output, item)
		evs = append(evs, s.event("response.output_item.done", map[string]any{
			"output_index": s.curIndex, "item": item,
		}))
	}
	s.kind = ""
	return evs
}

// object 组装当前响应对象（流式过程中 output 可能还不完整）。
func (s *ResponsesStream) object() map[string]any {
	u := s.usage
	if !s.sawUsage {
		u = respUsage{Prompt: s.promptLen, Reasoning: s.totalReason}
		u.Completion = s.totalText + u.Reasoning
	}
	status, incomplete := responseStatus(s.finishReason)
	obj := responseObject(s.req, s.model, status, incomplete, s.output, u)
	obj["id"] = s.id
	obj["created_at"] = s.createdAt
	return obj
}

// event 生成一条带 sequence_number 的事件。
func (s *ResponsesStream) event(typ string, data map[string]any) StreamEvent {
	data["type"] = typ
	data["sequence_number"] = s.seq
	s.seq++
	return StreamEvent{Type: typ, Data: data}
}

// rawStr 从 map[string]json.RawMessage 取字符串字段。
func rawStr(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}
