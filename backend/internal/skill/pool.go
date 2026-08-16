// Package skill 实现 v3 §5 设计的 Skill 库管理。
//
// 设计分层（自上而下逐级收敛，严禁把整池注入子 Agent 上下文）：
//
//	SkillPool        全局 Skill 池（含所有可用 Skill）
//	↓ FilterByDomain 按领域 + 标签规则筛选
//	↓ AssembleSet    LLM 二次选择，最多保留 maxKeep（默认 8）个
//	SkillSet         分发给单个 DomainAgent 的子集（≤8）
//	↓ Registry.Bind  绑定到具体 Agent 实例
//	Selector         由 LLM 在 SkillSet 中推理选出最合适的 1 个 Skill 调用
//
// 严格禁止把整个 SkillPool 注入到子 Agent 的上下文，否则违反 v3 §2.2
// "上下文即稀缺资源" 原则。
package skill

import (
	"context" // 上下文传递，用于 LLM 调用取消与超时
	"encoding/json"
	"fmt"
	"strings"
	"sync" // 读写锁，保护 Pool/Registry 的并发访问
	"time" // SkillSet.CreatedAt 时间戳

	"github.com/blockmemory/agent/backend/pkg/types"
)

// LLMClient skill 包内自用的最小 LLM 接口，避免反向依赖 model/graph
//
// 设计意图：skill 包只关心"给 prompt 拿回一段文本"，不绑定具体模型实现，
// 因此定义窄接口，由上层注入（model.EinoClient 或测试用 fakeLLM 均可）。
type LLMClient interface {
	// Generate 发送 prompt 并返回 LLM 生成的文本；失败时返回 error。
	Generate(ctx context.Context, prompt string) (string, error)
}

// Pool 全局 Skill 池
//
// 职责：以 skill_id 为键保存所有可用 Skill，提供注册/查询/领域筛选/装配能力。
// 并发安全：所有读写均经 mu 保护。
// 副作用：无外部副作用，纯内存结构。
type Pool struct {
	mu              sync.RWMutex            // 读写锁：保护 skills / policy 字段的并发访问
	skills          map[string]*types.Skill // skill_id -> skill，键为 SkillID
	selectionPolicy SkillSelectionPolicy    // 二次选择策略（AssembleSet 使用）
	pickOnePolicy   SkillPickOnePolicy      // “1 选 1”策略（SelectOne 使用）
}

// NewPool 创建空 Skill 池
//
// 返回：指向已初始化 skills map 的 *Pool，可直接 Register。
func NewPool() *Pool {
	// 初始化 skills map 并安装默认策略，保证返回的 Pool 立即可用。
	return &Pool{
		skills:          make(map[string]*types.Skill),
		selectionPolicy: &DefaultSkillPolicy{},
		pickOnePolicy:   &DefaultSkillPolicy{},
	}
}

// NewPoolFromSkills 从已有 Skill 列表构建（常用于 yaml 启动加载）
//
// 参数 skills：初始 Skill 切片，可为空或含 nil 项（nil 会被 Register 忽略）。
// 返回：填充后的 *Pool。
func NewPoolFromSkills(skills []*types.Skill) *Pool {
	p := NewPool() // 先建空池
	// 遍历输入列表，逐个注册；复用 Register 完成空值/空 ID 校验与加锁。
	for _, s := range skills {
		p.Register(s) // 逐个注册，复用 Register 的空值/空 ID 校验
	}
	return p
}

// SetSelectionPolicy 设置 Skill 二次选择策略；传入 nil 时恢复为默认策略。
func (p *Pool) SetSelectionPolicy(policy SkillSelectionPolicy) {
	// 防御：nil 策略回退到默认实现，避免后续 AssembleSet 出现空指针。
	if policy == nil {
		policy = &DefaultSkillPolicy{}
	}
	p.mu.Lock()         // 加写锁：修改策略字段
	defer p.mu.Unlock() // 函数退出时释放锁，保证锁的成对释放
	p.selectionPolicy = policy
}

