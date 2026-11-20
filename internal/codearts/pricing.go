package codearts

import "strconv"

// ModelPricing 是一个模型的积分计费参数。
//
// 字段直接来自 `GET /v1/model/builtin` 的 `credit[]`（见 CreditTier）：
//
//	ratio        1 积分折合多少（元），例如 deepseek-v4.1-flash 是 0.0486
//	input_price  新输入 token 的单价（元/百万 token）
//	cache_price  命中上下文缓存的输入 token 单价（元/百万 token）
//	output_price 输出 token 的单价（元/百万 token）
//
// 换算公式（2026-11-20 用真实扣费标定，见 docs/CREDITS.md）：
//
//	credits = (in*input_price + cached*cache_price + out*output_price) / ratio / 1e6
//
// 也就是说 `ratio` 是「每积分多少元」，不是折扣系数。两个模型各用一条
// 大 prompt 实测对上了：
//
//	glm-5.3     35608 token / 0 缓存 → 实测 6.44，公式 6.43
//	deepseek    36568 token / 0 缓存 → 实测 1.51，公式 1.50
//	deepseek    155 + 26880 缓存     → 实测 0.12，公式 0.118
type ModelPricing struct {
	Ratio       float64 `json:"ratio"`
	InputPrice  float64 `json:"input_price"`
	CachePrice  float64 `json:"cache_price"`
	OutputPrice float64 `json:"output_price"`
}

// Valid 报告这组价格能不能用来算积分。
//
// ratio <= 0 的模型（上游没公布计费档，例如图片模型）直接跳过：
// 除以 0 会把积分算成 +Inf，写进状态文件后就再也修不回来了。
func (p ModelPricing) Valid() bool {
	return p.Ratio > 0 && (p.InputPrice > 0 || p.OutputPrice > 0)
}

// Credits 计算一次请求消耗的积分。
//
// cached 是命中缓存的输入 token 数（上游 usage.prompt_tokens_details.cached_tokens），
// 它已经包含在 in 里；这里按「(in-cached) 全价 + cached 缓存价」拆开算。
func (p ModelPricing) Credits(in, cached, out int) float64 {
	if !p.Valid() {
		return 0
	}
	if in < 0 {
		in = 0
	}
	if cached < 0 {
		cached = 0
	}
	if cached > in {
		cached = in
	}
	if out < 0 {
		out = 0
	}
	cost := float64(in-cached)*p.InputPrice +
		float64(cached)*p.CachePrice +
		float64(out)*p.OutputPrice
	return cost / p.Ratio / 1e6
}

// Pricing 把上游的一条计费档位转成可计算的价格。
//
// 只取第一档：上游对同一模型可能给多档（deepseek-v4.1-flash-1017 有
// 0.0486/0.0365 两档），实测扣费用的是第一档；按「档位越高越优先」猜会把
// 估算打成 1.3 倍误差。
//
// ok=false 表示这条档位没有可用价格（字段缺失 / 解析失败 / ratio<=0）。
func (t CreditTier) Pricing() (ModelPricing, bool) {
	p := ModelPricing{
		Ratio:       parseFloat(t.Ratio),
		InputPrice:  parseFloat(t.InputPrice),
		CachePrice:  parseFloat(t.CachePrice),
		OutputPrice: parseFloat(t.OutputPrice),
	}
	if !p.Valid() {
		return ModelPricing{}, false
	}
	if p.CachePrice <= 0 {
		// 缺 cache_price 时按输入价算，宁可高估也不要凭空打折。
		p.CachePrice = p.InputPrice
	}
	return p, true
}

// parseFloat 解析上游的「数字字符串」字段，失败时返回 0。
func parseFloat(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}
