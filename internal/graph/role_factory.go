package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"unicode"

	"github.com/blockmemory/agent/internal/model"
	"github.com/blockmemory/agent/pkg/config"
	"github.com/blockmemory/agent/pkg/types"
)

// RoleFactory 角色工厂（动态创建临时角色）
type RoleFactory struct {
	registry    *RoleRegistry
	modelFactory *model.ModelFactory
	cfg         *config.RoleConfigFile
	seq         atomic.Int64
}

// NewRoleFactory 创建角色工厂
func NewRoleFactory(registry *RoleRegistry, modelFactory *model.ModelFactory, cfg *config.RoleConfigFile) *RoleFactory {
	return &RoleFactory{
		registry:     registry,
		modelFactory: modelFactory,
		cfg:          cfg,
	}
}

// CreateDomainAgent 动态创建领域Agent
func (f *RoleFactory) CreateDomainAgent(ctx context.Context, sessionID, domain, goal string, parentID string) (*types.RoleInstance, error) {
	// 1. 检查是否已存在该领域的DomainAgent
	existing := f.findDomainAgent(sessionID, domain)
	if existing != nil {
		return existing, nil
	}

	// 2. 调用大模型生成角色定义
	roleDef, err := f.generateDomainRoleDef(ctx, domain, goal)
	if err != nil {
		return nil, fmt.Errorf("generate domain role: %w", err)
	}

	// 3. 注册动态角色定义
	if err := f.registry.RegisterDynamicRole(roleDef); err != nil {
		return nil, fmt.Errorf("register dynamic role: %w", err)
	}

	// 4. 创建实例
	inst, err := f.registry.CreateInstance(roleDef.ID, sessionID, domain, parentID)
	if err != nil {
		return nil, fmt.Errorf("create domain instance: %w", err)
	}

	return inst, nil
}

// CreateAssistant 动态创建助手角色
func (f *RoleFactory) CreateAssistant(ctx context.Context, sessionID, taskDesc string, parentID string, parentDefID string) (*types.RoleInstance, error) {
	// 1. 调用大模型生成助手角色定义
	roleDef, err := f.generateAssistantRoleDef(ctx, taskDesc, parentDefID)
	if err != nil {
		return nil, fmt.Errorf("generate assistant role: %w", err)
	}

	// 2. 注册动态角色定义
	if err := f.registry.RegisterDynamicRole(roleDef); err != nil {
		return nil, fmt.Errorf("register dynamic role: %w", err)
	}

	// 3. 创建实例
	inst, err := f.registry.CreateInstance(roleDef.ID, sessionID, "", parentID)
	if err != nil {
		return nil, fmt.Errorf("create assistant instance: %w", err)
	}

	return inst, nil
}

// generateDomainRoleDef 让大模型生成领域角色定义
func (f *RoleFactory) generateDomainRoleDef(ctx context.Context, domain, goal string) (*types.RoleDefinition, error) {
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

	llmClient, err := f.modelFactory.GetDomainModel(ctx)
	if err != nil {
		return nil, fmt.Errorf("get domain model: %w", err)
	}

	resp, err := llmClient.Generate(ctx, prompt)
	if err != nil {
		return nil, err
	}

	// 尝试解析LLM返回的JSON，失败则用模板
	roleDef := &types.RoleDefinition{
		ID:          fmt.Sprintf("domain_%s_%d", sanitizeID(domain), f.seq.Add(1)),
		Type:        types.RoleTypeDomain,
		Lifecycle:   types.RoleLifecycleSession,
		CanBeCalled: false,
	}

	if err := json.Unmarshal([]byte(extractJSON(resp)), roleDef); err != nil || roleDef.Name == "" {
		// JSON解析失败，使用模板
		roleDef.Name = domain + "负责人"
		roleDef.Description = fmt.Sprintf("负责%s领域的上下文管理与任务分发", domain)
		roleDef.SystemPrompt = fmt.Sprintf("你是%s领域的负责人。你的职责是：\n1. 管理该领域的上下文信息\n2. 分析任务并分发给合适的助手\n3. 汇总助手结果并输出\n\n领域目标: %s", domain, goal)
		roleDef.Keywords = []string{domain, goal}
		roleDef.Skills = []string{"任务分析", "上下文管理", "结果汇总"}
	} else {
		// 确保关键字段正确
		roleDef.ID = fmt.Sprintf("domain_%s_%d", sanitizeID(domain), f.seq.Add(1))
		roleDef.Type = types.RoleTypeDomain
		roleDef.Lifecycle = types.RoleLifecycleSession
		roleDef.CanBeCalled = false
	}

	return roleDef, nil
}

