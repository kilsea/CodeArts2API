package store

import (
	"sort"
	"strings"
	"time"
)

// 日志查询与用量聚合。
//
// 日志是排障的主入口，所以过滤必须在服务端做：控制台一次只拉需要的那些行，
// 而不是把 500 条全量传给浏览器再过滤（列表一长，前端筛选也卡）。

// LogFilter 是日志查询条件（零值表示不过滤）。
type LogFilter struct {
	// Status 支持精确值（"400"）与区间（"4xx" / "5xx" / "2xx"）。
	Status string
	// Class 是 err_class（审核 / 参数 / 登录态 …）。
	Class string
	// Model 是模型 ID（精确匹配）。
	Model string
	// Account 是账号展示名（精确匹配）。
	Account string
	// MinCredits > 0 时只返回估算积分 >= 该值的请求（「只看花钱多的」）。
	MinCredits float64
	// Q 是关键字：request_id / 错误原文 / 账号名 / 密钥。
	Q string
	// Since 是起始时间（unix 秒），0 表示不限。
	Since int64
}

func (f LogFilter) match(e LogEntry) bool {
	if f.Status != "" && !matchStatus(f.Status, e.Status) {
		return false
	}
	if f.Class != "" && e.ErrClass != f.Class {
		return false
	}
	if f.Model != "" && e.Model != f.Model {
		return false
	}
	if f.Account != "" && e.Account != f.Account {
		return false
	}
	if f.MinCredits > 0 && e.Credits < f.MinCredits {
		return false
	}
	if f.Since > 0 && e.Time < f.Since {
		return false
	}
	if f.Q != "" {
		q := strings.ToLower(f.Q)
		if !strings.Contains(strings.ToLower(e.RequestID), q) &&
			!strings.Contains(strings.ToLower(e.Err), q) &&
			!strings.Contains(strings.ToLower(e.Account), q) &&
			!strings.Contains(strings.ToLower(e.Model), q) {
			return false
		}
	}
	return true
}

