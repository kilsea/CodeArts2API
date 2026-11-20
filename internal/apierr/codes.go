package apierr

import "strings"

// 对外规范错误码（OpenAI error.code 的取值）。
//
// 为什么需要它：以前网关的错误体里 `code` 是一个 HTTP 状态码整数，
// 客户端只能看到「400」这一个语义模糊的数字 —— `Message role cannot empty`
// 与 `temperature 越界` 在客户端侧完全无法区分，也没法据此做本地修正。
// 现在每个可归因的失败都带一个稳定的字符串码，客户端可以按码分流。
const (
	// CodeInvalidRole 表示 messages[i].role 不是上游支持的取值。
	CodeInvalidRole = "invalid_role"
	// CodeModelNotFound 表示请求的模型名既不在清单里也不是已知别名。
	CodeModelNotFound = "model_not_found"
	// CodeInvalidValue 表示某个参数越界或类型不对（temperature / max_tokens …）。
	CodeInvalidValue = "invalid_value"
	// CodeUnsupportedParam 表示参数本身合法，但网关/上游不支持。
	CodeUnsupportedParam = "unsupported_parameter"
	// CodeRequestTooLarge 表示请求体或 messages 总长度超过上限。
	CodeRequestTooLarge = "request_too_large"
	// CodeContextLength 表示上下文超长。
	CodeContextLength = "context_length_exceeded"
	// CodeContentPolicy 表示命中内容审核。
	CodeContentPolicy = "content_policy_violation"
	// CodeConcurrency 表示上游「每账号 3 个并发会话」打满。
	CodeConcurrency = "concurrency_limit_exceeded"
	// CodeRateLimited 表示网关侧限流或上游 429。
	CodeRateLimited = "rate_limit_exceeded"
	// CodeModelNotMultimodal 表示给纯文本模型发了图片。
	CodeModelNotMultimodal = "model_not_multimodal"
	// CodeModelNameMismatch 表示 body.model 与 model-id 头不一致（上游 406）。
	CodeModelNameMismatch = "model_name_mismatch"
	// CodeCredentials 表示账号登录态失效，需要人工重新登录。
	CodeCredentials = "credentials_expired"
	// CodeNoAccount 表示账号池为空。
	CodeNoAccount = "no_account_available"
	// CodeUpstream 表示其余上游故障（5xx / 网络 / 未映射的业务码）。
	CodeUpstream = "upstream_error"
	// CodeInternal 表示网关自身的问题。
	CodeInternal = "internal_error"
	// CodeCreditDisabled 表示控制台关掉了「允许真实消耗积分」开关。
	CodeCreditDisabled = "credit_models_disabled"
	// CodePreviousResponseNotFound 表示 Responses API 的 previous_response_id
	// 指向的响应不存在（从未存在过，或已被淘汰 / 进程重启后丢失）。
	//
	// 取值与 OpenAI 一致：客户端（官方 SDK）靠它判断「这条链断了，得重发上下文」。
	CodePreviousResponseNotFound = "previous_response_not_found"
	// CodeResponseNotFound 表示按 id 查询 / 删除一个不存在的响应。
	CodeResponseNotFound = "not_found"
)

// Coder 由「能说清自己该归到哪个规范错误码」的错误类型实现。
//
// 用接口而不是给每个错误加字段：错误类型的作者最清楚自己的语义，
// 让它自己声明，`From` 只负责兜底 —— 避免在 From 里堆一长串类型断言。
type Coder interface {
	// APICode 返回规范错误码（见本文件常量）。
	APICode() string
	// APIParam 返回出错参数在请求体里的路径（如 "messages[3].role"），可空。
	APIParam() string
}

// InvalidRoleError 表示 messages[i].role 不是上游支持的取值。
//
// 上游只认 system / user / assistant / tool，其余一律回
// HTTP 400 {"error_code":…,"message":"Message role cannot empty"} ——
// 那句文案会把人引向「role 是空的」这个错误方向，因此网关必须自己拦。
type InvalidRoleError struct {
	// Index 是出错消息在 messages 里的下标；<0 表示整体结构有问题。
	Index int
	// Role 是客户端发来的原始 role（已 trim）。
	Role string
	// Reason 是补充说明（例如「messages 必须是数组」）。
	Reason string
}

// Error 实现 error。
func (e *InvalidRoleError) Error() string {
	if e.Reason != "" {
		return e.Reason
	}
	return "unsupported role: " + e.Role
}

// APICode 实现 Coder。
func (e *InvalidRoleError) APICode() string { return CodeInvalidRole }

