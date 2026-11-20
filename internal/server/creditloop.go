package server

import (
	"context"
	"log"
	"math/rand/v2"
	"time"
)

// 后台积分快照刷新。
//
// 「到期积分优先」调度需要每个账号的即将到期额度（CreditInfo.Expiring），
// 而额度查询是一次真实的上游请求（snap-manager 统计插件，只读、不消耗积分），
// 不能进请求路径。这里周期性批量刷新并写回账号快照；调度侧只信任 24h 内的
// 快照，所以刷新失败只是让该账号暂时退回普通轮询，不会把调度带坏。
//
// 频率控制（防封号的几道闸）：
//   - 周期默认 10 分钟，可用 -credit-refresh 调整，设 0 完全关闭；
//   - 账号之间随机错开 0.3~1.5 秒，多个账号不会在同一毫秒成串打出去；
//   - 5 分钟内刚刷新过的账号（控制台手动刷新 / 上一轮）直接跳过；
//   - 刷新失败的账号指数退避（10m → 20m → … 封顶 2h），
//     不会拿一个过期凭据每 10 分钟去敲一次门。

// creditRefreshDefault 是积分快照的默认刷新周期。
const creditRefreshDefault = 10 * time.Minute

// creditBackoffMax 是失败退避的上限。
const creditBackoffMax = 2 * time.Hour

// creditFailure 记录一个账号连续刷新失败的情况。
type creditFailure struct {
	fails int
	until time.Time
}

// SetCreditRefresh 设置积分快照刷新周期，0 表示关闭后台刷新。
//
// 关闭后余额页不再有新采样，只有控制台里手动刷新积分时才会落点。
func (s *Server) SetCreditRefresh(every time.Duration) {
	if every < 0 {
		every = 0
	}
	s.creditEvery = every
}

// SetCreditLookup 开关「每笔请求反查上游账单」。
//
// 关掉后积分按本地价格表估算（日志里显示 ≈），少一次上游调用。
func (s *Server) SetCreditLookup(on bool) {
	s.creditLookup = on
}

// StartCreditLoop 启动后台积分快照刷新。
func (s *Server) StartCreditLoop(ctx context.Context) {
	every := s.creditEvery
	if every <= 0 {
		log.Printf("积分快照刷新已关闭（-credit-refresh 0）：余额页只在手动刷新时采样")
		return
	}
	go func() {
		// 启动后先等一会儿，让账号导入与首轮凭据解析完成
		select {
		case <-ctx.Done():
			return
		case <-time.After(15 * time.Second):
		}
		s.RefreshAccountCredits(ctx)

		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.RefreshAccountCredits(ctx)
			}
		}
	}()
}

// RefreshAccountCredits 对所有启用账号各刷新一次积分快照，有变更时落盘。
// 返回成功刷新的账号数。
func (s *Server) RefreshAccountCredits(ctx context.Context) int {
	every := s.creditEvery
	if every <= 0 {
		every = creditRefreshDefault
	}
	// 刚刷新过的账号跳过：省掉和手动刷新 / 上一轮的重复调用。
	skipWithin := every / 2

	ok := 0
	for _, a := range s.Store.Accounts() {
		if ctx.Err() != nil {
			return ok
		}
		if !a.Enabled {
			continue
		}
		if s.creditBackingOff(a.ID) {
			continue
		}
		if a.CreditRefreshedAt > 0 && time.Since(time.Unix(a.CreditRefreshedAt, 0)) < skipWithin {
			continue
		}
		// 走 creditFor 的短缓存：控制台刚查过的账号不再打上游
		info, err := s.creditFor(ctx, a, false)
		if err != nil {
			wait := s.creditFailed(a.ID)
			log.Printf("账号 %s 积分快照刷新失败（%s 后重试）: %v", a.DisplayName(), wait.Round(time.Minute), err)
		} else {
			s.creditSucceeded(a.ID)
			if s.Store.SetAccountCredit(a.ID, info) {
				ok++
			}
		}
		// 账号之间错开：并发/连续打同一个上游容易被风控注意到。
		select {
		case <-ctx.Done():
			return ok
		case <-time.After(time.Duration(300+rand.IntN(1200)) * time.Millisecond):
		}
	}
	if ok > 0 {
		s.persist()
	}
	return ok
}

// creditBackingOff 报告该账号是否还在失败退避窗口里。
func (s *Server) creditBackingOff(id string) bool {
	s.creditMu.Lock()
	defer s.creditMu.Unlock()
	f, ok := s.creditFails[id]
	return ok && time.Now().Before(f.until)
}

// creditFailed 记一次失败并返回下次允许重试的等待时长。
func (s *Server) creditFailed(id string) time.Duration {
	s.creditMu.Lock()
	defer s.creditMu.Unlock()
	if s.creditFails == nil {
		s.creditFails = map[string]*creditFailure{}
	}
	f := s.creditFails[id]
	if f == nil {
		f = &creditFailure{}
		s.creditFails[id] = f
	}
	f.fails++
	wait := creditRefreshDefault
	for i := 1; i < f.fails && wait < creditBackoffMax; i++ {
		wait *= 2
	}
	if wait > creditBackoffMax {
		wait = creditBackoffMax
	}
	f.until = time.Now().Add(wait)
	return wait
}

// creditSucceeded 清掉失败退避状态。
func (s *Server) creditSucceeded(id string) {
	s.creditMu.Lock()
	defer s.creditMu.Unlock()
	delete(s.creditFails, id)
}
