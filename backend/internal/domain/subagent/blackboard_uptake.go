package subagent

// blackboard_uptake.go 实现黑板模式的每轮兄弟产出摄取：
// DomainAgent 每轮 Assemble 末尾按 scope（parent_id + task_domain）查询黑板块记忆，
// 把同领域兄弟 Agent 的完成结论作为【兄弟产出】尾部 system 消息注入上下文。
//
// 解决"兄弟 B 在 A 启动后完成，A 永远看不到"缺口：旧召回仅在子 Agent 播种时一次性跑，
// 运行中兄弟的新产出对等待中的 DomainAgent 不可见，致其"等待回传"叙事空转。
//
// 仅 DomainAgent 包装（叶子 code_assistant 不包装--噪声门，干活 Agent 无需兄弟态势）。
// 尾部 system 消息位置 = 前缀缓存安全（同【近期事件】/【当前时间】类不可缓存尾部）。
// 查询失败 fail-open（不注入不崩）；seen 去重防同一事实每轮重复注入。

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/decision"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// siblingOutputHeader 是每轮兄弟产出注入段头部。标注"已完成结论"语义，防模型把兄弟
// 已完成的改动当当前任务待办重复执行（同 blockMemoryRecallHeader 的防护意图）。
const siblingOutputHeader = "【兄弟产出】（同领域兄弟 Agent 的完成结论沉淀，仅作背景参考；不要重复执行其中已完成的改动）"

// siblingRosterHeader 是每轮拓扑名册实时刷新注入段头部（2026-09-21 可见性矩阵定版）：
// 系统提示词【运行时身份】块的名册是派发时点快照，运行中兄弟/下级的增删不可见。
// 此处每轮比对当前活跃名册与已播报快照——任何变化（新增或消失）注入**全量当前名册**
// 替换旧认知；无变化零注入（增量 diff，不占上下文）。尾部 system 消息位置同
//【兄弟产出】（前缀缓存安全）。
const siblingRosterHeader = "【拓扑名册更新】（实时同组/同级执行者名册，替换了派发时点的旧快照；send_message 的 to_agent_id 用下列实例 id）"

// rosterEntry 兄弟名册条目（id/展示标签/任务简报）。
type rosterEntry struct {
	id, label, task string
}

// siblingUptakePipeline 包装 MemoryPipeline，每轮 Assemble 末尾按 scope 查询兄弟产出注入。
// 镜像 agent/board_context.go 的 boardInjectingPipeline 包装模式。
type siblingUptakePipeline struct {
	inner                          agent.MemoryPipeline
	bb                             BlackboardSearcher
	sid, parentID, taskDomain      string
	selfID                         string
	seen                           map[int64]bool // 已注入过的 rec ID（含播种召回 seed），防每轮重复
	mu                             sync.Mutex
	rosterFn                       func() []rosterEntry // 每轮查询活跃兄弟名册（nil=不注入名册更新）
	seenRoster                     map[string]bool      // 已告知的兄弟 id（初始=系统提示词首注名册）
	decLayer                       *decision.Layer      // 决策层④摄取打分（TODO #23）；nil=不打分
}

// newSiblingUptakePipeline 构造摄取包装器。selfID 用于排除自身产出（不回显自己刚写的结论）。
func newSiblingUptakePipeline(inner agent.MemoryPipeline, bb BlackboardSearcher, sid, parentID, taskDomain, selfID string) *siblingUptakePipeline {
	return &siblingUptakePipeline{
		inner:     inner,
		bb:        bb,
		sid:       sid,
		parentID:  parentID,
		taskDomain: taskDomain,
		selfID:    selfID,
		seen:      make(map[int64]bool),
	}
}

// WithRoster 装配每轮兄弟名册增量注入。seeded = 系统提示词首注时已列出的兄弟 id
//（这些不再重复播报）；fn 每轮返回当前活跃兄弟名册。fn 为 nil 时零行为（测试兼容）。
func (p *siblingUptakePipeline) WithRoster(fn func() []rosterEntry, seeded map[string]bool) *siblingUptakePipeline {
	p.rosterFn = fn
	p.seenRoster = seeded
	if p.seenRoster == nil {
		p.seenRoster = make(map[string]bool)
	}
	return p
}

// WithDecisionLayer 装配决策层摄取打分（TODO #23 切入点4）。nil=不打分（测试兼容）。
func (p *siblingUptakePipeline) WithDecisionLayer(l *decision.Layer) *siblingUptakePipeline {
	p.decLayer = l
	return p
}

// seedSeen 把播种召回命中的事实 ID 标为已见，防每轮摄取重复注入同一条。
// runSubAgentOnce 在 injectScopedRecall 后调用。
func (p *siblingUptakePipeline) seedSeen(recs []*types.KnowledgeRecord) {
	if len(recs) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range recs {
		if r.ID > 0 {
			p.seen[r.ID] = true
		}
	}
}

