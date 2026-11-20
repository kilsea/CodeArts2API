package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"codearts2api/internal/apierr"
)

// 本文件是 role 规范化层的回归。
//
// 实测矩阵（同一账号、同一模型，逐个构造验证，2026-11-20）：
//
//	role         上游结果
//	user/system  200
//	tool（带 tool_call_id）  200
//	content 为数组（多模态）  200
//	developer（OpenAI 2024 新规范）  400
//	function（旧 tool calling）      400
//	"" 或字段缺失                    400
//
// 上游的 400 文案是「Message role cannot empty」，与真实原因（role 不认识）
// 无关，会把客户端引向完全错误的排查方向。网关必须在转发前自己消化掉。

// decodeMsgs 把规范化后的请求体解回 messages，便于断言上游实际收到的内容。
func decodeMsgs(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var root struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("规范化结果不是合法 JSON: %v\n%s", err, body)
	}
	return root.Messages
}

// TestNormalizeRolesMatrix 覆盖实测矩阵里的全部 role 取值。
func TestNormalizeRolesMatrix(t *testing.T) {
	cases := []struct {
		name string
		// in 是原始 messages 数组（JSON 文本）。
		in string
		// want 是逐条的期望 role（空串表示该条不该存在）。
		want []string
		// wantErr 非空时期望报错，值是期望的对外错误码。
		wantErr string
	}{
		{
			name: "user 与 system 原样通过",
			in:   `[{"role":"system","content":"你是助手"},{"role":"user","content":"你好"}]`,
			want: []string{"system", "user"},
		},
		{
			name: "tool 带 tool_call_id 原样通过",
			in: `[{"role":"user","content":"天气"},
			      {"role":"assistant","content":null,"tool_calls":[{"id":"call_abc","type":"function","function":{"name":"weather","arguments":"{}"}}]},
			      {"role":"tool","tool_call_id":"call_abc","content":"晴"}]`,
			want: []string{"user", "assistant", "tool"},
		},
		{
			name: "content 为数组（多模态）不受影响",
			in:   `[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}]}]`,
			want: []string{"user"},
		},
		{
			name: "developer 改写为 system",
			in:   `[{"role":"developer","content":"你是助手"},{"role":"user","content":"你好"}]`,
			want: []string{"system", "user"},
		},
		{
			name: "function 改写为 tool 并带上 tool_call_id",
			in: `[{"role":"user","content":"天气"},
			      {"role":"assistant","content":null,"tool_calls":[{"id":"call_xyz","type":"function","function":{"name":"weather","arguments":"{}"}}]},
			      {"role":"function","name":"weather","content":"晴"}]`,
			want: []string{"user", "assistant", "tool"},
		},
		{
			name: "空 role 按位置推断",
			in:   `[{"role":"","content":"你是助手"},{"content":"你好"},{"role":"","content":"再来"}]`,
			want: []string{"system", "user", "user"},
		},
		{
			name:    "未知 role 直接 400",
			in:      `[{"role":"critic","content":"你好"}]`,
			wantErr: apierr.CodeInvalidRole,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := []byte(`{"model":"glm-5.3","messages":` + c.in + `}`)
			out, err := normalizeRoles(body)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("期望报错 %s，实际通过：%s", c.wantErr, out)
				}
				e := apierr.From(err)
				if e.Status != http.StatusBadRequest {
					t.Errorf("状态码 = %d，期望 400", e.Status)
				}
				if e.Code != c.wantErr {
					t.Errorf("错误码 = %q，期望 %q", e.Code, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("规范化失败: %v", err)
			}
			msgs := decodeMsgs(t, out)
			if len(msgs) != len(c.want) {
				t.Fatalf("消息数 = %d，期望 %d", len(msgs), len(c.want))
			}
			for i, w := range c.want {
				if got, _ := msgs[i]["role"].(string); got != w {
					t.Errorf("messages[%d].role = %q，期望 %q", i, got, w)
				}
			}
		})
	}
}

// TestNormalizeRolesFunctionUsesRealToolCallID 钉住 function → tool 的关键细节：
// tool_call_id 必须复用前面 assistant 声明的真实 id，而不是凭函数名瞎编 ——
// 编错的话上游会以「工具调用链断裂」为由拒绝。
func TestNormalizeRolesFunctionUsesRealToolCallID(t *testing.T) {
	body := []byte(`{"model":"glm-5.3","messages":[
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_real_1","type":"function","function":{"name":"weather","arguments":"{}"}}]},
		{"role":"function","name":"weather","content":"晴"}]}`)

	out, err := normalizeRoles(body)
	if err != nil {
		t.Fatalf("规范化失败: %v", err)
	}
	msgs := decodeMsgs(t, out)
	if len(msgs) != 2 {
		t.Fatalf("消息数 = %d，期望 2", len(msgs))
	}
	if got, _ := msgs[1]["tool_call_id"].(string); got != "call_real_1" {
		t.Errorf("tool_call_id = %q，期望复用 call_real_1", got)
	}
	if _, ok := msgs[1]["name"]; ok {
		t.Error("name 字段应被移除（已映射进 tool_call_id）")
	}
}

