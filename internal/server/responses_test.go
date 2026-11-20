package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codearts2api/internal/codearts"
	"codearts2api/internal/registry"
)

// 本文件是 Responses API 的端到端回归：直接打 handler，顶替上游 HTTP，
// 断言「客户端拿到什么」与「上游实际收到什么」。
//
// 只看网关响应不够：工具声明没翻译、include_usage 没注入、历史消息没带进去
// 这类问题在网关侧全都表现为「200 + 一份看起来正常的响应」。

// okToolCompletion 是一次带工具调用的非流式上游响应。
const okToolCompletion = `{"id":"chatcmpl-t","object":"chat.completion","created":1700000000,` +
	`"model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"",` +
	`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"北京\"}"}}]},` +
	`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`

// okStream 是一段最小可用的上游 SSE。
const okStream = `data: {"id":"c","object":"chat.completion.chunk","model":"glm-5.3","choices":[{"index":0,"delta":{"reasoning_content":"想"}}]}` + "\n\n" +
	`data: {"choices":[{"index":0,"delta":{"content":"你好"}}]}` + "\n\n" +
	`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
	`data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":2,` +
	`"prompt_tokens_details":{"cached_tokens":1},"completion_tokens_details":{"reasoning_tokens":1}}}` + "\n\n" +
	"data: [DONE]\n\n"

// setupResponsesServer 构造能跑完整 Responses 链路的 Server。
func setupResponsesServer(t *testing.T) (*Server, *capture) {
	t.Helper()
	s := newTestServer(t, 1, 1)
	s.Cred = codearts.NewManager(s.Up)
	s.Reg.Replace([]registry.Model{{ID: "glm-5.3"}})
	s.Store.SetAccountSession("a", &codearts.Session{
		AccessKey:     "ak",
		SecretKey:     "sk",
		SecurityToken: "st",
		ExpiresAt:     time.Now().Add(time.Hour),
	})

	cap := &capture{}
	s.Up.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		cap.mu.Lock()
		cap.bodies = append(cap.bodies, string(raw))
		cap.mu.Unlock()

		var req struct {
			Stream bool `json:"stream"`
			Tools  any  `json:"tools"`
		}
		_ = json.Unmarshal(raw, &req)
		body := okCompletion
		ctype := "application/json"
		if req.Stream {
			body, ctype = okStream, "text/event-stream"
		} else if req.Tools != nil {
			body = okToolCompletion
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{ctype}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})}
	return s, cap
}

// postResponses 向 /v1/responses 发一次请求。
func postResponses(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:5555"
	rec := httptest.NewRecorder()
	s.handleResponses(rec, req)
	return rec
}

// TestResponsesBasicNonStream 覆盖最小非流式链路与上游请求体。
func TestResponsesBasicNonStream(t *testing.T) {
	s, cap := setupResponsesServer(t)

	rec := postResponses(t, s, `{"model":"glm-5.3","instructions":"你是助手","input":"你好"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body=%s", rec.Code, rec.Body.String())
	}

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if out["object"] != "response" || out["status"] != "completed" {
		t.Errorf("响应形态不对: object=%v status=%v", out["object"], out["status"])
	}
	if !strings.HasPrefix(out["id"].(string), "resp_") {
		t.Errorf("id = %v，期望 resp_ 前缀", out["id"])
	}
	items, _ := out["output"].([]any)
	if len(items) != 1 {
		t.Fatalf("output 条目数 = %d，期望 1", len(items))
	}
	msg := items[0].(map[string]any)
	if msg["type"] != "message" {
		t.Fatalf("output[0].type = %v，期望 message", msg["type"])
	}

	up := cap.at(t, 0)
	msgs, _ := up["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("上游消息数 = %d，期望 2", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "你是助手" {
		t.Errorf("上游首条消息 = %v，期望 system/你是助手", first)
	}
	if _, ok := up["stream_options"]; ok {
		t.Errorf("非流式请求不该带 stream_options")
	}
}

// TestResponsesStreamEvents 覆盖流式：事件序列、include_usage、不发 [DONE]。
func TestResponsesStreamEvents(t *testing.T) {
	s, cap := setupResponsesServer(t)

	rec := postResponses(t, s, `{"model":"glm-5.3","input":"你好","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q，期望 text/event-stream", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: response.created",
		"event: response.in_progress",
		"event: response.reasoning_summary_text.delta",
		"event: response.output_text.delta",
		"event: response.output_text.done",
		"event: response.completed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("SSE 缺少 %q\n%s", want, body)
		}
	}
	// Responses 协议没有 [DONE]：多发一个会让严格解析的客户端直接报错。
	if strings.Contains(body, "[DONE]") {
		t.Errorf("Responses 流不该出现 [DONE]")
	}

	up := cap.at(t, 0)
	so, _ := up["stream_options"].(map[string]any)
	if so["include_usage"] != true {
		t.Errorf("上游 stream_options = %v，期望 include_usage=true", up["stream_options"])
	}
}

