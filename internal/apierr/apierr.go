// Package apierr 把内部错误统一映射为对外 HTTP 语义。
//
// 网关对外暴露两套协议（OpenAI 与 Anthropic），同一个内部错误在两套协议里
// 需要不同的 error.type，但 HTTP 状态码是共通的。把三者的映射集中在一个
// 地方，避免 chat.go / anthropic.go 各维护一份表、改一处漏一处。
//
// 设计要点：映射是「内部错误 → 对外表述」的纯函数，不依赖任何 HTTP 上下文，
// 因此可以被单元测试完整覆盖。
package apierr

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"codearts2api/internal/codearts"
)

// NoAccount 表示账号池为空（需要用户先去控制台导入 / 登录）。
var NoAccount = errors.New("没有可用账号，请先在控制台「导入本机桌面端」或在 CodeArts Agent 里登录")

// ModelNotFound 表示请求的模型名既不在上游清单里，也不是已知别名。
//
// 免费权益通道下线后每个模型都真实扣积分，因此**不能**像以前那样
// 静默回落到某个默认模型 —— 猜错模型等于替用户乱花钱。
type ModelNotFound struct {
	// Name 是客户端请求的模型名（可能为空：请求体里没写 model）。
	Name string
	// Available 是当前可用的模型 ID 列表，用于拼出可操作的提示。
	Available []string
}

// Error 实现 error。
func (e *ModelNotFound) Error() string {
	if e.Name == "" {
		return "请求未指定 model，且没有配置默认模型"
	}
	msg := "模型 " + e.Name + " 不存在"
	if len(e.Available) > 0 {
		msg += "；可用模型：" + strings.Join(e.Available, ", ")
	}
	return msg
}

// APICode 实现 Coder。
func (e *ModelNotFound) APICode() string { return CodeModelNotFound }

// APIParam 实现 Coder。
func (e *ModelNotFound) APIParam() string { return "model" }

// AccountBusy 表示**全部账号的并发槽位都已占满**，排队超时。
//
// 与 RateLimited 的区别：RateLimited 是「你这个密钥打得太快」，
// AccountBusy 是「所有账号都被占着」—— 后者与客户端频率无关，
// 换一个密钥也未必能立刻成功，但等一会儿一定会好。
type AccountBusy struct {
	// RetryAfter 是建议客户端等待的时长，恒大于 0。
	RetryAfter time.Duration
}

// Error 实现 error。
func (e *AccountBusy) Error() string {
	secs := int(math.Ceil(e.RetryAfter.Seconds()))
	if secs < 1 {
		secs = 1
	}
	return fmt.Sprintf("全部账号的并发会话已占满（上游每账号上限 3 个），请在 %d 秒后重试", secs)
}

// RateLimited 表示**网关侧**的限流拦截。
//
// 必须与上游 429 区分开：上游 429 是「账号的并发会话满了，换个时机或换个账号再试」，
// 网关侧限流是「这个密钥自己打得太快」。两者对外状态码相同，但语义与退避时长不同 ——
// 这里的 RetryAfter 是网关能算准的，上游给不了。
type RateLimited struct {
	// RetryAfter 是建议客户端等待的时长，恒大于 0。
	RetryAfter time.Duration
}

// Error 实现 error。
func (e *RateLimited) Error() string {
	secs := int(math.Ceil(e.RetryAfter.Seconds()))
	if secs < 1 {
		secs = 1
	}
	return fmt.Sprintf("请求过于频繁：该密钥已触发网关限流，请在 %d 秒后重试", secs)
}

// Error 是一次失败的对外表述。
type Error struct {
	// Status 是对外的 HTTP 状态码。
	Status int
	// OpenAI 是 OpenAI 风格的 error.type。
	OpenAI string
	// Anthropic 是 Anthropic 风格的 error.type。
	Anthropic string
	// Code 是规范错误码（见 codes.go 常量），可空。
	Code string
	// Param 是出错参数在请求体里的路径（如 "messages[3].role"），可空。
	Param string
	// Upstream 是上游原始错误原文：仅当 Code 映射不出来时才填，
	// 让客户端至少能看到上游说了什么，而不是一句笼统的 upstream_error。
	Upstream string
	// RetryAfter > 0 时由写出方补 Retry-After 响应头。
	RetryAfter time.Duration
}

