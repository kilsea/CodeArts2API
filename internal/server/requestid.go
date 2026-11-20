package server

import (
	"context"
	"net/http"
	"strings"

	"codearts2api/internal/codearts"
)

// 请求关联标识（X-Request-Id）。
//
// 排障时最痛的一点是「客户端贴来一条错误，日志里搜不到对应请求」——
// 请求日志里没有正文（设计如此），也没有能把客户端响应与服务端日志
// 对上的唯一标识。这里给每个请求分配一个 id：
//
//   - 响应头 X-Request-Id 一定带（成功与失败都有）；
//   - 错误体里的 message 不变，但客户端可凭响应头回查日志；
//   - 请求日志 LogEntry.RequestID 同值，控制台可按 id 过滤。

// requestIDKey 是 context 里的键（非导出类型，避免与其它包冲突）。
type requestIDKey struct{}

// withRequestID 为每个请求分配（或沿用客户端传入的）关联 id。
func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get(requestIDHeader))
		if id == "" {
			id = codearts.RandomHex(8)
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

// withRequestIDValue 确保请求上下文里带有关联 id，并把它写进响应头。
//
// 正常路径由 withRequestID 中间件提供；这里是兜底，让 handler 自己也能
// 保证「响应头一定带 X-Request-Id」这条契约 —— 测试与重放接口都是直接
// 调用 handler，不经过中间件。
func withRequestIDValue(w http.ResponseWriter, r *http.Request) *http.Request {
	if id := requestID(r.Context()); id != "" {
		return r
	}
	id := sanitizeRequestID(r.Header.Get(requestIDHeader))
	if id == "" {
		id = codearts.RandomHex(8)
	}
	if w.Header().Get(requestIDHeader) == "" {
		w.Header().Set(requestIDHeader, id)
	}
	return r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id))
}

// requestID 取出当前请求的关联 id（没有时返回空串）。
func requestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// sanitizeRequestID 校验客户端传入的 id：只接受短的可打印 ASCII，
// 防止把换行 / 控制字符回写进响应头（响应头注入）。
func sanitizeRequestID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 64 {
		return ""
	}
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return ""
		}
	}
	return raw
}
