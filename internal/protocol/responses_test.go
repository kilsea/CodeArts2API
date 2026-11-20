package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// 本文件是 Responses API ↔ Chat Completions 协议转换的**金标准**测试。
//
// 转换层出错不会报错，只会让客户端拿到语义错误的结果：input 条目顺序错、
// function_call 没合并、instructions 被当成 user 消息、流式事件缺一条 done ——
// 这些都必须在这里钉死，而不是等官方 SDK 在线上崩。

// decodeReq 解析一份 Responses 请求体。
func decodeReq(t *testing.T, raw string) *ResponsesRequest {
	t.Helper()
	var req ResponsesRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("请求体解析失败: %v", err)
	}
	return &req
}

// TestResponsesStringInput 覆盖最小请求：input 是字符串、instructions 是字符串。
func TestResponsesStringInput(t *testing.T) {
	req := decodeReq(t, `{
		"model":"glm-5.3",
		"instructions":"你是助手",
		"input":"你好",
		"max_output_tokens":128,
		"temperature":0.5
	}`)
	out, dropped, err := ToChatRequest(req, "glm-5.3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 0 {
		t.Errorf("不该有被摘掉的字段: %v", dropped)
	}
	jsonEq(t, out, `{
		"model":"glm-5.3",
		"stream":false,
		"max_tokens":128,
		"temperature":0.5,
		"messages":[
			{"role":"system","content":"你是助手"},
			{"role":"user","content":"你好"}
		]
	}`)
}

// TestResponsesInputItems 覆盖 message / function_call / function_call_output
// 三种条目的顺序与形态。
func TestResponsesInputItems(t *testing.T) {
	req := decodeReq(t, `{
		"model":"glm-5.3",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"北京天气"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"我查一下"}]},
			{"type":"function_call","call_id":"call_1","name":"weather","arguments":"{\"city\":\"北京\"}"},
			{"type":"function_call","call_id":"call_2","name":"time","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"晴"},
			{"type":"function_call_output","call_id":"call_2","output":"12:00"}
		]
	}`)
	out, _, err := ToChatRequest(req, "glm-5.3", nil)
	if err != nil {
		t.Fatal(err)
	}
	// 关键点：两条 function_call 必须合并进同一条 assistant 消息，
	// 否则上游会把 tool 结果当成「找不到归属」而报 ModelArts.81001。
	jsonEq(t, out, `{
		"model":"glm-5.3",
		"stream":false,
		"messages":[
			{"role":"user","content":"北京天气"},
			{"role":"assistant","content":"我查一下"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"北京\"}"}},
				{"id":"call_2","type":"function","function":{"name":"time","arguments":"{}"}}
			]},
			{"role":"tool","tool_call_id":"call_1","content":"晴"},
			{"role":"tool","tool_call_id":"call_2","content":"12:00"}
		]
	}`)
}

// TestResponsesInstructionsOverrideHistory 覆盖 previous_response_id 续写时
// 新 instructions 覆盖旧 system 提示的语义。
func TestResponsesInstructionsOverrideHistory(t *testing.T) {
	req := decodeReq(t, `{"model":"glm-5.3","instructions":"新提示","input":"继续"}`)
	prefix := []map[string]any{
		{"role": "system", "content": "旧提示"},
		{"role": "user", "content": "第一轮"},
		{"role": "assistant", "content": "回答"},
	}
	out, _, err := ToChatRequest(req, "glm-5.3", prefix)
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := out["messages"].([]map[string]any)
	if len(msgs) != 4 {
		t.Fatalf("消息数 = %d，期望 4（新 system + 历史 2 条 + 新 user）", len(msgs))
	}
	if msgs[0]["content"] != "新提示" {
		t.Errorf("首条 system = %v，期望新提示", msgs[0]["content"])
	}
	if msgs[1]["content"] != "第一轮" {
		t.Errorf("历史第一条 = %v，期望「第一轮」", msgs[1]["content"])
	}
}

