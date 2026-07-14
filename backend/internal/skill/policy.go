package skill

import (
	"context" // 上下文，用于 LLM 调用取消与超时
	"fmt"     // 格式化 prompt 字符串
	"strings" // 字符串比较与裁剪

	"github.com/blockmemory/agent/backend/pkg/types"
)

// SkillSelectionPolicy 为 SkillPool 的二次选择阶段提供策略抽象。
//
// 职责：从规则粗筛后的候选 Skill 中，按领域与目标进一步收敛到 ≤maxKeep 个。
// 实现可以基于 LLM、规则排序、成本预算等不同策略。
type SkillSelectionPolicy interface {
	// Select 从 candidates 中选择最多 maxKeep 个 Skill。
	//
	// 参数：
	//   - ctx：上下文，用于超时/取消控制
	//   - candidates：规则粗筛后的候选 Skill 列表
	//   - domain：目标领域名
	//   - goal：当前任务目标
	//   - maxKeep：最多返回条数
	//
	// 返回：选中的 Skill 切片；error 表示选择过程中发生的错误（调用方可选择忽略并兜底）。
	Select(ctx context.Context, candidates []*types.Skill, domain, goal string, maxKeep int) ([]*types.Skill, error)
}

// SkillPickOnePolicy 为已装配 SkillSet 中的“1 选 1”决策提供策略抽象。
//
// 职责：给定一组 Skill 与当前任务目标，返回最合适的一个 Skill；
// 无合适 Skill 时返回 nil。
type SkillPickOnePolicy interface {
	// PickOne 从 set 中选择最适合 goal 的一个 Skill。
	//
	// 参数：
	//   - ctx：上下文，用于超时/取消控制
	//   - set：已装配的 Skill 列表
	//   - goal：当前任务目标
	//
	// 返回：命中的 Skill 指针；无合适 Skill 时返回 nil。
	PickOne(ctx context.Context, set []*types.Skill, goal string) (*types.Skill, error)
}

// DefaultSkillPolicy 是 SkillSelectionPolicy 与 SkillPickOnePolicy 的默认实现，
// 复用 pool.go 中已有的 LLM 选择逻辑，保证行为不变。
type DefaultSkillPolicy struct {
	llm LLMClient // LLM 客户端；Select 使用 llmPickSkills，PickOne 直接生成 prompt
}

// NewDefaultSkillPolicy 创建持有指定 LLM 的默认策略。
func NewDefaultSkillPolicy(llm LLMClient) *DefaultSkillPolicy {
	return &DefaultSkillPolicy{llm: llm}
}

// WithLLM 返回使用新 LLM 的策略副本，便于在保持原策略不变的情况下注入运行时 LLM。
func (d *DefaultSkillPolicy) WithLLM(llm LLMClient) *DefaultSkillPolicy {
	return &DefaultSkillPolicy{llm: llm}
}

// Select 实现 SkillSelectionPolicy，复用 llmPickSkills + trimSkills 兜底逻辑。
//
// 当 LLM 不可用或选择失败时，按 Cost 升序、ID 字典序截断到 maxKeep 个。
func (d *DefaultSkillPolicy) Select(ctx context.Context, candidates []*types.Skill, domain, goal string, maxKeep int) ([]*types.Skill, error) {
	// 优先调用 LLM 做语义选择；失败或 LLM 为 nil 时返回空切片。
	picked := llmPickSkills(ctx, d.llm, candidates, domain, goal, maxKeep)
	if len(picked) == 0 {
		// LLM 不可用或选择失败：用确定性规则兜底，保证返回非空（只要 candidates 非空）。
		picked = trimSkills(candidates, maxKeep)
	}
	return picked, nil
}

// PickOne 实现 SkillPickOnePolicy，复用原 SelectOne 的 LLM 决策逻辑。
//
// 返回 set 中命中的 Skill；LLM 失败、返回 NONE 或非法 ID 时返回 nil。
func (d *DefaultSkillPolicy) PickOne(ctx context.Context, set []*types.Skill, goal string) (*types.Skill, error) {
	// 前置校验：空集合或无 LLM 无法决策，直接返回 nil。
	if len(set) == 0 || d.llm == nil {
		return nil, nil
	}

	// 构造单选 prompt：把当前 Skill 清单与任务交给 LLM，要求只输出 skill_id 或 NONE。
	prompt := fmt.Sprintf(`你是 Skill 调用决策器。当前 Agent 持有以下 Skill：
%s
请基于任务选择最合适的一个 Skill，输出其 skill_id。
若无任何 Skill 合适，输出 "NONE"。
任务: %s

仅输出 skill_id 或 NONE，不要其他文字。`, (&types.SkillSet{Skills: set}).PromptList(), goal)
	resp, err := d.llm.Generate(ctx, prompt)
	// 与原 SelectOne 行为一致：LLM 失败静默返回 nil。
	if err != nil {
		return nil, nil
	}
	resp = strings.TrimSpace(resp)
	// 空响应或 LLM 认为无合适 Skill，均返回 nil。
	if resp == "" || strings.EqualFold(resp, "NONE") {
		return nil, nil
	}
	// 遍历候选 Skill，找到与 LLM 输出匹配（不区分大小写）的项。
	for _, s := range set {
		if strings.EqualFold(strings.TrimSpace(resp), s.SkillID) {
			return s, nil
		}
	}
	return nil, nil
}
