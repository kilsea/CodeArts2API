package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"codearts2api/internal/codearts"
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

// TestUsageKeyedByAccount 用量桶必须带账号维度：同一个模型在不同账号上
// 是两行，各自的积分独立累计（积分是账号的额度）。
func TestUsageKeyedByAccount(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().Unix()
	s.Record(LogEntry{Time: now, AccountID: "a", Account: "acct-a", Model: "glm-5.3",
		Status: 200, PromptLen: 10, InTokens: 100, OutTokens: 20, Credits: 0.5, CreditSource: "upstream"}, 5)
	s.Record(LogEntry{Time: now, AccountID: "b", Account: "acct-b", Model: "glm-5.3",
		Status: 200, PromptLen: 10, InTokens: 300, OutTokens: 40, Credits: 1.5, CreditSource: "upstream"}, 5)

	rows := s.Usage(7)
	if len(rows) != 2 {
		t.Fatalf("按账号分桶后应有两行，得到 %d: %+v", len(rows), rows)
	}
	byAcct := map[string]UsageRow{}
	for _, r := range rows {
		byAcct[r.AccountID] = r
	}
	if got := byAcct["a"]; got.Credits != 0.5 || got.InTokens != 100 || got.Requests != 1 {
		t.Errorf("账号 a 聚合不对: %+v", got)
	}
	if got := byAcct["b"]; got.Credits != 1.5 || got.OutTokens != 40 {
		t.Errorf("账号 b 聚合不对: %+v", got)
	}

	total := SumUsage(rows)
	if total.Requests != 2 || total.Credits != 2 || total.InTokens != 400 {
		t.Errorf("总计不对: %+v", total)
	}
	if s.Stats().TotalCredits != 2 {
		t.Errorf("Stats.TotalCredits = %v，期望 2", s.Stats().TotalCredits)
	}

	// 按模型 / 按账号归并都要能回到同一份总计（口径只有一处）。
	if m := UsageByModel(rows); len(m) != 1 || m[0].Credits != 2 {
		t.Errorf("按模型归并不对: %+v", m)
	}
	if a := UsageByAccount(rows); len(a) != 2 {
		t.Errorf("按账号归并不对: %+v", a)
	}
}

