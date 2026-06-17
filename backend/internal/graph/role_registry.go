package graph

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// RoleRegistry 角色注册表（固定+动态）
type RoleRegistry struct {
	mu sync.RWMutex

	// 固定角色定义（来自配置文件）
	fixedDefs map[string]*types.RoleDefinition

	// 动态角色定义（大模型生成）
	dynamicDefs map[string]*types.RoleDefinition

	// 角色实例（运行时）
	instances map[string]*types.RoleInstance

	// 配置引用
	cfg *config.RoleConfigFile

	// 实例ID序号
	seq atomic.Int64
}

// NewRoleRegistry 创建角色注册表
func NewRoleRegistry(cfg *config.RoleConfigFile) *RoleRegistry {
	r := &RoleRegistry{
		fixedDefs:   make(map[string]*types.RoleDefinition),
		dynamicDefs: make(map[string]*types.RoleDefinition),
		instances:   make(map[string]*types.RoleInstance),
		cfg:         cfg,
	}

	// 加载固定角色
	for i := range cfg.FixedRoles {
		def := &cfg.FixedRoles[i]
		r.fixedDefs[def.ID] = def
	}

	return r
}

// GetRoleDef 获取角色定义（先查固定，再查动态）
func (r *RoleRegistry) GetRoleDef(roleDefID string) *types.RoleDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if def, ok := r.fixedDefs[roleDefID]; ok {
		return def
	}
	return r.dynamicDefs[roleDefID]
}

// GetFixedRoleDefs 获取所有固定角色定义
func (r *RoleRegistry) GetFixedRoleDefs() []*types.RoleDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*types.RoleDefinition
	for _, def := range r.fixedDefs {
		result = append(result, def)
	}
	return result
}

// GetAssistantRoleDefs 获取所有助手角色定义（固定+动态）
func (r *RoleRegistry) GetAssistantRoleDefs() []*types.RoleDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*types.RoleDefinition
	for _, def := range r.fixedDefs {
		if def.Type == types.RoleTypeFixed {
			result = append(result, def)
		}
	}
	for _, def := range r.dynamicDefs {
		if def.Type == types.RoleTypeDynamic {
			result = append(result, def)
		}
	}
	return result
}

// RegisterDynamicRole 注册动态角色定义
func (r *RoleRegistry) RegisterDynamicRole(def *types.RoleDefinition) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.fixedDefs[def.ID]; exists {
		return fmt.Errorf("role %s conflicts with fixed role", def.ID)
	}
	r.dynamicDefs[def.ID] = def
	return nil
}

// CreateInstance 创建角色实例
func (r *RoleRegistry) CreateInstance(roleDefID, sessionID string, domain string, parentID string) (*types.RoleInstance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	def := r.fixedDefs[roleDefID]
	if def == nil {
		def = r.dynamicDefs[roleDefID]
	}
	if def == nil {
		return nil, fmt.Errorf("role def %s not found", roleDefID)
	}

	instID := fmt.Sprintf("%s_%s_%d", roleDefID, sessionID[:min(8, len(sessionID))], r.seq.Add(1))
	inst := &types.RoleInstance{
		ID:         instID,
		RoleDefID:  roleDefID,
		Type:       def.Type,
		Lifecycle:  def.Lifecycle,
		SessionID:  sessionID,
		Domain:     domain,
		Status:     types.RoleStatusIdle,
		CreatedAt:  time.Now(),
		ParentID:   parentID,
		Children:   make([]string, 0),
	}

	// 设置过期时间
	if def.Lifecycle == types.RoleLifecycleSession {
		t := inst.CreatedAt.Add(24 * time.Hour)
		inst.ExpiresAt = &t
	} else if def.Lifecycle == types.RoleLifecycleTask {
		t := inst.CreatedAt.Add(1 * time.Hour)
		inst.ExpiresAt = &t
	}

	r.instances[instID] = inst

	// 更新父角色的children
	if parentID != "" {
		if parent, ok := r.instances[parentID]; ok {
			parent.Children = append(parent.Children, instID)
		}
	}

	return inst, nil
}

// GetInstance 获取角色实例
func (r *RoleRegistry) GetInstance(instID string) *types.RoleInstance {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.instances[instID]
}

// GetInstancesBySession 获取会话的所有角色实例
func (r *RoleRegistry) GetInstancesBySession(sessionID string) []*types.RoleInstance {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*types.RoleInstance
	for _, inst := range r.instances {
		if inst.SessionID == sessionID {
			result = append(result, inst)
		}
	}
	return result
}

// UpdateInstanceStatus 更新实例状态
func (r *RoleRegistry) UpdateInstanceStatus(instID string, status types.RoleStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if inst, ok := r.instances[instID]; ok {
		inst.Status = status
	}
}

// RemoveInstance 移除角色实例
func (r *RoleRegistry) RemoveInstance(instID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.instances, instID)
}

// CleanupExpired 清理过期实例
func (r *RoleRegistry) CleanupExpired() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	removed := 0
	for id, inst := range r.instances {
		if inst.ExpiresAt != nil && inst.ExpiresAt.Before(now) {
			delete(r.instances, id)
			removed++
		}
	}
	return removed
}

// MatchRoleByGoal 根据目标匹配角色定义
func (r *RoleRegistry) MatchRoleByGoal(goal string) *types.RoleDefinition {
	goalLower := strings.ToLower(goal)
	var bestMatch *types.RoleDefinition
	bestScore := 0

	// 先匹配固定角色
	for _, def := range r.fixedDefs {
		score := matchScore(def, goalLower)
		if score > bestScore {
			bestScore = score
			bestMatch = def
		}
	}

	// 再匹配动态角色
	for _, def := range r.dynamicDefs {
		score := matchScore(def, goalLower)
		if score > bestScore {
			bestScore = score
			bestMatch = def
		}
	}

	return bestMatch
}

// CanCall 检查实例caller是否可以调用角色定义callee
func (r *RoleRegistry) CanCall(callerInstID, calleeDefID string) bool {
	caller := r.GetInstance(callerInstID)
	if caller == nil {
		return false
	}
	callee := r.GetRoleDef(calleeDefID)
	if callee == nil {
		return false
	}
	// DomainAgent/SubDomainAgent可以调用任何助手
	if caller.Type == types.RoleTypeDomain || caller.Type == types.RoleTypeSubDomain {
		return callee.Type == types.RoleTypeFixed || callee.Type == types.RoleTypeDynamic || callee.Type == types.RoleTypeSubDomain
	}
	// MetaAgent可以调用DomainAgent/SubDomainAgent
	if caller.Type == types.RoleTypeMeta {
		return callee.Type == types.RoleTypeDomain || callee.Type == types.RoleTypeSubDomain
	}
	// 固定角色：委托给配置检查Parents等
	return r.cfg.CanCall(caller.RoleDefID, calleeDefID)
}

func matchScore(def *types.RoleDefinition, goalLower string) int {
	score := 0
	for _, kw := range def.Keywords {
		if strings.Contains(goalLower, strings.ToLower(kw)) {
			score += 10
		}
	}
	if strings.Contains(strings.ToLower(def.Description), goalLower) {
		score += 5
	}
	if strings.Contains(goalLower, strings.ToLower(def.Name)) {
		score += 8
	}
	return score
}
