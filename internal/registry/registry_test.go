package registry

import (
	"testing"

	"codearts2api/internal/codearts"
)

// TestResolveOnlyKnownModels 锁定「未知模型名必须解析失败」这条底线。
//
// 免费权益通道下线后每个模型都真实扣积分，静默回落到某个默认模型
// 等于替用户乱花钱，因此这里逐条确认解析路径不会「猜」。
func TestResolveOnlyKnownModels(t *testing.T) {
	r := New()
	r.Replace(FromBuiltin([]codearts.BuiltinModel{
		{ModelID: "glm-5.3", ModelName: "GLM-5.3", Enable: true,
			Credit: []codearts.CreditTier{{RatioDisplay: "0.79x"}}},
		{ModelID: "GLM-5.2", ModelName: "GLM-5.2", Enable: true,
			Credit: []codearts.CreditTier{{RatioDisplay: "0.7x"}}},
		{ModelID: "deepseek-v4.1-flash", ModelName: "DeepSeek-V4.1-Flash", Enable: true,
			Credit: []codearts.CreditTier{{RatioDisplay: "0.18x"}}},
		{ModelID: "disabled-model", ModelName: "disabled-model", Enable: false},
	}))

	cases := []struct {
		query  string
		wantID string
		wantOK bool
	}{
		{"glm-5.3", "glm-5.3", true},
		{"GLM-5.3", "glm-5.3", true}, // 大小写无关
		{"glm-5.3-flash", "", false}, // 已下线的旧模型名
		{"deepseek-v4-pro-0813", "", false},
		// 内置兼容别名**默认不解析**：gpt-4o 实际由 GLM 回答，静默换模型
		// 等于替用户乱花钱。要它们生效必须显式打开 ModelFallback，
		// 走 ResolveLegacy（见下一个用例）。
		{"gpt-4o", "", false},
		{"gpt-5", "", false},
		{"deepseek-chat", "", false},
		{"disabled-model", "", false}, // 上游标记 disabled 的不暴露
		{"glm", "", false},            // 不做模糊匹配
		{"", "glm-5.3", true},         // 未指定模型 → 默认模型
	}
	for _, c := range cases {
		m, ok := r.Resolve(c.query, nil)
		if ok != c.wantOK {
			t.Errorf("Resolve(%q) ok = %v, want %v", c.query, ok, c.wantOK)
			continue
		}
		if ok && m.ID != c.wantID {
			t.Errorf("Resolve(%q).ID = %q, want %q", c.query, m.ID, c.wantID)
		}
	}
}

// TestCreditRatioExposed 确认积分倍率会透传到模型清单。
func TestCreditRatioExposed(t *testing.T) {
	r := New()
	r.Replace(FromBuiltin([]codearts.BuiltinModel{
		{ModelID: "glm-5.3", Enable: true, Credit: []codearts.CreditTier{{RatioDisplay: "0.79x"}}},
	}))
	list := r.List(nil)
	var found bool
	for _, m := range list {
		if m.ID == "glm-5.3" {
			found = true
			if m.Credit != "0.79x" {
				t.Errorf("credit = %q, want 0.79x", m.Credit)
			}
		}
	}
	if !found {
		t.Fatal("模型清单里没有 glm-5.3")
	}
}

// TestDisabledBuiltinSkipped 确认上游标记为 disabled 的模型不会暴露出去。
func TestDisabledBuiltinSkipped(t *testing.T) {
	r := New()
	r.Replace(FromBuiltin([]codearts.BuiltinModel{
		{ModelID: "gone", Enable: false},
	}))
	for _, m := range r.List(nil) {
		if m.ID == "gone" {
			t.Error("disabled 的模型不应出现在清单里")
		}
	}
}

// TestMarkStreamOnly 确认流式标记能落到模型上。
func TestMarkStreamOnly(t *testing.T) {
	r := New()
	r.Replace(FromBuiltin([]codearts.BuiltinModel{{ModelID: "glm-5.3", Enable: true}}))
	if m, ok := r.Resolve("glm-5.3", nil); !ok || m.StreamOnly {
		t.Fatal("初始不应是 stream-only")
	}
	r.MarkStreamOnly("glm-5.3")
	if m, ok := r.Resolve("glm-5.3", nil); !ok || !m.StreamOnly {
		t.Error("MarkStreamOnly 后应标记为 stream-only")
	}
}

// TestCustomAlias 确认自定义别名解析到目标模型。
func TestCustomAlias(t *testing.T) {
	r := New()
	r.Replace(FromBuiltin([]codearts.BuiltinModel{{ModelID: "glm-5.3", Enable: true}}))

	m, ok := r.Resolve("my-cheap", map[string]string{"my-cheap": "glm-5.3"})
	if !ok || m.ID != "glm-5.3" {
		t.Fatalf("自定义别名解析错误: %+v ok=%v", m, ok)
	}
	if m.Source != "alias" {
		t.Errorf("别名的 Source 应为 alias，得到 %q", m.Source)
	}

	// 别名指向一个不存在的模型：解析失败，而不是把死模型名发给上游。
	if _, ok := r.Resolve("my-dead", map[string]string{"my-dead": "glm-5.3-flash"}); ok {
		t.Error("别名指向已下线的模型时应解析失败")
	}
	// 也不应该在 /v1/models 里宣传它。
	for _, x := range r.List(map[string]string{"my-dead": "glm-5.3-flash"}) {
		if x.ID == "my-dead" {
			t.Error("目标不存在的别名不应出现在模型清单里")
		}
	}
}

// TestResolveLegacyAliases 确认内置兼容别名只能显式解析（ResolveLegacy），
// 且解析结果指向当前真实存在的模型。
//
// 回归背景：gpt-4o 曾经默认被解析成 glm-5.3，客户端以为在用 GPT-4o，
// 实际由 GLM 回答并真实扣积分，而 README 又声称「无法识别的模型名直接 404」。
func TestResolveLegacyAliases(t *testing.T) {
	r := New()
	r.Replace(FromBuiltin([]codearts.BuiltinModel{
		{ModelID: "glm-5.3", Enable: true},
		{ModelID: "GLM-5.2", Enable: true},
	}))

	if _, ok := r.Resolve("gpt-4o", nil); ok {
		t.Fatal("Resolve 不应解析内置兼容别名")
	}
	m, ok := r.ResolveLegacy("GPT-4o", nil)
	if !ok || m.ID != "glm-5.3" {
		t.Fatalf("ResolveLegacy(gpt-4o) = %+v ok=%v，期望 glm-5.3", m, ok)
	}
	// 别名指向已下线的模型时同样解析失败，不能把死模型名发给上游。
	if _, ok := r.ResolveLegacy("nope", nil); ok {
		t.Error("未知名字应解析失败")
	}
	if names := LegacyAliasNames(); len(names) == 0 {
		t.Error("LegacyAliasNames 为空")
	}
}

// TestFallbackModelsAreCurrent 确认兜底清单里没有已下线的免费通道模型。
func TestFallbackModelsAreCurrent(t *testing.T) {
	if len(Fallback) == 0 {
		t.Fatal("兜底清单不能为空")
	}
	dead := map[string]bool{
		"glm-5.3-flash":          true,
		"deepseek-v4-flash-0731": true,
		"deepseek-v4-pro-0813":   true,
	}
	for _, m := range Fallback {
		if dead[m.ID] {
			t.Errorf("兜底清单仍包含已下线的模型 %q", m.ID)
		}
		if m.Credit == "" {
			t.Logf("提示：%q 没有积分倍率（上游未公布计费档）", m.ID)
		}
	}
}
