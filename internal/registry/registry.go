// Package registry 维护对外暴露的模型清单与别名解析。
package registry

import (
	"sort"
	"strings"
	"sync"
	"time"

	"codearts2api/internal/codearts"
)

// Model 是一个对外可用的模型。
type Model struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Context   int    `json:"context"`
	MaxOutput int    `json:"max_output"`
	Desc      string `json:"description"`
	// Source 标记来源：upstream=上游清单，alias=别名。
	Source string `json:"source"`
	// Credit 是积分消耗倍率（如 "0.79x"）；上游未公布计费档时为空。
	Credit string `json:"credit,omitempty"`
	// StreamOnly 标记上游只支持流式：非流式会返回一个空 body 的 400。
	// 网关会自动降级为「流式 + 本地聚合」，这里只是避免每次多打一次空枪。
	StreamOnly bool `json:"stream_only,omitempty"`
}

// Fallback 是上游模型接口不可达时使用的兜底清单。
//
// 取自 2026-10 实测的 `GET /v1/model/builtin`（积分通道，benefit 通道已下线）。
// 上游不返回上下文长度，因此 Context / MaxOutput 留空（控制台显示「—」）。
var Fallback = []Model{
	{ID: "glm-5.3", Name: "GLM-5.3", Credit: "0.79x", Source: "upstream",
		Desc: "【限时体验】面向复杂编程与长程任务"},
	{ID: "GLM-5.2", Name: "GLM-5.2", Credit: "0.7x", Source: "upstream",
		Desc: "最新旗舰模型，专为长程任务打造，复杂工程稳定交付"},
	{ID: "glm-5.2-sft-harmony", Name: "GLM-5.2-ArkTS-SPARK", Credit: "0.7x", Source: "upstream",
		Desc: "基于GLM-5.2增训鸿蒙代码与开发知识"},
	{ID: "openpangu-2.0-pro", Name: "OpenPangu-2.0-Pro", Credit: "0.7x", Source: "upstream",
		Desc: "最新旗舰模型，专为长程任务打通，复杂工程稳定交付"},
	{ID: "openpangu-2.0-flash", Name: "OpenPangu-2.0-Flash", Credit: "0.32x", Source: "upstream",
		Desc: "均衡推理效果与性能"},
	{ID: "deepseek-v4.1-flash", Name: "DeepSeek-V4.1-Flash", Credit: "0.18x", Source: "upstream",
		Desc: "【限时体验】能力更高, 速度更快"},
	{ID: "Qwen3-VL-235B", Name: "Qwen3-VL-235B", Source: "upstream",
		Desc: "多模态模型"},
	{ID: "kimi-k2.6-vl", Name: "Kimi-K2.6-VL", Source: "upstream",
		Desc: "编程性能突出"},
}

// legacyAliases 是「旧客户端兼容别名」，指向 CodeArts 上游模型 ID。
//
// 2026-10 免费通道下线后，原来的 glm-5.3-flash / deepseek-v4-pro-0813 /
// deepseek-v4-flash-0731 都不存在了，这里统一重映射到当前可用的积分模型。
//
// **默认不生效**：`gpt-4o` 实际由 GLM 回答、`claude-sonnet-4` 实际由 GLM-5.2
// 回答，用户以为在用 A 实际在跑 B 并真实扣积分 —— 这是「静默烧钱」，
// 与 README「无法识别的模型名直接返回 404」的承诺相反。
// 现在它们只在控制台显式打开「兼容别名」后生效（store.Settings.ModelFallback），
// 且每次命中都会回写 X-Gateway-Model-Fallback 响应头并记一条告警日志。
//
// 想要长期使用这些名字，请改在设置里配 model_aliases（自定义别名），
// 那是用户显式声明过的映射，不需要任何「回落」语义。
var legacyAliases = map[string]string{
	// 通用档 → glm-5.3
	"gpt-4o":            "glm-5.3",
	"gpt-4o-mini":       "glm-5.3",
	"gpt-4":             "glm-5.3",
	"gpt-4-turbo":       "glm-5.3",
	"gpt-3.5-turbo":     "glm-5.3",
	"claude-3-haiku":    "glm-5.3",
	"glm-4":             "glm-5.3",
	"codearts":          "glm-5.3",
	"codearts-flash":    "glm-5.3",
	"deepseek-chat":     "deepseek-v4.1-flash",
	"deepseek-reasoner": "GLM-5.2",
	// 旗舰档 → GLM-5.2
	"gpt-5":             "GLM-5.2",
	"claude-3-5-sonnet": "GLM-5.2",
	"claude-sonnet-4":   "GLM-5.2",
	"codearts-pro":      "GLM-5.2",
}

