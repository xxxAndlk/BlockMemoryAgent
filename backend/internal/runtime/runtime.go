// Package runtime 把 v3 §4-7 中各运行时组件（任务看板 / 邮箱 /
// Watchdog / 人格 / Skill 注册表）聚合成单一对象，供 ReAct 引擎
// 与 server 层共享访问，避免在多个 Agent 结构上挂太多字段。
package runtime

import (
	"fmt"

	"github.com/blockmemory/agent/backend/internal/board"
	"github.com/blockmemory/agent/backend/internal/cmdqueue"
	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/internal/skill"
	"github.com/blockmemory/agent/backend/internal/soul"
	"github.com/blockmemory/agent/backend/internal/watchdog"
)

// Runtime 进程级运行时聚合器。
//
// 设计意图：v3 把看板、邮箱、Skill、人格、Watchdog 拆成独立组件后，
// 若让 MetaAgent / DomainAgent / SubDomainAgent 各自持有这些指针，
// 会出现“每个 Agent 结构体都挂一串字段、构造函数都传一遍”的散乱局面，
// 且动态构造的 DomainAgent 节点容易漏注。Runtime 把它们收口到单一
// 对象，在 main.go 一次性装配、注入 ReAct Service 与 server 层，
// 再向下游组件透明传播，避免散挂指针与漏注风险。
type Runtime struct {
	Boards   *board.Manager      // 多看板管理器：每会话一个 TaskBoard，记录目标/子任务/约束/进度
	Mailbox  *mailbox.Mailbox    // Agent 间异步邮箱：事件投递与拉取，避免上下文交叉污染
	Skills   *skill.Registry     // Skill 注册表：持有 Pool 并维护 Agent→SkillSet 装配映射
	Soul     *soul.Loader        // 人格加载器：注入 soul.md 并提供热重载与温度策略
	Watchdog *watchdog.Watchdog  // 上下文看门狗：按软/硬阈值发出压缩或切换信号
	AgentCfg *config.AgentConfig // Agent 运行时动态参数（上下文窗口/工具轮数/重试等）
	CmdQueue *cmdqueue.Manager   // 用户指令队列（特性6：抢占中断 / 队列注入）
}

// RuntimeOption 用于在构造 Runtime 时注入或覆盖依赖。
type RuntimeOption func(*Runtime)

// WithBoards 注入多看板管理器。
func WithBoards(bm *board.Manager) RuntimeOption {
	return func(r *Runtime) { r.Boards = bm }
}

// WithMailbox 注入 Agent 间异步邮箱。
func WithMailbox(mb *mailbox.Mailbox) RuntimeOption {
	return func(r *Runtime) { r.Mailbox = mb }
}

// WithWatchdog 注入上下文看门狗。
func WithWatchdog(w *watchdog.Watchdog) RuntimeOption {
	return func(r *Runtime) { r.Watchdog = w }
}

// WithCmdQueue 注入用户指令队列。
func WithCmdQueue(cq *cmdqueue.Manager) RuntimeOption {
	return func(r *Runtime) { r.CmdQueue = cq }
}

// WithSoul 注入人格加载器；提供时跳过从 soulPath 加载。
func WithSoul(loader *soul.Loader) RuntimeOption {
	return func(r *Runtime) { r.Soul = loader }
}

// New 创建带默认依赖的运行时。
//
// 职责：在 main.go 启动阶段一次性装配 Runtime 的五大组件，让上层 graph
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
	// skillPool 为 nil 时退回内置技能池作为兜底
	if skillPool == nil {
		skillPool = skill.BuiltinPool()
	}

	// 先用默认值装配 Runtime，再通过选项覆盖依赖
	rt := &Runtime{
		Boards:   board.NewManager(),                     // 空看板管理器，会话启动时由 MetaAgent 按需 GetOrCreate
		Mailbox:  mailbox.New(),                          // 空邮箱，各 Agent 通过 Send/Drain 异步通信
		Skills:   skill.NewRegistry(skillPool),           // 以技能池初始化注册表
		Watchdog: watchdog.New(watchdog.DefaultConfig()), // 默认基于 32k 上下文窗口（soft=16k/hard=25.6k），SetAgentConfig 后按 context_window 更新
		CmdQueue: cmdqueue.NewManager(),                  // 用户指令队列（特性6）
	}

	for _, opt := range opts {
		opt(rt)
	}

	// 若未通过选项注入 Soul，则从 soulPath 加载人格；空路径视为未配置，使用空 Persona。
	if rt.Soul == nil {
		loader := soul.NewLoader(soulPath)
		if soulPath != "" {
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
// 副作用：根据 cfg.ContextWindow 同步更新 Watchdog 软/硬阈值。
func (r *Runtime) SetAgentConfig(cfg *config.AgentConfig) {
	r.AgentCfg = cfg
	if cfg != nil && cfg.ContextWindow > 0 {
		r.Watchdog.SetConfig(watchdog.ConfigForWindow(cfg.ContextWindow))
	}
}