// APIParam 实现 Coder。
func (e *InvalidRoleError) APIParam() string {
	if e.Index < 0 {
		return "messages"
	}
	return "messages[" + itoa(e.Index) + "].role"
}

// upstreamCode 是「上游业务码 → 网关规范码」的一条映射。
type upstreamCode struct {
	// marker 是错误原文里出现即命中（大小写敏感的稳定子串）。
	marker string
	code   string
	param  string
}

// upstreamCodes 是上游业务错误码的映射表。
//
// 取值与 docs/UPSTREAM.md §8「错误码对照」一一对应 —— 那份表是实测记录，
// 这里只是它的可执行形态。**新增映射必须同时更新那份文档**。
//
// 只放「能在客户端侧做点什么的」码：映射不了的错误保持 upstream_error，
// 原文通过 `upstream_error` 字段透出，不做猜测。
var upstreamCodes = []upstreamCode{
	{"TM.00001041", CodeConcurrency, ""},
	{"InferHub.001001005", CodeInvalidValue, ""},
	{"InferHub.001001011", CodeModelNameMismatch, "model"},
	{"InferHub.001001020", CodeModelNotMultimodal, "messages"},
	{"ModelArts.81011", CodeContentPolicy, ""},
	// 81001 = 参数非法。实测：messages 里出现 null content、或 tool 消息缺少
	// 可匹配的 tool_call 时返回。不指定 param —— 上游不告诉我们是哪个字段。
	{"ModelArts.81001", CodeInvalidValue, ""},
	{"APIG.0301", CodeCredentials, ""},
	{"APIG.0303", CodeCredentials, ""},
	{"STS5.1804", CodeCredentials, ""},
	{"STS5.1806", CodeCredentials, ""},
	{"invalid_grant", CodeCredentials, ""},
	// 4005 是「该通道不支持此模型」，用 "4005:" 而不是裸 "4005"，
	// 避免误命中错误信息里其它位置的数字。
	{"4005:", CodeModelNotFound, "model"},
}

// mapUpstream 在错误原文里找规范码。找不到返回空串（调用方按 upstream_error 处理）。
func mapUpstream(msg string) (code, param string) {
	for _, m := range upstreamCodes {
		if strings.Contains(msg, m.marker) {
			return m.code, m.param
		}
	}
	return "", ""
}

// itoa 是 strconv.Itoa 的极小替身，避免为一个拼接引入 import。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// InvalidParamError 表示某个请求参数不合法（越界 / 类型不对 / 不受支持）。
//
// 与 InvalidRoleError 同理：这类错误必须在触达上游之前就 400，
// 否则既浪费一次真实扣积分的上游调用，又会把「客户端写错了」记成「上游故障」。
type InvalidParamError struct {
	// Param 是出错参数在请求体里的路径（如 "temperature"、"messages"）。
	Param string
	// Reason 是一句能直接展示给用户的话。
	Reason string
	// Code 是规范码，留空时按 invalid_value 处理。
	Code string
}

// Error 实现 error。
func (e *InvalidParamError) Error() string { return e.Reason }

// ResponseNotFound 表示 Responses API 里的响应 id 查不到。
//
// Param 为空表示客户端是**直接按 id 查**（GET /v1/responses/{id}），
// 非空表示它来自请求体里的某个字段（previous_response_id）。
// 两种情况对外状态码都是 404，但规范码不同：OpenAI 对前者回 `not_found`，
// 对后者回 `previous_response_not_found`。
type ResponseNotFound struct {
	// ID 是客户端提供的响应 id。
	ID string
	// Param 是请求体里的字段路径，空表示按 id 直查。
	Param string
}

// Error 实现 error。
func (e *ResponseNotFound) Error() string {
	if e.Param != "" {
		return "previous_response_id 指向的响应 " + e.ID + " 不存在或已过期，请重新发送完整上下文"
	}
	return "响应 " + e.ID + " 不存在或已过期"
}

// APICode 实现 Coder。
func (e *ResponseNotFound) APICode() string {
	if e.Param != "" {
		return CodePreviousResponseNotFound
	}
	return CodeResponseNotFound
}

// APIParam 实现 Coder。
func (e *ResponseNotFound) APIParam() string { return e.Param }

// APICode 实现 Coder。
func (e *InvalidParamError) APICode() string {
	if e.Code != "" {
		return e.Code
	}
	return CodeInvalidValue
}

// APIParam 实现 Coder。
func (e *InvalidParamError) APIParam() string { return e.Param }
