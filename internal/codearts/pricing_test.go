package codearts

import (
	"math"
	"testing"
)

// TestModelPricingCredits 固化 2026-11-20 实测标定的换算公式。
//
// 期望值来自真实扣费（账号积分快照差值 / /v1/ops/credit/usage 的 creditCost），
// 不是「按公式算一遍再断言公式」—— 公式一旦被改错，这里必须红。
func TestModelPricingCredits(t *testing.T) {
	deepseek := ModelPricing{Ratio: 0.0486, InputPrice: 2, CachePrice: 0.2, OutputPrice: 8}
	glm := ModelPricing{Ratio: 0.0443, InputPrice: 8, CachePrice: 2, OutputPrice: 28}

	cases := []struct {
		name string
		p    ModelPricing
		in   int
		cach int
		out  int
		want float64
	}{
		// 无缓存 36568 token → 上游实测 1.51
		{"deepseek 无缓存", deepseek, 36568, 0, 8, 1.5062},
		// 26880 缓存命中 → 上游实测 0.12
		{"deepseek 缓存命中", deepseek, 27035, 26880, 8, 0.1183},
		// glm-5.3 35608 token 无缓存 → 上游实测 6.44（公式 6.43）
		{"glm 无缓存", glm, 35608, 0, 8, 6.4354},
	}
	for _, c := range cases {
		got := c.p.Credits(c.in, c.cach, c.out)
		if math.Abs(got-c.want) > 0.001 {
			t.Errorf("%s: got %.4f, want %.4f", c.name, got, c.want)
		}
	}
}

// TestModelPricingInvalid 无计费档位（ratio<=0）时不能算出 +Inf。
func TestModelPricingInvalid(t *testing.T) {
	bad := ModelPricing{Ratio: 0, InputPrice: 8, OutputPrice: 28}
	if got := bad.Credits(1000, 0, 100); got != 0 {
		t.Errorf("ratio=0 时 got %v, want 0", got)
	}
	if bad.Valid() {
		t.Error("ratio=0 应判为无效")
	}
	noPrice := ModelPricing{Ratio: 0.05}
	if noPrice.Valid() {
		t.Error("没有价格应判为无效")
	}
}

// TestModelPricingClampsCached 缓存数超过输入数时按输入数封顶（不产生负单价）。
func TestModelPricingClampsCached(t *testing.T) {
	p := ModelPricing{Ratio: 0.05, InputPrice: 2, CachePrice: 0.2, OutputPrice: 8}
	got := p.Credits(100, 999, 0)
	want := p.Credits(100, 100, 0)
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("cached 未封顶: got %v, want %v", got, want)
	}
}

// TestCreditTierPricing 解析上游的字符串档位；缺失价格时按无效处理，
// cache_price 缺失时退回输入价（宁可高估，不要凭空打折）。
func TestCreditTierPricing(t *testing.T) {
	p, ok := CreditTier{Ratio: "0.0486", InputPrice: "2", CachePrice: "0.2", OutputPrice: "8"}.Pricing()
	if !ok || p.Ratio != 0.0486 || p.CachePrice != 0.2 {
		t.Fatalf("解析失败: %+v ok=%v", p, ok)
	}
	p, ok = CreditTier{Ratio: "0.05", InputPrice: "2", OutputPrice: "8"}.Pricing()
	if !ok || p.CachePrice != 2 {
		t.Errorf("缺 cache_price 应退回输入价: %+v", p)
	}
	if _, ok := (CreditTier{Ratio: "", InputPrice: "2"}).Pricing(); ok {
		t.Error("缺 ratio 应判无效")
	}
}

// TestParseCreditUsage 解析 /v1/ops/credit/usage 的线上驼峰字段。
func TestParseCreditUsage(t *testing.T) {
	raw := []byte(`{"code":0,"data":{"totalTokenUsed":146,"promptTokenUsed":38,` +
		`"completionTokenUsed":108,"cachedTokenUsed":12,"cacheHitRatio":0.3158,"creditCost":0.02}}`)
	u, err := parseCreditUsage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.TotalTokens != 146 || u.PromptTokens != 38 || u.CompletionTokens != 108 ||
		u.CachedTokens != 12 || u.CreditCost != 0.02 {
		t.Errorf("解析结果不对: %+v", u)
	}
}

// TestParseCreditUsageErrors 业务错误（会话不存在）要变成 error，而不是 0 积分。
func TestParseCreditUsageErrors(t *testing.T) {
	if _, err := parseCreditUsage([]byte(`{"code":40401,"message":"session not found"}`)); err == nil {
		t.Error("code!=0 且无 data 应报错")
	}
	if _, err := parseCreditUsage([]byte(`{"code":0}`)); err == nil {
		t.Error("缺 data 应报错")
	}
}
