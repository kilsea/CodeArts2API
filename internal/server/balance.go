package server

import (
	"net/http"
	"time"

	"codearts2api/internal/store"
)

// 余额趋势（积分水位）聚合。
//
// 数据全部来自 store 的 30 分钟水位采样（见 internal/store/balance.go）。
// 聚合放在服务端而不是前端，理由与用量页相同：
//   - 「今日 / 昨日 / 净变化 / 消耗 / 到账」的口径只能有一处，
//     前端各写一遍必然出现「图上掉了 100，表里写 98」；
//   - 采样点最多 30 天 × 48 × 账号数，前端全量拉下来再算也浪费。

// balanceTrendMaxPoints 是单账号返回的采样点上限。
//
// 30 天 × 48 点 = 1440 点，6 个账号就是 8600 点；SVG 画得下，
// 但 JSON 和重绘都明显变重。降到 1 小时粒度（或按需分桶）后
// 仍然能看出充值台阶和掉额斜率，所以按上限合并。
const balanceTrendMaxPoints = 400

// balancePoint 是趋势图上的一个点。
type balancePoint struct {
	At int64   `json:"at"`
	V  float64 `json:"v"`
}

// balanceSeries 是一个账号在窗口内的水位走势与派生指标。
type balanceSeries struct {
	AccountID string         `json:"account_id"`
	Account   string         `json:"account"`
	Enabled   bool           `json:"enabled"`
	Points    []balancePoint `json:"points"`

	// Current 是最新采样值；First 是窗口内第一个采样值。
	Current float64 `json:"current"`
	First   float64 `json:"first"`
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
	// Net = Current - First（窗口净变化）。
	Net float64 `json:"net"`

	// Today / Yesterday 是「日末水位差」：今日 = 今日末 - 昨 日末。
	Today     float64 `json:"today"`
	Yesterday float64 `json:"yesterday"`

	// Spend / Gain 是把相邻采样差拆成正负两侧：负差累计为消耗，正差为到账
	// （充值、额度重置、赠送都会表现为正差）。
	SpendToday  float64 `json:"spend_today"`
	GainToday   float64 `json:"gain_today"`
	SpendWindow float64 `json:"spend_window"`
	GainWindow  float64 `json:"gain_window"`

	Samples int  `json:"samples"`
	HasData bool `json:"has_data"`
}

// balanceDailyCell 是「某天某账号」的日变化。
type balanceDailyCell struct {
	AccountID string  `json:"account_id"`
	Account   string  `json:"account"`
	Delta     float64 `json:"delta"`
	// Complete=false 表示当天采样不完整（网关停机 / 账号当天才加入），
	// 前端用浅色或斜纹柱表示，避免把「没采到」当成「没花钱」。
	Complete bool `json:"complete"`
	Samples  int  `json:"samples"`
}

// balanceDaily 是一天的分组（柱状图的一列）。
type balanceDaily struct {
	Day   string             `json:"day"`
	Cells []balanceDailyCell `json:"cells"`
}

// balanceSummary 是卡片顶部那一行总账。
type balanceSummary struct {
	Accounts     int     `json:"accounts"`
	Available    int     `json:"available"`
	CurrentTotal float64 `json:"current_total"`
	NetChange    float64 `json:"net_change"`
	Samples      int     `json:"samples"`
}

