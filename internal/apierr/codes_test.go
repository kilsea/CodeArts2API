package apierr

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codearts2api/internal/codearts"
)

// 本文件是「上游错误码 → 对外规范码」映射的回归。
//
// 映射表的取值与 docs/UPSTREAM.md §8「错误码对照」一一对应：改这里必须
// 同时改那份文档，否则文档会慢慢变成谎话。

// TestUpstreamCodeMapping 逐条钉住映射表。
func TestUpstreamCodeMapping(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		msg       string
		wantCode  string
		wantParam string
		wantType  string
	}{
		{
			name:     "并发会话上限",
			status:   http.StatusBadRequest,
			msg:      `{"error_code":"TM.00001041","error_msg":"session limit"}`,
			wantCode: CodeConcurrency,
			wantType: "rate_limit_error",
		},
		{
			name:     "参数越界",
			status:   http.StatusBadRequest,
			msg:      "InferHub.001001005.400: temperature should be between 0 and 2",
			wantCode: CodeInvalidValue,
			wantType: "invalid_request_error",
		},
		{
			name:      "模型名与请求头不一致",
			status:    http.StatusNotAcceptable,
			msg:       "InferHub.001001011.406: The model not equals requestHeader's Model-Id",
			wantCode:  CodeModelNameMismatch,
			wantParam: "model",
			wantType:  "invalid_request_error",
		},
		{
			name:      "纯文本模型收到图片",
			status:    http.StatusNotAcceptable,
			msg:       "InferHub.001001020.406: The request model is not multimodal",
			wantCode:  CodeModelNotMultimodal,
			wantParam: "messages",
			wantType:  "invalid_request_error",
		},
		{
			name:     "内容审核",
			status:   http.StatusForbidden,
			msg:      "ModelArts.81011: Input image May contain sensitive information",
			wantCode: CodeContentPolicy,
			wantType: "invalid_request_error",
		},
		{
			name:     "签名失效",
			status:   http.StatusBadRequest,
			msg:      "APIG.0301: signature mismatch",
			wantCode: CodeCredentials,
			wantType: "invalid_request_error",
		},
		{
			name:      "旧通道不支持该模型",
			status:    http.StatusBadRequest,
			msg:       "4005: invalid channel, group not found",
			wantCode:  CodeModelNotFound,
			wantParam: "model",
			wantType:  "invalid_request_error",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := From(&codearts.UpstreamError{Status: c.status, Message: c.msg})
			if e.Code != c.wantCode {
				t.Errorf("code = %q，期望 %q", e.Code, c.wantCode)
			}
			if c.wantParam != "" && e.Param != c.wantParam {
				t.Errorf("param = %q，期望 %q", e.Param, c.wantParam)
			}
			if e.OpenAI != c.wantType {
				t.Errorf("type = %q，期望 %q", e.OpenAI, c.wantType)
			}
			// 能映射的错误不该再把上游原文塞给客户端：已经翻译成人话了。
			if e.Upstream != "" {
				t.Errorf("已映射的错误不应带 upstream_error，得到 %q", e.Upstream)
			}
		})
	}
}

// TestUpstreamUnmappedKeepsOriginal 映射不出来的错误必须保留原文。
func TestUpstreamUnmappedKeepsOriginal(t *testing.T) {
	raw := `{"error_code":"InferHub.009999999.503","error_msg":"something odd happened"}`
	e := From(&codearts.UpstreamError{Status: http.StatusServiceUnavailable, Message: raw})
	if e.Code != CodeUpstream {
		t.Errorf("code = %q，期望 %q", e.Code, CodeUpstream)
	}
	if e.Status != http.StatusBadGateway {
		t.Errorf("status = %d，期望 502", e.Status)
	}
	if e.Upstream != raw {
		t.Errorf("upstream_error = %q，期望保留原文", e.Upstream)
	}
}

