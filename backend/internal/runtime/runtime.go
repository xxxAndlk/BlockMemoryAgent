// Package runtime 把运行时组件（任务看板 / 邮箱 /
// Skill 注册表 / 人格）聚合成单一对象，供 ReAct 引擎
// 与 server 层共享访问，避免在多个 Agent 结构上挂太多字段。
package runtime

import (
	"fmt"

	"github.com/blockmemory/agent/backend/internal/board"
	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/internal/skill"
	"github.com/blockmemory/agent/backend/internal/soul"
)

// Runtime 进程级运行时聚合器。
//
// 设计意图：v3 把看板、邮箱、Skill、人格拆成独立组件后，
// 若让 MetaAgent / DomainAgent / SubDomainAgent 各自持有这些指针，
// 会出现“每个 Agent 结构体都挂一串字段、构造函数都传一遍”的散乱局面，
// 且动态构造的 DomainAgent 节点容易漏注。Runtime 把它们收口到单一
// 对象，在 main.go 一次性装配、注入 ReAct Service 与 server 层，
// 再向下游组件透明传播，避免散挂指针与漏注风险。
//
// 步骤 6（2026-07-30）：删 Runtime 死重。Watchdog（无 .Check 热路径调用方）
// 与 CmdQueue（仅 SetLogger，无消费方）已删，对应 internal/watchdog 与
// internal/cmdqueue 包整体移除。Board/Skill/Soul 保留：Board 被 TUI 作类型
// 消费（Snapshot/TaskStatus），Skill/Soul 被 server API 作字典消费。
type Runtime struct {
	Boards   *board.Manager   // 多看板管理器：每会话一个 TaskBoard，记录目标/子任务/约束/进度
	Mailbox  *mailbox.Mailbox // Agent 间异步邮箱：事件投递与拉取，避免上下文交叉污染
	Skills   *skill.Pool      // 全局技能池：skills.yaml + 约定目录扫描 + 插件 bundle 注入的合并视图
	Soul     *soul.Loader     // 人格加载器：注入 soul.md 并提供热重载与温度策略
	AgentCfg *config.AgentConfig // Agent 运行时动态参数（上下文窗口/工具轮数/重试等）
}

// RuntimeOption 用于在构造 Runtime 时注入或覆盖依赖。
// 它接收 *Runtime 指针，可直接修改字段。
type RuntimeOption func(*Runtime)

// WithBoards 注入多看板管理器；常用于测试或需要预置 Board 的场景。
//
// 参数：bm 为待注入的 board.Manager 实例。
// 返回：一个 RuntimeOption，供 runtime.New 使用。
func WithBoards(bm *board.Manager) RuntimeOption {
	return func(r *Runtime) { r.Boards = bm }
}

// WithMailbox 注入 Agent 间异步邮箱。
//
// 参数：mb 为待注入的 mailbox.Mailbox 实例。
// 返回：一个 RuntimeOption，供 runtime.New 使用。
func WithMailbox(mb *mailbox.Mailbox) RuntimeOption {
	return func(r *Runtime) { r.Mailbox = mb }
}

// WithSoul 注入人格加载器；提供时跳过从 soulPath 加载。
//
// 参数：loader 为待注入的 soul.Loader 实例。
// 返回：一个 RuntimeOption，供 runtime.New 使用。
func WithSoul(loader *soul.Loader) RuntimeOption {
	return func(r *Runtime) { r.Soul = loader }
}

// New 创建带默认依赖的运行时。
//
// 职责：在 main.go 启动阶段一次性装配 Runtime 的组件，让上层
// 只需依赖单一 Runtime 指针即可访问全部运行时能力。
//
// 参数：
//
//	soulPath   人格文件路径；空路径时 loader.Load() 会返回错误，由调用方处理
//	skillPool  技能池，由 main.go 从 yaml 加载；nil 时退回 BuiltinPool 兜底
//	opts       可选依赖注入，用于测试或高级定制
//
// 返回：装配完成的 *Runtime 与构造错误。AgentCfg 默认 nil，需调用方通过 SetAgentConfig 注入。
//
// 副作用：触发一次 loader.Load()；文件读取失败返回包装错误，不再 panic。
func New(soulPath string, skillPool *skill.Pool, opts ...RuntimeOption) (*Runtime, error) {
	// skillPool 为 nil 时退回内置技能池作为兜底，确保 Skills 字段始终非空。
	if skillPool == nil {
		skillPool = skill.BuiltinPool()
	}

	// 先用默认值装配 Runtime，再通过选项覆盖依赖；这样保证所有字段都有合理初始值。
	rt := &Runtime{
		Boards:  board.NewManager(),  // 空看板管理器，会话启动时由 MetaAgent 按需 GetOrCreate
		Mailbox: mailbox.New(),       // 空邮箱，各 Agent 通过 Send/Drain 异步通信
		Skills:  skillPool,           // 技能池（nil 时已回退 BuiltinPool）
	}

	// 应用所有选项注入/覆盖依赖。
	for _, opt := range opts {
		opt(rt)
	}

	// 若未通过选项注入 Soul，则从 soulPath 加载人格；空路径视为未配置，使用空 Persona。
	if rt.Soul == nil {
		loader := soul.NewLoader(soulPath)
		if soulPath != "" {
			// 路径非空时尝试加载人格文件；失败返回错误，避免启动时使用未配置的人格。
			if err := loader.Load(); err != nil {
				return nil, fmt.Errorf("load soul.md: %w", err)
			}
		}
		rt.Soul = loader
	}

	return rt, nil
}

// SetAgentConfig 注入 Agent 运行时动态参数。
// 由 main.go 在装载 Runtime 后调用；nil 时下游节点应回退到各自默认值。
//
// 参数：cfg 为 Agent 运行时配置指针；为 nil 时不更新。
func (r *Runtime) SetAgentConfig(cfg *config.AgentConfig) {
	// 保存配置指针，供下游组件读取上下文窗口、最大工具轮数等参数。
	r.AgentCfg = cfg
}