// TestResponsesToolsAndChoice 覆盖扁平工具声明与 tool_choice 的翻译。
func TestResponsesToolsAndChoice(t *testing.T) {
	req := decodeReq(t, `{
		"model":"glm-5.3",
		"input":"查天气",
		"tools":[{"type":"function","name":"weather","description":"查天气",
			"parameters":{"type":"object","properties":{"city":{"type":"string"}}}}],
		"tool_choice":{"type":"function","name":"weather"}
	}`)
	out, _, err := ToChatRequest(req, "glm-5.3", nil)
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := out["tools"].([]map[string]any)
	if len(tools) != 1 {
		t.Fatalf("工具数 = %d，期望 1", len(tools))
	}
	fn, _ := tools[0]["function"].(map[string]any)
	if fn["name"] != "weather" {
		t.Errorf("工具名 = %v，期望 weather", fn["name"])
	}
	jsonEq(t, out["tool_choice"], `{"type":"function","function":{"name":"weather"}}`)
}

// TestResponsesTextFormat 覆盖 text.format 的三种取值。
func TestResponsesTextFormat(t *testing.T) {
	req := decodeReq(t, `{"model":"m","input":"x","text":{"format":{"type":"json_object"}}}`)
	out, _, err := ToChatRequest(req, "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	jsonEq(t, out["response_format"], `{"type":"json_object"}`)

	bad := decodeReq(t, `{"model":"m","input":"x","text":{"format":{"type":"json_schema","name":"s","schema":{}}}}`)
	_, _, err = ToChatRequest(bad, "m", nil)
	var pe *ParamError
	if !asParamError(err, &pe) || !pe.Unsupported {
		t.Fatalf("json_schema 应返回 unsupported ParamError，实际 %v", err)
	}
}

// TestResponsesUnsupportedFields 覆盖显式拒绝的字段。
func TestResponsesUnsupportedFields(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		param string
	}{
		{"background", `{"model":"m","input":"x","background":true}`, "background"},
		{"conversation", `{"model":"m","input":"x","conversation":"conv_1"}`, "conversation"},
		{"prompt", `{"model":"m","input":"x","prompt":{"id":"p"}}`, "prompt"},
		{"truncation", `{"model":"m","input":"x","truncation":"auto"}`, "truncation"},
		{"non-function tool", `{"model":"m","input":"x","tools":[{"type":"code_interpreter"}]}`, "tools[0].type"},
		{"input_file", `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_file"}]}]}`, "input[0].content[0]"},
		{"item_reference", `{"model":"m","input":[{"type":"item_reference","id":"x"}]}`, "input[0]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := ToChatRequest(decodeReq(t, c.body), "m", nil)
			var pe *ParamError
			if !asParamError(err, &pe) {
				t.Fatalf("应返回 ParamError，实际 %v", err)
			}
			if !pe.Unsupported {
				t.Errorf("应为 unsupported 错误")
			}
			if pe.Param != c.param {
				t.Errorf("param = %q，期望 %q", pe.Param, c.param)
			}
		})
	}
}