// matchStatus 处理 "4xx" 这类区间写法。
func matchStatus(want string, got int) bool {
	if len(want) == 3 && (want[1] == 'x' || want[1] == 'X') && (want[2] == 'x' || want[2] == 'X') {
		hi := int(want[0]-'0') * 100
		return got >= hi && got < hi+100
	}
	return strings.TrimSpace(want) == itoa(got)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// QueryLogs 按条件过滤日志，返回**时间正序**（调用方决定展示顺序）。
func (s *Store) QueryLogs(f LogFilter, limit int) []LogEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]LogEntry, 0, len(s.st.Logs))
	for _, e := range s.st.Logs {
		if f.match(e) {
			out = append(out, e)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// UsageKeepDays 是用量聚合的保留天数。
//
// 用量只做趋势，不做审计；30 天足够看出「这周比上周多花了多少」。
const UsageKeepDays = 30

// UsageHourlyKeepHours 是逐小时桶的保留窗口（小时）。
//
// 72 小时足够画「最近 48 小时」时序图，也让状态文件不随运行时长膨胀；
// 更长的趋势看按天桶（UsageKeepDays 天）。
const UsageHourlyKeepHours = 72

// UsageRow 是按「天 + 账号 + 模型」聚合的用量。
//
// 为什么带账号：积分是账号的额度，控制台既要看「今天一共花了多少」，
// 也要看「哪个账号花了多少、被哪个模型花的」。只按天 + 模型聚合的话，
// 多账号场景下账号维度就永久丢了（事后再拆不可能），所以聚合键一开始就带上。
//
// 旧版本（v2 状态文件）的 Usage 行没有 AccountID，加载后 Account 为空：
// 它们仍然计入总分，控制台显示为「（历史）」。
type UsageRow struct {
	Day       string `json:"day"`
	AccountID string `json:"account_id,omitempty"`
	Account   string `json:"account,omitempty"`
	Model     string `json:"model"`
	Requests  int64  `json:"requests"`
	CharsIn   int64  `json:"chars_in"`
	CharsOut  int64  `json:"chars_out"`
	// InTokens / OutTokens / CachedTokens 是上游 usage 的累计（真实 token）。
	InTokens     int64 `json:"in_tokens"`
	OutTokens    int64 `json:"out_tokens"`
	CachedTokens int64 `json:"cached_tokens"`
	// Credits 是积分消耗：优先上游账单口径（credit_source=upstream），
	// 查不到时是本地估算（estimate）。口径由每条日志的 credit_source 记录。
	Credits float64 `json:"credits"`
	// LatencyMS 是请求耗时之和；平均延迟 = LatencyMS / TimedRequests。
	// 失败也计入（慢失败同样是体验问题）。
	LatencyMS int64 `json:"latency_ms"`
	// TimedRequests 是有耗时记录的请求数。
	//
	// 升级前的日志没有 latency_ms，把那些行算进平均延迟会得到「175ms」这种
	// 假数字；分母只算真正有记录的那些，没有记录时显示「—」。
	TimedRequests int64 `json:"timed_requests"`
	// TimedOutTokens / OutLatencyMS 是「既有输出 token 又有耗时记录」那批请求的
	// 分子 / 分母，专门算吐字速度。两者必须同一批请求，否则升级前那些只有
	// token、没有耗时的历史行会把速度算成虚高（420 token / 2.6s = 160 tok/s）。
	TimedOutTokens int64 `json:"timed_out_tokens"`
	OutLatencyMS   int64 `json:"out_latency_ms"`
	// Failures 是其中失败（status >= 400 或带错误）的条数。
	Failures int64 `json:"failures"`
	// Hour 只出现在逐小时桶里（"2026-11-20T15"）；日桶为空。
	Hour string `json:"hour,omitempty"`
}

// bumpUsageLocked 把一次请求累加进「天 × 账号 × 模型」和「小时 × 账号 × 模型」两个桶。
//
// 小时桶只用于时序图（保留 UsageHourlyKeepHours 小时），日桶是长周期口径；
// 两者都按 requests/token/积分/延迟累加，单条日志不落进聚合表。
func (s *Store) bumpUsageLocked(e LogEntry, outChars int) {
	ts := time.Unix(e.Time, 0)
	day := ts.Format("2006-01-02")
	hour := ts.Format("2006-01-02T15")

	s.st.Usage = bumpUsageBucket(s.st.Usage,
		func(r *UsageRow) bool { return r.Day == day && r.AccountID == e.AccountID && r.Model == e.Model },
		UsageRow{Day: day, AccountID: e.AccountID, Account: e.Account, Model: e.Model},
		e, outChars)

	s.st.UsageHour = bumpUsageBucket(s.st.UsageHour,
		func(r *UsageRow) bool { return r.Hour == hour && r.AccountID == e.AccountID && r.Model == e.Model },
		UsageRow{Hour: hour, AccountID: e.AccountID, Account: e.Account, Model: e.Model},
		e, outChars)

	// 日桶每天裁一次；小时桶按小时裁一次（每条请求都扫一遍是浪费）。
	if s.lastUsageDay != day {
		s.lastUsageDay = day
		s.pruneUsageLocked(day)
	}
	if s.lastUsageHour != hour {
		s.lastUsageHour = hour
		s.pruneUsageHourLocked(ts)
	}
}

// bumpUsageBucket 在 rows 里找到命中的桶累加，找不到就追加一行。
func bumpUsageBucket(rows []UsageRow, match func(*UsageRow) bool, proto UsageRow, e LogEntry, outChars int) []UsageRow {
	failed := int64(0)
	if e.Status >= 400 || e.Err != "" {
		failed = 1
	}
	for i := range rows {
		r := &rows[i]
		if match(r) {
			r.Requests++
			r.CharsIn += int64(e.PromptLen)
			r.CharsOut += int64(outChars)
			r.InTokens += int64(e.InTokens)
			r.OutTokens += int64(e.OutTokens)
			r.CachedTokens += int64(e.CachedTokens)
			r.Credits += e.Credits
			r.LatencyMS += e.LatencyMS
			r.Failures += failed
			if e.LatencyMS > 0 {
				r.TimedRequests++
			}
			if e.OutTokens > 0 && e.LatencyMS > 0 {
				r.TimedOutTokens += int64(e.OutTokens)
				r.OutLatencyMS += e.LatencyMS
			}
			if r.Account == "" {
				r.Account = e.Account
			}
			return rows
		}
	}
	proto.Requests = 1
	proto.CharsIn = int64(e.PromptLen)
	proto.CharsOut = int64(outChars)
	proto.InTokens = int64(e.InTokens)
	proto.OutTokens = int64(e.OutTokens)
	proto.CachedTokens = int64(e.CachedTokens)
	proto.Credits = e.Credits
	proto.LatencyMS = e.LatencyMS
	proto.Failures = failed
	if e.LatencyMS > 0 {
		proto.TimedRequests = 1
	}
	if e.OutTokens > 0 && e.LatencyMS > 0 {
		proto.TimedOutTokens = int64(e.OutTokens)
		proto.OutLatencyMS = e.LatencyMS
	}
	return append(rows, proto)
}

// pruneUsageLocked 丢掉超过 UsageKeepDays 的行。
//
// 只在「新出现一天」时做：每天最多一次，不必每条请求都扫。
func (s *Store) pruneUsageLocked(today string) {
	t, err := time.Parse("2006-01-02", today)
	if err != nil {
		return
	}
	cutoff := t.AddDate(0, 0, -UsageKeepDays).Format("2006-01-02")
	kept := s.st.Usage[:0]
	for _, r := range s.st.Usage {
		if r.Day >= cutoff {
			kept = append(kept, r)
		}
	}
	s.st.Usage = kept
	keptSpend := s.st.CreditSpend[:0]
	for _, r := range s.st.CreditSpend {
		if r.Day >= cutoff {
			keptSpend = append(keptSpend, r)
		}
	}
	s.st.CreditSpend = keptSpend
}

// pruneUsageHourLocked 丢掉超过 UsageHourlyKeepHours 的小时桶。
//
// 小时桶的键是 "2006-01-02T15"，字典序即时间序，直接比较字符串即可。
func (s *Store) pruneUsageHourLocked(now time.Time) {
	cutoff := now.Add(-UsageHourlyKeepHours * time.Hour).Format("2006-01-02T15")
	kept := s.st.UsageHour[:0]
	for _, r := range s.st.UsageHour {
		if r.Hour >= cutoff {
			kept = append(kept, r)
		}
	}
	s.st.UsageHour = kept
}

// UsageHourly 返回最近 hours 小时的逐小时桶（含账号 / 模型维度）。
func (s *Store) UsageHourly(hours int) []UsageRow {
	if hours <= 0 || hours > UsageHourlyKeepHours {
		hours = 48
	}
	cutoff := time.Now().Add(-time.Duration(hours-1) * time.Hour).Format("2006-01-02T15")
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]UsageRow, 0, len(s.st.UsageHour))
	for _, r := range s.st.UsageHour {
		if r.Hour >= cutoff {
			out = append(out, r)
		}
	}
	return out
}

