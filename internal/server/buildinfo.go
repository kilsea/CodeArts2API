package server

import "runtime"

// 构建信息。
//
// 为什么要在 /health 和 /admin/api/state 里暴露：升级后第一件事是确认
// 「现在跑的到底是不是我刚编的那个二进制」。之前只能靠 exe 修改时间、
// 或者试探某个新接口是否 404 来推断，非常不可靠。
//
// 由 main 在启动时调用 SetBuildInfo 注入（值来自 -ldflags -X）。
var (
	BuildVersion = "dev"
	BuildCommit  = ""
	BuildTime    = ""
)

// SetBuildInfo 注入构建信息（main 调用）。
func SetBuildInfo(version, commit, buildTime string) {
	if version != "" {
		BuildVersion = version
	}
	BuildCommit = commit
	BuildTime = buildTime
}

// buildInfo 返回对外暴露的构建信息。
func buildInfo() map[string]any {
	return map[string]any{
		"version":    BuildVersion,
		"commit":     BuildCommit,
		"build_time": BuildTime,
		"go":         runtime.Version(),
	}
}
