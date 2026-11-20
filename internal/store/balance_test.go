package store

import (
	"path/filepath"
	"testing"
	"time"

	"codearts2api/internal/codearts"
)

// 余额（积分水位）采样的回归。
//
// 关键约束：
//   - 30 分钟桶内只保留最后一次值（刷新 10 分钟一次，不能把状态写胖）；
//   - 超过保留期的样本会被裁掉；
//   - 上一次快照能补一个历史起点，新部署不必等半小时才有线。

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestBalanceSampleBucketCoalesce(t *testing.T) {
	s := openTemp(t)
	base := time.Date(2026, 11, 20, 10, 5, 0, 0, time.Local)

	s.AddBalanceSample(base, "a", "A", 100, 200, 100)
	s.AddBalanceSample(base.Add(10*time.Minute), "a", "A", 90, 200, 110)

	got := s.BalanceSamples(0)
	if len(got) != 1 {
		t.Fatalf("同桶应该只留一条，实际 %d 条: %+v", len(got), got)
	}
	if got[0].Remaining != 90 {
		t.Errorf("同桶应保留最后一次值，Remaining = %v", got[0].Remaining)
	}

	// 下一个桶重新追加。
	s.AddBalanceSample(base.Add(31*time.Minute), "a", "A", 80, 200, 120)
	got = s.BalanceSamples(0)
	if len(got) != 2 || got[1].Remaining != 80 {
		t.Fatalf("跨桶应追加一条: %+v", got)
	}

	// since 过滤的是桶起点（10:30 的桶，不是 10:31 这个具体时刻）。
	if got := s.BalanceSamples(got[1].At); len(got) != 1 || got[0].Remaining != 80 {
		t.Errorf("since 过滤不对: %+v", got)
	}
}

func TestBalanceSamplePrune(t *testing.T) {
	s := openTemp(t)
	now := time.Now()
	old := now.AddDate(0, 0, -(BalanceKeepDays + 2))

	s.AddBalanceSample(old, "a", "A", 1, 2, 1)
	if got := s.BalanceSamples(0); len(got) != 0 {
		t.Fatalf("过期样本应该被裁掉: %+v", got)
	}

	s.AddBalanceSample(now, "a", "A", 2, 2, 0)
	got := s.BalanceSamples(0)
	if len(got) != 1 || got[0].Remaining != 2 {
		t.Fatalf("窗口内样本应保留: %+v", got)
	}
}

func TestSetAccountCreditSeedsBaseline(t *testing.T) {
	s := openTemp(t)
	baselineAt := time.Now().Add(-2 * time.Hour).Unix()
	a := &codearts.Account{ID: "acct-a", Name: "以七", Enabled: true}
	a.CreditTotal, a.CreditUsed, a.CreditRemaining = 1000, 100, 900
	a.CreditUsedAt = baselineAt
	if !s.UpsertAccount(a) {
		t.Fatal("账号写入失败")
	}

	if !s.SetAccountCredit("acct-a", &codearts.CreditInfo{
		Available: true, Total: 1000, Used: 150, Remaining: 850,
	}) {
		t.Fatal("SetAccountCredit 失败")
	}

	got := s.BalanceSamples(0)
	if len(got) != 2 {
		t.Fatalf("应该有「旧快照起点 + 本次采样」两条，实际 %d: %+v", len(got), got)
	}
	if got[0].Remaining != 900 || got[0].At != time.Unix(baselineAt, 0).Truncate(BalanceBucket).Unix() {
		t.Errorf("起点补写不对: %+v", got[0])
	}
	if got[1].Remaining != 850 || got[1].Account != "以七" {
		t.Errorf("新采样不对: %+v", got[1])
	}
}
