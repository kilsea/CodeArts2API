package store

// 统计与用量的延迟落盘。
//
// Record() 只改内存：每条请求都写一次 state.json 是不可接受的（全量重写 +
// 和凭据抢写锁）。但完全不写又会让「重启后统计归零、用量趋势缺当天」。
//
import "time"

// 折中：Record 置脏标记，后台每 N 秒把脏状态刷一次盘。日志本身已经由
// JSONL 逐条落盘，所以即使进程被杀，最坏也只丢最后一次刷盘以来的**计数**。

// dirty 标记内存状态与磁盘不一致。
func (s *Store) MarkDirty() { s.dirty.Store(true) }

// FlushIfDirty 在有变更时落盘一次（后台定时调用）。
//
// 失败时恢复脏标记，让下一轮重试 —— 否则一次临时占用就会让统计永久停在
// 上一次成功保存的时刻。
func (s *Store) FlushIfDirty() error {
	if !s.dirty.Swap(false) {
		return nil
	}
	if err := s.Save(); err != nil {
		s.dirty.Store(true)
		return err
	}
	return nil
}

// Dirty 报告是否有未落盘的变更（测试与优雅退出用）。
func (s *Store) Dirty() bool { return s.dirty.Load() }

// flushDelay 是合并落盘的延迟。
//
// 取 2 秒：连续对话时多个请求的计数合并成一次写；而进程被强杀（kill -9 /
// 任务管理器结束进程）时最多丢最近 2 秒的计数 —— 日志本身走 JSONL，不受影响。
const flushDelay = 2 * time.Second

// scheduleFlush 安排一次延迟落盘（已有待执行的就不再排）。
func (s *Store) scheduleFlush() {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	if s.flushTimer != nil {
		return
	}
	s.flushTimer = time.AfterFunc(flushDelay, func() {
		s.flushMu.Lock()
		s.flushTimer = nil
		s.flushMu.Unlock()
		_ = s.FlushIfDirty()
	})
}