// TestResponsesToolsTranslation 覆盖扁平工具 → 嵌套工具，以及 function_call 输出。
func TestResponsesToolsTranslation(t *testing.T) {
	s, cap := setupResponsesServer(t)

	rec := postResponses(t, s, `{"model":"glm-5.3","input":"查天气",
		"tools":[{"type":"function","name":"weather","description":"查天气",
		"parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body=%s", rec.Code, rec.Body.String())
	}

	up := cap.at(t, 0)
	tools, _ := up["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("上游工具数 = %d，期望 1", len(tools))
	}
	first, _ := tools[0].(map[string]any)
	if first["type"] != "function" {
		t.Fatalf("上游工具 type = %v", first["type"])
	}
	fn, _ := first["function"].(map[string]any)
	if fn["name"] != "weather" {
		t.Errorf("上游工具名 = %v，期望 weather", fn["name"])
	}

	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	items, _ := out["output"].([]any)
	if len(items) != 1 {
		t.Fatalf("output 条目数 = %d，期望 1", len(items))
	}
	call := items[0].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_1" || call["name"] != "weather" {
		t.Errorf("function_call 形态不对: %v", call)
	}
}

// TestResponsesPreviousResponseID 覆盖续写：历史消息必须带回上游。
func TestResponsesPreviousResponseID(t *testing.T) {
	s, cap := setupResponsesServer(t)

	first := postResponses(t, s, `{"model":"glm-5.3","input":"第一轮"}`)
	if first.Code != http.StatusOK {
		t.Fatalf("首轮状态码 = %d；body=%s", first.Code, first.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(first.Body.Bytes(), &out)
	id := out["id"].(string)

	second := postResponses(t, s, `{"model":"glm-5.3","previous_response_id":"`+id+`","input":"第二轮"}`)
	if second.Code != http.StatusOK {
		t.Fatalf("续写状态码 = %d；body=%s", second.Code, second.Body.String())
	}

	up := cap.at(t, 1)
	msgs, _ := up["messages"].([]any)
	// user(第一轮) + assistant(ok) + user(第二轮)
	if len(msgs) != 3 {
		t.Fatalf("续写时上游消息数 = %d，期望 3：%v", len(msgs), msgs)
	}
	if m, _ := msgs[1].(map[string]any); m["role"] != "assistant" {
		t.Errorf("历史第二条 role = %v，期望 assistant", m["role"])
	}
	if m, _ := msgs[2].(map[string]any); m["content"] != "第二轮" {
		t.Errorf("新输入 = %v，期望「第二轮」", m["content"])
	}
}

// TestResponsesPreviousResponseIDNotFound 未知 id 必须 404 + 规范码。
func TestResponsesPreviousResponseIDNotFound(t *testing.T) {
	s, cap := setupResponsesServer(t)

	rec := postResponses(t, s, `{"model":"glm-5.3","previous_response_id":"resp_missing","input":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404；body=%s", rec.Code, rec.Body.String())
	}
	e := errBody(t, rec)
	if e["code"] != "previous_response_not_found" {
		t.Errorf("code = %v，期望 previous_response_not_found", e["code"])
	}
	if e["param"] != "previous_response_id" {
		t.Errorf("param = %v", e["param"])
	}
	if cap.count() != 0 {
		t.Errorf("不该打到上游（实际 %d 次）", cap.count())
	}
}

// TestResponsesStoreFalse 覆盖 store=false：不保存，按 id 查 404。
func TestResponsesStoreFalse(t *testing.T) {
	s, _ := setupResponsesServer(t)

	rec := postResponses(t, s, `{"model":"glm-5.3","input":"x","store":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["store"] != false {
		t.Errorf("store = %v，期望 false", out["store"])
	}

	get := httptest.NewRequest(http.MethodGet, "/v1/responses/"+out["id"].(string), nil)
	get.SetPathValue("id", out["id"].(string))
	grec := httptest.NewRecorder()
	s.handleResponsesByID(grec, get)
	if grec.Code != http.StatusNotFound {
		t.Fatalf("store=false 的响应不该能查到，状态码 = %d", grec.Code)
	}
}

// TestResponsesGetAndDelete 覆盖按 id 查询与删除。
func TestResponsesGetAndDelete(t *testing.T) {
	s, _ := setupResponsesServer(t)

	rec := postResponses(t, s, `{"model":"glm-5.3","input":"x"}`)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	id := out["id"].(string)

	get := httptest.NewRequest(http.MethodGet, "/v1/responses/"+id, nil)
	get.SetPathValue("id", id)
	grec := httptest.NewRecorder()
	s.handleResponsesByID(grec, get)
	if grec.Code != http.StatusOK {
		t.Fatalf("GET 状态码 = %d；body=%s", grec.Code, grec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(grec.Body.Bytes(), &got)
	if got["id"] != id {
		t.Errorf("GET 返回的 id = %v，期望 %v", got["id"], id)
	}

	del := httptest.NewRequest(http.MethodDelete, "/v1/responses/"+id, nil)
	del.SetPathValue("id", id)
	drec := httptest.NewRecorder()
	s.handleResponsesByID(drec, del)
	if drec.Code != http.StatusOK {
		t.Fatalf("DELETE 状态码 = %d", drec.Code)
	}

	get2 := httptest.NewRequest(http.MethodGet, "/v1/responses/"+id, nil)
	get2.SetPathValue("id", id)
	grec2 := httptest.NewRecorder()
	s.handleResponsesByID(grec2, get2)
	if grec2.Code != http.StatusNotFound {
		t.Errorf("删除后 GET 状态码 = %d，期望 404", grec2.Code)
	}
}

// TestResponsesInputItems 覆盖 input_items 接口。
func TestResponsesInputItems(t *testing.T) {
	s, _ := setupResponsesServer(t)

	rec := postResponses(t, s, `{"model":"glm-5.3","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"你好"}]}]}`)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	id := out["id"].(string)

	req := httptest.NewRequest(http.MethodGet, "/v1/responses/"+id+"/input_items", nil)
	req.SetPathValue("id", id)
	irec := httptest.NewRecorder()
	s.handleResponsesInputItems(irec, req)
	if irec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；body=%s", irec.Code, irec.Body.String())
	}
	var list map[string]any
	_ = json.Unmarshal(irec.Body.Bytes(), &list)
	data, _ := list["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("input_items 条数 = %d，期望 1", len(data))
	}
}

// TestResponsesUnsupportedRejectedBeforeUpstream 不支持的参数必须前置 400。
func TestResponsesUnsupportedRejectedBeforeUpstream(t *testing.T) {
	s, cap := setupResponsesServer(t)

	rec := postResponses(t, s, `{"model":"glm-5.3","input":"x","background":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400；body=%s", rec.Code, rec.Body.String())
	}
	e := errBody(t, rec)
	if e["code"] != "unsupported_parameter" || e["param"] != "background" {
		t.Errorf("错误体 = %v", e)
	}
	if cap.count() != 0 {
		t.Errorf("前置拒绝不该打到上游（实际 %d 次）", cap.count())
	}
}

// TestResponsesDroppedHeader 摘掉的字段必须在响应头声明。
func TestResponsesDroppedHeader(t *testing.T) {
	s, _ := setupResponsesServer(t)

	rec := postResponses(t, s, `{"model":"glm-5.3","input":"x","reasoning":{"effort":"high"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；body=%s", rec.Code, rec.Body.String())
	}
	got := rec.Header().Get(unsupportedHeader)
	if !strings.Contains(got, "reasoning.effort") {
		t.Errorf("%s = %q，期望包含 reasoning.effort", unsupportedHeader, got)
	}
}

// TestResponsesInputTokens 覆盖本地估算接口（不打上游）。
func TestResponsesInputTokens(t *testing.T) {
	s, cap := setupResponsesServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens",
		strings.NewReader(`{"model":"glm-5.3","input":"你好"}`))
	rec := httptest.NewRecorder()
	s.handleResponsesInputTokens(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["object"] != "response.input_tokens" {
		t.Errorf("object = %v", out["object"])
	}
	if n, _ := out["input_tokens"].(float64); n <= 0 {
		t.Errorf("input_tokens = %v，期望 > 0", out["input_tokens"])
	}
	if cap.count() != 0 {
		t.Errorf("估算接口不该打上游（实际 %d 次）", cap.count())
	}
}

// TestResponsesRouting 覆盖路由注册（含带 id 的 GET）。
func TestResponsesRouting(t *testing.T) {
	s, _ := setupResponsesServer(t)
	h := s.Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"glm-5.3","input":"x"}`))
	req.RemoteAddr = "127.0.0.1:5555"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/responses 状态码 = %d；body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)

	get := httptest.NewRequest(http.MethodGet, "/v1/responses/"+out["id"].(string), nil)
	get.RemoteAddr = "127.0.0.1:5555"
	grec := httptest.NewRecorder()
	h.ServeHTTP(grec, get)
	if grec.Code != http.StatusOK {
		t.Fatalf("GET /v1/responses/{id} 状态码 = %d；body=%s", grec.Code, grec.Body.String())
	}

	// input_tokens 是字面量路由，必须优先于 {id} 通配。
	it := httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens",
		strings.NewReader(`{"model":"glm-5.3","input":"x"}`))
	it.RemoteAddr = "127.0.0.1:5555"
	irec := httptest.NewRecorder()
	h.ServeHTTP(irec, it)
	if irec.Code != http.StatusOK {
		t.Fatalf("POST /v1/responses/input_tokens 状态码 = %d；body=%s", irec.Code, irec.Body.String())
	}
}

// TestResponsesStreamThenContinue 覆盖「流式 → 存快照 → 续写」这条最容易被漏的链路。
func TestResponsesStreamThenContinue(t *testing.T) {
	s, cap := setupResponsesServer(t)

	rec := postResponses(t, s, `{"model":"glm-5.3","input":"第一轮","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("流式状态码 = %d；body=%s", rec.Code, rec.Body.String())
	}
	// 从 response.completed 里取 id。
	var id string
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Type     string `json:"type"`
			Response struct {
				ID string `json:"id"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) != nil {
			continue
		}
		if ev.Type == "response.completed" {
			id = ev.Response.ID
		}
	}
	if id == "" {
		t.Fatalf("流里没有 response.completed 的 id\n%s", rec.Body.String())
	}

	second := postResponses(t, s, `{"model":"glm-5.3","previous_response_id":"`+id+`","input":"第二轮"}`)
	if second.Code != http.StatusOK {
		t.Fatalf("续写状态码 = %d；body=%s", second.Code, second.Body.String())
	}
	up := cap.at(t, 1)
	msgs, _ := up["messages"].([]any)
	// user + assistant(流式累积的「你好」) + user
	if len(msgs) != 3 {
		t.Fatalf("续写时上游消息数 = %d，期望 3：%v", len(msgs), msgs)
	}
	if m, _ := msgs[1].(map[string]any); m["content"] != "你好" {
		t.Errorf("流式 assistant 历史 = %v，期望「你好」", m["content"])
	}
}
