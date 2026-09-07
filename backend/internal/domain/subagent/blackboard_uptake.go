package subagent

// blackboard_uptake.go 实现黑板模式（TODO #42）的每轮兄弟产出摄取：
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
	"log"
	"sync"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// siblingOutputHeader 是每轮兄弟产出注入段头部。标注"已完成结论"语义，防模型把兄弟
// 已完成的改动当当前任务待办重复执行（同 blockMemoryRecallHeader 的防护意图）。
const siblingOutputHeader = "【兄弟产出】（同领域兄弟 Agent 的完成结论沉淀，仅作背景参考；不要重复执行其中已完成的改动）"

// siblingUptakePipeline 包装 MemoryPipeline，每轮 Assemble 末尾按 scope 查询兄弟产出注入。
// 镜像 agent/board_context.go 的 boardInjectingPipeline 包装模式。
type siblingUptakePipeline struct {
	inner                          agent.MemoryPipeline
	bb                             BlackboardSearcher
	sid, parentID, taskDomain      string
	selfID                         string
	seen                           map[int64]bool // 已注入过的 rec ID（含播种召回 seed），防每轮重复
	mu                             sync.Mutex
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
// 查询失败/无新事实时零注入（fail-open）。
func (p *siblingUptakePipeline) Assemble(role types.RoleDefinition, agentID string, history []agent.ReactMessage) []agent.ReactMessage {
	out := p.inner.Assemble(role, agentID, history)
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
		return out
	}
	rankBlockMemory(fresh)
	msg := renderRecalledMemory(siblingOutputHeader, fresh)
	if msg == "" {
		return out
	}
	// 注入测量（TODO 第七项③）：纯加法 slog，量化每轮黑板摄取对上下文的增量贡献。
	log.Printf("[subagent] ctx_inject: sub=%s stage=sibling_uptake fresh=%d uptake=%d runes", agentID, len(fresh), len([]rune(msg)))
	return append(out, agent.ReactMessage{Role: "system", Content: msg})
}

// Write 委托内层流水线。
func (p *siblingUptakePipeline) Write(agentID string, ev agent.MemoryEvent) error {
	return p.inner.Write(agentID, ev)
}
