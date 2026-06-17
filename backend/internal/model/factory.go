package model

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// LLMClient 大模型客户端接口（与 graph 包兼容）
type LLMClient interface {
	Generate(ctx context.Context, prompt string) (string, error)
}

// TemperatureAware 可选接口：支持 per-call temperature 覆盖。
// EinoClient 实现此接口；mockClient 不实现，调用方需走 type assertion。
type TemperatureAware interface {
	GenerateWithOptions(ctx context.Context, prompt string, temperature float64) (string, error)
}

// GenerateWithTemperature 工具函数：若客户端实现 TemperatureAware
// 则用 per-call 温度，否则退回到 Generate。
func GenerateWithTemperature(ctx context.Context, c LLMClient, prompt string, temperature float64) (string, error) {
	if t, ok := c.(TemperatureAware); ok {
		return t.GenerateWithOptions(ctx, prompt, temperature)
	}
	return c.Generate(ctx, prompt)
}

// mockClient 无API Key时的回退客户端
type mockClient struct{}

func (m *mockClient) Generate(ctx context.Context, prompt string) (string, error) {
	return fmt.Sprintf("[模拟响应] 收到请求长度: %d 字符", len(prompt)), nil
}

// ModelFactory ChatModel 工厂，按角色缓存模型实例
type ModelFactory struct {
	mu     sync.RWMutex
	models map[string]LLMClient // key: roleDefID or "meta" or "domain"
	cfg    *config.RoleConfigFile
}

// NewModelFactory 创建模型工厂
func NewModelFactory(cfg *config.RoleConfigFile) *ModelFactory {
	return &ModelFactory{
		models: make(map[string]LLMClient),
		cfg:    cfg,
	}
}

// GetModel 获取指定角色的 LLMClient（带缓存）
func (f *ModelFactory) GetModel(ctx context.Context, roleDefID string) (LLMClient, error) {
	f.mu.RLock()
	if m, ok := f.models[roleDefID]; ok {
		f.mu.RUnlock()
		return m, nil
	}
	f.mu.RUnlock()

	f.mu.Lock()
	defer f.mu.Unlock()

	// 双重检查
	if m, ok := f.models[roleDefID]; ok {
		return m, nil
	}

	// 根据角色ID选择配置
	modelCfg := f.resolveConfig(roleDefID)

	// 无API Key时回退到Mock
	if strings.TrimSpace(modelCfg.APIKey) == "" {
		f.models[roleDefID] = &mockClient{}
		return f.models[roleDefID], nil
	}

	client, err := NewEinoClient(ctx, modelCfg)
	if err != nil {
		return nil, fmt.Errorf("create eino client for %s: %w", roleDefID, err)
	}

	f.models[roleDefID] = client
	return client, nil
}

// GetMetaModel 获取 MetaAgent 的模型
func (f *ModelFactory) GetMetaModel(ctx context.Context) (LLMClient, error) {
	return f.GetModel(ctx, "meta")
}

// GetDomainModel 获取 DomainAgent 的模型（所有 DomainAgent 共用）
func (f *ModelFactory) GetDomainModel(ctx context.Context) (LLMClient, error) {
	return f.GetModel(ctx, "domain")
}

// resolveConfig 根据角色ID解析模型配置
func (f *ModelFactory) resolveConfig(roleDefID string) types.AgentModelConfig {
	switch roleDefID {
	case "meta":
		return f.cfg.MetaAgent.ModelConfig
	case "domain":
		return f.cfg.DomainAgent.ModelConfig
	default:
		// 查找固定角色配置
		if role := f.cfg.GetFixedRole(roleDefID); role != nil {
			return role.ModelConfig
		}
		// 回退到 DomainAgent 配置（动态助手）
		return f.cfg.DomainAgent.ModelConfig
	}
}

// WarmUp 预热常用模型（避免首次调用延迟）
func (f *ModelFactory) WarmUp(ctx context.Context) error {
	// 预热 MetaAgent 模型
	if _, err := f.GetMetaModel(ctx); err != nil {
		return fmt.Errorf("warmup meta model: %w", err)
	}
	// 预热 DomainAgent 模型
	if _, err := f.GetDomainModel(ctx); err != nil {
		return fmt.Errorf("warmup domain model: %w", err)
	}
	return nil
}
