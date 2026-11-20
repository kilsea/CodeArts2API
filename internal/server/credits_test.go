package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"codearts2api/internal/codearts"
	"codearts2api/internal/registry"
	"codearts2api/internal/store"
)

// TestUsageFromJSON 从上游响应里抠 token；没有 usage 时必须报告 ok=false。
func TestUsageFromJSON(t *testing.T) {
	raw := []byte(`{"id":"x","choices":[],"usage":{"prompt_tokens":38,"completion_tokens":108,` +
		`"prompt_tokens_details":{"cached_tokens":12}}}`)
	u, ok := usageFromJSON(raw)
	if !ok || u.In != 38 || u.Out != 108 || u.Cached != 12 {
		t.Fatalf("解析不对: %+v ok=%v", u, ok)
	}
	if _, ok := usageFromJSON([]byte(`{"choices":[]}`)); ok {
		t.Error("没有 usage 应返回 ok=false")
	}
	if _, ok := usageFromJSON([]byte(`not json`)); ok {
		t.Error("非 JSON 应返回 ok=false")
	}
}

// TestRoundCredits 与上游一致：两位小数，四舍五入。
func TestRoundCredits(t *testing.T) {
	cases := map[float64]float64{0: 0, 0.0049: 0, 0.005: 0.01, 1.5049: 1.5, 6.4354: 6.44, -1: 0}
	for in, want := range cases {
		if got := roundCredits(in); got != want {
			t.Errorf("roundCredits(%v) = %v, want %v", in, got, want)
		}
	}
}

// creditFakeTransport 同时顶替对话接口和积分账单接口。
func creditFakeTransport(t *testing.T, creditBody string, creditStatus int) roundTripFunc {
	t.Helper()
	return func(r *http.Request) (*http.Response, error) {
		status := http.StatusOK
		body := okCompletion
		if strings.Contains(r.URL.Path, "/v1/ops/credit/usage") {
			status = creditStatus
			body = creditBody
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	}
}

// setupCreditServer 构造一个「会反查上游账单」的服务：1 个账号 + 带价格档位
// 的模型 + 可替换的上游 Transport。
func setupCreditServer(t *testing.T, transport roundTripFunc) *Server {
	t.Helper()
	s := newTestServer(t, 1, 1)
	s.Cred = codearts.NewManager(s.Up)
	s.creditLookup = true
	s.Reg.Replace([]registry.Model{{
		ID: "glm-5.3", Credit: "0.79x",
		Pricing: &codearts.ModelPricing{Ratio: 0.0443, InputPrice: 8, CachePrice: 2, OutputPrice: 28},
	}})
	s.Store.SetAccountSession("a", &codearts.Session{
		AccessKey: "ak", SecretKey: "sk", SecurityToken: "st", ExpiresAt: time.Now().Add(time.Hour),
	})
	s.Up.HTTP = &http.Client{Transport: transport}
	return s
}

// TestChatRecordsUpstreamCredits 一次成功的对话必须把上游账单口径的积分、
// token 和账号一起写进日志与用量聚合。
func TestChatRecordsUpstreamCredits(t *testing.T) {
	s := setupCreditServer(t, creditFakeTransport(t,
		`{"code":0,"data":{"totalTokenUsed":146,"promptTokenUsed":38,"completionTokenUsed":108,`+
			`"cachedTokenUsed":12,"cacheHitRatio":0.08,"creditCost":0.27}}`, http.StatusOK))

	rec := postChat(t, s, `{"model":"glm-5.3","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200: %s", rec.Code, rec.Body.String())
	}

	logs := s.Store.QueryLogs(store.LogFilter{}, 0)
	if len(logs) != 1 {
		t.Fatalf("日志条数 = %d，期望 1", len(logs))
	}
	e := logs[0]
	if e.CreditSource != "upstream" {
		t.Errorf("credit_source = %q，期望 upstream", e.CreditSource)
	}
	if e.Credits != 0.27 {
		t.Errorf("credits = %v，期望 0.27（上游账单）", e.Credits)
	}
	if e.InTokens != 38 || e.OutTokens != 108 || e.CachedTokens != 12 {
		t.Errorf("token 记账不对: in=%d out=%d cached=%d", e.InTokens, e.OutTokens, e.CachedTokens)
	}
	if e.AccountID != "a" {
		t.Errorf("account_id = %q，期望 a（积分必须跟着账号）", e.AccountID)
	}

	rows := s.Store.Usage(1)
	if len(rows) != 1 || rows[0].Credits != 0.27 || rows[0].AccountID != "a" || rows[0].Model != "glm-5.3" {
		t.Errorf("用量聚合不对: %+v", rows)
	}
}

// TestChatFallsBackToEstimate 账单接口失败时退回价格档位估算，并标记来源。
func TestChatFallsBackToEstimate(t *testing.T) {
	// 用一个「大 prompt」的响应，让估算值明显不为 0：
	// glm-5.3 档位 8/28 → 35608*8/0.0443/1e6 ≈ 6.43。
	const bigCompletion = `{"id":"chatcmpl-t","object":"chat.completion","created":1700000000,` +
		`"model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},` +
		`"finish_reason":"stop"}],"usage":{"prompt_tokens":35608,"completion_tokens":8}}`
	s := setupCreditServer(t, func(r *http.Request) (*http.Response, error) {
		status, body := http.StatusOK, bigCompletion
		if strings.Contains(r.URL.Path, "/v1/ops/credit/usage") {
			status, body = http.StatusInternalServerError, `{"code":500,"message":"boom"}`
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})

	rec := postChat(t, s, `{"model":"glm-5.3","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}

	e := s.Store.QueryLogs(store.LogFilter{}, 0)[0]
	if e.CreditSource != "estimate" {
		t.Fatalf("credit_source = %q，期望 estimate", e.CreditSource)
	}
	if e.Credits != 6.44 {
		t.Errorf("估算积分 = %v，期望 6.44", e.Credits)
	}
	if e.InTokens != 35608 || e.OutTokens != 8 {
		t.Errorf("估算用的 token 应从响应的 usage 取: in=%d out=%d", e.InTokens, e.OutTokens)
	}
}