// SetPickOnePolicy 设置“1 选 1”决策策略；传入 nil 时恢复为默认策略。
func (p *Pool) SetPickOnePolicy(policy SkillPickOnePolicy) {
	// 防御：nil 策略回退到默认实现，避免后续 SelectOne 出现空指针。
	if policy == nil {
		policy = &DefaultSkillPolicy{}
	}
	p.mu.Lock()         // 加写锁：修改策略字段
	defer p.mu.Unlock() // 函数退出时释放锁
	p.pickOnePolicy = policy
}

// Register 注册一个 Skill；同 ID 覆盖
//
// 参数 s：待注册 Skill；nil 或 SkillID 为空时静默忽略，避免脏数据入库。
// 副作用：写入 p.skills，可能覆盖同 ID 旧值。
// 并发安全：加写锁。
func (p *Pool) Register(s *types.Skill) {
	// 防御：空 Skill 或缺 ID 直接跳过，防止 map 中出现空键或 nil 值。
	if s == nil || s.SkillID == "" {
		return
	}
	p.mu.Lock()             // 加写锁：修改 skills map
	defer p.mu.Unlock()     // 函数退出时释放锁
	p.skills[s.SkillID] = s // 同 ID 覆盖，保证最新定义生效
}

// Get 按 ID 取出 Skill
//
// 参数 id：SkillID。
// 返回：命中则返回 *types.Skill，未命中返回 nil。
// 并发安全：加读锁。
func (p *Pool) Get(id string) *types.Skill {
	p.mu.RLock()         // 加读锁：只读访问 skills map
	defer p.mu.RUnlock() // 函数退出时释放锁
	return p.skills[id]
}

// Remove 摘除一个 Skill；不存在时静默（幂等）。
// 供 bundle 插件包 reload 时清理消失目录的 skill（设计文档 §3.3）。
// 并发安全：加写锁。
func (p *Pool) Remove(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.skills, id)
}

// All 返回当前所有 Skill（拷贝，避免外部并发修改）
//
// 返回：新建切片，包含池中全部 Skill 指针（指针共享，调用方不应修改 Skill 字段）。
// 并发安全：加读锁后拷贝。
func (p *Pool) All() []*types.Skill {
	p.mu.RLock()         // 加读锁：只读遍历 skills map
	defer p.mu.RUnlock() // 函数退出时释放锁
	// 预分配容量，避免后续 append 触发扩容拷贝。
	out := make([]*types.Skill, 0, len(p.skills))
	// 遍历 map，将 Skill 指针追加到新切片；外部 append/重排不影响内部 map。
	for _, s := range p.skills {
		out = append(out, s) // 拷贝指针到新切片，外部 append/重排不影响内部 map
	}
	return out
}

// FilterByDomain 规则筛选（关键字 + 领域名子串匹配，作为 LLM 不可用的回退）
//
// 参数 domain：目标领域名（如 "code"、"ops"），空字符串视为不筛选，返回全部。
// 返回：命中的 Skill 切片（按 Domain 子串或 Tags 命中即纳入）。
// 设计意图：仅做粗筛，不评估 Skill 相关性；精细化收敛交给 AssembleSet 的 LLM。
func (p *Pool) FilterByDomain(domain string) []*types.Skill {
	// 无领域约束时直接返回全量，避免无意义的字符串匹配。
	if domain == "" {
		return p.All()
	}
	// 统一小写做不区分大小写匹配，避免大小写差异导致漏匹配。
	domainLower := strings.ToLower(domain)
	// 结果切片按需增长，domain 为空的情况已在上面返回，此处一定有筛选。
	var out []*types.Skill
	// 遍历全量 Skill，按 domain 与 tags 做子串匹配。
	for _, s := range p.All() {
		// Skill 的 Domain 字段小写化，用于不区分大小写比较。
		dm := strings.ToLower(s.Domain)
		// 命中条件：Domain 为空/通配 "*"、或双向子串匹配（覆盖 "code,doc" 这类多领域写法）
		if dm == "" || dm == "*" || strings.Contains(dm, domainLower) || strings.Contains(domainLower, dm) {
			out = append(out, s)
			continue // 已命中 Domain，无需再检查 Tags
		}
		// Domain 未命中则回退到 Tags 匹配，标签作为补充领域信号。
		for _, tag := range s.Tags {
			// 只要 domain 包含任一 tag 子串，即认为命中。
			if strings.Contains(domainLower, strings.ToLower(tag)) {
				out = append(out, s)
				break // 命中一个标签即可，避免重复 append
			}
		}
	}
	return out
}

