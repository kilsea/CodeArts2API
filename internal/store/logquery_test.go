package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 日志落盘与查询的回归。
//
// 关键约束：
//   - 日志不再写进 state.json，重启后必须从 JSONL 恢复；
//   - 旧版本（state.json 里带 logs）要能一次性迁过来；
//   - 清空日志要同时清内存与文件。

func TestLogsPersistAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s1.Record(LogEntry{Time: time.Now().Unix(), RequestID: "rid-1", Model: "glm-5.3", Status: 200}, 42)
	// Record 本身不落 state.json（只有统计/用量需要延迟落盘），这里显式存一次
	// 验证「日志不再写进状态文件」。
	if err := s1.Save(); err != nil {
		t.Fatal(err)
	}
	s1.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if containsJSONField(raw, `"logs"`) {
		t.Error("日志不该再写进 state.json")
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	logs := s2.Logs(0)
	if len(logs) != 1 || logs[0].RequestID != "rid-1" {
		t.Fatalf("重启后日志丢失: %+v", logs)
	}
}

// TestLogsMigrateFromStateFile 旧版本把日志写在 state.json 里，启动时迁到 JSONL。
func TestLogsMigrateFromStateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := `{"version":2,"logs":[{"time":1700000000,"request_id":"old-1","model":"glm-5.3","status":200}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	logs := s.Logs(0)
	if len(logs) != 1 || logs[0].RequestID != "old-1" {
		t.Fatalf("旧日志没有迁移过来: %+v", logs)
	}
	s.Close()

	// 再开一次：这次应当从 JSONL 读到，而不是又依赖 state.json。
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := s2.Logs(0); len(got) != 1 || got[0].RequestID != "old-1" {
		t.Fatalf("JSONL 回读失败: %+v", got)
	}
}

func TestClearLogsRemovesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Record(LogEntry{Time: time.Now().Unix(), RequestID: "rid-x", Status: 200}, 1)
	s.ClearLogs()
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := s2.Logs(0); len(got) != 0 {
		t.Fatalf("清空后仍有 %d 条", len(got))
	}
}

// TestQueryLogsFilter 过滤在服务端做：状态码（含 4xx 区间）、分类、模型、关键字、起始时间。
func TestQueryLogsFilter(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := int64(1700000000)
	rows := []LogEntry{
		{Time: base, RequestID: "a", Model: "glm-5.3", Status: 200},
		{Time: base + 1, RequestID: "b", Model: "glm-5.3", Status: 400, ErrClass: "client_request", Err: "invalid_role"},
		{Time: base + 2, RequestID: "c", Model: "kimi-k3", Status: 502, ErrClass: "upstream", Account: "acct-c"},
	}
	for _, r := range rows {
		s.Record(r, 0)
	}

	cases := []struct {
		name string
		f    LogFilter
		want []string
	}{
		{"全部", LogFilter{}, []string{"a", "b", "c"}},
		{"精确状态", LogFilter{Status: "502"}, []string{"c"}},
		{"4xx 区间", LogFilter{Status: "4xx"}, []string{"b"}},
		{"5xx 区间", LogFilter{Status: "5xx"}, []string{"c"}},
		{"分类", LogFilter{Class: "client_request"}, []string{"b"}},
		{"模型", LogFilter{Model: "glm-5.3"}, []string{"a", "b"}},
		{"关键字命中错误原文", LogFilter{Q: "INVALID_ROLE"}, []string{"b"}},
		{"关键字命中账号", LogFilter{Q: "acct-c"}, []string{"c"}},
		{"起始时间", LogFilter{Since: base + 1}, []string{"b", "c"}},
	}
	for _, c := range cases {
		got := s.QueryLogs(c.f, 0)
		if len(got) != len(c.want) {
			t.Errorf("%s: 得到 %d 条，期望 %d", c.name, len(got), len(c.want))
			continue
		}
		for i := range got {
			if got[i].RequestID != c.want[i] {
				t.Errorf("%s: 第 %d 条 = %q，期望 %q", c.name, i, got[i].RequestID, c.want[i])
			}
		}
	}

	// limit 取最近的 N 条。
	if got := s.QueryLogs(LogFilter{}, 2); len(got) != 2 || got[1].RequestID != "c" {
		t.Errorf("limit 语义不对: %+v", got)
	}
}

// TestUsageAggregates 用量按「天 + 模型」聚合，超过保留天数的行被裁掉。
func TestUsageAggregates(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now()
	for i := 0; i < 3; i++ {
		s.Record(LogEntry{Time: now.Unix(), Model: "glm-5.3", Status: 200, PromptLen: 10}, 5)
	}
	s.Record(LogEntry{Time: now.Unix(), Model: "kimi-k3", Status: 200, PromptLen: 1}, 2)

	rows := s.Usage(7)
	if len(rows) != 2 {
		t.Fatalf("聚合行数 = %d，期望 2", len(rows))
	}
	for _, r := range rows {
		if r.Model == "glm-5.3" {
			if r.Requests != 3 || r.CharsIn != 30 || r.CharsOut != 15 {
				t.Errorf("glm-5.3 聚合不对: %+v", r)
			}
		}
	}

	// 造一条 40 天前的记录：应该被裁掉。
	old := now.AddDate(0, 0, -40)
	s.Record(LogEntry{Time: old.Unix(), Model: "old-model", Status: 200}, 1)
	for _, r := range s.Usage(UsageKeepDays) {
		if r.Model == "old-model" {
			t.Error("超过保留天数的用量行应被裁剪")
		}
	}
}

func containsJSONField(raw []byte, field string) bool {
	return len(raw) > 0 && stringContains(string(raw), field)
}

func stringContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