// TestCreditSpendLedger 账号积分快照差值：只累计增量，额度重置（Used 变小）
// 不能被记成负数或倒扣。
// 小时桶是时序图的数据源：同一小时累加、跨小时分开、超过保留窗口裁掉。
func TestUsageHourlyBucket(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().Truncate(time.Hour)
	s.Record(LogEntry{Time: now.Add(5 * time.Minute).Unix(), AccountID: "a", Account: "acct-a",
		Model: "glm-5.3", Status: 200, InTokens: 100, OutTokens: 20, CachedTokens: 40,
		Credits: 1.5, LatencyMS: 800}, 20)
	s.Record(LogEntry{Time: now.Add(20 * time.Minute).Unix(), AccountID: "a", Account: "acct-a",
		Model: "glm-5.3", Status: 500, Err: "boom", LatencyMS: 200}, 0)

	rows := s.UsageHourly(48)
	if len(rows) != 1 {
		t.Fatalf("同一小时应合成 1 行，得到 %d: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.Requests != 2 || r.InTokens != 100 || r.OutTokens != 20 || r.CachedTokens != 40 {
		t.Errorf("小时后聚合不对: %+v", r)
	}
	if r.Credits != 1.5 || r.LatencyMS != 1000 || r.Failures != 1 {
		t.Errorf("积分/延迟/失败数不对: %+v", r)
	}
	// 吐字速度的分母只算「有输出的请求」，平均延迟的分母只算「有耗时记录的请求」。
	if r.TimedRequests != 2 || r.TimedOutTokens != 20 || r.OutLatencyMS != 800 {
		t.Errorf("吐字/延迟分母不对: %+v", r)
	}

	// 跨小时必须分开，否则时序图会变成「一天一根柱」。
	s.Record(LogEntry{Time: now.Add(time.Hour).Unix(), Model: "glm-5.3", Status: 200, LatencyMS: 100}, 1)
	if got := len(s.UsageHourly(48)); got != 2 {
		t.Errorf("跨小时应分成 2 行，得到 %d", got)
	}

	// 超过 72 小时的桶要么没进来、要么被裁掉（取决于插入顺序）。
	s.Record(LogEntry{Time: now.Add(-80 * time.Hour).Unix(), Model: "old", Status: 200}, 1)
	for _, r := range s.UsageHourly(72) {
		if r.Model == "old" {
			t.Error("超过保留窗口的小时桶应被裁掉")
		}
	}
}

func TestCreditSpendLedger(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.UpsertAccount(&codearts.Account{ID: "a", Name: "acct-a", Enabled: true})

	// 第一次快照只建立基线。
	s.SetAccountCredit("a", &codearts.CreditInfo{Available: true, Total: 100, Used: 10, Remaining: 90, ResetDate: "2026-12-21"})
	if rows := s.CreditSpend(2); len(rows) != 0 {
		t.Fatalf("首次快照不应产生消耗，得到 %+v", rows)
	}

	// 第二次 +1.5 → 记 1.5。
	s.SetAccountCredit("a", &codearts.CreditInfo{Available: true, Total: 100, Used: 11.5, Remaining: 88.5, ResetDate: "2026-12-21"})
	rows := s.CreditSpend(2)
	if len(rows) != 1 || rows[0].Credits != 1.5 || rows[0].AccountID != "a" {
		t.Fatalf("差值记账不对: %+v", rows)
	}

	// 同一天再 +0.5 → 合并进同一行。
	s.SetAccountCredit("a", &codearts.CreditInfo{Available: true, Total: 100, Used: 12, Remaining: 88, ResetDate: "2026-12-21"})
	rows = s.CreditSpend(2)
	if len(rows) != 1 || rows[0].Credits != 2 {
		t.Fatalf("同日合并不对: %+v", rows)
	}

	// 额度重置：Used 归零 → 不产生（负）消耗。
	s.SetAccountCredit("a", &codearts.CreditInfo{Available: true, Total: 100, Used: 0.2, Remaining: 99.8, ResetDate: "2027-01-21"})
	rows = s.CreditSpend(2)
	if len(rows) != 1 || rows[0].Credits != 2 {
		t.Fatalf("重置后不应倒扣: %+v", rows)
	}

	// 重置后重新累计：从 0.2 涨到 0.9 → +0.7。
	s.SetAccountCredit("a", &codearts.CreditInfo{Available: true, Total: 100, Used: 0.9, Remaining: 99.1, ResetDate: "2027-01-21"})
	rows = s.CreditSpend(2)
	if len(rows) != 1 || rows[0].Credits != 2.7 {
		t.Fatalf("重置后增量不对: %+v", rows)
	}

	// 重置到恰好 0（不是 0.2）：之后第一次增长 0 → 0.5 必须被记上。
	// 拿「CreditUsed > 0」当基线判据的实现会把这 0.5 永久丢掉。
	s.SetAccountCredit("a", &codearts.CreditInfo{Available: true, Total: 100, Used: 0, Remaining: 100, ResetDate: "2027-02-21"})
	s.SetAccountCredit("a", &codearts.CreditInfo{Available: true, Total: 100, Used: 0.5, Remaining: 99.5, ResetDate: "2027-02-21"})
	rows = s.CreditSpend(2)
	if len(rows) != 1 || rows[0].Credits != 3.2 {
		t.Fatalf("从 0 起算的增量丢了: %+v", rows)
	}
}

// TestLogFilterAccountAndCredits 新增的两个筛选维度：账号与最低积分。
func TestLogFilterAccountAndCredits(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().Unix()
	s.Record(LogEntry{Time: now, RequestID: "cheap", Account: "acct-a", Model: "m", Status: 200, Credits: 0.01}, 0)
	s.Record(LogEntry{Time: now + 1, RequestID: "pricey", Account: "acct-a", Model: "m", Status: 200, Credits: 2.5}, 0)
	s.Record(LogEntry{Time: now + 2, RequestID: "other", Account: "acct-b", Model: "m", Status: 200, Credits: 1}, 0)

	if got := s.QueryLogs(LogFilter{Account: "acct-a"}, 0); len(got) != 2 {
		t.Errorf("按账号筛选得到 %d 条，期望 2", len(got))
	}
	got := s.QueryLogs(LogFilter{MinCredits: 1}, 0)
	if len(got) != 2 || got[0].RequestID != "pricey" || got[1].RequestID != "other" {
		t.Errorf("按最低积分筛选不对: %+v", got)
	}
}
