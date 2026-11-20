package server

import (
	"testing"
	"time"
)

// 积分快照刷新的失败退避：连续失败要拉长重试间隔，成功后立刻恢复。
func TestCreditBackoff(t *testing.T) {
	s := &Server{}

	if s.creditBackingOff("a") {
		t.Fatal("没失败过不该退避")
	}
	w1 := s.creditFailed("a")
	if w1 != creditRefreshDefault {
		t.Fatalf("第一次失败应退避一个周期，得到 %s", w1)
	}
	if !s.creditBackingOff("a") {
		t.Fatal("失败后应进入退避")
	}
	w2 := s.creditFailed("a")
	if w2 <= w1 {
		t.Fatalf("连续失败应拉长退避：%s -> %s", w1, w2)
	}

	// 一直失败也封顶，不能退避成「永远不刷新」。
	for i := 0; i < 20; i++ {
		s.creditFailed("a")
	}
	s.creditMu.Lock()
	f := s.creditFails["a"]
	s.creditMu.Unlock()
	if d := time.Until(f.until); d > creditBackoffMax+time.Minute {
		t.Fatalf("退避时长超过上限: %s", d)
	}

	// 成功一次就清空，下一次仍是正常周期。
	s.creditSucceeded("a")
	if s.creditBackingOff("a") {
		t.Fatal("成功后应退出退避")
	}
	if w := s.creditFailed("a"); w != creditRefreshDefault {
		t.Fatalf("成功后应从头计数，得到 %s", w)
	}

	// 账号之间互不影响。
	if s.creditBackingOff("b") {
		t.Fatal("退避不该串到别的账号")
	}
}
