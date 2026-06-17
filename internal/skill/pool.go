// Package skill 实现 v3 §5 设计的 Skill 库管理。
//
// 设计分层：
//
//   SkillPool        全局 Skill 池（含所有可用 Skill）
//   ↓ 按领域 + LLM 筛选
//   SkillSet         分发给单个 DomainAgent 的子集
//   ↓ LLM 决策
//   Selector         由 LLM 推理选出最合适的 1 个 Skill 调用
//
// 严格禁止把整个 SkillPool 注入到子 Agent 的上下文，否则违反 v3 §2.2
// "上下文即稀缺资源" 原则。
package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/blockmemory/agent/pkg/types"
)

// LLMClient skill 包内自用的最小 LLM 接口，避免反向依赖 model/graph
type LLMClient interface {
	Generate(ctx context.Context, prompt string) (string, error)
}

// Pool 全局 Skill 池
type Pool struct {
	mu     sync.RWMutex
	skills map[string]*types.Skill // skill_id -> skill
}

// NewPool 创建空 Skill 池
func NewPool() *Pool {
	return &Pool{skills: make(map[string]*types.Skill)}
}

// NewPoolFromSkills 从已有 Skill 列表构建（常用于 yaml 启动加载）
func NewPoolFromSkills(skills []*types.Skill) *Pool {
	p := NewPool()
	for _, s := range skills {
		p.Register(s)
	}
	return p
}

// Register 注册一个 Skill；同 ID 覆盖
func (p *Pool) Register(s *types.Skill) {
	if s == nil || s.SkillID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.skills[s.SkillID] = s
}

// Get 按 ID 取出 Skill
func (p *Pool) Get(id string) *types.Skill {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.skills[id]
}

// All 返回当前所有 Skill（拷贝，避免外部并发修改）
func (p *Pool) All() []*types.Skill {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*types.Skill, 0, len(p.skills))
	for _, s := range p.skills {
		out = append(out, s)
	}
	return out
}

