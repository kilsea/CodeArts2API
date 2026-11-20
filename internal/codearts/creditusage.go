package codearts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// CreditUsagePath 是「按轮次 / 按会话查真实积分消耗」的入口。
//
// 来自 CodeArts Agent 26.11.200 的 `out/extension.js`（`queryTurnUsage` /
// `querySessionUsage`）：`${chatDomain}/v1/ops/credit/usage`。
//
//	GET ?type=msg&id=<user-message-id>       一轮对话的消耗
//	GET ?type=session&id=<upstream-session>  一个会话窗口的累计消耗
//
// 这是**上游账单口径**：data.creditCost 是服务端算好的积分，客户端不再自己乘价。
// 网关的逐请求记录只能拿到 usage token，本地按价格档位估算（见 ModelPricing），
// 所以这个接口是「估算 vs 账单」对账的关键 —— 但它的 id 维度是 CodeArts 自己的
// msg/session，网关侧无法凭空构造，只能拿真实会话 ID 去查（用于标定与排查）。
//
// 鉴权与其它 snap 接口一致：SDK-HMAC-SHA256 签名 + `Agent-Type: PromptCenter`。
const CreditUsagePath = "/v1/ops/credit/usage"

// CreditUsage 是上游返回的积分消耗明细。
type CreditUsage struct {
	// TotalTokens 是总 token；Prompt/Completion/Cached 是拆分。
	TotalTokens      int64 `json:"total_tokens"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
	// CacheHitRatio 是上游给的缓存命中率（0–1，部分响应不带）。
	CacheHitRatio float64 `json:"cache_hit_ratio"`
	// CreditCost 是上游账单口径的积分消耗（保留两位小数）。
	CreditCost float64 `json:"credit_cost"`
}

// FetchCreditUsage 查询一轮对话（kind=msg）或一个会话（kind=session）的积分消耗。
//
// 这是一次只读查询，不消耗积分。id 必须来自真实上游上下文：
// 网关转发的请求里 user-msg-id / x-ot-session-id 都是自己生成的，
// 所以正常请求路径并不调用它，只在标定公式、排查账目时用真实 ID 去查。
func (c *Client) FetchCreditUsage(ctx context.Context, sess *Session, kind, id string) (*CreditUsage, error) {
	switch kind {
	case "msg", "session":
	default:
		return nil, fmt.Errorf("不支持的积分查询类型 %q（只支持 msg / session）", kind)
	}
	if id == "" {
		return nil, fmt.Errorf("积分查询缺少 id")
	}
	h := c.baseHeaders(sess, "", "")
	h["Agent-Type"] = "PromptCenter"
	path := CreditUsagePath + "?type=" + url.QueryEscape(kind) + "&id=" + url.QueryEscape(id)
	resp, err := c.do(ctx, sess, http.MethodGet, SnapHost, path, nil, h)
	if err != nil {
		return nil, err
	}
	if resp.Body == nil {
		return nil, fmt.Errorf("积分消耗响应没有 body")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &UpstreamError{Status: resp.StatusCode, Message: truncate(string(raw), 300)}
	}
	return parseCreditUsage(raw)
}

// parseCreditUsage 解析 /v1/ops/credit/usage 的响应。
//
// 线上字段是驼峰（totalTokenUsed / creditCost …），对外暴露成下划线命名，
// 避免把上游的命名风格漏到控制台和状态文件里。
func parseCreditUsage(raw []byte) (*CreditUsage, error) {
	var payload struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    *struct {
			TotalTokenUsed      int64   `json:"totalTokenUsed"`
			PromptTokenUsed     int64   `json:"promptTokenUsed"`
			CompletionTokenUsed int64   `json:"completionTokenUsed"`
			CachedTokenUsed     int64   `json:"cachedTokenUsed"`
			CacheHitRatio       float64 `json:"cacheHitRatio"`
			CreditCost          float64 `json:"creditCost"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("积分消耗解析失败: %w", err)
	}
	if payload.Data == nil {
		if payload.Code != 0 {
			return nil, fmt.Errorf("积分消耗查询失败: code=%d %s", payload.Code, payload.Message)
		}
		return nil, fmt.Errorf("积分消耗响应缺少 data")
	}
	return &CreditUsage{
		TotalTokens:      payload.Data.TotalTokenUsed,
		PromptTokens:     payload.Data.PromptTokenUsed,
		CompletionTokens: payload.Data.CompletionTokenUsed,
		CachedTokens:     payload.Data.CachedTokenUsed,
		CacheHitRatio:    payload.Data.CacheHitRatio,
		CreditCost:       payload.Data.CreditCost,
	}, nil
}
