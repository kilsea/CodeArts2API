package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
)

// 失败请求重放。
//
// 用途：客户端报告「某次请求失败了」，用响应头里的 X-Request-Id 就能把
// 那次请求**原样**再发一遍（前提是开了采样，见 dump.go）。不用再猜
// 「他到底发了什么」，也不用让对方装抓包工具。
//
// 安全边界：
//   - 走管理鉴权（withAdminAuth），与其它 /admin/api/* 一致；
//   - 只重放采样文件里真实存在的那一条，不接受客户端自带请求体；
//   - 重放会真实消耗积分（和原请求一样），因此响应里明确回传实际状态码。

// replayPrefix 是重放路由前缀。
const replayPrefix = "/admin/api/replay/"

// handleAdminReplay 实现 POST /admin/api/replay/<request-id>。
func (s *Server) handleAdminReplay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST", "invalid_request_error")
		return
	}
	if !s.dumps.Enabled() {
		writeErr(w, http.StatusConflict,
			"请求采样未开启：请在控制台「日志 → 调试采样」打开开关（默认关闭，保留 3 天）",
			"invalid_request_error")
		return
	}
	rid := strings.TrimPrefix(r.URL.Path, replayPrefix)
	rid = strings.TrimSpace(rid)
	if rid == "" || strings.ContainsAny(rid, "/\\") {
		writeErr(w, http.StatusBadRequest, "缺少 request id", "invalid_request_error")
		return
	}
	entry, ok := s.dumps.Find(rid)
	if !ok {
		writeErr(w, http.StatusNotFound,
			"找不到该 request id 的采样（可能未命中采样率，或已随滚动文件清理）",
			"invalid_request_error")
		return
	}

	// 在内部重建一次请求：不经过鉴权 / 限流（管理端已经鉴权过），
	// 但要经过与线上完全相同的处理链（校验 → 规范化 → 选号 → 上游）。
	body := []byte(entry.Body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, entry.Path, bytes.NewReader(body))
	// RemoteAddr 设成环回：与「本机直连」语义一致，避免误触发来源限制。
	req.RemoteAddr = "127.0.0.1:0"
	req.Header.Set("Content-Type", "application/json")
	// 沿用原 request id 的派生 id，避免重放结果覆盖原日志的检索键。
	req.Header.Set(requestIDHeader, rid+"-replay")

	switch entry.Path {
	case "/v1/messages":
		s.handleAnthropicMessages(rec, req)
	case "/v1/responses":
		s.handleResponses(rec, req)
	default:
		s.handleChatCompletions(rec, req)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"request_id": rid,
		"path":       entry.Path,
		"status":     rec.Code,
		"headers": map[string]string{
			"content_type": rec.Header().Get("Content-Type"),
			"request_id":   rec.Header().Get(requestIDHeader),
		},
		"body": json.RawMessage(rec.Body.Bytes()),
	})
}