// From 把内部错误映射为对外表述。
//
// 映射表（与 README「错误语义」一节一一对应）：
//
//	情况                        状态码  OpenAI type            Anthropic type
//	账号池为空                   503     server_error         overloaded_error
//	凭据失效（无法续期）          502     credentials_error    authentication_error
//	网关侧限流（本机密钥打太快）   429     rate_limit_error     rate_limit_error
//	上游并发上限 / 429           429     rate_limit_error     rate_limit_error
//	参数或模型名不合法            400     invalid_request_error invalid_request_error
//	模型不存在                   404     invalid_request_error invalid_request_error
//	上游 5xx / 网络错误           502     upstream_error       overloaded_error
//
// 「凭据失效」必须与上游临时故障区分开：前者重试没有意义，只能重新登录；
// 把它报成 overloaded_error 会让客户端一直退避重试。
func From(err error) Error {
	if err == nil {
		return Error{Status: http.StatusOK}
	}
	if errors.Is(err, NoAccount) {
		return Error{Status: http.StatusServiceUnavailable, OpenAI: "server_error", Anthropic: "overloaded_error"}
	}
	var mnf *ModelNotFound
	if errors.As(err, &mnf) {
		return coded(Error{Status: http.StatusNotFound, OpenAI: "invalid_request_error", Anthropic: "invalid_request_error"}, err)
	}
	var rl *RateLimited
	if errors.As(err, &rl) {
		e := coded(Error{Status: http.StatusTooManyRequests, OpenAI: "rate_limit_error", Anthropic: "rate_limit_error", Code: CodeRateLimited}, err)
		e.RetryAfter = rl.RetryAfter
		return e
	}
	var busy *AccountBusy
	if errors.As(err, &busy) {
		e := coded(Error{Status: http.StatusTooManyRequests, OpenAI: "rate_limit_error", Anthropic: "rate_limit_error", Code: CodeConcurrency}, err)
		e.RetryAfter = busy.RetryAfter
		return e
	}
	// 请求结构本身不合法（role 不认识 / messages 形状不对）：这是纯粹的客户端问题，
	// 必须在触达上游之前就报 400，而不是先打一次上游再把它当成上游故障（502）。
	var ire *InvalidRoleError
	if errors.As(err, &ire) {
		return coded(Error{Status: http.StatusBadRequest, OpenAI: "invalid_request_error", Anthropic: "invalid_request_error"}, err)
	}
	// 参数越界 / 类型不对 / 不受支持：同样是纯粹的客户端问题。
	var ipe *InvalidParamError
	if errors.As(err, &ipe) {
		// request_too_large 必须配 413：它描述的是「请求太大」而不是
		// 「参数写错了」。客户端与 CDN / 反代都靠状态码决定要不要重试。
		status := http.StatusBadRequest
		if ipe.APICode() == CodeRequestTooLarge {
			status = http.StatusRequestEntityTooLarge
		}
		return coded(Error{Status: status, OpenAI: "invalid_request_error", Anthropic: "invalid_request_error"}, err)
	}

	var ue *codearts.UpstreamError
	if !errors.As(err, &ue) {
		// 凭据不可用、网络错误等：都归为网关侧问题（502），
		// 但凭据失效要给客户端一个「别重试、去登录」的明确信号。
		if codearts.IsCredentialsError(err) {
			return coded(Error{Status: http.StatusBadGateway, OpenAI: "credentials_error", Anthropic: "authentication_error", Code: CodeCredentials}, err)
		}
		return coded(Error{Status: http.StatusBadGateway, OpenAI: "upstream_error", Anthropic: "overloaded_error", Code: CodeUpstream}, err)
	}

	var out Error
	switch {
	case codearts.IsTransientUpstream(err):
		// 上游 5xx 用 502；并发上限（上游用 400 表达）与 429 用 429，
		// 让客户端知道「等一会再试」而不是「请求错了」。
		if ue.Status >= 500 {
			out = Error{Status: http.StatusBadGateway, OpenAI: "upstream_error", Anthropic: "overloaded_error"}
		} else {
			out = Error{Status: http.StatusTooManyRequests, OpenAI: "rate_limit_error", Anthropic: "rate_limit_error", Code: CodeRateLimited}
		}
	case ue.Status == http.StatusNotFound:
		out = Error{Status: http.StatusNotFound, OpenAI: "invalid_request_error", Anthropic: "invalid_request_error"}
	case ue.Status == http.StatusOK:
		// 防御分支：业务错误不再产出 Status=200（`detectUpstreamError` 现在从
		// error_code 里取真实状态码，见 `statusFromCode`）。保留它只为兜住将来
		// 某个调用方又写出 `Status: 200` 的错误 —— 那属于客户端问题，不该报成网关故障。
		out = Error{Status: http.StatusBadRequest, OpenAI: "invalid_request_error", Anthropic: "invalid_request_error"}
	case ue.Status >= 400 && ue.Status < 500:
		out = Error{Status: http.StatusBadRequest, OpenAI: "invalid_request_error", Anthropic: "invalid_request_error"}
	default:
		out = Error{Status: http.StatusBadGateway, OpenAI: "upstream_error", Anthropic: "overloaded_error"}
	}
	return withUpstreamCode(out, ue.Message)
}

// coded 用错误类型自己声明的规范码填充 Error。
func coded(e Error, err error) Error {
	var c Coder
	if errors.As(err, &c) {
		e.Code = c.APICode()
		e.Param = c.APIParam()
	}
	if e.Code == "" {
		e.Code = codeFromStatus(e)
	}
	return e
}

// withUpstreamCode 为上游错误补规范码：映射表命中就用映射值，
// 否则落到按状态码推导的通用码，并把上游原文放进 Upstream 字段。
//
// 「不能映射就保留原文」是刻意的：网关替客户端猜错的代价比不猜高得多，
// 客户端至少能从 upstream_error 里看到上游到底说了什么。
func withUpstreamCode(e Error, msg string) Error {
	if code, param := mapUpstream(msg); code != "" {
		e.Code = code
		if e.Param == "" {
			e.Param = param
		}
		return e
	}
	if e.Code == "" {
		e.Code = codeFromStatus(e)
	}
	// 能映射的已经翻译成人话了，再把上游原串塞进响应只会让客户端更难判断；
	// 映射不出来的才透出原文。
	if msg != "" {
		e.Upstream = msg
	}
	return e
}

// codeFromStatus 是「没有更具体的码」时的兜底。
func codeFromStatus(e Error) string {
	switch {
	case e.Status == http.StatusServiceUnavailable:
		return CodeNoAccount
	case e.Status == http.StatusTooManyRequests:
		return CodeRateLimited
	case e.Status >= 400 && e.Status < 500:
		return CodeInvalidValue
	case e.Status >= 500:
		return CodeUpstream
	}
	return ""
}
