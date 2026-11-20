package server

import (
	"encoding/json"
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
