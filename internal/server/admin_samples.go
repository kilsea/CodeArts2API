package server

import (
	"net/http"
	"strings"
)

// 调试采样的控制台接口。
//
// 与 dump.go 的分工：那边管「怎么采、怎么滚、怎么过期删」，这里只管
// 「控制台能看到什么、能不能取回一条完整请求体」。
//
// 安全边界与其它 /admin/api/* 一致（withAdminAuth）：
//   - 采样文件里是**脱敏后**的请求正文，仍然可能包含用户对话内容；
//   - 因此只在本机直连 / 管理密码通过时可见，且响应一律 no-store。

// samplesPrefix 是单条采样的路由前缀。
const samplesPrefix = "/admin/api/samples/"

// handleAdminSamples 返回调试采样概览：开关、采样率、目录、保留天数、最近条目。
func (s *Server) handleAdminSamples(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 GET", "invalid_request_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":     s.dumps.Enabled(),
		"sample_rate": s.dumps.SampleRate(),
		"dir":         s.dumps.Dir(),
		"keep_days":   maxDumpDays,
		"samples":     s.dumps.Recent(intQuery(r, "limit")),
	})
}

// handleAdminSampleDetail 返回单条采样（含脱敏后的原始请求体）。
func (s *Server) handleAdminSampleDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 GET", "invalid_request_error")
		return
	}
	rid := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, samplesPrefix))
	// request id 会拼进文件名扫描逻辑，必须挡住路径分隔符。
	if rid == "" || strings.ContainsAny(rid, "/\\") {
		writeErr(w, http.StatusBadRequest, "缺少 request id", "invalid_request_error")
		return
	}
	entry, ok := s.dumps.Find(rid)
	if !ok {
		writeErr(w, http.StatusNotFound,
			"找不到该 request id 的采样（可能未命中采样率，或已超过保留天数被清理）",
			"invalid_request_error")
		return
	}
	writeJSON(w, http.StatusOK, entry)
}