// Assemble 委托内层流水线后，按 scope 查询兄弟产出并追加【兄弟产出】尾部消息。
// 空 query=纯 scope 过滤（省 embedding）；excludeSelf=selfID 排除自身产出。
// 查询失败/无新事实时零注入（fail-open）。bb 为 nil（searcher 未实现黑板接口）时
// 跳过黑板查询，仅保留兄弟名册增量更新（名册来自权威树，不依赖黑板）。
func (p *siblingUptakePipeline) Assemble(role types.RoleDefinition, agentID string, history []agent.ReactMessage) []agent.ReactMessage {
	out := p.inner.Assemble(role, agentID, history)
	if p.bb == nil {
		return p.appendRosterUpdates(out, agentID)
	}
	// Assemble 无 ctx 参数（接口约束），用 Background + 10s 超时（同 pipeline.go summarizer 模式）。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	recs, err := p.bb.Query(ctx, p.sid, p.parentID, p.taskDomain, "", blockMemoryRecallTopK, p.selfID)
	if err != nil {
		log.Printf("[subagent] sibling-uptake query failed: agent=%s err=%v", agentID, err)
		return out
	}
	// 去重已见记录，收集新事实。
	p.mu.Lock()
	var fresh []*types.KnowledgeRecord
	for _, r := range recs {
		if r.ID > 0 && p.seen[r.ID] {
			continue
		}
		fresh = append(fresh, r)
	}
	// 把新事实 ID 标为已见（本轮注入后下轮不重复）。
	for _, r := range fresh {
		if r.ID > 0 {
			p.seen[r.ID] = true
		}
	}
	p.mu.Unlock()
	if len(fresh) == 0 {
		return p.appendRosterUpdates(out, agentID)
	}
	rankBlockMemory(fresh)
	// 决策层④摄取相关性打分（TODO #23 切入点4）：现排序规则不动，决策层逐候选 Score
	// 影子对拍；enforce 点低于 score_floor 剔除。fail-open 故障/影子期原样保留。
	fresh = gateUptakeScore(ctx, p.decLayer, agentID, "", fresh)
	if len(fresh) == 0 {
		return p.appendRosterUpdates(out, agentID)
	}
	msg := renderRecalledMemory(siblingOutputHeader, fresh)
	if msg == "" {
		return p.appendRosterUpdates(out, agentID)
	}
	// 注入测量（TODO 第七项③）：纯加法 slog，量化每轮黑板摄取对上下文的增量贡献。
	log.Printf("[subagent] ctx_inject: sub=%s stage=sibling_uptake fresh=%d uptake=%d runes", agentID, len(fresh), len([]rune(msg)))
	return p.appendRosterUpdates(append(out, agent.ReactMessage{Role: "system", Content: msg}), agentID)
}

// appendRosterUpdates 实时刷新拓扑名册：当前活跃集合与已播报快照比对——
// 任何差异（新节点加入或已有节点终结）注入全量当前名册替换旧认知；
// 无变化零注入。兄弟/下级完成不单独播报，全量名册自然反映其消失。
func (p *siblingUptakePipeline) appendRosterUpdates(out []agent.ReactMessage, agentID string) []agent.ReactMessage {
	if p.rosterFn == nil {
		return out
	}
	sibs := p.rosterFn()
	p.mu.Lock()
	changed := len(sibs) != len(p.seenRoster)
	if !changed {
		for _, s := range sibs {
			if !p.seenRoster[s.id] {
				changed = true
				break
			}
		}
	}
	if changed {
		// 全量替换快照（含"消失"语义：不再活跃的 id 从快照移除）。
		next := make(map[string]bool, len(sibs))
		for _, s := range sibs {
			next[s.id] = true
		}
		p.seenRoster = next
	}
	p.mu.Unlock()
	if !changed {
		return out
	}
	var b strings.Builder
	b.WriteString(siblingRosterHeader + "\n")
	if len(sibs) == 0 {
		b.WriteString("（当前无同组活跃执行者）\n")
	} else {
		for _, s := range sibs {
			fmt.Fprintf(&b, "- %s（%s，任务：%s）\n", s.id, s.label, s.task)
		}
	}
	msg := b.String()
	log.Printf("[subagent] ctx_inject: sub=%s stage=sibling_roster fresh=%d uptake=%d runes", agentID, len(sibs), len([]rune(msg)))
	return append(out, agent.ReactMessage{Role: "system", Content: msg})
}

// Write 委托内层流水线。
func (p *siblingUptakePipeline) Write(agentID string, ev agent.MemoryEvent) error {
	return p.inner.Write(agentID, ev)
}