// AssembleSet 为 ownerAgent 在 domain 领域装配一个 SkillSet
//
// 职责：按 v3 §5 分层流程，从全局池中收敛出 ≤ maxKeep 个 Skill，组成 SkillSet。
// 流程：
//  1. 规则预筛（FilterByDomain 按领域命中），得到候选集 candidates
//  2. 若候选集 ≤ maxKeep 或 LLM 不可用，直接截断/全量返回
//  3. 否则交由 LLM 二次选择 maxKeep 个最相关 Skill
//
// 参数：
//   - ctx：用于 LLM 调用取消与超时
//   - llm：LLM 客户端，nil 时跳过 LLM 选择走规则截断
//   - ownerAgent：持有者 Agent 实例 ID，写入返回的 SkillSet.OwnerAgent
//   - domain：目标领域，用于 FilterByDomain 预筛
//   - goal：当前任务目标，作为 LLM 选择 prompt 的上下文
//   - maxKeep：SkillSet 上限，≤0 时取默认 8（受上下文预算约束）
//
// 返回：装配好的 *types.SkillSet（Skills 长度 ≤ maxKeep）。
// 副作用：仅读池与调 LLM；不写池、不写 Registry。
// 并发安全：依赖 FilterByDomain/Lock 的并发安全保证。
func (p *Pool) AssembleSet(ctx context.Context, llm LLMClient, ownerAgent, domain, goal string, maxKeep int) *types.SkillSet {
	// maxKeep 非法时回退到默认值 8，对应 v3 §5 "≤8" 的上下文预算约束。
	if maxKeep <= 0 {
		maxKeep = 8
	}

	candidates := p.FilterByDomain(domain) // 第一步：规则粗筛
	// 候选数已在预算内，或无 LLM 可用，直接走规则路径，避免不必要的 LLM 调用。
	if len(candidates) <= maxKeep || llm == nil {
		return &types.SkillSet{
			OwnerAgent: ownerAgent,
			Domain:     domain,
			Skills:     trimSkills(candidates, maxKeep), // 即便无需 LLM 也要保证上限
			CreatedAt:  time.Now(),
		}
	}

	// 候选过多，注入运行时 LLM 并委托给选择策略。
	policy := p.selectionPolicy
	// 默认策略需要 LLM 实例，此处生成带 LLM 的副本，不修改原策略。
	if dp, ok := policy.(*DefaultSkillPolicy); ok {
		policy = dp.WithLLM(llm)
	}
	// 由策略完成二次选择；忽略 error，因为下方有兜底逻辑。
	picked, _ := policy.Select(ctx, candidates, domain, goal, maxKeep)
	if len(picked) == 0 {
		// LLM 失败兜底：按 Cost 升序排序后截断，保证不空返
		picked = trimSkills(candidates, maxKeep)
	}
	return &types.SkillSet{
		OwnerAgent: ownerAgent,
		Domain:     domain,
		Skills:     picked, // 策略选出的子集
		CreatedAt:  time.Now(),
	}
}

// trimSkills 简单按 cost 升序、ID 字典序保留前 N 个
//
// 参数 in：待裁剪切片；max：保留数量上限。
// 返回：长度 ≤ max 的切片。若 in 长度已 ≤ max，原样返回（零拷贝）。
// 设计意图：LLM 不可用或失败时的确定性兜底，避免 SkillSet 超长。
func trimSkills(in []*types.Skill, max int) []*types.Skill {
	// 已在预算内，直接返回原切片，避免无意义拷贝与排序。
	if len(in) <= max {
		return in
	}
	// 拷贝后再排序，避免改动调用方传入的切片。
	cp := make([]*types.Skill, len(in))
	copy(cp, in)
	// 简单选择排序：cost 升序，再 id 升序（O(n^2)，Skill 数量小可接受）
	for i := 0; i < len(cp); i++ {
		for j := i + 1; j < len(cp); j++ {
			ci, cj := cp[i].Cost, cp[j].Cost
			// cost 更大者后移；cost 相同时按 SkillID 字典序
			if ci > cj || (ci == cj && cp[i].SkillID > cp[j].SkillID) {
				cp[i], cp[j] = cp[j], cp[i]
			}
		}
	}
	return cp[:max] // 截取前 max 个
}

