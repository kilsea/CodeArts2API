package codearts

import "testing"

// TestUpstreamErrorTextExtractsMessage 覆盖「上游错误体是嵌套 JSON」。
//
// 以前整段 JSON 塞进 message，客户端看到的是
// `上游 HTTP 400: {"error":{"code":…,"span_id":…}}` ——
// 一行 JSON 里真正有用的只有中间那句中文。
func TestUpstreamErrorTextExtractsMessage(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "嵌套 error.message",
			raw:  `{"error":{"code":"ModelArts.81001","message":"未正常接收到prompt参数。","type":"BadRequest"},"span_id":"abc"}`,
			want: "未正常接收到prompt参数。",
		},
		{
			name: "顶层 error_msg",
			raw:  `{"error_code":"TM.00001041","error_msg":"concurrency exceeded"}`,
			want: "concurrency exceeded",
		},
		{
			name: "顶层 message",
			raw:  `{"message":"bad request"}`,
			want: "bad request",
		},
		{
			name: "非 JSON 原文",
			raw:  `plain text error`,
			want: "plain text error",
		},
	}
	for _, c := range cases {
		if got := upstreamErrorText([]byte(c.raw)); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// TestUpstreamErrorTextKeepsEmptyForEmptyBody 是 stream-only 检测的命门：
// IsEmptyRejection 靠「message 是空串」判断「模型只支持流式」。
// 如果这里把空 body 变成 "{}" 之类的占位符，能力探测就全错了。
func TestUpstreamErrorTextKeepsEmptyForEmptyBody(t *testing.T) {
	for _, raw := range []string{"", "   ", "\n\t "} {
		if got := upstreamErrorText([]byte(raw)); got != "" {
			t.Errorf("upstreamErrorText(%q) = %q，期望空串", raw, got)
		}
	}
	// 空 JSON 对象不是「空 body」：上游确实说了点什么（只是没内容），
	// 不能当成只支持流式的信号。
	if got := upstreamErrorText([]byte(`{}`)); got != "{}" {
		t.Errorf("upstreamErrorText(\"{}\") = %q，期望保留原文", got)
	}
}

// TestEmptyRejectionStillWorks 确认改写后 IsEmptyRejection 的语义没变。
func TestEmptyRejectionStillWorks(t *testing.T) {
	empty := &UpstreamError{Status: 400, Message: upstreamErrorText(nil)}
	if !IsEmptyRejection(empty) {
		t.Error("空 body 应判定为 empty rejection")
	}
	withText := &UpstreamError{Status: 400, Message: upstreamErrorText([]byte(`{"error":{"message":"x"}}`))}
	if IsEmptyRejection(withText) {
		t.Error("有正文时不应判定为 empty rejection")
	}
}
