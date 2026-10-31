// buildinfo 编译期注入的构建标识（打包/更新时用于核对"正在跑的是不是新版本"）。
//
// 直接 go build / go run 时保持 dev 默认值；打包工具（bma-app.ps1 pack）用
// -ldflags "-X ...buildinfo.Version=..." 注入版本、提交号与构建时间。
// 对外经 /api/health 的 build 字段暴露，更新脚本据此确认切换是否生效
//（2026-10-30 事故：旧进程占着端口、新进程 bind 失败退出，健康检查却仍 200）。
package buildinfo

import "runtime"

var (
	// Version 打包版本（形如 20261030-142530-ab12cd3）。
	Version = "dev"
	// Commit git 短提交号（无 git 时 unknown）。
	Commit = "unknown"
	// BuiltAt 构建时间（RFC3339）。
	BuiltAt = "unknown"
)

// Info 返回可序列化的构建信息。
func Info() map[string]string {
	return map[string]string{
		"version":  Version,
		"commit":   Commit,
		"built_at": BuiltAt,
		"go":       runtime.Version(),
	}
}
