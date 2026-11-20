package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"codearts2api/internal/apierr"
)

// 本文件是请求体前置校验的回归。
//
// 回归背景：temperature / top_p / n / max_tokens / response_format / logprobs
// 全部原样透传，上游报错就是上游的错 —— 每次非法参数都要先打一次上游
// （真实扣积分）才知道写错了，而且客户端只拿到一句英文长句。

// TestValidateChatRequestRanges 越界参数必须在网关侧就被拒。
func TestValidateChatRequestRanges(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		param string
		code  string
	}{
		{"temperature 上界", `{"temperature":2.5}`, "temperature", apierr.CodeInvalidValue},
		{"temperature 下界", `{"temperature":-0.1}`, "temperature", apierr.CodeInvalidValue},
		{"temperature 类型", `{"temperature":"hot"}`, "temperature", apierr.CodeInvalidValue},
		{"top_p 越界", `{"top_p":1.5}`, "top_p", apierr.CodeInvalidValue},
		{"presence_penalty 越界", `{"presence_penalty":5}`, "presence_penalty", apierr.CodeInvalidValue},
		{"frequency_penalty 越界", `{"frequency_penalty":-5}`, "frequency_penalty", apierr.CodeInvalidValue},
		{"max_tokens 为 0", `{"max_tokens":0}`, "max_tokens", apierr.CodeInvalidValue},
		{"max_tokens 为负", `{"max_tokens":-5}`, "max_tokens", apierr.CodeInvalidValue},
		{"n 大于 1", `{"n":2}`, "n", apierr.CodeUnsupportedParam},
		{"n 为 0", `{"n":0}`, "n", apierr.CodeUnsupportedParam},
		{"response_format 不认识", `{"response_format":{"type":"json_schema"}}`, "response_format", apierr.CodeUnsupportedParam},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := validateChatRequest([]byte(c.body))
			if err == nil {
				t.Fatalf("期望报错，实际通过：%s", c.body)
			}
			e := apierr.From(err)
			if e.Status != http.StatusBadRequest {
				t.Errorf("状态码 = %d，期望 400", e.Status)
			}
			if e.Param != c.param {
				t.Errorf("param = %q，期望 %q", e.Param, c.param)
			}
			if e.Code != c.code {
				t.Errorf("code = %q，期望 %q", e.Code, c.code)
			}
		})
	}
}

// TestValidateChatRequestAcceptsLegal 合法取值必须原样通过。
func TestValidateChatRequestAcceptsLegal(t *testing.T) {
	legal := []string{
		`{"temperature":0}`,
		`{"temperature":2}`,
		`{"top_p":1}`,
		`{"n":1}`,
		`{"max_tokens":1}`,
		`{"response_format":{"type":"json_object"}}`,
		`{"response_format":{"type":"text"}}`,
		`{"presence_penalty":-2,"frequency_penalty":2}`,
		`{"temperature":null}`,
	}
	for _, body := range legal {
		if _, dropped, err := validateChatRequest([]byte(body)); err != nil {
			t.Errorf("%s 应通过，实际报错 %v", body, err)
		} else if len(dropped) != 0 {
			t.Errorf("%s 不该摘掉字段，得到 %v", body, dropped)
		}
	}
}