// LegacyAliasNames 返回全部内置兼容别名的名字（升序），供文档与控制台展示。
func LegacyAliasNames() []string {
	out := make([]string, 0, len(legacyAliases))
	for k := range legacyAliases {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Registry 是可热更新的模型清单。
type Registry struct {
	mu      sync.RWMutex
	models  []Model
	updated time.Time
}

// New 构造注册表，初始使用兜底清单。
func New() *Registry {
	r := &Registry{}
	r.models = append([]Model(nil), Fallback...)
	return r
}

// Replace 用上游返回的模型清单整体替换当前清单。
//
// 上游清单是唯一的真相来源：拿不到就是拿不到，不做「部分合并」——
// 否则已下线的模型会一直残留在 /v1/models 里，客户端拿到后必然报错。
func (r *Registry) Replace(models []Model) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.models = append([]Model(nil), models...)
	r.updated = time.Now()
}

// List 返回当前模型清单（含自定义别名）。
func (r *Registry) List(custom map[string]string) []Model {
	r.mu.RLock()
	base := append([]Model(nil), r.models...)
	r.mu.RUnlock()

	seen := map[string]bool{}
	out := make([]Model, 0, len(base)+len(custom))
	for _, m := range base {
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	keys := make([]string, 0, len(custom))
	for k := range custom {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, alias := range keys {
		target := custom[alias]
		m, ok := lookup(base, target)
		if !ok {
			// 目标模型不在当前清单里：这个别名解析不了，就不要在 /v1/models 里宣传它。
			continue
		}
		if seen[alias] {
			continue
		}
		seen[alias] = true
		m.ID = alias
		m.Name = alias + " → " + m.Name
		m.Source = "alias"
		out = append(out, m)
	}
	return out
}

// MarkStreamOnly 记录某模型只支持流式（上游非流式会返回空 body 的 400）。
//
// 实测 glm-5.3 就是这样；网关会自动降级为「流式 + 本地聚合」，
// 这里记下来只是省掉每次多打一次空枪。
func (r *Registry) MarkStreamOnly(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.models {
		if strings.EqualFold(r.models[i].ID, id) {
			r.models[i].StreamOnly = true
			return
		}
	}
}

// Updated 返回最近一次从上游刷新模型清单的时间。
func (r *Registry) Updated() time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.updated
}

// Resolve 把用户传入的模型名解析为上游模型。
//
// 依次尝试：精确 ID → 自定义别名 → 大小写无关匹配。
// 全部失败时返回 false —— 调用方必须报错，**绝不静默落到别的模型**：
// 现在每个模型都真实扣积分，猜错模型等于替用户乱花钱。
//
// 内置的 legacyAliases 不在这里解析，见 ResolveLegacy。
func (r *Registry) Resolve(name string, custom map[string]string) (Model, bool) {
	r.mu.RLock()
	base := append([]Model(nil), r.models...)
	r.mu.RUnlock()

	n := strings.TrimSpace(name)
	if n == "" {
		return defaultModel(base)
	}
	if m, ok := lookup(base, n); ok {
		return m, true
	}
	if target, ok := custom[n]; ok {
		return aliasModel(base, target)
	}
	for _, m := range base {
		if strings.EqualFold(m.ID, n) {
			return m, true
		}
	}
	return Model{}, false
}

// ResolveLegacy 解析内置兼容别名（gpt-4o → glm-5.3 等）。
//
// **只在设置里显式打开 ModelFallback 时才能被调用**：这些名字会让用户
// 以为在用 A 模型，实际跑的是 B 模型并真实扣积分。调用方必须把命中结果
// 回写到响应头 X-Gateway-Model-Fallback 并记告警日志，不能静默生效。
func (r *Registry) ResolveLegacy(name string, custom map[string]string) (Model, bool) {
	r.mu.RLock()
	base := append([]Model(nil), r.models...)
	r.mu.RUnlock()

	// 自定义别名优先：用户显式配的映射比内置兼容表更权威。
	if target, ok := custom[strings.TrimSpace(name)]; ok {
		return aliasModel(base, target)
	}
	if target, ok := legacyAliases[strings.ToLower(strings.TrimSpace(name))]; ok {
		return aliasModel(base, target)
	}
	return Model{}, false
}

// defaultModel 优先返回 codearts.DefaultModel，其次返回清单里的第一个模型。
func defaultModel(list []Model) (Model, bool) {
	if m, ok := lookup(list, codearts.DefaultModel); ok {
		return m, true
	}
	if len(list) > 0 {
		return list[0], true
	}
	if m, ok := lookup(Fallback, codearts.DefaultModel); ok {
		return m, true
	}
	return Model{}, false
}

// aliasModel 把别名目标解析为清单里的模型。
//
// 目标不在清单里就返回 false —— 别名指向一个已下线的模型时，
// 报 404 让用户改别名，而不是把死模型名发到上游换回 4005。
func aliasModel(list []Model, target string) (Model, bool) {
	m, ok := lookup(list, target)
	if !ok {
		return Model{}, false
	}
	m.Source = "alias"
	return m, true
}

func lookup(list []Model, id string) (Model, bool) {
	for _, m := range list {
		if strings.EqualFold(m.ID, id) {
			return m, true
		}
	}
	return Model{}, false
}

// FromBuiltin 把上游模型清单转换为 registry.Model。
//
// 上游返回的 credit[].ratio_display 就是积分消耗倍率，透传给控制台展示；
// 图片模型没有计费档，倍率留空。
func FromBuiltin(models []codearts.BuiltinModel) []Model {
	out := make([]Model, 0, len(models))
	for _, m := range models {
		if !m.Enable {
			continue
		}
		desc := m.ModelCategory
		if m.ModelDesc != "" {
			desc = m.ModelDesc
		}
		out = append(out, Model{
			ID:     m.ModelID,
			Name:   m.ModelName,
			Desc:   desc,
			Source: "upstream",
			Credit: m.CreditRatio(),
		})
	}
	return out
}
