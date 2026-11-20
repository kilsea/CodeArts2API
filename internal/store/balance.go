package store

import (
	"sort"
	"time"

	"codearts2api/internal/codearts"
)

// 余额（积分水位）时序采样。
//
// 为什么单独存一份，而不复用 CreditSpend：
//   - CreditSpend 只累加「已用增量」，是账本（花掉多少），画不出水位曲线，
//     也看不出充值 / 额度重置；
//   - 这里存的是每次积分快照刷新时的**剩余额度**，按 30 分钟桶落盘。
//     30 分钟是「一天 48 个点」的折中：够画出充值台阶和掉额台阶，
//     40 天 × 48 × 账号数的状态文件增量也还在几十 KB 量级。
//
// 采样只在积分快照刷新循环里发生（10 分钟一次），所以不需要额外的定时器；
// 页面打开时看到的点数取决于网关部署了多久。

// BalanceKeepDays 是余额样本的保留天数。
//
// 控制台最长看 30 天窗口，多留 2 天余量（首日 / 昨日的日末差要往前取）。
// 40 天会多存 1/3 的样本，而状态文件是**全量重写**的，没必要。
const BalanceKeepDays = 32

// BalanceBucket 是采样桶宽度（同桶只保留最后一次值）。
const BalanceBucket = 30 * time.Minute

// BalanceSample 是一次积分水位采样。
type BalanceSample struct {
	// At 是桶起点（unix 秒），不是精确时刻 —— 同桶覆盖写，避免抖动。
	At        int64   `json:"at"`
	AccountID string  `json:"account_id"`
	Account   string  `json:"account"`
	Remaining float64 `json:"remaining"`
	Total     float64 `json:"total"`
	Used      float64 `json:"used"`
}

// recordBalanceLocked 记录一次积分水位（调用方已持写锁）。
//
// 同桶覆盖：刷新周期（10 分钟）比桶宽（30 分钟）短，桶内只保留最后一次值。
// 这样既不会把状态文件写胖，又能让「充值后立刻刷新」的值马上可见。
func (s *Store) recordBalanceLocked(now time.Time, a *codearts.Account) {
	if a == nil || a.CreditUsedAt == 0 {
		return
	}
	bucket := now.Truncate(BalanceBucket).Unix()

	// 样本按时间正序追加，账号交错排列；找该账号的最后一条即可。
	// 注意：补写的历史起点会破坏全局顺序，所以不能「找到第一处就 break」，
	// 必须扫完（几千条，一次刷新一遍，开销可忽略）。
	last := -1
	for i := len(s.st.Balances) - 1; i >= 0; i-- {
		if s.st.Balances[i].AccountID != a.ID {
			continue
		}
		if last < 0 || s.st.Balances[i].At > s.st.Balances[last].At {
			last = i
		}
	}
	if last >= 0 && s.st.Balances[last].At == bucket {
		b := &s.st.Balances[last]
		b.Remaining = a.CreditRemaining
		b.Total = a.CreditTotal
		b.Used = a.CreditUsed
		if a.DisplayName() != "" {
			b.Account = a.DisplayName()
		}
		return
	}

	s.st.Balances = append(s.st.Balances, BalanceSample{
		At:        bucket,
		AccountID: a.ID,
		Account:   a.DisplayName(),
		Remaining: a.CreditRemaining,
		Total:     a.CreditTotal,
		Used:      a.CreditUsed,
	})
	// 保留窗口按墙钟算（而不是入参 now）：回填历史点时不该用「历史时刻」
	// 当基准，否则刚补的旧数据不会被裁掉。
	s.pruneBalancesLocked(time.Now())
	s.dirty.Store(true)
}

// pruneBalancesLocked 丢掉保留窗口之外的样本。
func (s *Store) pruneBalancesLocked(now time.Time) {
	cutoff := now.AddDate(0, 0, -BalanceKeepDays).Unix()
	kept := s.st.Balances[:0]
	for _, b := range s.st.Balances {
		if b.At >= cutoff {
			kept = append(kept, b)
		}
	}
	s.st.Balances = kept
}

// AddBalanceSample 在指定时间追加一条水位样本。
//
// 正常路径由 SetAccountCredit 调 recordBalanceLocked 写入；导出的版本留给
// 测试与控制台的历史回填（曲线刚上线时可以把已有快照补进去）。
func (s *Store) AddBalanceSample(now time.Time, id, name string, remaining, total, used float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordBalanceLocked(now, &codearts.Account{
		ID: id, Name: name,
		CreditRemaining: remaining, CreditTotal: total, CreditUsed: used,
		CreditUsedAt: now.Unix(),
	})
}

// hasBalanceLocked 报告该账号是否已有水位样本。
func (s *Store) hasBalanceLocked(id string) bool {
	for i := len(s.st.Balances) - 1; i >= 0; i-- {
		if s.st.Balances[i].AccountID == id {
			return true
		}
	}
	return false
}

// BalanceSamples 返回 since（unix 秒）之后的所有水位样本，按时间正序。
//
// 返回副本：调用方在锁外做聚合，不能让它拿到内部切片。
func (s *Store) BalanceSamples(since int64) []BalanceSample {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]BalanceSample, 0, len(s.st.Balances))
	for _, b := range s.st.Balances {
		if b.At >= since {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].At == out[j].At {
			return out[i].AccountID < out[j].AccountID
		}
		return out[i].At < out[j].At
	})
	return out
}
