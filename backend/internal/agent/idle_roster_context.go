package agent

// idle_roster_context.go 实现热驻领域 Agent 清单注入（Domain 热驻 + 复用权重）：
// MetaAgent 每轮上下文末尾追加【空闲领域Agent】system 消息——热驻 idle domain 的
// 可复用信息（agent_id/领域/最近任务/reuse_count/剩余寿命/忙碌态），供 MetaAgent
// 自主判定"新任务与哪个领域强相关"并经 call_sub_agent(reuse_agent_id=X) 复用，
// 弱相关则新建 domain。清单缺失/热驻未开启时零变化。
//
// 实现：包装 MemoryPipeline 的 Assemble（与 board_context.go 同模式），只对
// MetaAgent 使用。每轮经闭包重查清单（复用/销毁实时变化，构造期查询会过期）。

import (
	"fmt"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// idleRosterInjectingPipeline 包装 MemoryPipeline，在 Assemble 输出末尾追加空闲领域清单段。
type idleRosterInjectingPipeline struct {
	inner  MemoryPipeline
	roster func() []IdleDomainInfo
}

// Assemble 委托内层流水线后追加清单段（不可缓存尾部，不破坏前缀缓存）。
func (p *idleRosterInjectingPipeline) Assemble(role types.RoleDefinition, agentID string, history []ReactMessage) []ReactMessage {
	out := p.inner.Assemble(role, agentID, history)
	if p.roster != nil {
		if txt := renderIdleRoster(p.roster()); txt != "" {
			out = append(out, ReactMessage{Role: "system", Content: txt})
		}
	}
	return out
}

// Write 委托内层流水线。
func (p *idleRosterInjectingPipeline) Write(agentID string, ev MemoryEvent) error {
	return p.inner.Write(agentID, ev)
}

// wrapMetaMemoryWithRoster 为 MetaAgent 记忆流水线叠加空闲领域清单注入。
// rosterFn 为 nil 时返回原流水线（零行为变化）。
func wrapMetaMemoryWithRoster(inner MemoryPipeline, rosterFn func() []IdleDomainInfo) MemoryPipeline {
	if rosterFn == nil {
		return inner
	}
	return &idleRosterInjectingPipeline{inner: inner, roster: rosterFn}
}

// renderIdleRoster 渲染清单为注入文本；空清单返回空串（不注入）。
func renderIdleRoster(infos []IdleDomainInfo) string {
	if len(infos) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("【空闲领域Agent】（热驻复用池。新任务与某领域**强相关**时必须优先复用：call_sub_agent(reuse_agent_id=该id, task=新任务)，")
	sb.WriteString("复用保留该领域全部上下文与知识；**弱相关或无关**则省略 reuse_agent_id 新建 domain。繁忙 Agent 的任务会入队，当前任务完成后自动执行。）\n")
	for _, info := range infos {
		line := fmt.Sprintf("- id=%s 领域=%s reuse=%d 状态=%s", info.AgentID, info.Domain, info.ReuseCount, idleStateLabel(info))
		if info.LastTask != "" {
			line += " 最近任务=" + info.LastTask
		}
		if info.IdleLeft > 0 {
			line += " 存活剩余=" + formatDurationCN(info.IdleLeft)
		}
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	return sb.String()
}

// idleStateLabel 返回槽状态标签。
func idleStateLabel(info IdleDomainInfo) string {
	if info.Busy {
		return "忙碌中"
	}
	return "空闲可复用"
}

// formatDurationCN 渲染剩余寿命（中文单位）。
func formatDurationCN(d time.Duration) string {
	if d >= time.Hour {
		return fmt.Sprintf("%.1f小时", d.Hours())
	}
	return fmt.Sprintf("%.0f分钟", d.Minutes())
}