// AssembleFromIDs 按 Skill ID 列表从池中重建 SkillSet（特性4 复用路径）。
//
// 职责：从归档存储恢复历史 domainAgent 的 Skill 子集时，按 ID 反查 Pool。
// 不存在的 ID 静默跳过，避免因 Skill 池变化导致整组失效。
//
// 参数：
//   - ownerAgent：持有者 Agent 实例 ID
//   - ids：归档时记录的 Skill ID 列表
//
// 返回：装配好的 *types.SkillSet；ids 为空或全部不存在时返回空 SkillSet。
func (p *Pool) AssembleFromIDs(ownerAgent string, ids []string) *types.SkillSet {
	p.mu.RLock()         // 加读锁：只读访问 skills map
	defer p.mu.RUnlock() // 函数退出时释放锁
	var skills []*types.Skill
	// 按 ID 顺序遍历，从 skills map 中反查；缺失或 nil 项静默跳过。
	for _, id := range ids {
		if s, ok := p.skills[id]; ok && s != nil {
			skills = append(skills, s)
		}
	}
	return &types.SkillSet{
		OwnerAgent: ownerAgent,
		Skills:     skills,
		CreatedAt:  time.Now(),
	}
}

// llmPickSkills 使用 LLM 从 candidates 中挑出 maxKeep 个最匹配的
//
// 参数：见 AssembleSet 调用处。
// 返回：LLM 选中的 Skill 切片（长度 ≤ maxKeep）；LLM 失败或 JSON 解析失败返回 nil。
// 设计意图：让 LLM 基于领域 + 目标做相关性判断，弥补纯规则筛选的语义盲区。
func llmPickSkills(ctx context.Context, llm LLMClient, candidates []*types.Skill, domain, goal string, maxKeep int) []*types.Skill {
	// 拼接候选清单，仅暴露 ID + Description，不泄露完整 Skill 定义，保护上下文空间。
	listing := strings.Builder{}
	for _, s := range candidates {
		listing.WriteString("- ")
		listing.WriteString(s.SkillID)
		listing.WriteString(": ")
		listing.WriteString(s.Description) // 一句话描述，遵循 v3 §5.3 上下文最小化
		listing.WriteByte('\n')
	}

	// 构造选择 prompt：要求 LLM 仅输出 JSON 数组，元素为 skill_id
	prompt := fmt.Sprintf(`你是 Skill 选择器。请从以下候选 Skill 中挑出最适合该领域的 %d 个 Skill。

领域: %s
目标: %s

候选 Skill (ID: 描述):
%s
要求：
- 仅输出 JSON 数组，元素为被选中的 skill_id 字符串。
- 不要解释，不要其他文字。
示例：["read_file","run_command"]`, maxKeep, domain, goal, listing.String())

	resp, err := llm.Generate(ctx, prompt) // 调用 LLM
	// LLM 出错或空响应，返回 nil，交由调用方兜底。
	if err != nil || resp == "" {
		return nil
	}
	jsonStr := extractJSONArray(resp) // 从可能含 ```json ``` 包裹的响应中提取数组文本
	var picked []string
	if err := json.Unmarshal([]byte(jsonStr), &picked); err != nil {
		return nil // JSON 解析失败，交由调用方兜底
	}

	// 用 set 去重并加速后续查找
	idSet := make(map[string]struct{}, len(picked))
	for _, id := range picked {
		idSet[strings.TrimSpace(id)] = struct{}{} // 去空白，避免 LLM 输出空格导致失配
	}
	var out []*types.Skill
	// 按 candidates 顺序遍历，保证输出顺序稳定（不受 LLM 返回顺序影响）
	for _, s := range candidates {
		if _, ok := idSet[s.SkillID]; ok {
			out = append(out, s)
		}
		// 达到上限即停，防止 LLM 多选越界。
		if len(out) >= maxKeep {
			break
		}
	}
	return out
}

