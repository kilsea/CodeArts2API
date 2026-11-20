package server

import (
	"log"
	"net/http"
	"strings"

	"codearts2api/internal/apierr"
	"codearts2api/internal/registry"
	"codearts2api/internal/store"
)

// resolveModel 把请求里的模型名解析为上游模型。
//
// 优先级：强制模型 → 请求里的 model → 控制台默认模型。
//
// 返回值 fallback 表示「这次解析用到了内置兼容别名」：即客户端要的模型
// 与真正执行的模型不是同一个。调用方**必须**把它暴露出去（响应头
// X-Gateway-Model-Fallback + 告警日志），否则就是静默换模型烧积分。
//
// 解析失败返回 *apierr.ModelNotFound（对外 404）—— 每个模型都真实扣积分，
// 绝不静默回落到别的模型。
func (s *Server) resolveModel(settings store.Settings, requested string) (registry.Model, bool, error) {
	name := strings.TrimSpace(requested)
	if settings.ForceModel != "" {
		name = settings.ForceModel
	}
	if name == "" {
		name = settings.DefaultModel
	}
	if m, ok := s.Reg.Resolve(name, settings.ModelAliases); ok {
		return m, false, nil
	}
	// 内置兼容别名：只有显式打开 ModelFallback 才允许，且必须留痕。
	if settings.ModelFallback {
		if m, ok := s.Reg.ResolveLegacy(name, settings.ModelAliases); ok {
			log.Printf("[model-fallback] 请求的模型 %q 不是真实模型，已改用 %q 执行（可在设置里关闭「兼容别名」）", name, m.ID)
			return m, true, nil
		}
	}
	return registry.Model{}, false, &apierr.ModelNotFound{
		Name:      name,
		Available: s.availableModelIDs(settings),
	}
}

// announceFallback 在响应头里声明「实际执行的模型与请求的不一致」。
//
// 必须在 WriteHeader 之前调用。头部是唯一不会被客户端忽略的告知方式：
// 很多客户端根本不看响应体里的 model 字段。
func announceFallback(w http.ResponseWriter, requested, actual string) {
	if requested == actual {
		return
	}
	w.Header().Set("X-Gateway-Model-Fallback", actual)
	w.Header().Set("X-Gateway-Model-Requested", requested)
}

// availableModelIDs 返回当前可用模型 ID，用于错误提示。
func (s *Server) availableModelIDs(settings store.Settings) []string {
	list := s.Reg.List(settings.ModelAliases)
	ids := make([]string, 0, len(list))
	for _, m := range list {
		ids = append(ids, m.ID)
	}
	return ids
}
