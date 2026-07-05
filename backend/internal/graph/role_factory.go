package graph

// 角色工厂：按需动态创建 DomainAgent / SubDomainAgent / Assistant 实例。
// 与 RoleRegistry 的分工：
//   - Registry 负责"存"（定义/实例表）。
//   - Factory 负责"造"：调 LLM 生成 system_prompt → 注册 Definition → CreateInstance。
// 无 modelFactory 时（测试 / 离线模式）回退到内置模板，保证图可跑通。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"unicode"

	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// RoleFactory 角色工厂（动态创建临时角色）。
// seq 用于生成全局唯一角色 ID 后缀；modelFactory 可选，缺失时走模板。
type RoleFactory struct {
	registry     *RoleRegistry          // 注册表：造好的角色要写回这里
	modelFactory *model.ModelFactory    // LLM 客户端工厂（可空）
	cfg          *config.RoleConfigFile // 原始配置（保留以读取模板）
	seq          atomic.Int64           // 角色 ID 自增序号
}

// NewRoleFactory 创建角色工厂。
// 参数：
//   - registry：角色注册表，工厂创建的角色会注册到这里。
//   - modelFactory：模型工厂，nil 时走离线模板。
//   - cfg：roles.yaml 配置（用于读取 dynamic_templates 等元信息）。
func NewRoleFactory(registry *RoleRegistry, modelFactory *model.ModelFactory, cfg *config.RoleConfigFile) *RoleFactory {
	return &RoleFactory{
		registry:     registry,
		modelFactory: modelFactory,
		cfg:          cfg,
	}
}

// CreateDomainAgent 动态创建领域Agent。
// 流程：去重 → LLM 生成 Definition → 注册 → 创建实例。
// 参数：
//   - ctx：用于 LLM 调用取消。
//   - sessionID：会话 ID。
//   - domain：领域名（如 "DBA" / "AIOps"）。
//   - goal：领域目标，喂给 LLM 生成 system_prompt。
//   - parentID：父实例 ID（通常是 MetaAgent 实例）。
//
// 返回：DomainAgent 实例；LLM 失败会回退模板，不会返回错误。
// 副作用：注册新的动态角色定义 + 创建实例。
func (f *RoleFactory) CreateDomainAgent(ctx context.Context, sessionID, domain, goal string, parentID string) (*types.RoleInstance, error) {
	// 1. 检查是否已存在该领域的DomainAgent
	// 同会话同领域只造一次，避免重复定义
	existing := f.findDomainAgent(sessionID, domain)
	if existing != nil {
		return existing, nil
	}

	// 2. 调用大模型生成角色定义
	// 失败时 generateDomainRoleDef 内部会回退模板，不返回错误
	roleDef, err := f.generateDomainRoleDef(ctx, domain, goal)
	if err != nil {
		return nil, fmt.Errorf("generate domain role: %w", err)
	}

	// 3. 注册动态角色定义
	// 写入 registry.dynamicDefs，供后续 CreateInstance / CanCall 查询
	if err := f.registry.RegisterDynamicRole(roleDef); err != nil {
		return nil, fmt.Errorf("register dynamic role: %w", err)
	}
	// 3.5 将动态角色模型配置注册到 ModelFactory（P3-4）
	if f.modelFactory != nil {
		f.modelFactory.RegisterDynamicModelConfig(roleDef.ID, roleDef.ModelConfig)
	}

	// 4. 创建实例
	// 把 Definition 实例化为运行时实体，挂到 parentID 之下
	inst, err := f.registry.CreateInstance(roleDef.ID, sessionID, domain, parentID)
	if err != nil {
		return nil, fmt.Errorf("create domain instance: %w", err)
	}

	return inst, nil
}

