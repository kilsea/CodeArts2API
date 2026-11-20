package apierr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	"codearts2api/internal/codearts"
)

// TestClassify 锁定内部错误到日志标签的映射顺序。
//
// 关键不变量：moderation 是 client_request 的子集，必须先于它判定，
// 否则审核拒绝会被记成普通「参数错误」，控制台上看不到独立的审核计数。
func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"内容审核（403 带码）", &codearts.UpstreamError{
			Status:  http.StatusForbidden,
			Message: `403: {"error_code":"ModelArts.81011","error":{"message":"Input image May contain sensitive information"}}`,
		}, "moderation"},
		{"内容审核（纯模板话术）", &codearts.UpstreamError{
			Status:  http.StatusForbidden,
			Message: "403: 抱歉，我无法提供关于您提问内容的建议。",
		}, "moderation"},
		{"参数越界（400）", &codearts.UpstreamError{
			Status:  http.StatusBadRequest,
			Message: "400: The request param is invalid",
		}, "client_request"},
		{"模态不符（406）", &codearts.UpstreamError{
			Status:  http.StatusNotAcceptable,
			Message: "406: InferHub.001001020.406 The request model is not multimodal",
		}, "client_request"},
		{"凭据失效", &codearts.CredentialsError{Account: "a", Reason: "已过期"}, "credentials"},
		{"账号池为空", NoAccount, "no_account"},
		{"上游 429", &codearts.UpstreamError{Status: http.StatusTooManyRequests, Message: "slow down"}, "transient"},
		{"上游 503", &codearts.UpstreamError{Status: http.StatusServiceUnavailable}, "transient"},
		{"并发上限（400 + TM.00001041）", &codearts.UpstreamError{
			Status:  http.StatusBadRequest,
			Message: "TM.00001041 concurrent limit",
		}, "transient"},
		{"上游 402（其他业务错误）", &codearts.UpstreamError{Status: 402, Message: "quota"}, "upstream"},
		{"客户端断开", context.Canceled, "client_abort"},
		{"网络中断（unexpected EOF）", io.ErrUnexpectedEOF, "network"},
		{"单次请求超时", context.DeadlineExceeded, "network"},
		{"网关内部错误", errors.New("json marshal failed"), "internal"},
	}
	for _, c := range cases {
		got := Classify(c.err)
		if got != c.want {
			t.Errorf("%s: Classify = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestClassifyUnwrapsWrapped 包装后的错误仍要按根因分类。
func TestClassifyUnwrapsWrapped(t *testing.T) {
	mod := &codearts.UpstreamError{
		Status:  http.StatusForbidden,
		Message: "403: ModelArts.81011 Input image May contain sensitive information",
	}
	if got := Classify(fmt.Errorf("chat 失败: %w", mod)); got != "moderation" {
		t.Errorf("包装后的审核拒绝 = %q, want moderation", got)
	}
	creds := &codearts.CredentialsError{Account: "a", Reason: "r"}
	if got := Classify(fmt.Errorf("上层: %w", creds)); got != "credentials" {
		t.Errorf("包装后的凭据失效 = %q, want credentials", got)
	}
}

// TestClassifyModerationBeforeClientRequest 顺序回归：
// moderation 必须先于 client_request 判定，否则被吞成 client_request。
func TestClassifyModerationBeforeClientRequest(t *testing.T) {
	e := &codearts.UpstreamError{
		Status:  http.StatusForbidden,
		Message: "403: ModelArts.81011 我无法提供关于您提问内容的建议",
	}
	if Classify(e) != "moderation" {
		t.Error("审核拒绝必须分类为 moderation，不能被 client_request 吞掉")
	}
}

// TestClassifyGatewayRequestErrors 网关自己判定的请求侧错误要归到 client_request。
//
// 回归背景：这些错误以前一律落到 internal，控制台按「参数」筛选时看不到
// 「temperature 越界」「模型不存在」「background 不支持」这类明确的请求侧问题，
// 日志里显示「内部」还会把排查方向带偏。
func TestClassifyGatewayRequestErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"参数越界", &InvalidParamError{Param: "temperature", Reason: "temperature 必须在 0 到 2 之间"}, "client_request"},
		{"不支持字段", &InvalidParamError{Param: "background", Reason: "background 暂不支持", Code: CodeUnsupportedParam}, "client_request"},
		{"模型不存在", &ModelNotFound{Name: "gpt-4o"}, "client_request"},
		{"响应 id 查不到", &ResponseNotFound{ID: "resp_x", Param: "previous_response_id"}, "client_request"},
	}
	for _, c := range cases {
		if got := Classify(c.err); got != c.want {
			t.Errorf("%s: Classify = %q，期望 %q", c.name, got, c.want)
		}
	}
}
