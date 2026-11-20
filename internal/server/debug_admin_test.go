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

// 调试采样的控制台侧回归：
//   - 默认关闭，开关可以运行时切换（不重启）；
//   - 日志按「最新在前」返回；
//   - 采样列表 / 单条详情接口可用，且 request id 不能穿越路径。

// TestDebugCaptureDefaultOff 默认不落盘：状态文件里没打开开关时一条都不写。
func TestDebugCaptureDefaultOff(t *testing.T) {
	dir := t.TempDir()
	s, _ := setupCompatServer(t)
	s.dumps = NewDumper(dir)
	t.Cleanup(s.dumps.Close)
	s.ApplyDebug() // 设置里默认 false

	postChat(t, s, `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)

	if s.dumps.Enabled() {
		t.Fatal("默认不应开启采样")
	}
	if got := s.dumps.Recent(0); len(got) != 0 {
		t.Errorf("默认不应有采样记录，得到 %d 条", len(got))
	}
}

// TestDebugCaptureRuntimeToggle 打开后立即采样，关掉后立即停止。
func TestDebugCaptureRuntimeToggle(t *testing.T) {
	s, _ := setupCompatServer(t)
	s.dumps = NewDumper(t.TempDir())
	t.Cleanup(s.dumps.Close)

	// 通过设置接口打开（模拟控制台点开关）。
	rec := httptest.NewRecorder()
	body := `{"debug_capture":true,"debug_capture_sample":1}`
	s.handleAdminSettings(rec, httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("设置接口状态码 = %d：%s", rec.Code, rec.Body.String())
	}
	if !s.dumps.Enabled() {
		t.Fatal("打开开关后应立即可用（不需要重启）")
	}

	postChat(t, s, `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
	if got := s.dumps.Recent(0); len(got) != 1 {
		t.Fatalf("采样条数 = %d，期望 1", len(got))
	}

	// 关掉后不再新增。
	rec = httptest.NewRecorder()
	s.handleAdminSettings(rec, httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(`{"debug_capture":false}`)))
	if s.dumps.Enabled() {
		t.Fatal("关闭开关后应停止采样")
	}
	postChat(t, s, `{"model":"glm-5.3","messages":[{"role":"user","content":"hi again"}]}`)
	if got := s.dumps.Recent(0); len(got) != 1 {
		t.Errorf("关闭后仍在采样：%d 条", len(got))
	}
}

// TestAdminSamplesAPI 列表按最新在前返回，详情能取回脱敏后的请求体。
func TestAdminSamplesAPI(t *testing.T) {
	s, _ := setupCompatServer(t)
	s.dumps = newDumperAt(t, t.TempDir(), 1)
	s.dumps.SetEnabled(true)

	postChat(t, s, `{"model":"glm-5.3","messages":[{"role":"user","content":"first"}]}`)
	postChat(t, s, `{"model":"glm-5.3","messages":[{"role":"user","content":"second"}]}`)

	rec := httptest.NewRecorder()
	s.handleAdminSamples(rec, httptest.NewRequest(http.MethodGet, "/admin/api/samples?limit=10", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("列表状态码 = %d", rec.Code)
	}
	var list struct {
		Enabled    bool         `json:"enabled"`
		SampleRate int          `json:"sample_rate"`
		Dir        string       `json:"dir"`
		KeepDays   int          `json:"keep_days"`
		Samples    []SampleMeta `json:"samples"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("列表响应不是合法 JSON: %v", err)
	}
	if !list.Enabled || list.SampleRate != 1 || list.KeepDays != maxDumpDays {
		t.Errorf("列表元信息不对: %+v", list)
	}
	if len(list.Samples) != 2 {
		t.Fatalf("采样条数 = %d，期望 2", len(list.Samples))
	}
	// 最新在前：第二条是 "second" 那次请求。
	rid := list.Samples[0].RequestID
	if rid == "" {
		t.Fatal("采样缺少 request id")
	}

	rec = httptest.NewRecorder()
	s.handleAdminSampleDetail(rec, httptest.NewRequest(http.MethodGet, samplesPrefix+rid, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("详情状态码 = %d：%s", rec.Code, rec.Body.String())
	}
	var entry DumpEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &entry); err != nil {
		t.Fatalf("详情响应不是合法 JSON: %v", err)
	}
	if !strings.Contains(string(entry.Body), "second") {
		t.Errorf("详情里应是第二条请求体：%s", entry.Body)
	}

	// 未知 id → 404；带路径分隔符 → 400。
	rec = httptest.NewRecorder()
	s.handleAdminSampleDetail(rec, httptest.NewRequest(http.MethodGet, samplesPrefix+"nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("未知 id 状态码 = %d，期望 404", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.handleAdminSampleDetail(rec, httptest.NewRequest(http.MethodGet, samplesPrefix+"../x", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("路径穿越状态码 = %d，期望 400", rec.Code)
	}
}

// TestAdminLogsNewestFirst 日志接口必须「最新的在前」。
func TestAdminLogsNewestFirst(t *testing.T) {
	s, _ := setupCompatServer(t)
	base := time.Now().Unix()
	s.Store.Record(store.LogEntry{Time: base, Model: "old"}, 0)
	s.Store.Record(store.LogEntry{Time: base + 1, Model: "new"}, 0)

	rec := httptest.NewRecorder()
	s.handleAdminLogs(rec, httptest.NewRequest(http.MethodGet, "/admin/api/logs?limit=10", nil))
	var out struct {
		Logs []store.LogEntry `json:"logs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("日志响应不是合法 JSON: %v", err)
	}
	if len(out.Logs) != 2 {
		t.Fatalf("日志条数 = %d，期望 2", len(out.Logs))
	}
	if out.Logs[0].Model != "new" {
		t.Errorf("第一条应是新日志，得到 %q", out.Logs[0].Model)
	}
}
