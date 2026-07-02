package graph

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
	"github.com/blockmemory/agent/backend/internal/skill"
	"github.com/blockmemory/agent/backend/pkg/types"
)


// ensureSkillSet 确保该 DomainAgent 已装配 Skill 子集。
//
// 职责：
//   - 若已装配则直接返回
//   - 特性4：先查归档存储是否有同领域历史 Agent，命中则复用其 Skill 子集
//     并权重+1、延后过期；未命中再走 LLM AssembleSet
//   - 调 Pool.AssembleSet 触发 LLM 选择 ≤8 个技能
//   - 通过 Registry.Bind 绑定到本 agent 实例 ID
//
// 参数：
//   - ctx：请求上下文
//   - inst：本实例
//   - state：图全局状态（取 DomainGoal 作为选择输入）
//
// 副作用：装配成功后向 registry 写入 SkillSet；命中归档时 BumpWeight。
//
// 设计意图：v3 §5，让每个 DomainAgent 只看到与其领域相关的技能子集，
// 避免全局技能列表污染 system prompt。特性4 在此基础上跨会话复用历史装配结果。
func (n *DomainAgentNode) ensureSkillSet(ctx context.Context, inst *types.RoleInstance, state *types.ThreeLayerState) {
	// Runtime 或 Skill 注册表缺失则跳过（退化模式）
	if n.rt == nil || n.rt.Skills == nil {
		return
	}
	// 已装配则直接返回，避免重复 LLM 调用
	if existing := n.rt.Skills.GetForAgent(n.instID); existing != nil {
		return
	}

	// 特性4：先查归档，命中则复用历史 Skill 子集
	// 检索路径：domain + DomainGoal 双条件做相似查询，topK=1 取最相关的一条
	if n.archiveStore != nil {
		if archives, err := n.archiveStore.SearchDomainArchive(ctx, inst.Domain, state.DomainGoal, 1); err == nil && len(archives) > 0 {
			arc := archives[0]
			// 用归档里保存的 Skill ID 列表重建 SkillSet；查不到任何技能则 fall-through 到 LLM 路径
			if reused := n.rt.Skills.Pool().AssembleFromIDs(n.instID, arc.Skills); reused != nil && len(reused.Skills) > 0 {
				n.rt.Skills.Bind(reused)
				// 权重 +1 且延后过期：让热点领域的归档越用越不容易被回收
				ttl := time.Duration(168) * time.Hour // 默认 7 天
				if n.rt.AgentCfg != nil && n.rt.AgentCfg.DomainArchiveTTLHours > 0 {
					ttl = time.Duration(n.rt.AgentCfg.DomainArchiveTTLHours) * time.Hour
				}
				_ = n.archiveStore.BumpDomainArchiveWeight(ctx, arc.ArchiveID, ttl)
				n.emit(ctx, "think", fmt.Sprintf("复用历史 domainAgent 归档: domain=%s weight=%d skills=%v", arc.Domain, arc.Weight, arc.Skills))
				return
			}
		}
	}

	// 取领域模型作为技能选择的 LLMClient；取不到则 llm 为 nil，AssembleSet 内部回退规则
	var llm skill.LLMClient
	if n.modelFactory != nil {
		// 尝试取领域模型
		if c, err := n.modelFactory.GetDomainModel(ctx); err == nil {
			llm = c
		}
	}
	// 装配：≤8 个技能（默认），可被 AgentCfg.SkillSetSize 覆盖（特性2）
	skillSetSize := 8
	if n.rt != nil && n.rt.AgentCfg != nil && n.rt.AgentCfg.SkillSetSize > 0 {
		skillSetSize = n.rt.AgentCfg.SkillSetSize
	}
	set := n.rt.Skills.Pool().AssembleSet(ctx, llm, n.instID, inst.Domain, state.DomainGoal, skillSetSize)
	// 绑定到本 agent
	n.rt.Skills.Bind(set)
}