// CreateAssistant 动态创建助手角色。
// 与 CreateDomainAgent 类似，但生命周期是 task 级（1h 后过期）。
// 参数：
//   - taskDesc：任务描述，喂给 LLM 生成专注于此任务的 system_prompt。
//   - parentID：父实例 ID（通常是 DomainAgent 实例）。
//   - parentDefID：父角色定义 ID，写入 Parents 字段建立调用关系。
//
// 返回：Assistant 实例。
// 副作用：注册动态角色定义 + 创建实例（task 生命周期）。
func (f *RoleFactory) CreateAssistant(ctx context.Context, sessionID, taskDesc string, parentID string, parentDefID string) (*types.RoleInstance, error) {
	// 1. 调用大模型生成助手角色定义
	// 助手是 task 级，每个任务一个新 Definition（不复用）
	roleDef, err := f.generateAssistantRoleDef(ctx, taskDesc, parentDefID)
	if err != nil {
		return nil, fmt.Errorf("generate assistant role: %w", err)
	}

	// 2. 注册动态角色定义
	if err := f.registry.RegisterDynamicRole(roleDef); err != nil {
		return nil, fmt.Errorf("register dynamic role: %w", err)
	}
	// 2.5 将动态角色模型配置注册到 ModelFactory（P3-4）
	if f.modelFactory != nil {
		f.modelFactory.RegisterDynamicModelConfig(roleDef.ID, roleDef.ModelConfig)
	}

	// 3. 创建实例
	// domain 留空：Assistant 不绑定领域，只服务于父 DomainAgent
	inst, err := f.registry.CreateInstance(roleDef.ID, sessionID, "", parentID)
	if err != nil {
		return nil, fmt.Errorf("create assistant instance: %w", err)
	}

	return inst, nil
}

// generateDomainRoleDef 让大模型生成领域角色定义；无 modelFactory 时回退模板。
// 策略：
//  1. 构造空 Definition（ID/Type/Lifecycle/CanBeCalled 已定）。
//  2. 无 modelFactory → 直接走模板。
//  3. 有 modelFactory → 调 LLM，解析 JSON；解析失败再回退模板。
//
// 参数：
//   - domain：领域名。
//   - goal：领域目标。
//
// 返回：填充好的 Definition；不会因 LLM 失败而返回 error（总是回退模板）。
func (f *RoleFactory) generateDomainRoleDef(ctx context.Context, domain, goal string) (*types.RoleDefinition, error) {
	roleDef := &types.RoleDefinition{
		ID:          fmt.Sprintf("domain_%s_%d", sanitizeID(domain), f.seq.Add(1)),
		Type:        enums.RoleTypeDomain,
		Lifecycle:   enums.RoleLifecycleSession, // 会话级
		CanBeCalled: false,                      // DomainAgent 不被直接调用，由 MetaAgent 路由
	}

	// 无 modelFactory 时（测试 / 离线模式）直接走模板
	if f.modelFactory == nil {
		f.fillDomainTemplate(roleDef, domain, goal)
		return roleDef, nil
	}

	// LLM 提示词：要求输出严格 JSON
	prompt := fmt.Sprintf(`你需要为以下业务领域创建一个AI Agent角色定义。

领域名称: %s
领域目标: %s

请生成一个JSON格式的角色定义，包含以下字段:
- id: 角色唯一标识（小写英文+下划线）
- name: 角色显示名称
- type: 必须是 "domain"
- lifecycle: 必须是 "session"
- description: 角色职责描述（50字以内）
- system_prompt: 系统提示词，定义该角色在该领域中的行为规范
- keywords: 关键词数组，用于路由匹配
- skills: 技能列表
- can_be_called: false（DomainAgent不能被直接调用）

只输出JSON，不要其他内容。`, domain, goal)

	// 取 Domain 模型客户端
	llmClient, err := f.modelFactory.GetDomainModel(ctx)
	if err != nil {
		// 取不到模型 → 回退模板
		f.fillDomainTemplate(roleDef, domain, goal)
		return roleDef, nil
	}

	resp, err := llmClient.Generate(ctx, prompt)
	if err != nil {
		// 调用失败 → 回退模板
		f.fillDomainTemplate(roleDef, domain, goal)
		return roleDef, nil
	}

	// 解析 JSON：失败或 name 为空都视为无效，回退模板
	if err := json.Unmarshal([]byte(extractJSON(resp)), roleDef); err != nil || roleDef.Name == "" {
		f.fillDomainTemplate(roleDef, domain, goal)
	} else {
		// 确保关键字段正确
		// 即使 LLM 改了 type/lifecycle/can_be_called，也要强制覆盖回 Domain 约束
		roleDef.ID = fmt.Sprintf("domain_%s_%d", sanitizeID(domain), f.seq.Add(1))
		roleDef.Type = enums.RoleTypeDomain
		roleDef.Lifecycle = enums.RoleLifecycleSession
		roleDef.CanBeCalled = false
		// LLM 未返回模型配置时，回退到动态模板配置
		if roleDef.ModelConfig.Provider == "" && roleDef.ModelConfig.Model == "" {
			roleDef.ModelConfig = f.dynamicTemplateModelConfig("domain_template")
		}
	}

	return roleDef, nil
}