// TestNormalizeRolesFunctionSynthesizesID 没有对应 assistant 声明时，
// 仍要合成一个非空、且同输入稳定的 tool_call_id。
func TestNormalizeRolesFunctionSynthesizesID(t *testing.T) {
	in := []byte(`{"model":"glm-5.3","messages":[{"role":"function","name":"lookup","content":"x"}]}`)
	a, err := normalizeRoles(in)
	if err != nil {
		t.Fatalf("规范化失败: %v", err)
	}
	b, err := normalizeRoles(in)
	if err != nil {
		t.Fatalf("规范化失败: %v", err)
	}
	if string(a) != string(b) {
		t.Errorf("同一输入两次规范化结果不同：\n%s\n%s", a, b)
	}
	msgs := decodeMsgs(t, a)
	id, _ := msgs[0]["tool_call_id"].(string)
	if id == "" {
		t.Fatal("合成的 tool_call_id 为空")
	}
	if !strings.Contains(id, "lookup") {
		t.Errorf("合成的 tool_call_id = %q，期望带上函数名便于排查", id)
	}
}

// TestNormalizeRolesKeepsAssistantToolCallsOnly 上游支持「assistant + content:null +
// tool_calls」，这条形态必须原样保留（改写 content 会断掉工具调用链）。
func TestNormalizeRolesKeepsAssistantToolCallsOnly(t *testing.T) {
	body := []byte(`{"model":"glm-5.3","messages":[
		{"role":"user","content":"天气"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"w","arguments":"{}"}}]}]}`)

	out, err := normalizeRoles(body)
	if err != nil {
		t.Fatalf("规范化失败: %v", err)
	}
	msgs := decodeMsgs(t, out)
	if got := msgs[1]["content"]; got != nil {
		t.Errorf("assistant.content 被改成了 %v，期望保持 null", got)
	}
	if _, ok := msgs[1]["tool_calls"]; !ok {
		t.Error("assistant.tool_calls 被丢掉了")
	}
}

// TestNormalizeRolesPreservesOtherFields 其余字段（tools / temperature / response_format）
// 必须原样保留 —— 规范化只碰 role 与它直接连带的字段。
func TestNormalizeRolesPreservesOtherFields(t *testing.T) {
	body := []byte(`{"model":"glm-5.3","temperature":0.3,
		"response_format":{"type":"json_object"},
		"tools":[{"type":"function","function":{"name":"w"}}],
		"messages":[{"role":"developer","content":"hi"}]}`)

	out, err := normalizeRoles(body)
	if err != nil {
		t.Fatalf("规范化失败: %v", err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatalf("规范化结果不是合法 JSON: %v", err)
	}
	for _, k := range []string{"temperature", "response_format", "tools", "model"} {
		if _, ok := root[k]; !ok {
			t.Errorf("字段 %s 被丢掉了", k)
		}
	}
}

// TestNormalizeRolesRejectsBadMessages messages 不是数组 / 为空时给出明确的 400。
func TestNormalizeRolesRejectsBadMessages(t *testing.T) {
	cases := []string{
		`{"model":"glm-5.3"}`,
		`{"model":"glm-5.3","messages":[]}`,
		`{"model":"glm-5.3","messages":null}`,
		`{"model":"glm-5.3","messages":"hi"}`,
	}
	for _, body := range cases {
		_, err := normalizeRoles([]byte(body))
		if err == nil {
			t.Errorf("期望报错，实际通过：%s", body)
			continue
		}
		var ire *apierr.InvalidRoleError
		if !errors.As(err, &ire) {
			t.Errorf("%s 的错误类型 = %T，期望 *apierr.InvalidRoleError", body, err)
		}
		if got := apierr.From(err).Status; got != http.StatusBadRequest {
			t.Errorf("%s 的状态码 = %d，期望 400", body, got)
		}
	}
}

// TestInvalidRoleParamPath 错误体里的 param 要指到具体那一条消息。
func TestInvalidRoleParamPath(t *testing.T) {
	err := &apierr.InvalidRoleError{Index: 3, Role: "critic"}
	e := apierr.From(err)
	if e.Param != "messages[3].role" {
		t.Errorf("param = %q，期望 messages[3].role", e.Param)
	}
	if e.Code != apierr.CodeInvalidRole {
		t.Errorf("code = %q，期望 %q", e.Code, apierr.CodeInvalidRole)
	}
	if !strings.Contains(err.Error(), "unsupported role: critic") {
		t.Errorf("message = %q，期望包含 unsupported role: critic", err.Error())
	}
}
