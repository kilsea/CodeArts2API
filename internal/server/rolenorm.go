package server

import (
	"encoding/json"
	"strconv"
	"strings"

	"codearts2api/internal/apierr"
)

// role 规范化层。
//
// 上游只认 system / user / assistant / tool 四种 role，其余一律回
// HTTP 400 {"error_code":"…","message":"Message role cannot empty"} ——
// 那句文案是错的（role 并不空），会把人引向完全错误的排查方向。
//
// 但主流客户端确实会发别的写法：
//
//	developer  OpenAI 2024 新规范（新版 SDK / Cherry Studio 默认用它）
//	function   旧版 tool calling
//	""         部分客户端省略 role 字段
//
// 于是网关在转发前统一改写；改写成什么都不像的 role 直接 400，不再透传。

// supportedRoles 是上游接受的 role 集合。
var supportedRoles = map[string]bool{
	"system":    true,
	"user":      true,
	"assistant": true,
	"tool":      true,
}

// normalizeRoles 规范化请求体里的 messages[].role，返回改写后的请求体。
//
// 改写规则（与 docs/COMPAT.md 的「角色映射」表一一对应）：
//
//	developer          → system
//	function           → tool（name 映射为 tool_call_id，缺失时合成一个）
//	"" / 字段缺失       → 按位置推断：第 0 条为 system，其余为 user
//	system/user/assistant/tool → 原样（role 统一小写）
//
// 其余取值一律返回 *apierr.InvalidRoleError（对外 400 invalid_role）。
//
// 只改 role 与它直接连带的字段，其余字段（content / tool_calls / tools …）
// 原样保留：上游本身就是 OpenAI 兼容协议，网关不做多余的翻译。
func normalizeRoles(body []byte) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, &apierr.InvalidRoleError{Index: -1, Reason: "请求体必须是 JSON 对象"}
	}

	raw, ok := root["messages"]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return nil, &apierr.InvalidRoleError{Index: -1, Reason: "messages 不能为空"}
	}
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return nil, &apierr.InvalidRoleError{Index: -1, Reason: "messages 必须是对象数组"}
	}
	if len(msgs) == 0 {
		return nil, &apierr.InvalidRoleError{Index: -1, Reason: "messages 不能为空"}
	}

	// toolCallIDs 记录前面 assistant 消息声明的 tool_call id（按函数名索引），
	// 供后面的 role=function 消息认领 —— 这样 function → tool 的改写能对上真实的
	// tool_call_id，而不是凭名字瞎编一个上游不认识的 id。
	toolCallIDs := map[string]string{}

	for i, m := range msgs {
		role := decodeRole(m["role"])

		switch strings.ToLower(strings.TrimSpace(role)) {
		case "system", "user", "assistant", "tool":
			setRole(m, strings.ToLower(strings.TrimSpace(role)))
		case "developer":
			// OpenAI 2024 的 developer 与 system 语义等价（更高优先级指令）。
			setRole(m, "system")
		case "function":
			name := decodeString(m["name"])
			id := toolCallIDs[name]
			if id == "" {
				// 合成一个稳定的 id：同名函数恒定得到同一个值，
				// 上游只要求它非空，不校验是否真的存在过。
				id = "call_" + sanitizeID(name) + "_" + strconv.Itoa(i)
			}
			setRole(m, "tool")
			m["tool_call_id"] = mustJSON(id)
			// name 在 OpenAI 规范里是 tool 消息的可选字段，但上游对多余字段
			// 一向不宽容；映射完成后它已无信息量，直接删掉。
			delete(m, "name")
		case "":
			// 字段缺失或空串：按位置推断。
			inferred := "user"
			if i == 0 {
				inferred = "system"
			}
			setRole(m, inferred)
		default:
			return nil, &apierr.InvalidRoleError{Index: i, Role: strings.TrimSpace(role)}
		}

		// assistant 的 tool_calls 是后续 tool 消息的 tool_call_id 来源。
		// 注意：assistant 且 content 为 null、只有 tool_calls 的形态必须原样保留 ——
		// 上游支持它，改写 content 会让工具调用链断掉。
		if strings.EqualFold(decodeRole(m["role"]), "assistant") {
			collectToolCallIDs(m, toolCallIDs)
		}
	}

	mb, err := json.Marshal(msgs)
	if err != nil {
		return nil, err
	}
	root["messages"] = mb
	return json.Marshal(root)
}

// setRole 写入规范化后的 role。
func setRole(m map[string]json.RawMessage, role string) {
	m["role"] = mustJSON(role)
}

// collectToolCallIDs 把一条 assistant 消息里的 tool_calls 记进索引。
func collectToolCallIDs(m map[string]json.RawMessage, index map[string]string) {
	raw, ok := m["tool_calls"]
	if !ok {
		return
	}
	var calls []struct {
		ID       string `json:"id"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &calls) != nil {
		return
	}
	for _, c := range calls {
		if c.ID != "" && c.Function.Name != "" {
			index[c.Function.Name] = c.ID
		}
	}
}

// decodeRole 取出 role 字段的字符串值（缺失 / null / 非字符串都返回空串）。
func decodeRole(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// decodeString 取出一个可选的字符串字段。
func decodeString(raw json.RawMessage) string {
	return decodeRole(raw)
}

// sanitizeID 把函数名压成适合放进 tool_call_id 的形态。
func sanitizeID(name string) string {
	if name == "" {
		return "fn"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// mustJSON 序列化一个字符串值；字符串不可能序列化失败，失败时退回 JSON null。
func mustJSON(s string) json.RawMessage {
	b, err := json.Marshal(s)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}
