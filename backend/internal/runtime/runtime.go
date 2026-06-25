// Package runtime 把 v3 §4-7 中各运行时组件（任务看板 / 邮箱 /
// Watchdog / 人格 / Skill 注册表）聚合成单一对象，供 graph 层
// 与 server 层共享访问，避免在 MetaAgent / DomainAgent 上挂太多字段。
package runtime

import (
	"fmt"

	"github.com/blockmemory/agent/backend/internal/board"
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
// 对象，在 main.go 一次性装配、经 ThreeLayerGraphBuilder.SetRuntime
// 注入图，再向下游节点透明传播，避免散挂指针与漏注风险。
type Runtime struct {
	Boards   *board.Manager     // 多看板管理器：每会话一个 TaskBoard，记录目标/子任务/约束/进度
	Mailbox  *mailbox.Mailbox   // Agent 间异步邮箱：事件投递与拉取，避免上下文交叉污染
	Skills   *skill.Registry    // Skill 注册表：持有 Pool 并维护 Agent→SkillSet 装配映射
	Soul     *soul.Loader       // 人格加载器：注入 soul.md 并提供热重载与温度策略
	Watchdog *watchdog.Watchdog // 上下文看门狗：按软/硬阈值发出压缩或切换信号
}

// New 创建带默认依赖的运行时。
//
// 职责：在 main.go 启动阶段一次性装配 Runtime 的五大组件，让上层 graph
// 只需依赖单一 Runtime 指针即可访问全部运行时能力。
//
// 参数：
//
//	soulPath  人格文件路径，必须存在（main.go 已校验）
//	skillPool 技能池，由 main.go 从 yaml 加载；nil 时退回 BuiltinPool 兜底
//
// 返回：装配完成的 *Runtime，各字段均已就绪。
//
// 副作用：触发一次 loader.Load()；文件读取失败 panic（main.go 已保证文件存在）。
func New(soulPath string, skillPool *skill.Pool) *Runtime {
	// skillPool 为 nil 时退回内置技能池作为兜底
	if skillPool == nil {
		skillPool = skill.BuiltinPool()
	}

	loader := soul.NewLoader(soulPath)
	if err := loader.Load(); err != nil {
		panic(fmt.Sprintf("load soul.md: %v", err))
	}

	// 一次性装配五大组件并返回进程级共享的 Runtime
	return &Runtime{
		Boards:   board.NewManager(),                     // 空看板管理器，会话启动时由 MetaAgent 按需 GetOrCreate
		Mailbox:  mailbox.New(),                          // 空邮箱，各 Agent 通过 Send/Drain 异步通信
		Skills:   skill.NewRegistry(skillPool),           // 以技能池初始化注册表
		Soul:     loader,                                 // 人格加载器，供 Agent 拼 system prompt 时注入
		Watchdog: watchdog.New(watchdog.DefaultConfig()), // 使用默认软/硬阈值（soft=12000/hard=20000）
	}
}
