package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"codearts2api/internal/apierr"
)

// 请求体前置校验。
//
// 之前 temperature / top_p / n / max_tokens / response_format / logprobs
// 全部原样透传：上游报错就是上游的错。问题有两个：
//
//  1. 每次非法参数都要先打一次上游（**真实扣积分**）才知道写错了；
//  2. 上游的报错文案是英文长句（`temperature -> temperature should be
//     between 0 and 2`），客户端拿不到结构化的字段名，只能人工读。
//
// 这里的策略（已写进 docs/COMPAT.md）：
//
//	参数越界 / 类型不对 / 取值不认识 → 400 + code=invalid_value + param=<字段>
//	n != 1                        → 400 + code=unsupported_parameter
//	上游不支持但无害的字段          → 从请求体里摘掉，并在响应头
//	                                X-Gateway-Unsupported: seed,logprobs 声明
//
// 二选一，**不允许**静默丢弃而不声明：客户端必须能从响应里看出
// 「我以为生效的东西其实没生效」。

// unsupportedFields 是上游不支持、网关会摘掉并声明的字段。
//
// 名单来自实测：上游对 logprobs / top_logprobs / seed 不做任何处理，
// 部分模型甚至直接报参数非法。摘掉它们能让请求顺利通过，
// 同时用响应头把「没生效」这件事说清楚。
var unsupportedFields = []string{"seed", "logprobs", "top_logprobs"}

// unsupportedHeader 声明本次请求里被网关摘掉的字段。
const unsupportedHeader = "X-Gateway-Unsupported"

// validateChatRequest 校验并规范化 OpenAI Chat Completions 请求体。
//
// 返回改写后的请求体与被摘掉的字段名（供写响应头）。校验失败返回
// *apierr.InvalidParamError（对外 400）。
func validateChatRequest(body []byte) ([]byte, []string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, nil, &apierr.InvalidParamError{Reason: "请求体必须是 JSON 对象"}
	}

	// messages 条数上限：上万条消息本身就是客户端 bug，早点拒掉比打一次上游便宜。
	if raw, ok := m["messages"]; ok {
		var list []json.RawMessage
		if err := json.Unmarshal(raw, &list); err == nil && len(list) > maxMessages {
			return nil, nil, &apierr.InvalidParamError{
				Param:  "messages",
				Reason: "messages 条数超过上限（" + strconv.Itoa(maxMessages) + " 条）",
				Code:   apierr.CodeRequestTooLarge,
			}
		}
	}

	// temperature ∈ [0, 2]：上游实测越界即报 RANGE_VALIDATOR。
	if err := checkRange(m, "temperature", 0, 2); err != nil {
		return nil, nil, err
	}
	// top_p ∈ [0, 1]。
	if err := checkRange(m, "top_p", 0, 1); err != nil {
		return nil, nil, err
	}
	// presence_penalty / frequency_penalty ∈ [-2, 2]（OpenAI 规范）。
	if err := checkRange(m, "presence_penalty", -2, 2); err != nil {
		return nil, nil, err
	}
	if err := checkRange(m, "frequency_penalty", -2, 2); err != nil {
		return nil, nil, err
	}

	// max_tokens / max_completion_tokens：必须为正整数。
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		if err := checkPositiveInt(m, key); err != nil {
			return nil, nil, err
		}
	}

	// n：上游只产出单个候选。n=1 正常放行，n>1 明确拒绝而不是「返回 1 个却不说」。
	if raw, ok := m["n"]; ok {
		var n int
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, nil, &apierr.InvalidParamError{Param: "n", Reason: "n 必须是整数"}
		}
		if n != 1 {
			return nil, nil, &apierr.InvalidParamError{
				Param:  "n",
				Reason: "上游只返回单个候选，n 必须为 1（网关不会悄悄替你降级）",
				Code:   apierr.CodeUnsupportedParam,
			}
		}
	}

	// response_format：只支持 text / json_object。
	if raw, ok := m["response_format"]; ok {
		var rf struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &rf); err != nil {
			return nil, nil, &apierr.InvalidParamError{Param: "response_format", Reason: "response_format 必须是对象"}
		}
		switch strings.ToLower(strings.TrimSpace(rf.Type)) {
		case "", "text", "json_object":
		default:
			return nil, nil, &apierr.InvalidParamError{
				Param:  "response_format",
				Reason: "response_format.type 只支持 text / json_object，收到 " + rf.Type,
				Code:   apierr.CodeUnsupportedParam,
			}
		}
	}

	// 上游不支持的字段：摘掉并记录，由调用方写响应头声明。
	var dropped []string
	for _, key := range unsupportedFields {
		if _, ok := m[key]; ok {
			delete(m, key)
			dropped = append(dropped, key)
		}
	}
	if len(dropped) == 0 {
		return body, nil, nil
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, nil, err
	}
	return out, dropped, nil
}

// checkRange 校验一个可选的数值字段落在 [lo, hi] 内。
func checkRange(m map[string]json.RawMessage, key string, lo, hi float64) error {
	raw, ok := m[key]
	if !ok || string(raw) == "null" {
		return nil
	}
	var v float64
	if err := json.Unmarshal(raw, &v); err != nil {
		return &apierr.InvalidParamError{Param: key, Reason: key + " 必须是数字"}
	}
	if v < lo || v > hi {
		return &apierr.InvalidParamError{
			Param:  key,
			Reason: key + " 必须在 " + trimFloat(lo) + " 到 " + trimFloat(hi) + " 之间，收到 " + trimFloat(v),
		}
	}
	return nil
}

// checkPositiveInt 校验一个可选的正整数字段。
func checkPositiveInt(m map[string]json.RawMessage, key string) error {
	raw, ok := m[key]
	if !ok || string(raw) == "null" {
		return nil
	}
	var v int
	if err := json.Unmarshal(raw, &v); err != nil {
		return &apierr.InvalidParamError{Param: key, Reason: key + " 必须是整数"}
	}
	if v <= 0 {
		return &apierr.InvalidParamError{Param: key, Reason: key + " 必须大于 0，收到 " + strconv.Itoa(v)}
	}
	return nil
}

// trimFloat 把 0.0 / 2.0 这类整数值打印成 "0" / "2"，让错误文案更像人话。
func trimFloat(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// announceUnsupported 在响应头里声明被摘掉的字段。
//
// 必须在 WriteHeader 之前调用。
func announceUnsupported(w http.ResponseWriter, dropped []string) {
	if len(dropped) == 0 {
		return
	}
	w.Header().Set(unsupportedHeader, strings.Join(dropped, ","))
}
