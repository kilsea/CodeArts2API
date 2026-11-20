package server

import (
	"bytes"
	"encoding/json"
	"math"
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
		if err := json.Unmarshal(raw, &list); err == nil {
			if len(list) > maxMessages {
				return nil, nil, &apierr.InvalidParamError{
					Param:  "messages",
					Reason: "messages 条数超过上限（" + strconv.Itoa(maxMessages) + " 条）",
					Code:   apierr.CodeRequestTooLarge,
				}
			}
			if err := checkMessageContents(list); err != nil {
				return nil, nil, err
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
	if raw, ok := m["n"]; ok && string(raw) != "null" {
		n, ok := decodeInt(raw)
		if !ok {
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

	// 整数字段的字面量规范化（见 normalizeIntFields）。
	changed := normalizeIntFields(m)

	// 上游不支持的字段：摘掉并记录，由调用方写响应头声明。
	var dropped []string
	for _, key := range unsupportedFields {
		if _, ok := m[key]; ok {
			delete(m, key)
			dropped = append(dropped, key)
		}
	}
	if len(dropped) == 0 && !changed {
		return body, nil, nil
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, nil, err
	}
	return out, dropped, nil
}

// intFields 是按整数语义解析、但客户端可能写成浮点的字段。
// top_logprobs 不在这里：它属于 unsupportedFields，会被整字段摘掉，
// 规范化没有意义。
var intFields = []string{"n", "max_tokens", "max_completion_tokens"}

// normalizeIntFields 把「整数值但写成浮点」的字段改写成整数文本，返回是否有改动。
//
// JSON 没有整数类型：Python / JS 客户端把 1 序列化成 1.0 是常态
// （openai-python 的 `n=1.0` 就长这样），OpenAI 官方接口照收。
// 但上游是**按字面量校验类型**的 —— 实测 `n:1.0` / `max_tokens:8.0` 会被回
// `400 The request param is invalid`。所以网关不能只「放行」，还得把它改写成 1 / 8：
// 既然我们已经确认它的值是整数，替客户端写规范形式是兼容层该做的事。
//
// 只改字面量，不改语义：`1.5` 仍然在校验阶段被拒。
func normalizeIntFields(m map[string]json.RawMessage) bool {
	changed := false
	for _, key := range intFields {
		raw, ok := m[key]
		if !ok || string(raw) == "null" {
			continue
		}
		if !bytes.ContainsAny(raw, ".eE") {
			continue // 已经是整数写法，保持原样（不碰 body 的数字精度）
		}
		v, ok := decodeInt(raw)
		if !ok {
			continue // 非整数值交给各字段自己的校验报错
		}
		m[key] = []byte(strconv.FormatInt(v, 10))
		changed = true
	}
	return changed
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
	v, ok := decodeInt(raw)
	if !ok {
		return &apierr.InvalidParamError{Param: key, Reason: key + " 必须是整数"}
	}
	if v <= 0 {
		return &apierr.InvalidParamError{Param: key, Reason: key + " 必须大于 0，收到 " + strconv.FormatInt(v, 10)}
	}
	return nil
}

// decodeInt 解析一个 JSON 数字并确认它的值是个整数。
//
// JSON 没有整数类型：Python / JS 客户端把 1 序列化成 1.0 是常态
// （openai-python 的 `n=1.0` 就长这样），OpenAI 官方接口照收。
// 以前这里直接 Unmarshal 到 int，1.0 会解析失败 → 400，
// 把一个合法请求挡在门外。现在只拒绝**真正的**非整数（如 1.5）。
func decodeInt(raw json.RawMessage) (int64, bool) {
	// json.Number 的底层是 string，Decoder 会宽容地把 JSON 字符串 "1"
	// 也解成 Number —— 那不是数字字面量，必须拒绝（否则 max_tokens:"100"
	// 会一路透传到上游再被拒）。
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] == '"' {
		return 0, false
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var num json.Number
	if err := dec.Decode(&num); err != nil {
		return 0, false
	}
	if i, err := num.Int64(); err == nil {
		return i, true
	}
	f, err := num.Float64()
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	if f != math.Trunc(f) || f > math.MaxInt64 || f < math.MinInt64 {
		return 0, false
	}
	return int64(f), true
}

// checkMessageContents 拒绝「content 为 null 且没有 tool_calls」的消息。
//
// 这种消息没有任何可发给上游的内容，上游会回
// `ModelArts.81001 未正常接收到prompt参数` —— 打一次上游就是一次真实扣积分，
// 而且客户端拿到的是一句跟真实原因无关的话。
//
// assistant 消息允许 content=null，只要带了 tool_calls / function_call
// （那是标准的「只要调用、不要文本」写法）。
func checkMessageContents(list []json.RawMessage) error {
	for i, raw := range list {
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue // 形状不对的消息交给 normalizeRoles 报
		}
		if c, ok := msg["content"]; ok && string(c) != "null" {
			// content 只能是字符串或分段数组。写成数字 / 布尔 / 对象时
			// 上游会回 `The type of content should be string or array` ——
			// 打一次上游就是一次真实扣积分，而这完全是客户端写错了。
			if t := bytes.TrimSpace(c); len(t) > 0 && t[0] != '"' && t[0] != '[' {
				return &apierr.InvalidParamError{
					Param:  "messages[" + strconv.Itoa(i) + "].content",
					Reason: "content 只能是字符串或内容分段数组",
				}
			}
			continue
		}
		if _, ok := msg["tool_calls"]; ok {
			continue
		}
		if _, ok := msg["function_call"]; ok {
			continue
		}
		return &apierr.InvalidParamError{
			Param:  "messages[" + strconv.Itoa(i) + "].content",
			Reason: "content 不能为空：该消息既没有文本内容，也没有 tool_calls",
		}
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
