// Package plugins 实现热插拔插件范式（设计文档《插件系统设计 v2》）。
//
// 插件本质是「一组 tool.Tool 的打包 + 生命周期管理」，唯一执行面是
// domain/tool.Registry：插件工具注册进去后，ReAct 循环每轮现取 Schema，
// 下一轮迭代即对 Agent 可见（热插拔）。
//
// 三种插件形态（Kind）：
//   - builtin：编译进二进制的进程内 Go 插件（实现 Plugin 接口，程序化注册）；
//   - mcp：外部 MCP server（stdio 子进程或 streamable HTTP），经 mcpbridge 桥接；
//   - bundle：Claude/Codex 插件目录包（.claude-plugin/plugin.json + .mcp.json +
//     skills/*/SKILL.md），由 bundle 包展开为若干 mcp 插件与 skill 条目；
//   - service：Docker 化长驻 HTTP 服务（无 MCP 工具，如 Open Design UI 设计台），
//     只挂生命周期（docker run -d / rm -f + 健康探针），与 mcp 插件一样热插拔。
//
// 状态机：registered → initialized → running → stopped / degraded。
package plugins

import (
	"context" // 上下文，用于 Init/Start/Stop 生命周期与工具执行取消
	"log/slog" // slog 结构化日志
	"os"       // 环境变量检查（RequiresEnv）

	"github.com/blockmemory/agent/backend/internal/domain/tool" // 插件工具挂载目标
)

// Kind 插件形态。
type Kind string

// 插件形态常量。
const (
	// KindBuiltin 编译进二进制的进程内 Go 插件。
	KindBuiltin Kind = "builtin"
	// KindMCP 外部 MCP server（stdio 子进程或远程 HTTP）。
	KindMCP Kind = "mcp"
	// KindBundle Claude/Codex 插件目录包（加载时展开为 mcp 插件 + skill 条目）。
	KindBundle Kind = "bundle"
	// KindService Docker 化长驻 HTTP 服务（无工具注入，仅生命周期管理）。
	KindService Kind = "service"
)

// State 插件实例生命周期状态。
type State string

// 状态机常量：registered → initialized → running → stopped / degraded。
const (
	// StateRegistered 已创建实例但未初始化（含 enable 失败回滚后的驻留态）。
	StateRegistered State = "registered"
	// StateInitialized Init 成功、Start 尚未成功。
	StateInitialized State = "initialized"
	// StateRunning 已 Start 且工具已注册，Agent 可见。
	StateRunning State = "running"
	// StateStopped 已被 Disable / 关闭（工具已摘除）。
	StateStopped State = "stopped"
	// StateDegraded 连接断开/子进程退出，工具已自动摘除，等待退避重连。
	StateDegraded State = "degraded"
)

// Manifest 插件元数据（Claude plugin.json 的超集）。
type Manifest struct {
	// ID 唯一标识，如 "web_search"、"bundle/dir/server"。
	ID string
	// Name 展示名。
	Name string
	// Version 版本号。
	Version string
	// Description 一句话功能描述。
	Description string
	// Kind 插件形态。
	Kind Kind
	// RequiresEnv 依赖的环境变量名；缺失时 Enable 拒绝并给出原因。
	RequiresEnv []string
	// ToolNames 声明提供的工具名（mcp 插件连接后按 ListTools 实报，此处为静态声明）。
	ToolNames []string
	// URL 服务访问地址（仅 service 插件使用，供前端/TUI 展示入口链接）。
	URL string
	// Roles 可见角色白名单，缺省 ["*"] 全角色可见。
	Roles []string
	// TopLevel 顶层必备能力标记（plugins.yaml settings.top_level: true）：
	// 与对接用户的顶层 Agent 直接相关的能力（如联网搜索），会话启动时自动预挂到
	// 顶层 scope，无需 tool_catalog+tool_mount 两步，也不占角色白名单。
	// 只挂顶层（fast/daily 档）；子 Agent scope 不预挂，仍走按需挂载。
	TopLevel bool
}

// VisibleForRole 判断插件工具是否对指定角色可见：
// Roles 为空或缺省 ["*"] 时全角色可见；否则精确匹配角色 ID。
func (m Manifest) VisibleForRole(roleID string) bool {
	if len(m.Roles) == 0 {
		return true
	}
	for _, r := range m.Roles {
		if r == "*" || r == roleID {
			return true
		}
	}
	return false
}

// MissingEnv 返回 RequiresEnv 中当前进程缺失的环境变量名列表。
func (m Manifest) MissingEnv() []string {
	var missing []string
	for _, name := range m.RequiresEnv {
		if os.Getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	return missing
}

// Deps 是插件初始化时注入的依赖。
type Deps struct {
	// Logger 结构化日志器（nil 时插件可自行忽略）。
	Logger *slog.Logger
	// Config 是 plugins.yaml 中该插件 settings 段的反序列化结果。
	Config map[string]any
	// WorkDir 是 Agent 工作目录。
	WorkDir string
}

// Plugin 插件生命周期接口（builtin 与 mcp 形态统一实现）。
//
// 契约：
//   - Init：校验配置、建立客户端；不得注册工具。
//   - Start：建立连接/就绪；不得注册工具（工具注册由 Manager 统一执行，失败可回滚）。
//   - Stop：释放资源；幂等。
//   - Tools：返回当前可用工具列表（Start 成功后被 Manager 注册进 tool.Registry）。
type Plugin interface {
	// Manifest 返回插件静态元数据。
	Manifest() Manifest
	// Init 初始化：校验配置、创建客户端；失败返回错误（不注册工具）。
	Init(ctx context.Context, deps Deps) error
	// Start 启动：连接 MCP server / 准备就绪；失败返回错误。
	Start(ctx context.Context) error
	// Stop 停止：断开连接、杀子进程、释放资源；幂等。
	Stop(ctx context.Context) error
	// Tools 返回当前可用工具列表；Manager 负责注册/摘除。
	Tools() []tool.Tool
}
