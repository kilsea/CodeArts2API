package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codearts2api/internal/store"
)

// 运维相关接口的回归：count_tokens、日志过滤、用量聚合、构建信息。

// TestCountTokens 必须本地估算、不打上游、不消耗积分。
func TestCountTokens(t *testing.T) {
	s, cap := setupCompatServer(t)

	body := `{"model":"glm-5.3","messages":[{"role":"user","content":"hello world"}]}`
	rec := httptest.NewRecorder()
	s.handleAnthropicCountTokens(rec, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d：%s", rec.Code, rec.Body.String())
	}
	var out struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if out.InputTokens <= 0 {
		t.Errorf("input_tokens = %d，期望 > 0", out.InputTokens)
	}
	if cap.count() != 0 {
		t.Error("count_tokens 不该打到上游")
	}

	// 空 messages 仍然要拒（与 /v1/messages 一致）。
	rec = httptest.NewRecorder()
	s.handleAnthropicCountTokens(rec, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"glm-5.3","messages":[]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("空 messages 状态码 = %d，期望 400", rec.Code)
	}
}

// TestAdminLogsFilters 服务端过滤：状态码区间、分类、模型、关键字。
func TestAdminLogsFilters(t *testing.T) {
	s, _ := setupCompatServer(t)
	base := time.Now().Unix()
	s.Store.Record(store.LogEntry{Time: base, RequestID: "ok-1", Model: "glm-5.3", Status: 200}, 0)
	s.Store.Record(store.LogEntry{Time: base + 1, RequestID: "bad-1", Model: "glm-5.3", Status: 400,
		ErrClass: "client_request", Err: "invalid_role"}, 0)
	s.Store.Record(store.LogEntry{Time: base + 2, RequestID: "up-1", Model: "kimi-k3", Status: 502,
		ErrClass: "upstream"}, 0)

	get := func(query string) []store.LogEntry {
		rec := httptest.NewRecorder()
		s.handleAdminLogs(rec, httptest.NewRequest(http.MethodGet, "/admin/api/logs"+query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s 状态码 = %d", query, rec.Code)
		}
		var out struct {
			Logs []store.LogEntry `json:"logs"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s 响应不是合法 JSON: %v", query, err)
		}
		return out.Logs
	}

	if got := get("?status=4xx"); len(got) != 1 || got[0].RequestID != "bad-1" {
		t.Errorf("status=4xx 结果不对: %+v", got)
	}
	if got := get("?class=upstream"); len(got) != 1 || got[0].RequestID != "up-1" {
		t.Errorf("class=upstream 结果不对: %+v", got)
	}
	if got := get("?model=kimi-k3"); len(got) != 1 {
		t.Errorf("model 过滤结果不对: %+v", got)
	}
	if got := get("?q=invalid_role"); len(got) != 1 || got[0].RequestID != "bad-1" {
		t.Errorf("关键字过滤结果不对: %+v", got)
	}
	// 返回顺序仍是「最新在前」。
	all := get("")
	if len(all) != 3 || all[0].RequestID != "up-1" {
		t.Errorf("全量顺序不对: %+v", all)
	}
}

// TestAdminUsage 用量聚合接口。
func TestAdminUsage(t *testing.T) {
	s, _ := setupCompatServer(t)
	s.Store.Record(store.LogEntry{Time: time.Now().Unix(), Model: "glm-5.3", Status: 200, PromptLen: 8}, 4)

	rec := httptest.NewRecorder()
	s.handleAdminUsage(rec, httptest.NewRequest(http.MethodGet, "/admin/api/usage?days=7", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	var out struct {
		Days int              `json:"days"`
		Rows []store.UsageRow `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if out.Days != 7 || len(out.Rows) != 1 {
		t.Fatalf("用量响应不对: %+v", out)
	}
	if out.Rows[0].Requests != 1 || out.Rows[0].CharsOut != 4 {
		t.Errorf("用量聚合不对: %+v", out.Rows[0])
	}
}

// TestHealthAndStateExposeBuild /health 与 /admin/api/state 都要带版本，
// 否则升级后没法确认「现在跑的到底是不是刚编的那个」。
func TestHealthAndStateExposeBuild(t *testing.T) {
	s, _ := setupCompatServer(t)
	SetBuildInfo("v9.9.9", "abc1234", "2026-01-01T00:00:00Z")
	t.Cleanup(func() { SetBuildInfo("dev", "", "") })

	rec := httptest.NewRecorder()
	s.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	var h map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if h["version"] != "v9.9.9" || h["commit"] != "abc1234" {
		t.Errorf("/health 缺少构建信息: %+v", h)
	}

	rec = httptest.NewRecorder()
	s.handleAdminState(rec, httptest.NewRequest(http.MethodGet, "/admin/api/state", nil))
	var st map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	build, ok := st["build"].(map[string]any)
	if !ok || build["version"] != "v9.9.9" {
		t.Errorf("/admin/api/state 缺少构建信息: %+v", st["build"])
	}
}
