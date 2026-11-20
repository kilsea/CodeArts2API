package store

import (
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

// UsageRow 是按「天 + 模型」聚合的用量。
type UsageRow struct {
	Day      string `json:"day"`
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
	CharsIn  int64  `json:"chars_in"`
	CharsOut int64  `json:"chars_out"`
}

// bumpUsageLocked 把一次请求累加进当天该模型的桶。
func (s *Store) bumpUsageLocked(e LogEntry, outChars int) {
	day := time.Unix(e.Time, 0).Format("2006-01-02")
	for i := range s.st.Usage {
		r := &s.st.Usage[i]
		if r.Day == day && r.Model == e.Model {
			r.Requests++
			r.CharsIn += int64(e.PromptLen)
			r.CharsOut += int64(outChars)
			return
		}
	}
	s.st.Usage = append(s.st.Usage, UsageRow{
		Day: day, Model: e.Model, Requests: 1,
		CharsIn: int64(e.PromptLen), CharsOut: int64(outChars),
	})
	s.pruneUsageLocked(day)
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