// TestResponsesDroppedFields 覆盖「摘掉 + 声明」的字段。
func TestResponsesDroppedFields(t *testing.T) {
	req := decodeReq(t, `{
		"model":"m","input":"x",
		"reasoning":{"effort":"high","summary":"auto"},
		"include":["reasoning.encrypted_content"],
		"max_tool_calls":3,
		"service_tier":"flex",
		"text":{"verbosity":"low"}
	}`)
	_, dropped, err := ToChatRequest(req, "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"text.verbosity", "reasoning.effort", "reasoning.summary", "include", "max_tool_calls", "service_tier"}
	for _, w := range want {
		if !containsStr(dropped, w) {
			t.Errorf("dropped 缺少 %q（实际 %v）", w, dropped)
		}
	}
}

// TestResponsesStreamRequestsUsage 流式必须显式要 usage。
func TestResponsesStreamRequestsUsage(t *testing.T) {
	req := decodeReq(t, `{"model":"m","input":"x","stream":true}`)
	out, _, err := ToChatRequest(req, "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	jsonEq(t, out["stream_options"], `{"include_usage":true}`)
}

// TestFromChatCompletionText 覆盖非流式文本 + 思维链的映射。
func TestFromChatCompletionText(t *testing.T) {
	raw := `{"id":"c1","model":"m","choices":[{"index":0,"message":{
		"role":"assistant","content":"你好","reasoning_content":"先想一想"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":10,"completion_tokens":5,
			"prompt_tokens_details":{"cached_tokens":2},
			"completion_tokens_details":{"reasoning_tokens":3}}}`
	req := decodeReq(t, `{"model":"m","input":"x"}`)
	reply := FromChatCompletion([]byte(raw), req, "m", 0)

	items, _ := reply.Payload["output"].([]any)
	if len(items) != 2 {
		t.Fatalf("output 条目数 = %d，期望 2（reasoning + message）", len(items))
	}
	if items[0].(map[string]any)["type"] != "reasoning" {
		t.Errorf("首条应为 reasoning，实际 %v", items[0])
	}
	msg := items[1].(map[string]any)
	if msg["type"] != "message" || msg["status"] != "completed" {
		t.Errorf("message 条目形态不对: %v", msg)
	}
	content := msg["content"].([]any)[0].(map[string]any)
	if content["type"] != "output_text" || content["text"] != "你好" {
		t.Errorf("output_text 不对: %v", content)
	}
	usage := reply.Payload["usage"].(map[string]any)
	if usage["input_tokens"] != 10 || usage["output_tokens"] != 5 || usage["total_tokens"] != 15 {
		t.Errorf("usage 不对: %v", usage)
	}
	details := usage["output_tokens_details"].(map[string]any)
	if details["reasoning_tokens"] != 3 {
		t.Errorf("reasoning_tokens 不对: %v", details)
	}
	if reply.Payload["status"] != "completed" {
		t.Errorf("status = %v，期望 completed", reply.Payload["status"])
	}
}

// TestFromChatCompletionTools 覆盖工具调用：message 条目不出现，function_call 出现。
func TestFromChatCompletionTools(t *testing.T) {
	raw := `{"id":"c1","model":"m","choices":[{"index":0,"message":{
		"role":"assistant","content":"","tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"北京\"}"}}]},
		"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`
	req := decodeReq(t, `{"model":"m","input":"x"}`)
	reply := FromChatCompletion([]byte(raw), req, "m", 0)

	items, _ := reply.Payload["output"].([]any)
	if len(items) != 1 {
		t.Fatalf("output 条目数 = %d，期望 1（只有 function_call）", len(items))
	}
	call := items[0].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_1" || call["name"] != "weather" {
		t.Errorf("function_call 形态不对: %v", call)
	}
	// Chat 形态快照必须带 tool_calls，供 previous_response_id 续写。
	calls, ok := reply.ChatMessage["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("ChatMessage.tool_calls 不对: %v", reply.ChatMessage)
	}
}

// TestFromChatCompletionLengthIncomplete 覆盖 finish_reason=length。
func TestFromChatCompletionLengthIncomplete(t *testing.T) {
	raw := `{"choices":[{"index":0,"message":{"role":"assistant","content":"截断"},"finish_reason":"length"}],
		"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	req := decodeReq(t, `{"model":"m","input":"x"}`)
	reply := FromChatCompletion([]byte(raw), req, "m", 0)
	if reply.Payload["status"] != "incomplete" {
		t.Fatalf("status = %v，期望 incomplete", reply.Payload["status"])
	}
	details := reply.Payload["incomplete_details"].(map[string]any)
	if details["reason"] != "max_output_tokens" {
		t.Errorf("incomplete_details.reason = %v", details["reason"])
	}
}

// TestResponsesStreamEventSequence 钉住流式事件的顺序。
func TestResponsesStreamEventSequence(t *testing.T) {
	req := decodeReq(t, `{"model":"m","input":"x","stream":true}`)
	st := NewResponsesStream(req, "m", 12)

	var events []StreamEvent
	feed := func(raw string) {
		evs, err := st.Feed([]byte(raw))
		if err != nil {
			t.Fatalf("Feed 失败: %v", err)
		}
		events = append(events, evs...)
	}

	feed(`{"choices":[{"index":0,"delta":{"reasoning_content":"想一想"}}]}`)
	feed(`{"choices":[{"index":0,"delta":{"content":"你"}}]}`)
	feed(`{"choices":[{"index":0,"delta":{"content":"好"}}]}`)
	feed(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"weather","arguments":"{\"city\":"}}]}}]}`)
	feed(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"北京\"}"}}]}}]}`)
	feed(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
	feed(`{"choices":[],"usage":{"prompt_tokens":20,"completion_tokens":9,
		"completion_tokens_details":{"reasoning_tokens":4}}}`)
	events = append(events, st.Finish()...)

	want := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added", // reasoning
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.output_item.done",  // reasoning 收块
		"response.output_item.added", // message
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",  // message 收块
		"response.output_item.added", // function_call
		"response.function_call_arguments.delta",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done", // function_call 收块
		"response.completed",
	}
	if len(events) != len(want) {
		var got []string
		for _, e := range events {
			got = append(got, e.Type)
		}
		t.Fatalf("事件数 = %d，期望 %d\n got: %v", len(events), len(want), got)
	}
	for i, w := range want {
		if events[i].Type != w {
			t.Errorf("事件 #%d = %q，期望 %q", i, events[i].Type, w)
		}
		if got, _ := events[i].Data["sequence_number"].(int); got != i {
			t.Errorf("事件 #%d 的 sequence_number = %v，期望 %d", i, events[i].Data["sequence_number"], i)
		}
	}

	// response.completed 里的 response 必须包含全部 output 与真实 usage。
	final := events[len(events)-1].Data["response"].(map[string]any)
	if final["status"] != "completed" {
		t.Errorf("final.status = %v", final["status"])
	}
	out := final["output"].([]any)
	if len(out) != 3 {
		t.Fatalf("final.output 条目数 = %d，期望 3", len(out))
	}
	usage := final["usage"].(map[string]any)
	if usage["input_tokens"] != 20 || usage["output_tokens"] != 9 {
		t.Errorf("final.usage = %v", usage)
	}

	// Chat 快照必须能直接当历史消息用。
	msg := st.ChatMessage()
	if msg["content"] != nil {
		t.Errorf("带 tool_calls 时 content 应为 null，实际 %v", msg["content"])
	}
	calls := msg["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls 数 = %d", len(calls))
	}
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["arguments"] != `{"city":"北京"}` {
		t.Errorf("累积参数 = %v", fn["arguments"])
	}
}

// TestResponsesStreamNoUsageFallback 上游不回 usage 时用字符数兜底。
func TestResponsesStreamNoUsageFallback(t *testing.T) {
	req := decodeReq(t, `{"model":"m","input":"x","stream":true}`)
	st := NewResponsesStream(req, "m", 7)
	if _, err := st.Feed([]byte(`{"choices":[{"index":0,"delta":{"content":"你好世界"}}]}`)); err != nil {
		t.Fatal(err)
	}
	evs := st.Finish()
	final := evs[len(evs)-1].Data["response"].(map[string]any)
	usage := final["usage"].(map[string]any)
	if usage["input_tokens"] != 7 {
		t.Errorf("兜底 input_tokens = %v，期望 7", usage["input_tokens"])
	}
	if usage["output_tokens"] != 4 {
		t.Errorf("兜底 output_tokens = %v，期望 4", usage["output_tokens"])
	}
}

// TestResponsesStreamFail 覆盖流中出错：只能以 response.failed 收尾。
func TestResponsesStreamFail(t *testing.T) {
	req := decodeReq(t, `{"model":"m","input":"x","stream":true}`)
	st := NewResponsesStream(req, "m", 1)
	if _, err := st.Feed([]byte(`{"choices":[{"index":0,"delta":{"content":"半句"}}]}`)); err != nil {
		t.Fatal(err)
	}
	evs := st.Fail("上游挂了", "upstream_error")
	last := evs[len(evs)-1]
	if last.Type != "response.failed" {
		t.Fatalf("最后一个事件 = %q，期望 response.failed", last.Type)
	}
	obj := last.Data["response"].(map[string]any)
	if obj["status"] != "failed" {
		t.Errorf("status = %v，期望 failed", obj["status"])
	}
	errObj := obj["error"].(map[string]any)
	if errObj["code"] != "upstream_error" {
		t.Errorf("error.code = %v", errObj["code"])
	}
}

// --- 小工具 ---

// asParamError 是 errors.As 的简版（避免本文件再引 errors 只为一处）。
func asParamError(err error, target **ParamError) bool {
	for err != nil {
		if pe, ok := err.(*ParamError); ok {
			*target = pe
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}
