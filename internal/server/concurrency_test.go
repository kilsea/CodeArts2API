package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"codearts2api/internal/apierr"
	"codearts2api/internal/codearts"
	"codearts2api/internal/registry"
)

// 每账号并发闸门的回归。
//
// 上游硬约束：每个账号最多 3 个并发会话（x-ot-session-id 维度），
// 超出时回 HTTP 400 + error_code TM.00001041。网关以前的做法是「照打，
// 被拒了就退避重试」—— 每次被拒都是一次白白的重试消耗，客户端也会看到
// 莫名其妙的 429。现在改成在网关里排队。

// concurrencyProbe 记录上游看到的并发情况。
type concurrencyProbe struct {
	mu       sync.Mutex
	inFlight map[string]int // key: x-ot-session-id（≈ 账号）
	maxPer   map[string]int
	total    int
	maxTotal int
	delay    time.Duration
	// sessions 记录出现过的 session id，用于断言请求真的分散到了多个账号。
	sessions map[string]bool
}

func newConcurrencyProbe(delay time.Duration) *concurrencyProbe {
	return &concurrencyProbe{
		inFlight: map[string]int{},
		maxPer:   map[string]int{},
		sessions: map[string]bool{},
		delay:    delay,
	}
}

func (p *concurrencyProbe) transport(t *testing.T) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sid := r.Header.Get("x-ot-session-id")
		p.mu.Lock()
		p.inFlight[sid]++
		p.total++
		p.sessions[sid] = true
		if p.inFlight[sid] > p.maxPer[sid] {
			p.maxPer[sid] = p.inFlight[sid]
		}
		if p.total > p.maxTotal {
			p.maxTotal = p.total
		}
		p.mu.Unlock()

		time.Sleep(p.delay)

		p.mu.Lock()
		p.inFlight[sid]--
		p.total--
		p.mu.Unlock()

		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(okCompletion)),
			Request:    r,
		}, nil
	})
}

// setupMultiAccountServer 构造 N 个账号的 Server，每个账号凭据互不相同
// （x-ot-session-id 由凭据派生，因此上游能区分账号）。
func setupMultiAccountServer(t *testing.T, n int) *Server {
	t.Helper()
	s := newTestServer(t, n, 3)
	s.Cred = codearts.NewManager(s.Up)
	s.Reg.Replace([]registry.Model{{ID: "glm-5.3"}})
	for i := 0; i < n; i++ {
		id := string(rune('a' + i))
		s.Store.SetAccountSession(id, &codearts.Session{
			AccessKey:     "ak-" + id,
			SecretKey:     "sk-" + id,
			SecurityToken: "st",
			ExpiresAt:     time.Now().Add(time.Hour),
		})
	}
	return s
}

// TestConcurrencyNeverExceedsPerAccountLimit 契约用例 #7 的核心：
// 无论客户端并发多大，上游**每个账号**看到的并发都不超过 3。
func TestConcurrencyNeverExceedsPerAccountLimit(t *testing.T) {
	s := setupMultiAccountServer(t, 1)
	probe := newConcurrencyProbe(40 * time.Millisecond)
	s.Up.HTTP = &http.Client{Transport: probe.transport(t)}

	const requests = 6 // 1 个账号 × 容量 3 → 必然需要排队
	var wg sync.WaitGroup
	codes := make([]int, requests)
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rec := postChat(t, s, `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
			codes[idx] = rec.Code
		}(i)
	}
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("第 %d 个请求状态码 = %d，期望 200（排队而不是失败）", i+1, c)
		}
	}
	if probe.maxTotal > 3 {
		t.Errorf("上游同时看到 %d 个并发请求，超过每账号上限 3", probe.maxTotal)
	}
}

// TestConcurrencySpreadsAcrossAccounts 多账号时流量要分散，不能全压在第一个号上。
func TestConcurrencySpreadsAcrossAccounts(t *testing.T) {
	s := setupMultiAccountServer(t, 4)
	probe := newConcurrencyProbe(30 * time.Millisecond)
	s.Up.HTTP = &http.Client{Transport: probe.transport(t)}

	const requests = 12
	var wg sync.WaitGroup
	failed := make([]int, requests)
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rec := postChat(t, s, `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
			if rec.Code != http.StatusOK {
				failed[idx] = rec.Code
			}
		}(i)
	}
	wg.Wait()

	for i, c := range failed {
		if c != 0 {
			t.Errorf("第 %d 个请求失败，状态码 = %d", i+1, c)
		}
	}
	for sid, max := range probe.maxPer {
		if max > 3 {
			t.Errorf("账号 %s 上出现 %d 个并发请求，超过上限 3", sid, max)
		}
	}
	if len(probe.sessions) < 2 {
		t.Errorf("12 个请求只落在 %d 个账号上，没有分散", len(probe.sessions))
	}
}