// UsageByHour 按小时归并（小时升序，用于时序柱状图）。
func UsageByHour(rows []UsageRow) []UsageRow {
	out := groupUsage(rows, func(r UsageRow) string { return r.Hour }, func(r UsageRow) UsageRow {
		return UsageRow{Hour: r.Hour}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Hour < out[j].Hour })
	return out
}

// Usage 返回最近 days 天的用量（按天 + 模型）。
func (s *Store) Usage(days int) []UsageRow {
	if days <= 0 || days > UsageKeepDays {
		days = 7
	}
	cutoff := time.Now().AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]UsageRow, 0, len(s.st.Usage))
	for _, r := range s.st.Usage {
		if r.Day >= cutoff {
			out = append(out, r)
		}
	}
	return out
}

// CreditSpendRow 是「天 + 账号」的真实积分消耗（上游快照差值）。
//
// 与 UsageRow 的分工：
//   - UsageRow.Credits 按模型估算（每次请求立刻能记，能拆模型，但只是估算）；
//   - CreditSpendRow 是按账号的真实账单增量（只到账号 / 天，没有模型维度，
//     而且受快照刷新周期影响，最长滞后一个刷新周期）。
//
// 两者应当接近；差得离谱就说明价格档位或公式变了（看 docs/CREDITS.md）。
type CreditSpendRow struct {
	Day       string  `json:"day"`
	AccountID string  `json:"account_id"`
	Account   string  `json:"account"`
	Credits   float64 `json:"credits"`
}

// bumpCreditSpendLocked 把一次快照增量累加进当天该账号的桶。
func (s *Store) bumpCreditSpendLocked(now time.Time, id, name string, delta float64) {
	day := now.Format("2006-01-02")
	for i := range s.st.CreditSpend {
		r := &s.st.CreditSpend[i]
		if r.Day == day && r.AccountID == id {
			r.Credits += delta
			if name != "" {
				r.Account = name
			}
			return
		}
	}
	s.st.CreditSpend = append(s.st.CreditSpend, CreditSpendRow{
		Day: day, AccountID: id, Account: name, Credits: delta,
	})
	s.pruneUsageLocked(day)
}

// CreditSpend 返回最近 days 天的真实积分消耗（按天 + 账号）。
func (s *Store) CreditSpend(days int) []CreditSpendRow {
	if days <= 0 || days > UsageKeepDays {
		days = 7
	}
	cutoff := time.Now().AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]CreditSpendRow, 0, len(s.st.CreditSpend))
	for _, r := range s.st.CreditSpend {
		if r.Day >= cutoff {
			out = append(out, r)
		}
	}
	return out
}