// fillDomainTemplate 用默认模板填充 RoleDefinition。
// 当 LLM 不可用 / 输出无效时调用，保证图能继续跑。
// 参数：
//   - roleDef：待填充的定义（ID/Type 已由调用方设置）。
//   - domain / goal：用于拼接提示词文案。
func (f *RoleFactory) fillDomainTemplate(roleDef *types.RoleDefinition, domain, goal string) {
	roleDef.Name = domain + "负责人"
	roleDef.Description = fmt.Sprintf("负责%s领域的上下文管理与任务分发", domain)
	roleDef.SystemPrompt = fmt.Sprintf("你是%s领域的负责人。你的职责是：\n1. 管理该领域的上下文信息\n2. 分析任务并分发给合适的助手\n3. 汇总助手结果并输出\n\n领域目标: %s", domain, goal)
	roleDef.Keywords = []string{domain, goal}
	roleDef.Skills = []string{"任务分析", "上下文管理", "结果汇总"}
	roleDef.ModelConfig = f.dynamicTemplateModelConfig("domain_template")
}

// generateAssistantRoleDef 让大模型生成助手角色定义；无 modelFactory 时使用模板。
// 与 generateDomainRoleDef 同构，区别：
//   - Type=dynamic, Lifecycle=task, CanBeCalled=true。
//   - 写入 Parents 字段建立调用关系。
func (f *RoleFactory) generateAssistantRoleDef(ctx context.Context, taskDesc, parentDefID string) (*types.RoleDefinition, error) {
	roleDef := &types.RoleDefinition{
		ID:          fmt.Sprintf("assistant_%d", f.seq.Add(1)),
		Type:        enums.RoleTypeDynamic,
		Lifecycle:   enums.RoleLifecycleTask, // 任务级，1h 过期
		CanBeCalled: true,                    // 助手可被父 DomainAgent 调用
		Parents:     []string{parentDefID},   // 建立调用关系
	}
	if f.modelFactory == nil {
		f.fillAssistantTemplate(roleDef, taskDesc)
		return roleDef, nil
	}

	prompt := fmt.Sprintf(`你需要为以下任务创建一个专门的AI助手角色定义。

任务描述: %s
父角色ID: %s

请生成一个JSON格式的角色定义，包含以下字段:
- id: 角色唯一标识（小写英文+下划线）
- name: 角色显示名称
- type: 必须是 "dynamic"
- lifecycle: "task"（单次任务后消亡）
- description: 角色职责描述
- system_prompt: 系统提示词，定义该助手专注处理此任务
- keywords: 关键词数组
- skills: 技能列表
- can_be_called: true（助手可被调用）

只输出JSON，不要其他内容。`, taskDesc, parentDefID)

	llmClient, err := f.modelFactory.GetDomainModel(ctx)
	if err != nil {
		f.fillAssistantTemplate(roleDef, taskDesc)
		return roleDef, nil
	}

	resp, err := llmClient.Generate(ctx, prompt)
	if err != nil {
		f.fillAssistantTemplate(roleDef, taskDesc)
		return roleDef, nil
	}

	if err := json.Unmarshal([]byte(extractJSON(resp)), roleDef); err != nil || roleDef.Name == "" {
		f.fillAssistantTemplate(roleDef, taskDesc)
	} else {
		// 强制覆盖关键字段，防止 LLM 篡改
		roleDef.ID = fmt.Sprintf("assistant_%d", f.seq.Add(1))
		roleDef.Type = enums.RoleTypeDynamic
		roleDef.Lifecycle = enums.RoleLifecycleTask
		roleDef.CanBeCalled = true
		roleDef.Parents = []string{parentDefID}
		// LLM 未返回模型配置时，回退到动态模板配置
		if roleDef.ModelConfig.Provider == "" && roleDef.ModelConfig.Model == "" {
			roleDef.ModelConfig = f.dynamicTemplateModelConfig("assistant_template")
		}
	}

	return roleDef, nil
}

// fillAssistantTemplate 用默认模板填充。
// 与 fillDomainTemplate 同理，离线兜底。
func (f *RoleFactory) fillAssistantTemplate(roleDef *types.RoleDefinition, taskDesc string) {
	roleDef.Name = "临时助手"
	roleDef.Description = taskDesc
	roleDef.SystemPrompt = fmt.Sprintf("你是一个专业助手。你的唯一任务是：%s\n\n请专注于此任务，不要处理无关事务。完成后立即返回结果。", taskDesc)
	roleDef.Keywords = extractKeywords(taskDesc) // 简单分词提取
	roleDef.Skills = []string{taskDesc}
	roleDef.ModelConfig = f.dynamicTemplateModelConfig("assistant_template")
}

