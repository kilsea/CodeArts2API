package protocol

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"os"
)

// thinking block 的 signature。
//
// Anthropic 官方 SDK 与 Claude Code 在**多轮回传** assistant 内容时会校验
// thinking 块上的 `signature`：缺失时后续请求直接 400。而网关的上游（CodeArts
// inferhub）根本不需要这个字段，于是「第一轮能用、第二轮崩」——
// 这类故障极难排查，因为错误出现在客户端，看起来与网关无关。
//
// 因此网关自己派生一个签名：
//
//	signature = base64url(HMAC-SHA256(key, thinking 正文))
//
// 它是**稳定**的（同样的 thinking 正文恒定得到同样的签名），客户端原样回传时
// 我们也能校验。但校验失败一律**忽略**而不是拒绝：真实 Anthropic 的签名
// 我们既签不出来也验不了，拒绝只会把合法客户端挡在门外。
//
// key 的来源：
//   - 环境变量 CODEARTS_THINKING_KEY（多实例部署时设同一个值，签名才能互认）；
//   - 未设置时进程启动随机生成 —— 重启会让旧签名失效，但因为「校验失败即忽略」，
//     多轮对话仍然可用，只是校验这一步退化成 no-op。

// thinkingKey 是本进程的签名密钥。
var thinkingKey = loadThinkingKey()

// loadThinkingKey 读取或生成签名密钥。
func loadThinkingKey() []byte {
	if v := os.Getenv("CODEARTS_THINKING_KEY"); v != "" {
		sum := sha256.Sum256([]byte(v))
		return sum[:]
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// 随机源不可用时退化成固定串：签名仍然稳定（只是不再保密），
		// 总比让客户端因为缺 signature 而崩掉好。
		sum := sha256.Sum256([]byte("codearts2api-thinking-fallback"))
		return sum[:]
	}
	return b
}

// ThinkingSignature 返回 thinking 正文的稳定签名。
//
// 空内容返回空串：没有 thinking 就不该有 signature，否则客户端会把它当成
// 一个「有推理内容但没有正文」的怪块。
func ThinkingSignature(thinking string) string {
	if thinking == "" {
		return ""
	}
	mac := hmac.New(sha256.New, thinkingKey)
	mac.Write([]byte(thinking))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// VerifyThinkingSignature 校验客户端回传的签名。
//
// 返回 false 表示「不是本网关签的」——调用方应当**忽略**而不是拒绝请求：
// 客户端可能来自另一个实例（key 不同），也可能压根没经过我们。
func VerifyThinkingSignature(thinking, signature string) bool {
	want := ThinkingSignature(thinking)
	if want == "" || signature == "" {
		return false
	}
	return hmac.Equal([]byte(want), []byte(signature))
}