// UsageTotals 是一组用量行的合计。
type UsageTotals struct {
	Requests       int64   `json:"requests"`
	CharsIn        int64   `json:"chars_in"`
	CharsOut       int64   `json:"chars_out"`
	InTokens       int64   `json:"in_tokens"`
	OutTokens      int64   `json:"out_tokens"`
	CachedTokens   int64   `json:"cached_tokens"`
	Credits        float64 `json:"credits"`
	LatencyMS      int64   `json:"latency_ms"`
	TimedRequests  int64   `json:"timed_requests"`
	TimedOutTokens int64   `json:"timed_out_tokens"`
	OutLatencyMS   int64   `json:"out_latency_ms"`
	Failures       int64   `json:"failures"`
}

// add 把一行累加进来。
func (t *UsageTotals) add(r UsageRow) {
	t.Requests += r.Requests
	t.CharsIn += r.CharsIn
	t.CharsOut += r.CharsOut
	t.InTokens += r.InTokens
	t.OutTokens += r.OutTokens
	t.CachedTokens += r.CachedTokens
	t.Credits += r.Credits
	t.LatencyMS += r.LatencyMS
	t.TimedRequests += r.TimedRequests
	t.TimedOutTokens += r.TimedOutTokens
	t.OutLatencyMS += r.OutLatencyMS
	t.Failures += r.Failures
}

// SumUsage 计算总合计（跨天 / 跨账号 / 跨模型）。
func SumUsage(rows []UsageRow) UsageTotals {
	var t UsageTotals
	for _, r := range rows {
		t.add(r)
	}
	return t
}

// groupUsage 按 key 归并行，key 返回空串表示跳过该维度。
func groupUsage(rows []UsageRow, key func(UsageRow) string, build func(UsageRow) UsageRow) []UsageRow {
	idx := map[string]int{}
	out := make([]UsageRow, 0, len(rows))
	for _, r := range rows {
		k := key(r)
		if k == "" {
			continue
		}
		if i, ok := idx[k]; ok {
			o := &out[i]
			o.Requests += r.Requests
			o.CharsIn += r.CharsIn
			o.CharsOut += r.CharsOut
			o.InTokens += r.InTokens
			o.OutTokens += r.OutTokens
			o.CachedTokens += r.CachedTokens
			o.Credits += r.Credits
			o.LatencyMS += r.LatencyMS
			o.TimedRequests += r.TimedRequests
			o.TimedOutTokens += r.TimedOutTokens
			o.OutLatencyMS += r.OutLatencyMS
			o.Failures += r.Failures
			continue
		}
		idx[k] = len(out)
		o := build(r)
		o.Requests = r.Requests
		o.CharsIn = r.CharsIn
		o.CharsOut = r.CharsOut
		o.InTokens = r.InTokens
		o.OutTokens = r.OutTokens
		o.CachedTokens = r.CachedTokens
		o.Credits = r.Credits
		o.LatencyMS = r.LatencyMS
		o.TimedRequests = r.TimedRequests
		o.TimedOutTokens = r.TimedOutTokens
		o.OutLatencyMS = r.OutLatencyMS
		o.Failures = r.Failures
		out = append(out, o)
	}
	return out
}

// UsageByDay 按天归并（天升序，用于趋势图）。
func UsageByDay(rows []UsageRow) []UsageRow {
	out := groupUsage(rows, func(r UsageRow) string { return r.Day }, func(r UsageRow) UsageRow {
		return UsageRow{Day: r.Day}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Day < out[j].Day })
	return out
}

// UsageByModel 按模型归并（积分降序）。
func UsageByModel(rows []UsageRow) []UsageRow {
	out := groupUsage(rows, func(r UsageRow) string { return r.Model }, func(r UsageRow) UsageRow {
		return UsageRow{Model: r.Model}
	})
	sort.Slice(out, func(i, j int) bool { return rankUsage(out[i]) > rankUsage(out[j]) })
	return out
}

// UsageByAccount 按账号归并（积分降序）。
func UsageByAccount(rows []UsageRow) []UsageRow {
	out := groupUsage(rows, func(r UsageRow) string {
		if r.AccountID == "" && r.Account == "" {
			return "history" // 旧版本没有账号维度的历史行，单独归成一档
		}
		return r.AccountID + "\x00" + r.Account
	}, func(r UsageRow) UsageRow {
		return UsageRow{AccountID: r.AccountID, Account: r.Account}
	})
	sort.Slice(out, func(i, j int) bool { return rankUsage(out[i]) > rankUsage(out[j]) })
	return out
}

// rankUsage 是「谁更重」的排序权重：积分优先，其次请求数。
func rankUsage(r UsageRow) float64 { return r.Credits*1000 + float64(r.Requests) }
