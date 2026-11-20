package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"codearts2api/internal/store"
)

// 余额趋势接口的回归：窗口净变化、今日 / 昨日、消耗 / 到账与每日分桶。
func TestAdminBalanceTrend(t *testing.T) {
	s, _ := setupCompatServer(t)

	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	d2 := today.AddDate(0, 0, -2)
	d1 := today.AddDate(0, 0, -1)

	add := func(when time.Time, v float64) {
		s.Store.AddBalanceSample(when, "a", "acct-a", v, 1000, 1000-v)
	}
	add(d2.Add(12*time.Hour), 1000)
	add(d1.Add(12*time.Hour), 900)
	add(d1.Add(18*time.Hour), 950) // 充值台阶
	add(today.Add(1*time.Minute), 800)
	add(today.Add(31*time.Minute), 780)

	rec := httptest.NewRecorder()
	s.handleAdminBalanceTrend(rec, httptest.NewRequest(http.MethodGet, "/admin/api/balance-trend?days=3", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d：%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Days     int             `json:"days"`
		Bucket   int             `json:"bucket_minutes"`
		Summary  balanceSummary  `json:"summary"`
		Accounts []balanceSeries `json:"accounts"`
		Daily    []balanceDaily  `json:"daily"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if out.Days != 3 || out.Bucket != 30 {
		t.Errorf("days/bucket = %d/%d", out.Days, out.Bucket)
	}
	if len(out.Accounts) != 1 {
		t.Fatalf("账号数 = %d，期望 1", len(out.Accounts))
	}
	a := out.Accounts[0]
	if !a.HasData || a.Account != "acct-a" {
		t.Fatalf("账号数据不对: %+v", a)
	}
	// 窗口内首值 1000、末值 780；min/max 也来自窗口内采样。
	if a.First != 1000 || a.Current != 780 || a.Net != -220 {
		t.Errorf("首值/末值/净变化 = %v/%v/%v", a.First, a.Current, a.Net)
	}
	if a.Min != 780 || a.Max != 1000 {
		t.Errorf("最低/最高 = %v/%v", a.Min, a.Max)
	}
	// 今日 = 今日末 780 - 昨 日末 950。
	if a.Today != -170 {
		t.Errorf("今日 = %v，期望 -170", a.Today)
	}
	// 消耗拆负差：1000→900、950→800、800→780 = 100+150+20；到账只有 900→950。
	if a.SpendWindow != 270 || a.GainWindow != 50 {
		t.Errorf("窗口消耗/到账 = %v/%v，期望 270/50", a.SpendWindow, a.GainWindow)
	}
	// 今日的消耗要算上「昨日末 → 今日首个采样」那一段：150 + 20。
	if a.SpendToday != 170 || a.GainToday != 0 {
		t.Errorf("今日消耗/到账 = %v/%v，期望 170/0", a.SpendToday, a.GainToday)
	}

	if out.Summary.Accounts != 1 || out.Summary.Available != 1 {
		t.Errorf("summary 账号 = %d/%d", out.Summary.Accounts, out.Summary.Available)
	}
	if out.Summary.CurrentTotal != 780 || out.Summary.Samples != 5 {
		t.Errorf("summary 合计/采样 = %v/%d", out.Summary.CurrentTotal, out.Summary.Samples)
	}

	if len(out.Daily) != 3 {
		t.Fatalf("daily 天数 = %d，期望 3", len(out.Daily))
	}
	// 第一天没有前值，delta 保持 0；最后一天：780 - 950 = -170。
	last := out.Daily[2].Cells[0]
	if last.Delta != -170 {
		t.Errorf("今日日变化 = %v，期望 -170", last.Delta)
	}
	// 昨天：950 - 900？不对 —— 日末差是「昨 日末 950 - 前一 日末 1000」= -50。
	if got := out.Daily[1].Cells[0].Delta; got != -50 {
		t.Errorf("昨 日变化 = %v，期望 -50", got)
	}
}

// TestDownsampleBalance 合并时保留每组最后一个值（阶梯量不能取平均）。
func TestDownsampleBalance(t *testing.T) {
	list := make([]store.BalanceSample, 0, 10)
	for i := 0; i < 10; i++ {
		list = append(list, store.BalanceSample{At: int64(i), Remaining: float64(i * 10)})
	}
	got := downsampleBalance(list, 4)
	if len(got) != 4 {
		t.Fatalf("点数 = %d，期望 4", len(got))
	}
	if got[0].V != 20 || got[3].V != 90 {
		t.Errorf("合并取值不对: %+v", got)
	}
	// 不超上限时原样返回。
	if got := downsampleBalance(list[:3], 10); len(got) != 3 {
		t.Errorf("未超上限不该合并: %+v", got)
	}
}
