package server

import (
	"context"
	"encoding/json"
	"time"

	"codearts2api/internal/codearts"
	"codearts2api/internal/protocol"
	"codearts2api/internal/registry"
	"codearts2api/internal/store"
)

// 逐请求积分记账。
//
// 上游其实**有**账单口径的逐请求用量接口（来自 CodeArts Agent 26.11.200 的
// `queryTurnUsage`）：
//
//	GET /v1/ops/credit/usage?type=msg&id=<user-msg-id>
//	→ { data: { promptTokenUsed, completionTokenUsed, cachedTokenUsed,
//	            cacheHitRatio, creditCost } }
//
// 网关自己决定 `user-msg-id` 请求头，所以可以在一次推理结束后反查「这次到底
// 扣了多少积分」。查不到（网络 / 上游异常 / 该请求没计费）时退回本地估算：
//
//	credits = (in*input_price + cached*cache_price + out*output_price) / ratio / 1e6
//
// 公式与上游前端 `calculateTurnCredits` 逐字一致（含 Math.round(x*100)/100 的
// 两位小数口径），详见 internal/codearts/pricing.go 与 docs/CREDITS.md。

// creditUsageQueryTimeout 是反查上游账单的超时。
//
// 这次查询发生在回复已经写完 / 流已经结束后，只影响处理函数返回的时刻，
// 不影响客户端拿数据；2 秒足够，超时就退回估算，不把请求拖死。
const creditUsageQueryTimeout = 2 * time.Second

// tokenUsage 是上游回报的真实 token 用量。
type tokenUsage struct {
	In     int
	Cached int
	Out    int
}

// valid 报告是否有可用的 token 数（全 0 表示上游没回 usage）。
func (u tokenUsage) valid() bool { return u.In > 0 || u.Out > 0 }

// creditRecord 是一次请求的记账结果。
type creditRecord struct {
	tokens  tokenUsage
	credits float64
	// source 是 "upstream" / "estimate" / ""（未知）。
	source string
	// msgID 是发给上游的 user-msg-id（对账用；无账号时为空）。
	msgID string
}

// newMsgID 生成一次请求的上游消息 ID。
//
// 上游按这个 id 记账，反查时用它当 key；必须唯一，否则两次请求的用量会被合并。
func newMsgID() string { return "msg_" + codearts.RandomHex(12) }

// usageFromJSON 从一段上游 JSON（chat.completion、聚合结果或流式 usage 分片）
// 里抠出 token 用量。
//
// OpenAI 兼容接口的 usage 形状是统一的；取不到（分片里没有 usage / 解析失败）
// 时返回 ok=false。
func usageFromJSON(raw []byte) (tokenUsage, bool) {
	var payload struct {
		Usage *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Usage == nil {
		return tokenUsage{}, false
	}
	u := tokenUsage{
		In:     payload.Usage.PromptTokens,
		Cached: payload.Usage.PromptTokensDetails.CachedTokens,
		Out:    payload.Usage.CompletionTokens,
	}
	return u, u.valid()
}

// settleCredits 结算一次请求的积分。
//
// 顺序：上游账单（准确）→ 本地估算（有价格档位时）→ 未知。
// fallback 是已经从响应里抠到的 token 用量；上游账单会覆盖它（账单里的
// token 数才是计费口径）。
func (s *Server) settleCredits(ctx context.Context, acct *codearts.Account, model registry.Model, msgID string, fallback tokenUsage) creditRecord {
	rec := creditRecord{tokens: fallback}
	if acct != nil {
		rec.msgID = msgID
	}

	if s.creditLookup && acct != nil && msgID != "" {
		// 客户端断开 / 请求取消不应该让记账跳过，所以脱离原 ctx 的取消信号。
		qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), creditUsageQueryTimeout)
		defer cancel()
		if sess, err := s.Cred.Session(qctx, acct, s.resolveOptions()); err == nil {
			if u, err := s.Up.FetchCreditUsage(qctx, sess, "msg", msgID); err == nil {
				rec.tokens = tokenUsage{
					In:     int(u.PromptTokens),
					Cached: int(u.CachedTokens),
					Out:    int(u.CompletionTokens),
				}
				rec.credits = roundCredits(u.CreditCost)
				rec.source = "upstream"
				return rec
			}
		}
	}

	if model.Pricing != nil && model.Pricing.Valid() && fallback.valid() {
		rec.credits = roundCredits(model.Pricing.Credits(fallback.In, fallback.Cached, fallback.Out))
		// 即使四舍五入到 0 也标 estimate：这表示「有价格、按 0.00 计」，
		// 与「根本没有价格档位」（source 为空）是两件事。
		rec.source = "estimate"
	}
	return rec
}

// roundCredits 把积分保留两位小数。
//
// 与上游前端一致：它对每轮也做 Math.round(x*100)/100。不做这一步的话，
// 累计值会带一长串浮点尾数，控制台显示和账号账单对不上（尾差会滚雪球）。
func roundCredits(v float64) float64 {
	if v <= 0 {
		return 0
	}
	return float64(int64(v*100+0.5)) / 100
}

// recordOpt 是 record 的附加信息。
type recordOpt func(*store.LogEntry)

// withCredit 把一次请求的 token / 积分记账结果写进日志条目。
func withCredit(c creditRecord) recordOpt {
	return func(e *store.LogEntry) {
		e.InTokens = c.tokens.In
		e.OutTokens = c.tokens.Out
		e.CachedTokens = c.tokens.Cached
		e.Credits = c.credits
		e.CreditSource = c.source
		e.UpstreamMsgID = c.msgID
	}
}

// streamUsage 把 Responses 转换器的 usage 取成统一形态。
func streamUsage(st *protocol.ResponsesStream) tokenUsage {
	in, cached, out, ok := st.TokenUsage()
	if !ok {
		return tokenUsage{}
	}
	return tokenUsage{In: in, Cached: cached, Out: out}
}

// withElapsed 覆盖日志里的耗时。
//
// 记账（反查上游账单）发生在响应写完之后，它的几百毫秒不该算进「上游延迟」——
// 否则控制台的耗时列会把「书签开销」当成模型变慢。
func withElapsed(d time.Duration) recordOpt {
	return func(e *store.LogEntry) { e.LatencyMS = d.Milliseconds() }
}