// FilterByDomain 规则筛选（关键字 + 领域名子串匹配，作为 LLM 不可用的回退）
func (p *Pool) FilterByDomain(domain string) []*types.Skill {
	if domain == "" {
		return p.All()
	}
	domainLower := strings.ToLower(domain)
	var out []*types.Skill
	for _, s := range p.All() {
		dm := strings.ToLower(s.Domain)
		if dm == "" || dm == "*" || strings.Contains(dm, domainLower) || strings.Contains(domainLower, dm) {
			out = append(out, s)
			continue
		}
		for _, tag := range s.Tags {
			if strings.Contains(domainLower, strings.ToLower(tag)) {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

// AssembleSet 为 ownerAgent 在 domain 领域装配一个 SkillSet
//
// 流程：
//  1. 规则预筛（按领域命中），得到候选集
//  2. 若候选集 ≤ maxKeep，直接返回
//  3. 否则交由 LLM 二次选择 maxKeep 个最相关 Skill
func (p *Pool) AssembleSet(ctx context.Context, llm LLMClient, ownerAgent, domain, goal string, maxKeep int) *types.SkillSet {
	if maxKeep <= 0 {
		maxKeep = 8
	}

	candidates := p.FilterByDomain(domain)
	if len(candidates) <= maxKeep || llm == nil {
		return &types.SkillSet{
			OwnerAgent: ownerAgent,
			Domain:     domain,
			Skills:     trimSkills(candidates, maxKeep),
			CreatedAt:  time.Now(),
		}
	}

	picked := llmPickSkills(ctx, llm, candidates, domain, goal, maxKeep)
	if len(picked) == 0 {
		// LLM 失败，按 Cost 排序后截断
		picked = trimSkills(candidates, maxKeep)
	}
	return &types.SkillSet{
		OwnerAgent: ownerAgent,
		Domain:     domain,
		Skills:     picked,
		CreatedAt:  time.Now(),
	}
}

// trimSkills 简单按 cost 升序、ID 字典序保留前 N 个
func trimSkills(in []*types.Skill, max int) []*types.Skill {
	if len(in) <= max {
		return in
	}
	cp := make([]*types.Skill, len(in))
	copy(cp, in)
	// 简单选择排序：cost 升序，再 id 升序
	for i := 0; i < len(cp); i++ {
		for j := i + 1; j < len(cp); j++ {
			ci, cj := cp[i].Cost, cp[j].Cost
			if ci > cj || (ci == cj && cp[i].SkillID > cp[j].SkillID) {
				cp[i], cp[j] = cp[j], cp[i]
			}
		}
	}
	return cp[:max]
}

// llmPickSkills 使用 LLM 从 candidates 中挑出 maxKeep 个最匹配的
func llmPickSkills(ctx context.Context, llm LLMClient, candidates []*types.Skill, domain, goal string, maxKeep int) []*types.Skill {
	listing := strings.Builder{}
	for _, s := range candidates {
		listing.WriteString("- ")
		listing.WriteString(s.SkillID)
		listing.WriteString(": ")
		listing.WriteString(s.Description)
		listing.WriteByte('\n')
	}

	prompt := fmt.Sprintf(`你是 Skill 选择器。请从以下候选 Skill 中挑出最适合该领域的 %d 个 Skill。

领域: %s
目标: %s

候选 Skill (ID: 描述):
%s
要求：
- 仅输出 JSON 数组，元素为被选中的 skill_id 字符串。
- 不要解释，不要其他文字。
示例：["read_file","run_command"]`, maxKeep, domain, goal, listing.String())

	resp, err := llm.Generate(ctx, prompt)
	if err != nil || resp == "" {
		return nil
	}
	jsonStr := extractJSONArray(resp)
	var picked []string
	if err := json.Unmarshal([]byte(jsonStr), &picked); err != nil {
		return nil
	}

	idSet := make(map[string]struct{}, len(picked))
	for _, id := range picked {
		idSet[strings.TrimSpace(id)] = struct{}{}
	}
	var out []*types.Skill
	for _, s := range candidates {
		if _, ok := idSet[s.SkillID]; ok {
			out = append(out, s)
		}
		if len(out) >= maxKeep {
			break
		}
	}
	return out
}

// SelectOne 给定 SkillSet 与具体 task，请 LLM 返回最合适的一个 SkillID
//
// 如果 LLM 认为现有 Skill 都不合适，返回 ""，调用方可决定是否
// 走"扩展技能"分支（v3 §5.3）。
func SelectOne(ctx context.Context, llm LLMClient, set *types.SkillSet, task string) string {
	if set == nil || len(set.Skills) == 0 || llm == nil {
		return ""
	}
	prompt := fmt.Sprintf(`你是 Skill 调用决策器。当前 Agent 持有以下 Skill：
%s
请基于任务选择最合适的一个 Skill，输出其 skill_id。
若无任何 Skill 合适，输出 "NONE"。
任务: %s

仅输出 skill_id 或 NONE，不要其他文字。`, set.PromptList(), task)
	resp, err := llm.Generate(ctx, prompt)
	if err != nil {
		return ""
	}
	resp = strings.TrimSpace(resp)
	if resp == "" || strings.EqualFold(resp, "NONE") {
		return ""
	}
	// 校验 ID 合法
	for _, s := range set.Skills {
		if strings.EqualFold(strings.TrimSpace(resp), s.SkillID) {
			return s.SkillID
		}
	}
	return ""
}

// extractJSONArray 从 LLM 响应中提取第一个 JSON 数组
func extractJSONArray(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:]
		if strings.HasPrefix(strings.ToLower(s), "json") {
			s = s[4:]
		}
		if end := strings.Index(s, "```"); end >= 0 {
			s = s[:end]
		}
	}
	start := strings.Index(s, "[")
	end := strings.LastIndex(s, "]")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}