// TestValidateChatRequestDropsUnsupportedFields 上游不支持的字段要摘掉并声明，
// 且**只能**摘掉声明过的那几个。
func TestValidateChatRequestDropsUnsupportedFields(t *testing.T) {
	body := `{"model":"glm-5.3","seed":42,"logprobs":true,"top_logprobs":3,
		"temperature":0.5,"tools":[{"type":"function"}],"messages":[{"role":"user","content":"hi"}]}`

	out, dropped, err := validateChatRequest([]byte(body))
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if strings.Join(dropped, ",") != "seed,logprobs,top_logprobs" {
		t.Errorf("dropped = %v，期望 seed,logprobs,top_logprobs", dropped)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	for _, k := range dropped {
		if _, ok := m[k]; ok {
			t.Errorf("字段 %s 没有被摘掉", k)
		}
	}
	// 其余字段一个都不能丢。
	for _, k := range []string{"model", "temperature", "tools", "messages"} {
		if _, ok := m[k]; !ok {
			t.Errorf("字段 %s 被误删", k)
		}
	}
}

// TestValidateChatRequestMessageLimit messages 条数超限要明确报错。
func TestValidateChatRequestMessageLimit(t *testing.T) {
	msgs := make([]string, maxMessages+1)
	for i := range msgs {
		msgs[i] = `{"role":"user","content":"x"}`
	}
	body := `{"messages":[` + strings.Join(msgs, ",") + `]}`
	_, _, err := validateChatRequest([]byte(body))
	if err == nil {
		t.Fatal("超限的 messages 应被拒绝")
	}
	e := apierr.From(err)
	if e.Code != apierr.CodeRequestTooLarge || e.Param != "messages" {
		t.Errorf("错误 = %+v，期望 code=request_too_large param=messages", e)
	}
}

// --- 端到端 ---

// TestChatRejectsOutOfRangeTemperature 越界参数不能打到上游（打一次就是真扣积分）。
func TestChatRejectsOutOfRangeTemperature(t *testing.T) {
	s, cap := setupCompatServer(t)

	rec := postChat(t, s, `{"model":"glm-5.3","temperature":9,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400；body=%s", rec.Code, rec.Body.String())
	}
	if n := cap.count(); n != 0 {
		t.Errorf("非法参数被发往上游 %d 次，期望 0 次", n)
	}
	body := errBody(t, rec)
	if got, _ := body["param"].(string); got != "temperature" {
		t.Errorf("error.param = %v，期望 temperature", body["param"])
	}
	if got, _ := body["code"].(string); got != "invalid_value" {
		t.Errorf("error.code = %v，期望 invalid_value", body["code"])
	}
}

// TestChatUnsupportedFieldsDeclaredInHeader 契约用例 #5 的配套：
// 被摘掉的字段必须出现在 X-Gateway-Unsupported 响应头里，且上游确实没收到。
func TestChatUnsupportedFieldsDeclaredInHeader(t *testing.T) {
	s, cap := setupCompatServer(t)

	rec := postChat(t, s, `{"model":"glm-5.3","seed":42,"logprobs":true,
		"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Gateway-Unsupported"); got != "seed,logprobs" {
		t.Errorf("X-Gateway-Unsupported = %q，期望 seed,logprobs", got)
	}
	up := cap.at(t, 0)
	for _, k := range []string{"seed", "logprobs"} {
		if _, ok := up[k]; ok {
			t.Errorf("字段 %s 仍然被发给了上游", k)
		}
	}
}

// TestChatNoUnsupportedHeaderWhenNothingDropped 没摘字段就不写头，避免噪音。
func TestChatNoUnsupportedHeaderWhenNothingDropped(t *testing.T) {
	s, _ := setupCompatServer(t)
	rec := postChat(t, s, `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
	if got := rec.Header().Get("X-Gateway-Unsupported"); got != "" {
		t.Errorf("X-Gateway-Unsupported = %q，期望为空", got)
	}
}

// TestDecodeIntAcceptsIntegralFloats 覆盖「JSON 没有整数类型」这个坑。
//
// Python / JS 客户端把 1 序列化成 1.0 是常态（openai-python 的 n=1.0 就长这样），
// OpenAI 官方接口照收。以前这里 Unmarshal 到 int 直接失败 → 400，
// 把一个完全合法的请求挡在门外。
func TestDecodeIntAcceptsIntegralFloats(t *testing.T) {
	ok := map[string]int64{
		"1":     1,
		"1.0":   1,
		"100.0": 100,
		"0":     0,
		"-3.0":  -3,
		"1e3":   1000,
	}
	for raw, want := range ok {
		got, fine := decodeInt([]byte(raw))
		if !fine || got != want {
			t.Errorf("decodeInt(%q) = (%d, %v)，期望 (%d, true)", raw, got, fine, want)
		}
	}
	bad := []string{"1.5", `"1"`, "true", "null", "[]", "1e400"}
	for _, raw := range bad {
		if got, fine := decodeInt([]byte(raw)); fine {
			t.Errorf("decodeInt(%q) = (%d, true)，期望失败", raw, got)
		}
	}
}

// TestValidateAcceptsIntegralFloatParams 端到端：n / max_tokens 写成 1.0 / 100.0 必须放行。
func TestValidateAcceptsIntegralFloatParams(t *testing.T) {
	cases := []string{
		`{"model":"glm-5.3","n":1.0,"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"glm-5.3","max_tokens":100.0,"messages":[{"role":"user","content":"hi"}]}`,
	}
	for _, body := range cases {
		if _, _, err := validateChatRequest([]byte(body)); err != nil {
			t.Errorf("%s → %v，期望通过", body, err)
		}
	}
}

// TestValidateRejectsNullContent 覆盖「content 为 null 且无 tool_calls」。
//
// 这种消息上游会回 ModelArts.81001「未正常接收到prompt参数」——
// 打一次上游就是一次真实扣积分，而客户端看到的报错跟真实原因无关。
func TestValidateRejectsNullContent(t *testing.T) {
	cases := []string{
		`{"model":"glm-5.3","messages":[{"role":"user","content":null}]}`,
		`{"model":"glm-5.3","messages":[{"role":"user"}]}`,
		`{"model":"glm-5.3","messages":[{"role":"user","content":"ok"},{"role":"user","content":null}]}`,
	}
	for _, body := range cases {
		_, _, err := validateChatRequest([]byte(body))
		if err == nil {
			t.Errorf("%s → 期望被拒", body)
			continue
		}
		var ipe *apierr.InvalidParamError
		if !errors.As(err, &ipe) {
			t.Errorf("%s → 错误类型 %T", body, err)
			continue
		}
		if !strings.HasPrefix(ipe.Param, "messages[") || !strings.HasSuffix(ipe.Param, "].content") {
			t.Errorf("%s → param = %q，期望 messages[i].content", body, ipe.Param)
		}
	}
}

// TestValidateAllowsNullContentWithToolCalls 确认标准的「只要调用、不要文本」写法不被误杀。
func TestValidateAllowsNullContentWithToolCalls(t *testing.T) {
	body := `{"model":"glm-5.3","messages":[
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"42"}]}`
	if _, _, err := validateChatRequest([]byte(body)); err != nil {
		t.Fatalf("带 tool_calls 的 assistant 消息被拒: %v", err)
	}
}

// TestMessagesOverLimitIs413 确认超限走 413 而不是 400 ——
// code 是 request_too_large 却回 400，客户端与反代都会判断错。
func TestMessagesOverLimitIs413(t *testing.T) {
	msgs := strings.Repeat(`{"role":"user","content":"hi"},`, maxMessages)
	body := `{"model":"glm-5.3","messages":[` + msgs + `{"role":"user","content":"hi"}]}`
	_, _, err := validateChatRequest([]byte(body))
	if err == nil {
		t.Fatal("期望被拒")
	}
	if got := apierr.From(err).Status; got != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d，期望 413", got)
	}
	if got := apierr.From(err).Code; got != apierr.CodeRequestTooLarge {
		t.Errorf("code = %q", got)
	}
}

// TestNormalizeIntFieldsRewritesFloatLiterals 覆盖「放行还不够，得改写」。
//
// 上游按**字面量**校验类型：实测 n:1.0 / max_tokens:8.0 会被回
// `400 The request param is invalid`。网关既然已经确认值是整数，
// 就必须替客户端写成 1 / 8，否则「接受 1.0」只是把 400 从网关挪到上游。
func TestNormalizeIntFieldsRewritesFloatLiterals(t *testing.T) {
	in := `{"model":"glm-5.3","n":1.0,"max_tokens":8.0,"top_logprobs":2e0,
		"temperature":1.0,"messages":[{"role":"user","content":"hi"}]}`
	out, _, err := validateChatRequest([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"n": "1", "max_tokens": "8",
	} {
		if got := string(m[key]); got != want {
			t.Errorf("%s = %s，期望 %s", key, got, want)
		}
	}
	// 浮点字段不该被整数化。
	if got := string(m["temperature"]); got != "1.0" {
		t.Errorf("temperature = %s，期望原样 1.0", got)
	}
}

// TestNormalizeIntFieldsLeavesIntegersAlone 确认已经规范的请求体逐字节不变。
func TestNormalizeIntFieldsLeavesIntegersAlone(t *testing.T) {
	in := `{"model":"glm-5.3","n":1,"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
	out, dropped, err := validateChatRequest([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped = %v", dropped)
	}
	if string(out) != in {
		t.Errorf("请求体被无谓改写:\n in=%s\nout=%s", in, out)
	}
}

// TestValidateRejectsNonStringContent 覆盖 content 类型非法（数字 / 布尔 / 对象）。
//
// 上游会回 `The type of content should be string or array`，
// 但那已经是一次真实扣积分的调用 —— 而这完全是客户端写错了。
func TestValidateRejectsNonStringContent(t *testing.T) {
	for _, c := range []string{"123", "true", `{"a":1}`} {
		body := `{"model":"glm-5.3","messages":[{"role":"user","content":` + c + `}]}`
		_, _, err := validateChatRequest([]byte(body))
		if err == nil {
			t.Errorf("content=%s → 期望被拒", c)
			continue
		}
		var ipe *apierr.InvalidParamError
		if !errors.As(err, &ipe) || ipe.Param != "messages[0].content" {
			t.Errorf("content=%s → %v", c, err)
		}
	}
	// 合法的两种形态必须放行。
	for _, c := range []string{`"hi"`, `[{"type":"text","text":"hi"}]`} {
		body := `{"model":"glm-5.3","messages":[{"role":"user","content":` + c + `}]}`
		if _, _, err := validateChatRequest([]byte(body)); err != nil {
			t.Errorf("content=%s 被误杀: %v", c, err)
		}
	}
}
