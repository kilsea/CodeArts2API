package server

import (
	"io"
	"net/http"

	"codearts2api/internal/apierr"
)

// 请求体大小上限。
//
// 之前是散落在各 handler 里的字面量 `32<<20`，既没有名字也没有对外的声明：
// 超限时 io.LimitReader 会**静默截断**，JSON 解析失败后客户端只看到一句
// 「请求体解析失败」——完全看不出真实原因是「太大了」。现在超限直接
// 413 request_too_large，客户端不必猜。
const (
	// maxRequestBody 是 /v1/* 请求体的字节上限（32 MiB）。
	//
	// 取 32 MiB 的理由：多模态请求会把图片以 base64 内联，一张 4 MB 的图
	// 编码后约 5.3 MB；32 MiB 能容纳 4~5 张图，同时避免有人拿 500 MB 的
	// 请求把网关内存打满。
	maxRequestBody = 32 << 20

	// maxMessages 是单次请求允许的最大消息条数。
	//
	// 上游对超长上下文会自己报错，但那时已经打了一次上游（真实扣积分）；
	// 而且上万条消息本身就是客户端 bug，早点拒掉更省事。
	maxMessages = 2000
)

// readBody 读取请求体，区分「读失败」与「超限」两种情况。
//
// 超限时会写好 413 响应并返回 ok=false，调用方直接 return 即可。
// style 决定错误体形状（OpenAI / Anthropic）。
func readBody(w http.ResponseWriter, r *http.Request, style errStyle) (body []byte, ok bool) {
	// 多读一个字节用来判断「是不是超限」，避免把正好等于上限的请求误判。
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	if err != nil {
		writeRequestErr(w, http.StatusBadRequest, "读取请求体失败: "+err.Error(),
			"invalid_request_error", apierr.CodeInvalidValue, "")
		return nil, false
	}
	if len(raw) > maxRequestBody {
		msg := "请求体超过上限（32 MiB）"
		if style == errAnthropic {
			writeAnthropicErr(w, http.StatusRequestEntityTooLarge, "invalid_request_error", msg)
			return nil, false
		}
		writeRequestErr(w, http.StatusRequestEntityTooLarge, msg,
			"invalid_request_error", apierr.CodeRequestTooLarge, "")
		return nil, false
	}
	return raw, true
}
