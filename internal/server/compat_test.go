package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"codearts2api/internal/codearts"
	"codearts2api/internal/protocol"
	"codearts2api/internal/registry"
	"codearts2api/internal/store"
)

// 本文件是「客户端兼容层」的端到端回归：直接打 handler，顶替上游 HTTP，
// 断言两件事 —— 网关对客户端返回什么，以及**上游实际收到了什么**。
//
// 只看网关响应是不够的：role 规范化这类改动如果只在网关侧改写却没真正
// 转发给上游，测试照样会「通过」，但线上依然 400。

// okCompletion 是一条最小可用的非流式上游响应。
const okCompletion = `{"id":"chatcmpl-t","object":"chat.completion","created":1700000000,` +
	`"model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},` +
	`"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`

// capture 记录上游收到的请求体。
type capture struct {
	mu     sync.Mutex
	bodies []string
}

// bodiesJSON 返回第 n 次上游请求体（解析成 map 便于断言）。
func (c *capture) at(t *testing.T, n int) map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) <= n {
		t.Fatalf("上游只收到 %d 次请求，取不到第 %d 次", len(c.bodies), n)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(c.bodies[n]), &m); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v\n%s", err, c.bodies[n])
	}
	return m
}

// count 返回上游收到的请求次数。
func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

// setupCompatServer 构造一个能跑完整对话链路的 Server：
// 1 个健康账号 + 有效凭据 + 顶替上游的 Transport。
func setupCompatServer(t *testing.T, models ...registry.Model) (*Server, *capture) {
	t.Helper()
	s := newTestServer(t, 1, 1)
	s.Cred = codearts.NewManager(s.Up)
	if len(models) == 0 {
		models = []registry.Model{{ID: "glm-5.3"}}
	}
	s.Reg.Replace(models)

	// 凭据必须「有效」，否则 Manager 会走续期分支（本文件不覆盖续期）。
	s.Store.SetAccountSession("a", &codearts.Session{
		AccessKey:     "ak",
		SecretKey:     "sk",
		SecurityToken: "st",
		ExpiresAt:     time.Now().Add(time.Hour),
	})
	if sess := s.Store.Account("a").Session; sess == nil || !sess.Valid(time.Minute) {
		t.Fatal("测试前置失败：账号凭据不可用")
	}

	cap := &capture{}
	s.Up.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		cap.mu.Lock()
		cap.bodies = append(cap.bodies, string(raw))
		cap.mu.Unlock()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(okCompletion)),
			Request:    r,
		}, nil
	})}
	return s, cap
}

// postChat 向 /v1/chat/completions 发一次请求。
func postChat(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:5555"
	rec := httptest.NewRecorder()
	s.handleChatCompletions(rec, req)
	return rec
}

// errBody 取出 OpenAI 风格的错误对象。
func errBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("错误体不是合法 JSON: %v\n%s", err, rec.Body.String())
	}
	if out.Error == nil {
		t.Fatalf("响应里没有 error 对象：%s", rec.Body.String())
	}
	return out.Error
}

// --- P0-1 角色规范化：端到端 ---

