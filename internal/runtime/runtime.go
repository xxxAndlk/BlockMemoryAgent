// Package runtime 把 v3 §4-7 中各运行时组件（任务看板 / 邮箱 /
// Watchdog / 人格 / Skill 注册表）聚合成单一对象，供 graph 层
// 与 server 层共享访问，避免在 MetaAgent / DomainAgent 上挂太多字段。
package runtime

import (
	"github.com/blockmemory/agent/internal/board"
	"github.com/blockmemory/agent/internal/mailbox"
	"github.com/blockmemory/agent/internal/skill"
	"github.com/blockmemory/agent/internal/soul"
	"github.com/blockmemory/agent/internal/watchdog"
)

// Runtime 进程级运行时
type Runtime struct {
	Boards   *board.Manager
	Mailbox  *mailbox.Mailbox
	Skills   *skill.Registry
	Soul     *soul.Loader
	Watchdog *watchdog.Watchdog
}

// New 创建带默认依赖的运行时
//
//	soulPath  人格文件路径，可为空（不加载）
//	skillPool 技能池，可为 nil（使用 BuiltinPool）
func New(soulPath string, skillPool *skill.Pool) *Runtime {
	if skillPool == nil {
		skillPool = skill.BuiltinPool()
	}
	loader := soul.NewLoader(soulPath)
	if soulPath != "" {
		_ = loader.Load() // 文件不存在不致命
	}
	return &Runtime{
		Boards:   board.NewManager(),
		Mailbox:  mailbox.New(),
		Skills:   skill.NewRegistry(skillPool),
		Soul:     loader,
		Watchdog: watchdog.New(watchdog.DefaultConfig()),
	}
}
