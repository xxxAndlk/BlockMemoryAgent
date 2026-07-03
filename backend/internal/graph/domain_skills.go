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
//   - 调 Pool.AssembleSet 触发 LLM 选择 ≤8 个技能
//   - 通过 Registry.Bind 绑定到本 agent 实例 ID
//
// 参数：
//   - ctx：请求上下文
//   - inst：本实例
//   - state：图全局状态（取 DomainGoal 作为选择输入）
//
// 副作用：装配成功后向 registry 写入 SkillSet。
func (n *DomainAgentNode) ensureSkillSet(ctx context.Context, inst *types.RoleInstance, state *types.ThreeLayerState) {
	// Runtime 或 Skill 注册表缺失则跳过（退化模式）
	if n.rt == nil || n.rt.Skills == nil {
		return
	}
	// 已装配则直接返回，避免重复 LLM 调用
	if existing := n.rt.Skills.GetForAgent(n.instID); existing != nil {
		return
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
		// 从任务结果中提取结构化 facts（domain + task 级）
		facts := collectBlockFacts(inst.Domain, block)
		// 异步归档避免阻塞图循环；失败仅结构化日志，不影响主流程
		// 注意：通过参数显式捕获 block/domain/goal/sum/facts，避免闭包捕获迭代变量
		// 成功后置 b.archived=true，供 switchToNextBlock 幂等兜底判断
		go func(b *types.SessionBlock, domain, goal, sum string, fs []BlockMemoryFact) {
			// 独立 ctx：与会话 ctx 解耦，会话结束后归档仍能完成
			bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := n.blockMemory.SaveBlockMemory(bgCtx, b.SessionID, domain, goal, sum, fs); err != nil {
				log.Printf("[DomainAgent] save block memory failed: session=%s domain=%s err=%v", b.SessionID, domain, err)
				return
			}
			b.MarkArchived() // 标记已落库，switchToNextBlock 据此跳过兜底
		}(block, inst.Domain, block.Goal, summary, facts)
	}
}

// buildBlockResult 根据本块所有子任务结果与归档记忆，生成本块返回给 MetaAgent 的 AgentResult（P0-1）。
func buildBlockResult(domain string, block *types.SessionBlock, summaries, memories []string) *types.AgentResult {
	if block == nil {
		return &types.AgentResult{}
	}
	summaryText := fmt.Sprintf("领域[%s]完成", domain)
	if len(summaries) > 0 {
		summaryText += ": " + strings.Join(summaries, "; ")
	}
	memoryText := fmt.Sprintf("领域 %s 完成，目标: %s", domain, block.Goal)
	if len(memories) > 0 {
		memoryText += "。关键记忆: " + strings.Join(memories, "; ")
	}
	var facts []string
	for _, entry := range block.MetaMemory {
		if entry.HasTag("fact") {
			facts = append(facts, entry.Content)
		}
	}
	return &types.AgentResult{
		SummaryForUser: summaryText,
		MemoryForMeta:  memoryText,
		Facts:          facts,
	}
}

// collectBlockFacts 从 SessionBlock 中提取 domain / task 级 facts。
// 规则：领域名作为 domain 级 fact；每个 TaskResult 作为 task 级 fact。
func collectBlockFacts(domain string, block *types.SessionBlock) []BlockMemoryFact {
	if block == nil {
		return nil
	}
	facts := []BlockMemoryFact{
		{Key: "domain", Value: domain, Scope: "domain"},
		{Key: "goal", Value: block.Goal, Scope: "domain"},
	}
	for task, result := range block.TaskResults {
		// 对结果做简短截断，避免 value 过长
		v := result
		if len(v) > 200 {
			v = v[:200] + "..."
		}
		facts = append(facts, BlockMemoryFact{Key: "task:" + task, Value: v, Scope: "task"})
	}
	return facts
}