// dynamicTemplateModelConfig 读取 dynamic_templates 中指定模板的模型配置。
// 未配置或模板不存在时返回零值，调用方（ModelFactory.resolveConfig）会回退到 DomainAgent 模型。
func (f *RoleFactory) dynamicTemplateModelConfig(templateID string) types.AgentModelConfig {
	if f.cfg == nil {
		return types.AgentModelConfig{}
	}
	tmpl := f.cfg.GetDynamicTemplate(templateID)
	if tmpl == nil {
		return types.AgentModelConfig{}
	}
	return tmpl.ModelConfig
}

// CreateSubDomainAgent 动态创建子领域Agent。
// 与 CreateDomainAgent 区别：不调 LLM，直接用模板构造（子领域通常由 DomainAgent 显式拆分，
// 不需要 LLM 重新造 system_prompt）。
// 参数：
//   - subDomain：子领域名。
//   - goal：子领域目标。
//   - parentDomainID：父 DomainAgent 的 Definition ID，写入 Parents。
//
// 返回：SubDomainAgent 实例。
// 副作用：注册动态角色定义 + 创建实例（session 生命周期）。
func (f *RoleFactory) CreateSubDomainAgent(ctx context.Context, sessionID, subDomain, goal string, parentDomainID string) (*types.RoleInstance, error) {
	roleDef := &types.RoleDefinition{
		ID:           fmt.Sprintf("subdomain_%s_%d", sanitizeID(subDomain), f.seq.Add(1)),
		Name:         subDomain + "子领域负责人",
		Type:         enums.RoleTypeSubDomain,
		Lifecycle:    enums.RoleLifecycleSession,
		Description:  fmt.Sprintf("负责%s子领域的任务执行与结果汇总", subDomain),
		SystemPrompt: fmt.Sprintf("你是%s子领域的负责人。你的职责是：\n1. 管理该子领域的上下文信息\n2. 分析任务并分发给合适的助手\n3. 汇总助手结果并返回给父领域\n\n子领域目标: %s", subDomain, goal),
		Keywords:     []string{subDomain, goal},
		Skills:       []string{"子任务分析", "上下文管理", "结果汇总"},
		CanBeCalled:  true, // SubDomain 可被父 Domain 调用
		Parents:      []string{parentDomainID},
		ModelConfig:  f.dynamicTemplateModelConfig("domain_template"),
	}

	if err := f.registry.RegisterDynamicRole(roleDef); err != nil {
		return nil, fmt.Errorf("register subdomain role: %w", err)
	}
	if f.modelFactory != nil {
		f.modelFactory.RegisterDynamicModelConfig(roleDef.ID, roleDef.ModelConfig)
	}

	inst, err := f.registry.CreateInstance(roleDef.ID, sessionID, subDomain, parentDomainID)
	if err != nil {
		return nil, fmt.Errorf("create subdomain instance: %w", err)
	}

	return inst, nil
}

// findDomainAgent 查找会话中指定领域的DomainAgent。
// 用途：CreateDomainAgent 第一步去重，避免同会话同领域重复创建。
// 参数：
//   - sessionID：会话 ID。
//   - domain：领域名。
//
// 返回：已存在的实例；无则 nil。
func (f *RoleFactory) findDomainAgent(sessionID, domain string) *types.RoleInstance {
	for _, inst := range f.registry.GetInstancesBySession(sessionID) {
		// 类型必须是 Domain，且 domain 字段匹配
		if inst.Type == enums.RoleTypeDomain && inst.Domain == domain {
			return inst
		}
	}
	return nil
}

// sanitizeID 清理ID中的非法字符（保留Unicode字母/数字）。
// 用途：把领域名（可能含中文/空格/符号）转成合法的 ID 片段。
// 规则：字母数字保留；空格/连字符/下划线统一转下划线；其余字符丢弃。
func sanitizeID(s string) string {
	var result strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			result.WriteRune(r) // 保留字母数字（含中文）
		} else if r == ' ' || r == '-' || r == '_' {
			result.WriteRune('_') // 分隔符统一
		}
		// 其余字符丢弃
	}
	return result.String()
}

