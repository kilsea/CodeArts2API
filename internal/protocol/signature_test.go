package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// thinking 块的 signature 回归。
//
// 背景：/v1/messages 返回的 thinking 块没有 signature，Claude Code / 官方
// Anthropic SDK 在多轮回传 assistant 内容时会校验它，缺失导致第二轮 400。
// 典型表现是「第一轮能用、第二轮崩」，而错误出现在客户端，看起来与网关无关。

// TestThinkingSignatureStable 同样的正文必须恒定得到同样的签名。
func TestThinkingSignatureStable(t *testing.T) {
	a := ThinkingSignature("先看看这段代码")
	b := ThinkingSignature("先看看这段代码")
	if a == "" {
		t.Fatal("签名不应为空")
	}
	if a != b {
		t.Errorf("同一正文的签名不稳定：%q vs %q", a, b)
	}
	if ThinkingSignature("换一段内容") == a {
		t.Error("不同正文应得到不同签名")
	}
}

// TestThinkingSignatureEmpty 空正文不该有签名。
func TestThinkingSignatureEmpty(t *testing.T) {
	if got := ThinkingSignature(""); got != "" {
		t.Errorf("空正文的签名 = %q，期望空串", got)
	}
}

// TestVerifyThinkingSignature 自己能验自己；别人的签名一律验不过（调用方应忽略而非拒绝）。
func TestVerifyThinkingSignature(t *testing.T) {
	text := "some reasoning"
	sig := ThinkingSignature(text)
	if !VerifyThinkingSignature(text, sig) {
		t.Error("自签的签名应能验过")
	}
	if VerifyThinkingSignature(text, "not-ours") {
		t.Error("陌生签名不应验过")
	}
	if VerifyThinkingSignature(text, "") {
		t.Error("空签名不应验过")
	}
	if VerifyThinkingSignature("", sig) {
		t.Error("空正文不应验过")
	}
}

// TestFromOpenAIIncludesThinkingSignature 非流式响应里 thinking 块必须带 signature。
func TestFromOpenAIIncludesThinkingSignature(t *testing.T) {
	raw := []byte(`{"id":"x","model":"glm-5.3","choices":[{"index":0,
		"message":{"role":"assistant","content":"答案","reasoning_content":"先想想"},
		"finish_reason":"stop"}]}`)

	out := FromOpenAI(raw, "glm-5.3")
	blocks, _ := out.Payload["content"].([]map[string]any)
	var found bool
	for _, b := range blocks {
		if b["type"] != "thinking" {
			continue
		}
		found = true
		sig, _ := b["signature"].(string)
		if sig == "" {
			t.Fatal("thinking 块缺少 signature")
		}
		if !VerifyThinkingSignature("先想想", sig) {
			t.Errorf("signature 与正文不匹配: %q", sig)
		}
	}
	if !found {
		t.Fatal("响应里没有 thinking 块")
	}
}

// TestThinkingBlockWithoutSignatureIsAccepted 客户端回传**不带** signature 的
// thinking 块也必须能转换（网关忽略而不是拒绝）。
func TestThinkingBlockWithoutSignatureIsAccepted(t *testing.T) {
	m := Message{
		Role:    "assistant",
		Content: json.RawMessage(`[{"type":"thinking","thinking":"x"},{"type":"text","text":"hi"}]`),
	}
	out, err := MessageToOpenAI(m)
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("消息数 = %d，期望 1", len(out))
	}
	if got, _ := out[0]["content"].(string); got != "hi" {
		t.Errorf("content = %q，期望 hi（thinking 不该回流）", got)
	}
}

// TestThinkingBlockWithForeignSignatureIsAccepted 带「陌生签名」的 thinking 块
// 同样不能拒绝：真实 Anthropic 的签名我们既签不出也验不了。
func TestThinkingBlockWithForeignSignatureIsAccepted(t *testing.T) {
	m := Message{
		Role: "assistant",
		Content: json.RawMessage(`[{"type":"thinking","thinking":"x",` +
			`"signature":"EqQBCkYIBxgCKkAxyz"},{"type":"text","text":"hi"}]`),
	}
	if _, err := MessageToOpenAI(m); err != nil {
		t.Fatalf("陌生签名不应导致失败: %v", err)
	}
}

// TestThinkingSignatureIsOpaqueBase64 签名形状要像 Anthropic 的（客户端只透传，
// 但有些实现会做 base64 校验）。
func TestThinkingSignatureIsOpaqueBase64(t *testing.T) {
	sig := ThinkingSignature("abc")
	for _, r := range sig {
		ok := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			t.Fatalf("签名含非 base64url 字符 %q: %s", r, sig)
		}
	}
	if strings.Contains(sig, "=") {
		t.Errorf("签名不应带 padding: %s", sig)
	}
}