// generateAssistantRoleDef 让大模型生成助手角色定义
func (f *RoleFactory) generateAssistantRoleDef(ctx context.Context, taskDesc, parentDefID string) (*types.RoleDefinition, error) {
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
		return nil, fmt.Errorf("get domain model: %w", err)
	}

	resp, err := llmClient.Generate(ctx, prompt)
	if err != nil {
		return nil, err
	}

	roleDef := &types.RoleDefinition{
		ID:          fmt.Sprintf("assistant_%d", f.seq.Add(1)),
		Type:        types.RoleTypeDynamic,
		Lifecycle:   types.RoleLifecycleTask,
		CanBeCalled: true,
		Parents:     []string{parentDefID},
	}

	if err := json.Unmarshal([]byte(extractJSON(resp)), roleDef); err != nil || roleDef.Name == "" {
		roleDef.Name = "临时助手"
		roleDef.Description = taskDesc
		roleDef.SystemPrompt = fmt.Sprintf("你是一个专业助手。你的唯一任务是：%s\n\n请专注于此任务，不要处理无关事务。完成后立即返回结果。", taskDesc)
		roleDef.Keywords = extractKeywords(taskDesc)
		roleDef.Skills = []string{taskDesc}
	} else {
		roleDef.ID = fmt.Sprintf("assistant_%d", f.seq.Add(1))
		roleDef.Type = types.RoleTypeDynamic
		roleDef.Lifecycle = types.RoleLifecycleTask
		roleDef.CanBeCalled = true
		roleDef.Parents = []string{parentDefID}
	}

	return roleDef, nil
}

// CreateSubDomainAgent 动态创建子领域Agent
func (f *RoleFactory) CreateSubDomainAgent(ctx context.Context, sessionID, subDomain, goal string, parentDomainID string) (*types.RoleInstance, error) {
	roleDef := &types.RoleDefinition{
		ID:          fmt.Sprintf("subdomain_%s_%d", sanitizeID(subDomain), f.seq.Add(1)),
		Name:        subDomain + "子领域负责人",
		Type:        types.RoleTypeSubDomain,
		Lifecycle:   types.RoleLifecycleSession,
		Description: fmt.Sprintf("负责%s子领域的任务执行与结果汇总", subDomain),
		SystemPrompt: fmt.Sprintf("你是%s子领域的负责人。你的职责是：\n1. 管理该子领域的上下文信息\n2. 分析任务并分发给合适的助手\n3. 汇总助手结果并返回给父领域\n\n子领域目标: %s", subDomain, goal),
		Keywords:    []string{subDomain, goal},
		Skills:      []string{"子任务分析", "上下文管理", "结果汇总"},
		CanBeCalled: true,
		Parents:     []string{parentDomainID},
	}

	if err := f.registry.RegisterDynamicRole(roleDef); err != nil {
		return nil, fmt.Errorf("register subdomain role: %w", err)
	}

	inst, err := f.registry.CreateInstance(roleDef.ID, sessionID, subDomain, parentDomainID)
	if err != nil {
		return nil, fmt.Errorf("create subdomain instance: %w", err)
	}

	return inst, nil
}

// findDomainAgent 查找会话中指定领域的DomainAgent
func (f *RoleFactory) findDomainAgent(sessionID, domain string) *types.RoleInstance {
	for _, inst := range f.registry.GetInstancesBySession(sessionID) {
		if inst.Type == types.RoleTypeDomain && inst.Domain == domain {
			return inst
		}
	}
	return nil
}

// sanitizeID 清理ID中的非法字符（保留Unicode字母/数字）
func sanitizeID(s string) string {
	var result strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			result.WriteRune(r)
		} else if r == ' ' || r == '-' || r == '_' {
			result.WriteRune('_')
		}
	}
	return result.String()
}

// extractKeywords 从任务描述中提取关键词
func extractKeywords(taskDesc string) []string {
	words := strings.Fields(taskDesc)
	var result []string
	for _, w := range words {
		if len(w) >= 2 {
			result = append(result, w)
		}
	}
	if len(result) > 5 {
		result = result[:5]
	}
	return result
}

// extractJSON 从LLM响应中提取JSON块
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	// 尝试提取 ```json ... ``` 代码块
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
	// 提取第一个 { 到最后一个 }
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start != -1 && end > start {
		return s[start : end+1]
	}
	return s
}