// extractKeywords 从任务描述中提取关键词。
// 简单分词：按空白切分，保留长度 >=2 的词，最多取前 5 个。
// 用途：fillAssistantTemplate 给 Definition.Keywords 填充，供后续路由匹配。
func extractKeywords(taskDesc string) []string {
	words := strings.Fields(taskDesc) // 按空白切分
	var result []string
	for _, w := range words {
		if len(w) >= 2 { // 过滤单字符噪声
			result = append(result, w)
		}
	}
	if len(result) > 5 {
		result = result[:5] // 限制关键词数量
	}
	return result
}

// extractJSON 从LLM响应中提取并修复JSON块。
// LLM 经常把 JSON 包在 ```json ... ``` 代码块里，或前后带解释文字，
// 还可能使用单引号、尾部逗号等不规范写法。本函数做以下容错：
//  1. 剥离 ```json / ``` 代码块标记。
//  2. 删除 // 行注释与 /* */ 块注释（LLM 偶尔加解释性注释）。
//  3. 数组优先提取：取第一个 '[' 到最后一个 ']'；否则对象提取。
//  4. 修复尾部逗号（`,]` / `,}`）。
//  5. 把 JSON 键/值的单引号包装统一为双引号（简单启发式，不破坏内嵌英文缩写）。
//
// 返回：修复后的 JSON 字符串；无法提取则返回原串。
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	// 1. 剥离代码块
	if idx := strings.Index(s, "```json"); idx != -1 {
		s = s[idx+7:]
		if end := strings.Index(s, "```"); end != -1 {
			s = s[:end]
		}
	} else if idx := strings.Index(s, "```"); idx != -1 {
		s = s[idx+3:]
		if end := strings.Index(s, "```"); end != -1 {
			s = s[:end]
		}
	}
	s = strings.TrimSpace(s)
	// 2. 删除注释
	s = removeComments(s)
	// 3. 提取 JSON 主体
	var body string
	if start := strings.Index(s, "["); start != -1 {
		if end := strings.LastIndex(s, "]"); end > start {
			body = s[start : end+1]
		}
	}
	if body == "" {
		start := strings.Index(s, "{")
		end := strings.LastIndex(s, "}")
		if start != -1 && end > start {
			body = s[start : end+1]
		}
	}
	if body == "" {
		return s
	}
	// 4. 修复尾部逗号
	body = fixTrailingCommas(body)
	// 5. 单引号 → 双引号（仅处理作为 JSON 字符串边界的单引号）
	body = normalizeJSONQuotes(body)
	return body
}

// removeComments 删除 s 中的 // 行注释与 /* */ 块注释。
func removeComments(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); {
		if i+1 < len(s) && s[i] == '/' && s[i+1] == '/' {
			// 跳过到行尾
			for i < len(s) && s[i] != '\n' {
				i++
			}
			continue
		}
		if i+1 < len(s) && s[i] == '/' && s[i+1] == '*' {
			// 跳过到 */
			i += 2
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			if i+1 < len(s) {
				i += 2
			}
			continue
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

// fixTrailingCommas 删除对象/数组最后一个元素后的多余逗号。
func fixTrailingCommas(s string) string {
	// 用临时占位避免连续替换破坏结构：先把 ",]" / ",}" 替换，再恢复
	s = strings.ReplaceAll(s, ",]", "]")
	s = strings.ReplaceAll(s, ",}", "}")
	return s
}

// normalizeJSONQuotes 把作为 JSON 字符串边界的单引号统一替换为双引号。
// 采用简单状态机：在 JSON 值区域遇到 ' 且未在双引号字符串内时视为字符串边界。
func normalizeJSONQuotes(s string) string {
	runes := []rune(s)
	var out strings.Builder
	inDouble := false
	inSingle := false
	for i := 0; i < len(runes); {
		r := runes[i]
		switch r {
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
			out.WriteRune(r)
		case '\'':
			if inDouble {
				// 在双引号字符串内，原样保留（可能是缩写或嵌套）
				out.WriteRune(r)
				i++
				continue
			}
			inSingle = !inSingle
			out.WriteRune('"')
		case '\\':
			out.WriteRune(r)
			// 转义下一字符原样输出，避免状态机误判，并跳过该字符
			if i+1 < len(runes) {
				out.WriteRune(runes[i+1])
				i += 2
				continue
			}
		default:
			out.WriteRune(r)
		}
		i++
	}
	return out.String()
}