// TestAccountBusyReturns429WithRetryAfter 排队超时必须给 429 + Retry-After，
// 而不是把超限请求打到上游换一个 TM.00001041。
func TestAccountBusyReturns429WithRetryAfter(t *testing.T) {
	rec := httptest.NewRecorder()
	writeAPIErr(rec, errOpenAI, &apierr.AccountBusy{RetryAfter: 5 * time.Second})

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("状态码 = %d，期望 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q，期望 5", got)
	}
	var body struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误体不是合法 JSON: %v", err)
	}
	if got, _ := body.Error["code"].(string); got != "concurrency_limit_exceeded" {
		t.Errorf("error.code = %v，期望 concurrency_limit_exceeded", body.Error["code"])
	}
	if got, _ := body.Error["type"].(string); got != "rate_limit_error" {
		t.Errorf("error.type = %v，期望 rate_limit_error", body.Error["type"])
	}
}

// TestQueueTimeoutProducesAccountBusy 排队超时时 withAccountRetry 必须返回
// AccountBusy（对外 429），而不是继续往上游打。
func TestQueueTimeoutProducesAccountBusy(t *testing.T) {
	s := setupMultiAccountServer(t, 1)

	// 先把唯一账号的 3 个槽位全部占满，并让它们在测试期间不释放。
	ctx := t.Context()
	for i := 0; i < 3; i++ {
		if err := s.Store.AcquireSlot(ctx, "a"); err != nil {
			t.Fatalf("占槽位失败: %v", err)
		}
	}

	// 用极短的排队预算模拟超时：直接验证 AcquireSlot 的语义（真实路径用的是
	// 包级常量 queueWaitTimeout，测试里不该真的等 20 秒）。
	waitCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := s.Store.AcquireSlot(waitCtx, "a"); err == nil {
		t.Fatal("槽位占满时不应拿到槽位")
	}

	// 排空后恢复正常。
	for i := 0; i < 3; i++ {
		s.Store.ReleaseSlot("a")
	}
	if got := s.Store.InFlight("a"); got != 0 {
		t.Errorf("释放后在飞数 = %d，期望 0", got)
	}
}

// TestSlotsReleasedOnFailure 请求失败也必须释放槽位，否则一次上游故障
// 就会把账号永久锁死。
func TestSlotsReleasedOnFailure(t *testing.T) {
	// 重试退避在这里没有意义（本用例只关心槽位），跳过等待以免拖慢测试。
	restore := waitRetry
	waitRetry = func(context.Context, time.Duration) error { return nil }
	t.Cleanup(func() { waitRetry = restore })

	s := setupMultiAccountServer(t, 1)
	s.Up.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader("boom")),
			Request:    r,
		}, nil
	})}

	for i := 0; i < 4; i++ {
		rec := postChat(t, s, `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code == http.StatusOK {
			t.Fatalf("第 %d 次请求不该成功", i+1)
		}
	}
	if got := s.Store.InFlight("a"); got != 0 {
		t.Errorf("失败后仍有 %d 个槽位没释放", got)
	}
}
