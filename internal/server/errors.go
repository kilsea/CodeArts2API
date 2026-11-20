package server

import (
	"net/http"
	"strconv"
	"time"

	"codearts2api/internal/apierr"
)

// 错误响应体规范化。
//
// 背景：以前 /v1/* 的错误体是 {"error":{"message":…,"type":…,"code":400}} ——
// `code` 是一个 HTTP 状态码整数，等于把状态码抄了一遍，没有携带任何
// 客户端可以用来分流的语义。上游的英文原文（`Message role cannot empty`）
// 又被原样塞进 message，客户端只能看到一个笼统的 invalid_request_error。
//
// 现在统一为 OpenAI 规范形状：
//
//	{"error":{"message":…,"type":…,"code":"invalid_role","param":"messages[2].role"}}
//
//   - code 是稳定的字符串码（apierr.Code*），客户端可据此本地修正；
//   - param 指出出错字段路径，缺省时省略；
//   - 映射不出规范码的上游错误额外带 upstream_error（上游原文）。
//
// Anthropic 侧形状不同（{"type":"error","error":{…}}），由 writeAnthropicErr 负责。

// writeOpenAIErr 写出 OpenAI 规范错误体。
func writeOpenAIErr(w http.ResponseWriter, status int, e apierr.Error, msg string) {
	writeRetryAfter(w, e.RetryAfter)
	body := map[string]any{
		"message": msg,
		"type":    e.OpenAI,
	}
	if e.Code != "" {
		body["code"] = e.Code
	}
	if e.Param != "" {
		body["param"] = e.Param
	}
	if e.Upstream != "" {
		body["upstream_error"] = e.Upstream
	}
	writeJSON(w, status, map[string]any{"error": body})
}

// writeAnthropicAPIErr 写出 Anthropic 规范错误体。
func writeAnthropicAPIErr(w http.ResponseWriter, e apierr.Error, msg string) {
	writeRetryAfter(w, e.RetryAfter)
	body := map[string]any{"type": e.Anthropic, "message": msg}
	if e.Code != "" {
		body["code"] = e.Code
	}
	if e.Param != "" {
		body["param"] = e.Param
	}
	if e.Upstream != "" {
		body["upstream_error"] = e.Upstream
	}
	writeJSON(w, e.Status, map[string]any{"type": "error", "error": body})
}

// writeRequestErr 用于「网关自己判定的请求侧错误」（role 非法、参数越界等）：
// 这类错误没有内部 error 值，直接给出状态码与规范码。
func writeRequestErr(w http.ResponseWriter, status int, msg, typ, code, param string) {
	writeOpenAIErr(w, status, apierr.Error{OpenAI: typ, Code: code, Param: param}, msg)
}

// writeRetryAfter 在错误可重试时补上 Retry-After 头。
//
// 必须与状态码一起给出：只给 429 不给等待时长，客户端只能自己猜退避，
// 通常就是立刻重试，把并发槽位又撞满。
func writeRetryAfter(w http.ResponseWriter, d time.Duration) {
	if d <= 0 {
		return
	}
	if w.Header().Get("Retry-After") != "" {
		return
	}
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(d)))
}

// requestIDHeader 是贯穿网关与日志的关联标识。
const requestIDHeader = "X-Request-Id"