// summarizeResults 汇总助手结果。
//
// 职责：把当前块的 TaskResults 拼成简短摘要，写入 state.Reason 供上层展示。
// 同时若启用块记忆存储（特性3），把摘要归档到 pgvector，供后续相似检索。
//
// 参数：
//   - state：图全局状态（原地修改 state.Reason）
//
// 副作用：修改 state.Reason；可能写 Postgres（块记忆归档）。
func (n *DomainAgentNode) summarizeResults(state *types.ThreeLayerState) {
	// 取本实例
	inst := n.registry.GetInstance(n.instID)
	if inst == nil {
		// 实例已被清理，直接返回
		return
	}

	// 取当前块并收集结果摘要
	block := state.ActiveBlocks[state.CurrentBlockID]
	summaries := CommonCollectTaskSummaries(block)

	// 有摘要则拼成一句话写入 state.Reason
	if len(summaries) > 0 {
		// 用分号连接所有摘要
		state.Reason = fmt.Sprintf("领域[%s]完成: %s", inst.Domain, strings.Join(summaries, "; "))
	}

	// 特性3：把块记忆归档到 pgvector，供后续 DomainAgent 相似检索
	if n.blockMemory != nil && block != nil {
		// 优先用 state.Reason（含各任务结果摘要）；为空时退化为领域目标
		summary := state.Reason
		if summary == "" {
			summary = block.Goal
		}
		// 异步归档避免阻塞图循环；失败仅结构化日志，不影响主流程
		// 注意：通过参数显式捕获 block/domain/goal/sum，避免闭包捕获迭代变量
		// 成功后置 b.archived=true，供 switchToNextBlock 幂等兜底判断（TODO #4）
		go func(b *types.SessionBlock, domain, goal, sum string) {
			// 独立 ctx：与会话 ctx 解耦，会话结束后归档仍能完成
			bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := n.blockMemory.SaveBlockMemory(bgCtx, b.SessionID, domain, goal, sum); err != nil {
				log.Printf("[DomainAgent] save block memory failed: session=%s domain=%s err=%v", b.SessionID, domain, err)
				return
			}
			b.MarkArchived() // 标记已落库，switchToNextBlock 据此跳过兜底
		}(block, inst.Domain, block.Goal, summary)
	}

	// 特性4：把 domainAgent 信息（领域/技能/上下文摘要）归档，跨会话可复用
	if n.archiveStore != nil && block != nil {
		// 收集当前实例绑定的 Skill ID 列表，作为下次复用的种子
		var archivedSkills []string
		if n.rt != nil && n.rt.Skills != nil {
			if set := n.rt.Skills.GetForAgent(n.instID); set != nil {
				for _, s := range set.Skills {
					archivedSkills = append(archivedSkills, s.SkillID)
				}
			}
		}
		// TTL 来自配置；默认 168h（7 天）保证热点领域归档不会过快失效
		ttl := 168 * time.Hour
		if n.rt != nil && n.rt.AgentCfg != nil && n.rt.AgentCfg.DomainArchiveTTLHours > 0 {
			ttl = time.Duration(n.rt.AgentCfg.DomainArchiveTTLHours) * time.Hour
		}
		// 摘要优先取 state.Reason（含各任务结果）；为空时退化为领域目标
		summary := state.Reason
		if summary == "" {
			summary = block.Goal
		}
		// 新归档权重从 1 起步；每次被复用时 BumpDomainArchiveWeight 会自增
		rec := &DomainArchiveRecord{
			SessionID:      block.SessionID,
			Domain:         inst.Domain,
			Goal:           block.Goal,
			RoleDefID:      inst.RoleDefID,
			Skills:         archivedSkills,
			ContextSummary: summary,
			Weight:         1,
			ExpiresAt:      time.Now().Add(ttl),
			CreatedAt:      time.Now(),
		}
		// 异步落库：独立 ctx 不受会话生命周期影响；失败仅结构化日志，主流程已结束不影响结果
		go func(r *DomainArchiveRecord) {
			bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := n.archiveStore.SaveDomainArchive(bgCtx, r); err != nil {
				log.Printf("[DomainAgent] save domain archive failed: session=%s domain=%s err=%v", r.SessionID, r.Domain, err)
			}
		}(rec)
	}
}