// TestChatDeveloperRoleReachesUpstreamAsSystem 契约用例 #1：
// developer 必须被改写成 system 后才发往上游。
func TestChatDeveloperRoleReachesUpstreamAsSystem(t *testing.T) {
	s, cap := setupCompatServer(t)

	rec := postChat(t, s, `{"model":"glm-5.3","messages":[
		{"role":"developer","content":"你是助手"},
		{"role":"user","content":"你好"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body=%s", rec.Code, rec.Body.String())
	}
	up := cap.at(t, 0)
	msgs, _ := up["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("上游收到的消息数 = %d，期望 2", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if got, _ := first["role"].(string); got != "system" {
		t.Errorf("上游收到的第一条 role = %q，期望 system", got)
	}
}

// TestChatFunctionRoleReachesUpstreamAsTool 契约用例 #2：
// function 必须被改写成 tool 并带上 tool_call_id。
func TestChatFunctionRoleReachesUpstreamAsTool(t *testing.T) {
	s, cap := setupCompatServer(t)

	rec := postChat(t, s, `{"model":"glm-5.3","messages":[
		{"role":"user","content":"天气"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{}"}}]},
		{"role":"function","name":"weather","content":"晴"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body=%s", rec.Code, rec.Body.String())
	}
	up := cap.at(t, 0)
	msgs, _ := up["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("上游收到的消息数 = %d，期望 3", len(msgs))
	}
	last, _ := msgs[2].(map[string]any)
	if got, _ := last["role"].(string); got != "tool" {
		t.Errorf("上游收到的第三条 role = %q，期望 tool", got)
	}
	if got, _ := last["tool_call_id"].(string); got != "call_1" {
		t.Errorf("上游收到的 tool_call_id = %q，期望 call_1", got)
	}
}

// TestChatEmptyRoleInferredByPosition 契约用例 #3：空 role 按位置推断。
func TestChatEmptyRoleInferredByPosition(t *testing.T) {
	s, cap := setupCompatServer(t)

	rec := postChat(t, s, `{"model":"glm-5.3","messages":[
		{"role":"","content":"你是助手"},
		{"content":"你好"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body=%s", rec.Code, rec.Body.String())
	}
	up := cap.at(t, 0)
	msgs, _ := up["messages"].([]any)
	want := []string{"system", "user"}
	for i, w := range want {
		m, _ := msgs[i].(map[string]any)
		if got, _ := m["role"].(string); got != w {
			t.Errorf("上游收到的 messages[%d].role = %q，期望 %q", i, got, w)
		}
	}
}

// TestChatUnknownRoleRejectedBeforeUpstream 未知 role 必须在触达上游之前 400，
// 且错误体是 OpenAI 规范形状（code + param）。
func TestChatUnknownRoleRejectedBeforeUpstream(t *testing.T) {
	s, cap := setupCompatServer(t)

	rec := postChat(t, s, `{"model":"glm-5.3","messages":[
		{"role":"user","content":"你好"},
		{"role":"critic","content":"我不干"}]}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400；body=%s", rec.Code, rec.Body.String())
	}
	if n := cap.count(); n != 0 {
		t.Errorf("非法 role 被发往上游了 %d 次，期望 0 次", n)
	}
	body := errBody(t, rec)
	if got, _ := body["code"].(string); got != "invalid_role" {
		t.Errorf("error.code = %v，期望 invalid_role", body["code"])
	}
	if got, _ := body["param"].(string); got != "messages[1].role" {
		t.Errorf("error.param = %v，期望 messages[1].role", body["param"])
	}
	if got, _ := body["type"].(string); got != "invalid_request_error" {
		t.Errorf("error.type = %v，期望 invalid_request_error", body["type"])
	}
	msg, _ := body["message"].(string)
	if !strings.Contains(msg, "unsupported role: critic") {
		t.Errorf("error.message = %q，期望包含 unsupported role: critic", msg)
	}
}

// --- P0-2 模型名校验 ---

// TestChatUnknownModelReturns404WithoutTouchingUpstream 契约用例 #4：
// gpt-4o 这类「内置兼容别名」默认必须 404 model_not_found，且一次上游都不打
// —— 一次都不能打，因为打一次就是真实扣积分。
func TestChatUnknownModelReturns404WithoutTouchingUpstream(t *testing.T) {
	s, cap := setupCompatServer(t)

	rec := postChat(t, s, `{"model":"gpt-4o","messages":[{"role":"user","content":"你好"}]}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404；body=%s", rec.Code, rec.Body.String())
	}
	if n := cap.count(); n != 0 {
		t.Fatalf("未知模型被发往上游 %d 次，期望 0 次（会真实扣积分）", n)
	}
	body := errBody(t, rec)
	if got, _ := body["code"].(string); got != "model_not_found" {
		t.Errorf("error.code = %v，期望 model_not_found", body["code"])
	}
	if got, _ := body["param"].(string); got != "model" {
		t.Errorf("error.param = %v，期望 model", body["param"])
	}
	msg, _ := body["message"].(string)
	if !strings.Contains(msg, "gpt-4o") || !strings.Contains(msg, "glm-5.3") {
		t.Errorf("error.message = %q，期望同时含请求名与可用模型", msg)
	}
}

// TestChatModelFallbackIsAnnounced 打开兼容别名后，必须显式声明「被换成了谁」：
// 响应头 + 响应体 model 都指向实际执行的模型，绝不静默。
func TestChatModelFallbackIsAnnounced(t *testing.T) {
	s, cap := setupCompatServer(t)
	s.Store.UpdateSettings(func(st *store.Settings) { st.ModelFallback = true })

	rec := postChat(t, s, `{"model":"gpt-4o","messages":[{"role":"user","content":"你好"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Gateway-Model-Fallback"); got != "glm-5.3" {
		t.Errorf("X-Gateway-Model-Fallback = %q，期望 glm-5.3", got)
	}
	if got := rec.Header().Get("X-Gateway-Model-Requested"); got != "gpt-4o" {
		t.Errorf("X-Gateway-Model-Requested = %q，期望 gpt-4o", got)
	}
	// 上游收到的必须是真实模型，不是 gpt-4o。
	up := cap.at(t, 0)
	if got, _ := up["model"].(string); got != "glm-5.3" {
		t.Errorf("上游收到的 model = %q，期望 glm-5.3", got)
	}
	// 响应体里的 model 也必须是实际执行的模型。
	var resp struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v", err)
	}
	if resp.Model != "glm-5.3" {
		t.Errorf("响应体 model = %q，期望 glm-5.3", resp.Model)
	}
}

// TestChatNoFallbackHeaderWhenExactMatch 精确命中时不写回落头，避免误导。
func TestChatNoFallbackHeaderWhenExactMatch(t *testing.T) {
	s, _ := setupCompatServer(t)
	rec := postChat(t, s, `{"model":"glm-5.3","messages":[{"role":"user","content":"你好"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	if got := rec.Header().Get("X-Gateway-Model-Fallback"); got != "" {
		t.Errorf("精确命中却写了回落头 %q", got)
	}
}

// TestWithExecutedModelRewritesBody 单测回填函数：只在 model 与期望不一致时改写，
// 没有 model 字段 / 非法 JSON 时原样返回（展示字段不该弄坏整个响应）。
func TestWithExecutedModelRewritesBody(t *testing.T) {
	in := []byte(`{"id":"x","model":"gpt-4o","choices":[]}`)
	out := withExecutedModel(in, "glm-5.3")
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("改写结果不是合法 JSON: %v", err)
	}
	if got, _ := m["model"].(string); got != "glm-5.3" {
		t.Errorf("model = %q，期望 glm-5.3", got)
	}

	same := []byte(`{"model":"glm-5.3"}`)
	if got := withExecutedModel(same, "glm-5.3"); string(got) != string(same) {
		t.Errorf("一致时不应改写：%s", got)
	}
	noModel := []byte(`{"choices":[]}`)
	if got := withExecutedModel(noModel, "glm-5.3"); string(got) != string(noModel) {
		t.Errorf("无 model 字段时不应改写：%s", got)
	}
	if got := withExecutedModel([]byte("not json"), "glm-5.3"); string(got) != "not json" {
		t.Errorf("非法 JSON 应原样返回：%s", got)
	}
}

// --- P0-3 错误语义归一 ---

// overrideUpstream 把上游顶替为固定响应。
func overrideUpstream(t *testing.T, s *Server, status int, body string) *capture {
	t.Helper()
	cap := &capture{}
	s.Up.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		cap.mu.Lock()
		cap.bodies = append(cap.bodies, string(raw))
		cap.mu.Unlock()
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})}
	return cap
}

// TestUpstreamBusinessErrorBecomes4xx 契约用例 #8：
// 上游用 HTTP 200 + error_code 表达业务错误时，网关必须返回 4xx + 规范错误体。
func TestUpstreamBusinessErrorBecomes4xx(t *testing.T) {
	s, _ := setupCompatServer(t)
	overrideUpstream(t, s, http.StatusOK,
		`{"error_code":"InferHub.001001005.400","error_msg":"temperature -> temperature should be between 0 and 2"}`)

	rec := postChat(t, s, `{"model":"glm-5.3","temperature":99,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400；body=%s", rec.Code, rec.Body.String())
	}
	body := errBody(t, rec)
	if got, _ := body["code"].(string); got != "invalid_value" {
		t.Errorf("error.code = %v，期望 invalid_value", body["code"])
	}
	if got, _ := body["type"].(string); got != "invalid_request_error" {
		t.Errorf("error.type = %v，期望 invalid_request_error", body["type"])
	}
	if _, ok := body["upstream_error"]; ok {
		t.Errorf("已映射的错误不该带 upstream_error：%v", body["upstream_error"])
	}
}

// TestUnmappedUpstreamErrorKeepsOriginal 映射不出来的错误保留原文。
func TestUnmappedUpstreamErrorKeepsOriginal(t *testing.T) {
	s, _ := setupCompatServer(t)
	raw := `{"error_code":"InferHub.009999999.503","error_msg":"something odd happened"}`
	want := "InferHub.009999999.503: something odd happened"
	overrideUpstream(t, s, http.StatusOK, raw)

	rec := postChat(t, s, `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502；body=%s", rec.Code, rec.Body.String())
	}
	body := errBody(t, rec)
	if got, _ := body["code"].(string); got != "upstream_error" {
		t.Errorf("error.code = %v，期望 upstream_error", body["code"])
	}
	if got, _ := body["upstream_error"].(string); got != want {
		t.Errorf("error.upstream_error = %q，期望 %q", got, want)
	}
}

// TestRequestIDCorrelatesErrorWithLog 所有错误响应都带 X-Request-Id，
// 且能在请求日志里用同一个 id 找到那一条。
func TestRequestIDCorrelatesErrorWithLog(t *testing.T) {
	s, _ := setupCompatServer(t)
	h := s.Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:5555"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	rid := rec.Header().Get("X-Request-Id")
	if rid == "" {
		t.Fatal("错误响应没有 X-Request-Id")
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404；body=%s", rec.Code, rec.Body.String())
	}

	logs := s.Store.Logs(0)
	var found bool
	for _, e := range logs {
		if e.RequestID == rid {
			found = true
			if e.Status != http.StatusNotFound {
				t.Errorf("日志 status = %d，期望 404", e.Status)
			}
		}
	}
	if !found {
		t.Fatalf("日志里找不到 request_id=%s 的记录（共 %d 条）", rid, len(logs))
	}
}

// TestRequestIDEchoedWhenValid 客户端自带合法 id 时沿用，便于跨服务串联。
func TestRequestIDEchoedWhenValid(t *testing.T) {
	s, _ := setupCompatServer(t)
	h := s.Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("X-Request-Id", "client-trace-42")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Request-Id"); got != "client-trace-42" {
		t.Errorf("X-Request-Id = %q，期望沿用 client-trace-42", got)
	}
}

// TestRequestIDRejectsHeaderInjection 非法 id（含换行 / 控制字符）必须被丢弃重发，
// 不能原样回写进响应头。
func TestRequestIDRejectsHeaderInjection(t *testing.T) {
	s, _ := setupCompatServer(t)
	h := s.Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("X-Request-Id", "bad\r\nX-Evil: 1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	got := rec.Header().Get("X-Request-Id")
	if got == "" || strings.ContainsAny(got, "\r\n") {
		t.Errorf("X-Request-Id = %q，期望被替换成安全的随机值", got)
	}
}

// TestOversizedBodyReturns413 超过上限时必须明确 413，而不是「JSON 解析失败」。
func TestOversizedBodyReturns413(t *testing.T) {
	s, _ := setupCompatServer(t)
	huge := strings.Repeat("a", maxRequestBody+1024)
	body := `{"model":"glm-5.3","messages":[{"role":"user","content":"` + huge + `"}]}`

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:5555"
	rec := httptest.NewRecorder()
	s.handleChatCompletions(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码 = %d，期望 413", rec.Code)
	}
	if got, _ := errBody(t, rec)["code"].(string); got != "request_too_large" {
		t.Errorf("error.code = %v，期望 request_too_large", got)
	}
}

// --- P1-1 Anthropic thinking signature ---

// TestAnthropicStreamEmitsThinkingSignature 流式响应里 thinking 块关闭前必须发
// signature_delta，且签名与整块正文匹配。
//
// 缺这个事件时，Claude Code / 官方 SDK 第一轮能用、第二轮直接 400。
func TestAnthropicStreamEmitsThinkingSignature(t *testing.T) {
	rec := httptest.NewRecorder()
	st := newAnthropicStream(rec, rec, "msg_1", "glm-5.3", 10)
	st.start()

	if _, err := st.consume([]byte(`{"choices":[{"delta":{"reasoning_content":"先"},"finish_reason":null}]}`)); err != nil {
		t.Fatalf("consume 失败: %v", err)
	}
	if _, err := st.consume([]byte(`{"choices":[{"delta":{"reasoning_content":"想想"},"finish_reason":null}]}`)); err != nil {
		t.Fatalf("consume 失败: %v", err)
	}
	if _, err := st.consume([]byte(`{"choices":[{"delta":{"content":"答案"},"finish_reason":"stop"}]}`)); err != nil {
		t.Fatalf("consume 失败: %v", err)
	}
	st.finish("")

	out := rec.Body.String()
	// 1) 有 signature_delta
	sigIdx := strings.Index(out, `"type":"signature_delta"`)
	if sigIdx < 0 {
		t.Fatalf("流式响应里没有 signature_delta：\n%s", out)
	}
	// 2) 它出现在 thinking 块的 content_block_stop 之前
	stopIdx := strings.Index(out[:sigIdx], `"type":"content_block_stop"`)
	// thinking 块是第一个块，所以第一个 content_block_stop 就是它的
	if stopIdx >= 0 {
		t.Errorf("signature_delta 出现在 thinking 块 stop 之后：\n%s", out)
	}
	// 3) 签名与整块正文（"先想想"）匹配
	var sig string
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Delta struct {
				Type      string `json:"type"`
				Signature string `json:"signature"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) != nil {
			continue
		}
		if ev.Delta.Type == "signature_delta" {
			sig = ev.Delta.Signature
		}
	}
	if sig == "" {
		t.Fatal("signature_delta 里没有 signature")
	}
	if !protocol.VerifyThinkingSignature("先想想", sig) {
		t.Errorf("签名 %q 与整块正文不匹配", sig)
	}
	// 4) thinking_delta 仍然是逐片下发的
	if n := strings.Count(out, `"type":"thinking_delta"`); n != 2 {
		t.Errorf("thinking_delta 数量 = %d，期望 2", n)
	}
}

// postMessages 向 /v1/messages 发一次请求。
func postMessages(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:5555"
	rec := httptest.NewRecorder()
	s.handleAnthropicMessages(rec, req)
	return rec
}

// TestAnthropicMultiTurnWithThinkingSignature 契约用例 #6：
// 两轮对话（第二轮把带 thinking + signature 的 assistant 内容回传）都必须 200。
func TestAnthropicMultiTurnWithThinkingSignature(t *testing.T) {
	s, _ := setupCompatServer(t)

	// 第一轮：上游回了 reasoning_content，网关应当生成 thinking 块 + signature。
	sig := protocol.ThinkingSignature("先想想")
	turn1 := `{"model":"glm-5.3","max_tokens":100,"messages":[{"role":"user","content":"你好"}]}`
	rec1 := postMessages(t, s, turn1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("第一轮状态码 = %d；body=%s", rec1.Code, rec1.Body.String())
	}

	// 第二轮：客户端把上一轮的 thinking 块（带 signature）原样回传。
	turn2 := `{"model":"glm-5.3","max_tokens":100,"messages":[
		{"role":"user","content":"你好"},
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"先想想","signature":"` + sig + `"},
			{"type":"text","text":"你好呀"}]},
		{"role":"user","content":"再说一次"}]}`
	rec2 := postMessages(t, s, turn2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("第二轮状态码 = %d；body=%s", rec2.Code, rec2.Body.String())
	}

	// 第二轮也不能把陌生签名当成错误。
	turn3 := strings.Replace(turn2, sig, "EqQBCkYIBxgCKkAxyz-foreign", 1)
	rec3 := postMessages(t, s, turn3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("陌生签名不该被拒绝：状态码 = %d；body=%s", rec3.Code, rec3.Body.String())
	}
}

// TestAnthropicResponseHasThinkingSignature 非流式 /v1/messages 的 thinking 块也带签名。
func TestAnthropicResponseHasThinkingSignature(t *testing.T) {
	s, _ := setupCompatServer(t)
	overrideUpstream(t, s, http.StatusOK,
		`{"id":"x","model":"glm-5.3","choices":[{"index":0,
		  "message":{"role":"assistant","content":"答案","reasoning_content":"推理"},
		  "finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)

	rec := postMessages(t, s, `{"model":"glm-5.3","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Content []struct {
			Type      string `json:"type"`
			Signature string `json:"signature"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	var found bool
	for _, b := range resp.Content {
		if b.Type == "thinking" {
			found = true
			if b.Signature == "" {
				t.Error("thinking 块缺少 signature")
			} else if !protocol.VerifyThinkingSignature("推理", b.Signature) {
				t.Errorf("signature 与正文不匹配: %q", b.Signature)
			}
		}
	}
	if !found {
		t.Fatal("响应里没有 thinking 块")
	}
}

// --- P1-3 能力矩阵 ---

// TestStreamOnlyModelSkipsNonStreamAttempt 已知「只支持流式」的模型必须直接走流式，
// 不再每次多打一次空枪（每次都是一次真实扣积分）。
func TestStreamOnlyModelSkipsNonStreamAttempt(t *testing.T) {
	s, cap := setupCompatServer(t)
	s.Reg.SetCapability("glm-5.3", registry.Capability{StreamOnly: true, Source: "probe"})

	// 上游只接受流式：非流式请求会拿到空 body 的 400（真实上游的行为）。
	s.Up.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		cap.mu.Lock()
		cap.bodies = append(cap.bodies, string(raw))
		cap.mu.Unlock()

		var body struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(raw, &body)
		if !body.Stream {
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    r,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sseFixture)),
			Request:    r,
		}, nil
	})}

	rec := postChat(t, s, `{"model":"glm-5.3","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body=%s", rec.Code, rec.Body.String())
	}
	if n := cap.count(); n != 1 {
		t.Fatalf("上游被打了 %d 次，期望 1 次（直接走流式，不再试探非流式）", n)
	}
	var resp struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if resp.Object != "chat.completion" || len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "你好世界" {
		t.Errorf("聚合结果不对: %+v", resp)
	}
}

// TestCapabilityRestoredAtStartup 能力矩阵必须从状态文件恢复（跨进程存活）。
func TestCapabilityRestoredAtStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.SetCapability("glm-5.3", registry.Capability{StreamOnly: true, Source: "probe"})

	// 用同一个状态文件重新构造 Server（模拟重启）。
	s := New(st)
	if c, ok := s.Reg.Capability("glm-5.3"); !ok || !c.StreamOnly {
		t.Fatalf("启动时没有恢复能力快照: %+v ok=%v", c, ok)
	}

	// 之后的能力变更必须回写状态文件。
	s.Reg.SetCapability("GLM-5.2", registry.Capability{StreamOnly: true, Source: "probe"})
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Capabilities()["glm-5.2"]; !ok {
		t.Error("能力变更没有落盘")
	}
}