// handleAdminBalanceTrend 返回余额趋势页所需的全部数据。
//
//	GET /admin/api/balance-trend?days=30
func (s *Server) handleAdminBalanceTrend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 GET", "invalid_request_error")
		return
	}
	days := intQuery(r, "days")
	if days <= 0 {
		days = 30
	}
	if days > store.BalanceKeepDays {
		days = store.BalanceKeepDays
	}

	now := time.Now()
	loc := now.Location()
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -(days - 1))
	// 多取两天：昨日 / 今日的日末差与「首日净变化」都要用到窗口前一天的末值。
	since := from.AddDate(0, 0, -2).Unix()
	samples := s.Store.BalanceSamples(since)

	// 账号顺序以账号池为准（颜色按这个下标分配，图例 / 柱子 / 表格才对得上）；
	// 采样里但已从池里删掉的账号追加在末尾，标记为不可用。
	accts := s.Store.Accounts()
	order := make([]string, 0, len(accts))
	byID := make(map[string]store.BalanceSample, len(accts))
	for _, a := range accts {
		order = append(order, a.ID)
	}
	seen := make(map[string]bool, len(accts))
	for _, id := range order {
		seen[id] = true
	}
	for _, sp := range samples {
		if !seen[sp.AccountID] {
			seen[sp.AccountID] = true
			order = append(order, sp.AccountID)
		}
		if _, ok := byID[sp.AccountID]; !ok || sp.At >= byID[sp.AccountID].At {
			byID[sp.AccountID] = sp
		}
	}

	// 先把每个账号的采样切出来（已按时间正序）。
	perAccount := make(map[string][]store.BalanceSample, len(order))
	for _, sp := range samples {
		perAccount[sp.AccountID] = append(perAccount[sp.AccountID], sp)
	}

	fromUnix := from.Unix()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	yesterday := today.AddDate(0, 0, -1)

	series := make([]balanceSeries, 0, len(order))
	var summary balanceSummary
	var currentTotal, netChange float64
	sampleCount := 0

	// 账号快照：还没有任何采样时（刚部署 / 刚导入），至少拿它当「当前值」，
	// 否则表格里全是 0，看起来像坏了。趋势图仍然空着（由 Samples 决定）。
	snapshot := make(map[string]float64, len(accts))
	for _, a := range accts {
		if a.CreditUsedAt > 0 {
			snapshot[a.ID] = a.CreditRemaining
		}
	}

	dayLast := func(id string, end time.Time) (float64, bool) {
		var v float64
		var ok bool
		for _, sp := range perAccount[id] {
			if sp.At > end.Unix() {
				break
			}
			v, ok = sp.Remaining, true
		}
		return v, ok
	}

	for _, id := range order {
		list := perAccount[id]
		sr := balanceSeries{AccountID: id, Points: []balancePoint{}}

		// 显示名取最新一条采样（账号可能被重命名）。
		if last, ok := byID[id]; ok {
			sr.Account = last.Account
		}
		for _, a := range accts {
			if a.ID == id {
				sr.Enabled = a.Enabled
				if name := a.DisplayName(); name != "" {
					sr.Account = name
				}
			}
		}
		if sr.Account == "" {
			sr.Account = id
		}

		// 窗口内的点用于画图与统计。
		var win []store.BalanceSample
		for _, sp := range list {
			if sp.At >= fromUnix {
				win = append(win, sp)
			}
		}
		if len(win) > 0 {
			sr.HasData = true
			sr.First = win[0].Remaining
			sr.Current = win[len(win)-1].Remaining
			sr.Net = sr.Current - sr.First
			sr.Min, sr.Max = win[0].Remaining, win[0].Remaining
			sr.Samples = len(win)
			for _, sp := range win {
				if sp.Remaining < sr.Min {
					sr.Min = sp.Remaining
				}
				if sp.Remaining > sr.Max {
					sr.Max = sp.Remaining
				}
			}
			summary.Available++
		} else if last, ok := byID[id]; ok {
			// 窗口内没采到，但历史上有值：当前值仍然真实，只是没有走势。
			sr.Current = last.Remaining
			sr.Min, sr.Max = last.Remaining, last.Remaining
		} else if cur, ok := snapshot[id]; ok {
			sr.Current = cur
			sr.Min, sr.Max = cur, cur
			sr.HasData = true
			summary.Available++
		}

		// 相邻采样差分 → 消耗 / 到账（只统计窗口内）。
		for i := 1; i < len(win); i++ {
			d := win[i].Remaining - win[i-1].Remaining
			if d > 0 {
				sr.GainWindow += d
			} else if d < 0 {
				sr.SpendWindow += -d
			}
		}
		// 今日的消耗 / 到账：跨日的那一段（昨日末 → 今日首个采样）也算进去，
		// 否则零点到第一个采样之间的花费会永久丢失。
		for i, sp := range list {
			if sp.At < today.Unix() {
				continue
			}
			prev := 0.0
			hasPrev := false
			if i > 0 {
				prev, hasPrev = list[i-1].Remaining, true
			}
			if !hasPrev {
				continue
			}
			d := sp.Remaining - prev
			if d > 0 {
				sr.GainToday += d
			} else if d < 0 {
				sr.SpendToday += -d
			}
		}

		// 日末水位差（今日 / 昨日）。
		if v, ok := dayLast(id, today.AddDate(0, 0, 1).Add(-time.Second)); ok {
			if pv, pok := dayLast(id, yesterday.AddDate(0, 0, 1).Add(-time.Second)); pok {
				sr.Today = v - pv
			}
		}
		if v, ok := dayLast(id, yesterday.AddDate(0, 0, 1).Add(-time.Second)); ok {
			if pv, pok := dayLast(id, yesterday); pok {
				sr.Yesterday = v - pv
			}
		}

		sr.Points = downsampleBalance(win, balanceTrendMaxPoints)
		series = append(series, sr)
		currentTotal += sr.Current
		netChange += sr.Net
		sampleCount += sr.Samples
	}

	summary.Accounts = len(accts)
	summary.CurrentTotal = currentTotal
	summary.NetChange = netChange
	summary.Samples = sampleCount

	// 每日变化：days 列，每列按账号顺序给出日末差。
	daily := make([]balanceDaily, 0, days)
	for d := 0; d < days; d++ {
		day := from.AddDate(0, 0, d)
		end := day.AddDate(0, 0, 1).Add(-time.Second)
		prevEnd := day.Add(-time.Second)
		row := balanceDaily{Day: day.Format("2006-01-02"), Cells: make([]balanceDailyCell, 0, len(order))}

		// 覆盖率用「当天首末采样跨了多久」判断，而不是数采样点个数：
		// 采样间隔可能因为上游慢 / 刷新失败而变稀，点少不等于覆盖不全；
		// 反过来，停机一整天只采到一个点，点多也说明不了覆盖。
		daySpan := int64(24 * time.Hour / time.Second)
		if day.Equal(today) {
			daySpan = now.Unix() - day.Unix()
			if daySpan < 3600 {
				daySpan = 3600
			}
		}
		for _, id := range order {
			cell := balanceDailyCell{AccountID: id}
			name := id
			for _, sr := range series {
				if sr.AccountID == id {
					name = sr.Account
					break
				}
			}
			cell.Account = name
			count := 0
			var firstAt, lastAt int64
			firstVal, lastVal := 0.0, 0.0
			for _, sp := range perAccount[id] {
				if sp.At >= day.Unix() && sp.At <= end.Unix() {
					if firstAt == 0 {
						firstAt = sp.At
						firstVal = sp.Remaining
					}
					lastAt = sp.At
					lastVal = sp.Remaining
					count++
				}
			}
			cell.Samples = count
			cell.Complete = count >= 2 && (lastAt-firstAt)*4 >= daySpan*3
			if v, ok := dayLast(id, end); ok {
				if pv, pok := dayLast(id, prevEnd); pok {
					cell.Delta = v - pv
				} else if count >= 2 {
					// 前一天一条采样都没有（新账号 / 停机一天）：退化成「当天首末差」。
					// 否则昨天之前每天都画不出柱子，新账号上线头几天页面是空的。
					cell.Delta = lastVal - firstVal
					cell.Complete = false
				}
			}
			row.Cells = append(row.Cells, cell)
		}
		daily = append(daily, row)
	}

	var updatedAt int64
	for _, sp := range samples {
		if sp.At > updatedAt {
			updatedAt = sp.At
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"days":           days,
		"from":           fromUnix,
		"to":             now.Unix(),
		"updated_at":     updatedAt,
		"bucket_minutes": int(store.BalanceBucket.Minutes()),
		"summary":        summary,
		"accounts":       series,
		"daily":          daily,
	})
}

// downsampleBalance 把采样合并到最多 max 个点。
//
// 余额是阶梯量，合并时保留**每组最后一个**值（不是平均）：
// 平均会把充值台阶抹成斜坡，看着像额度在缓慢增长。
func downsampleBalance(list []store.BalanceSample, max int) []balancePoint {
	out := make([]balancePoint, 0, len(list))
	if max <= 0 || len(list) <= max {
		for _, sp := range list {
			out = append(out, balancePoint{At: sp.At, V: sp.Remaining})
		}
		return out
	}
	size := (len(list) + max - 1) / max
	for i := 0; i < len(list); i += size {
		j := i + size
		if j > len(list) {
			j = len(list)
		}
		out = append(out, balancePoint{At: list[j-1].At, V: list[j-1].Remaining})
	}
	return out
}