// SelectOne 给定 SkillSet 与具体 task，请 LLM 返回最合适的一个 SkillID
//
// 职责：在已装配的 SkillSet（≤8）内做"1 选 1"决策，输出 skill_id。
// 参数：
//   - ctx：用于 LLM 调用取消与超时
//   - llm：LLM 客户端
//   - set：当前 Agent 持有的 SkillSet
//   - task：当前任务描述
//
// 返回：命中的 SkillID；若 LLM 认为无合适 Skill 或返回非法 ID，返回 ""。
// 副作用：无。
// 并发安全：纯读，无锁。
//
// 如果 LLM 认为现有 Skill 都不合适，返回 ""，调用方可决定是否
// 走"扩展技能"分支（v3 §5.3）。
// SelectOne 给定 SkillSet 与具体 task，请 LLM 返回最合适的一个 SkillID。
//
// 本函数为兼容旧调用方的包级入口，内部委托给 DefaultSkillPolicy。
func SelectOne(ctx context.Context, llm LLMClient, set *types.SkillSet, task string) string {
	// 前置校验：空 SkillSet 或无 LLM 时无法做选择，直接返回空串。
	if set == nil || len(set.Skills) == 0 || llm == nil {
		return ""
	}
	// 用运行时 LLM 构造默认策略并执行 PickOne；忽略 error，由返回 nil 表示失败。
	picked, _ := NewDefaultSkillPolicy(llm).PickOne(ctx, set.Skills, task)
	if picked == nil {
		return ""
	}
	return picked.SkillID
}

// SelectOne 在 Pool 持有的 pickOnePolicy 上执行“1 选 1”决策。
func (p *Pool) SelectOne(ctx context.Context, llm LLMClient, set *types.SkillSet, task string) string {
	// 前置校验：空 SkillSet 或无 LLM 时无法做选择，直接返回空串。
	if set == nil || len(set.Skills) == 0 || llm == nil {
		return ""
	}
	p.mu.RLock() // 加读锁：读取当前 pickOnePolicy
	policy := p.pickOnePolicy
	p.mu.RUnlock() // 立即释放读锁，避免持有锁期间调用 LLM
	// 默认策略需要运行时 LLM，生成副本后执行。
	if dp, ok := policy.(*DefaultSkillPolicy); ok {
		policy = dp.WithLLM(llm)
	}
	picked, _ := policy.PickOne(ctx, set.Skills, task)
	if picked == nil {
		return ""
	}
	return picked.SkillID
}

// extractJSONArray 从 LLM 响应中提取第一个 JSON 数组
//
// 参数 s：LLM 原始响应，可能包含 ```json ``` 围栏或解释性文字。
// 返回：从首个 '[' 到末尾 ']' 的子串；未找到合法边界时原样返回。
// 设计意图：容忍 LLM 不严格遵守"仅输出 JSON"的要求，提高解析鲁棒性。
func extractJSONArray(s string) string {
	s = strings.TrimSpace(s)
	// 处理 ```json ... ``` 或 ``` ... ``` 围栏
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:] // 跳过开头三个反引号
		if strings.HasPrefix(strings.ToLower(s), "json") {
			s = s[4:] // 去掉 "json" 语言标记
		}
		if end := strings.Index(s, "```"); end >= 0 {
			s = s[:end] // 截到闭合围栏前
		}
	}
	start := strings.Index(s, "[")   // 第一个左方括号
	end := strings.LastIndex(s, "]") // 最后一个右方括号
	// 只有找到成对的方括号才返回截取区间，否则原样返回让上层解析报错。
	if start >= 0 && end > start {
		return s[start : end+1] // 返回最外层数组区间
	}
	return s // 无合法边界，原样返回交由上层解析报错
}