// TestClientSideNeverReturns200 锁定「客户端侧问题必须 4xx，不得 200」。
//
// 回归背景：上游用 HTTP 200 + error_code 表达业务错误，早期实现把
// Status 固定写成 200，于是「参数非法」在客户端看来是一次成功。
func TestClientSideNeverReturns200(t *testing.T) {
	errs := []error{
		&codearts.UpstreamError{Status: http.StatusOK, Message: "4005: unsupported model"},
		&codearts.UpstreamError{Status: http.StatusBadRequest, Message: "InferHub.001001005.400: bad"},
		&InvalidRoleError{Index: 0, Role: "critic"},
		&ModelNotFound{Name: "gpt-4o"},
		&RateLimited{},
	}
	for _, err := range errs {
		e := From(err)
		if e.Status == http.StatusOK || e.Status < 400 {
			t.Errorf("%v 的对外状态码 = %d，期望 >= 400", err, e.Status)
		}
	}
}

// TestEveryMappedCodeIsDocumented 防止有人往映射表里加码却不改文档。
//
// 判据很粗但有效：codes.go 里每个 upstreamCodes 条目的 marker 都必须
// 在 docs/UPSTREAM.md 里出现过。漏改文档会立刻失败。
func TestEveryMappedCodeIsDocumented(t *testing.T) {
	doc, err := readUpstreamDoc()
	if err != nil {
		t.Skipf("跳过：读不到 docs/UPSTREAM.md（%v）", err)
	}
	for _, m := range upstreamCodes {
		marker := strings.TrimSuffix(m.marker, ":")
		if !strings.Contains(doc, marker) {
			t.Errorf("映射表里的 %q 没有出现在 docs/UPSTREAM.md §8，请同步文档", marker)
		}
	}
}

// TestCodeStability 钉住几个最常被客户端引用的码值 —— 改它们等于破坏契约。
func TestCodeStability(t *testing.T) {
	want := map[string]string{
		CodeInvalidRole:        "invalid_role",
		CodeModelNotFound:      "model_not_found",
		CodeInvalidValue:       "invalid_value",
		CodeContentPolicy:      "content_policy_violation",
		CodeConcurrency:        "concurrency_limit_exceeded",
		CodeRateLimited:        "rate_limit_exceeded",
		CodeRequestTooLarge:    "request_too_large",
		CodeModelNotMultimodal: "model_not_multimodal",
	}
	for got, expect := range want {
		if got != expect {
			t.Errorf("错误码 %q 被改成了 %q，这会破坏客户端的兼容性", expect, got)
		}
	}
}

// TestInvalidRoleErrorMessage 错误文案必须能直接指向问题所在。
func TestInvalidRoleErrorMessage(t *testing.T) {
	e := &InvalidRoleError{Index: 2, Role: "critic"}
	if got, want := e.Error(), "unsupported role: critic"; got != want {
		t.Errorf("Error() = %q，期望 %q", got, want)
	}
	if got, want := e.APIParam(), "messages[2].role"; got != want {
		t.Errorf("APIParam() = %q，期望 %q", got, want)
	}
	// 整体结构问题（Index < 0）指向 messages 本身。
	bad := &InvalidRoleError{Index: -1, Reason: "messages 必须是对象数组"}
	if got, want := bad.APIParam(), "messages"; got != want {
		t.Errorf("APIParam() = %q，期望 %q", got, want)
	}
}

// TestWrappedErrorsStillCoded 错误被 fmt.Errorf("%w") 包过一层后仍要保留规范码。
func TestWrappedErrorsStillCoded(t *testing.T) {
	err := fmt.Errorf("handler: %w", &InvalidRoleError{Index: 1, Role: "x"})
	e := From(err)
	if e.Code != CodeInvalidRole || e.Status != http.StatusBadRequest {
		t.Errorf("包装后的错误丢失了语义: %+v", e)
	}
}

// readUpstreamDoc 读取 docs/UPSTREAM.md（测试工作目录是包目录，需要回退两级）。
func readUpstreamDoc() (string, error) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "UPSTREAM.md"))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
