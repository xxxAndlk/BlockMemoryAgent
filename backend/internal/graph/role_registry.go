package graph

// 角色注册表：管理"角色定义"和"角色实例"两层映射。
//   - 角色定义（RoleDefinition）：描述一个角色的能力（system_prompt、keywords 等），可复用。
//   - 角色实例（RoleInstance）：某次会话中按 Definition 创建的运行时实体，有生命周期。
// 角色定义分两类：固定（来自 roles.yaml）+ 动态（LLM 生成）。

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// RoleRegistry 角色注册表（固定+动态）。
// 持有 fixedDefs / dynamicDefs / instances 三张表，所有读写均由 mu 保护。
// seq 提供实例 ID 自增序号，避免同会话内重复。
type RoleRegistry struct {
	mu sync.RWMutex // 保护下面三张 map

	// 固定角色定义（来自配置文件）
	// key = RoleDefinition.ID
	fixedDefs map[string]*types.RoleDefinition

	// 动态角色定义（大模型生成）
	// key = RoleDefinition.ID
	dynamicDefs map[string]*types.RoleDefinition

	// 角色实例（运行时）
	// key = RoleInstance.ID
	instances map[string]*types.RoleInstance

	// 配置引用
	// 保留原始 RoleConfigFile 以查询 Parents 等静态权限关系
	cfg *config.RoleConfigFile

	// 实例ID序号
	// atomic 保证并发安全自增，拼到实例 ID 末尾避免冲突
	seq atomic.Int64
}

// NewRoleRegistry 创建角色注册表。
// 参数：
//   - cfg：从 roles.yaml 解析出的角色配置，含 fixed_roles[] / dynamic_templates。
//
// 返回：已加载所有固定角色定义的注册表；动态角色按需 RegisterDynamicRole。
func NewRoleRegistry(cfg *config.RoleConfigFile) *RoleRegistry {
	r := &RoleRegistry{
		fixedDefs:   make(map[string]*types.RoleDefinition),
		dynamicDefs: make(map[string]*types.RoleDefinition),
		instances:   make(map[string]*types.RoleInstance),
		cfg:         cfg,
	}

	// 加载固定角色：把 cfg.FixedRoles 切片转成 map，便于按 ID 查找
	for i := range cfg.FixedRoles {
		def := &cfg.FixedRoles[i] // 取地址避免拷贝大结构
		r.fixedDefs[def.ID] = def
	}

	return r
}

// GetRoleDef 获取角色定义（先查固定，再查动态）。
// 参数：
//   - roleDefID：角色定义 ID。
//
// 返回：定义指针；不存在返回 nil。
// 并发安全：持读锁。
func (r *RoleRegistry) GetRoleDef(roleDefID string) *types.RoleDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// 优先固定角色（命中率高）
	if def, ok := r.fixedDefs[roleDefID]; ok {
		return def
	}
	return r.dynamicDefs[roleDefID] // 回退到动态角色
}

// GetMetaRoleDef 获取 MetaAgent 自身的角色定义。
//
// MetaAgent 配置来自 roles.yaml 的 meta_agent 段，不属于 fixed_roles，
// 因此不会出现在 fixedDefs 中；本方法根据 cfg.MetaAgent.SystemPrompt 构造一个
// 临时的 RoleDefinition，供 RouteDirectTool 等需要 MetaAgent 自身 system prompt 的路径使用。
//
// 返回：定义指针；配置未加载返回 nil。
// 并发安全：只读 cfg，无锁。
func (r *RoleRegistry) GetMetaRoleDef() *types.RoleDefinition {
	if r.cfg == nil {
		return nil
	}
	return &types.RoleDefinition{
		ID:           "meta",
		Name:         "MetaAgent",
		Type:         enums.RoleTypeMeta,
		SystemPrompt: r.cfg.MetaAgent.SystemPrompt,
	}
}

// GetFixedRoleDefs 获取所有固定角色定义。
// 用途：MetaAgent 构建系统提示词时把所有可调用的固定角色列给 LLM。
// 返回：固定角色定义切片（无序）；表为空时返回 nil。
// 并发安全：持读锁。
func (r *RoleRegistry) GetFixedRoleDefs() []*types.RoleDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*types.RoleDefinition
	for _, def := range r.fixedDefs {
		result = append(result, def)
	}
	return result
}

// GetAssistantRoleDefs 获取所有助手角色定义（固定+动态）。
// 助手 = RoleTypeFixed + RoleTypeDynamic（不含 Meta/Domain/SubDomain）。
// 用途：DomainAgent 在分发子任务时，把候选助手列表喂给 LLM 选型。
// 并发安全：持读锁。
func (r *RoleRegistry) GetAssistantRoleDefs() []*types.RoleDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*types.RoleDefinition
	// 固定助手
	for _, def := range r.fixedDefs {
		if def.Type == enums.RoleTypeFixed {
			result = append(result, def)
		}
	}
	// 动态助手
	for _, def := range r.dynamicDefs {
		if def.Type == enums.RoleTypeDynamic {
			result = append(result, def)
		}
	}
	return result
}

// RegisterDynamicRole 注册动态角色定义。
// 副作用：写入 dynamicDefs；若 ID 与固定角色冲突则拒绝（避免覆盖配置）。
// 并发安全：持写锁。
func (r *RoleRegistry) RegisterDynamicRole(def *types.RoleDefinition) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// 不允许动态角色覆盖固定角色
	if _, exists := r.fixedDefs[def.ID]; exists {
		return fmt.Errorf("role %s conflicts with fixed role", def.ID)
	}
	r.dynamicDefs[def.ID] = def
	return nil
}

// CreateInstance 创建角色实例。
// 流程：查 Definition → 生成实例 ID → 设置过期时间 → 挂到父实例的 Children。
// 参数：
//   - roleDefID：角色定义 ID（固定或动态皆可）。
//   - sessionID：所属会话 ID。
//   - domain：领域名（仅 Domain/SubDomain 实例填，Assistant 留空）。
//   - parentID：父实例 ID（DomainAgent 的 parent 是 MetaAgent，Assistant 的 parent 是 DomainAgent）。
//
// 返回：新实例指针；定义不存在返回 error。
// 副作用：写入 instances 表；若 parentID 非空则追加到父实例的 Children 切片。
// 并发安全：持写锁（整个创建过程原子）。
func (r *RoleRegistry) CreateInstance(roleDefID, sessionID string, domain string, parentID string) (*types.RoleInstance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// 查定义：先固定后动态
	def := r.fixedDefs[roleDefID]
	if def == nil {
		def = r.dynamicDefs[roleDefID]
	}
	if def == nil {
		return nil, fmt.Errorf("role def %s not found", roleDefID)
	}

	// 生成实例 ID：roleDefID + sessionID 前 8 字符 + 自增序号
	// sessionID 前缀便于按会话筛日志；序号防同会话内冲突
	instID := fmt.Sprintf("%s_%s_%d", roleDefID, sessionID[:min(8, len(sessionID))], r.seq.Add(1))
	inst := &types.RoleInstance{
		ID:        instID,
		RoleDefID: roleDefID,
		Type:      def.Type,      // 类型继承自定义
		Lifecycle: def.Lifecycle, // 生命周期继承自定义
		SessionID: sessionID,
		Domain:    domain,
		Status:    enums.RoleStatusIdle, // 初始空闲
		CreatedAt: time.Now(),
		ParentID:  parentID,
		Children:  make([]string, 0),
	}

	// 设置过期时间：按生命周期策略
	// session 级 24h、task 级 1h，过期后由 CleanupExpired 清理
	if def.Lifecycle == enums.RoleLifecycleSession {
		t := inst.CreatedAt.Add(24 * time.Hour)
		inst.ExpiresAt = &t
	} else if def.Lifecycle == enums.RoleLifecycleTask {
		t := inst.CreatedAt.Add(1 * time.Hour)
		inst.ExpiresAt = &t
	}

	r.instances[instID] = inst

	// 更新父角色的children：建立调用树
	if parentID != "" {
		if parent, ok := r.instances[parentID]; ok {
			parent.Children = append(parent.Children, instID)
		}
	}

	return inst, nil
}

// GetInstance 获取角色实例。
// 参数：instID 实例 ID。
// 返回：实例指针；不存在返回 nil。
// 并发安全：持读锁。
func (r *RoleRegistry) GetInstance(instID string) *types.RoleInstance {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.instances[instID]
}

// GetInstancesBySession 获取会话的所有角色实例。
// 用途：findDomainAgent 按 sessionID + domain 去重；会话结束时批量回收。
// 返回：实例切片（无序）。
// 并发安全：持读锁。
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

// UpdateInstanceStatus 更新实例状态。
// 用途：节点开始执行时置 Active，结束后回 Idle。
// 并发安全：持写锁（虽然只改一个字段，但保持一致性）。
func (r *RoleRegistry) UpdateInstanceStatus(instID string, status enums.RoleStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if inst, ok := r.instances[instID]; ok {
		inst.Status = status
	}
}

// RemoveInstance 移除角色实例。
// 用途：会话结束 / 任务完成时显式回收；不会清理 Children 关系（由调用方负责）。
// 并发安全：持写锁。
func (r *RoleRegistry) RemoveInstance(instID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.instances, instID)
}

// CleanupExpired 清理过期实例。
// 由后台周期任务调用（或会话结束时手动调一次）。
// 返回：本次清理的实例数。
// 副作用：从 instances 表删除所有 ExpiresAt 早于 now 的实例。
// 并发安全：持写锁。
func (r *RoleRegistry) CleanupExpired() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	removed := 0
	for id, inst := range r.instances {
		// 只清理显式设了过期时间且已过期的实例
		if inst.ExpiresAt != nil && inst.ExpiresAt.Before(now) {
			delete(r.instances, id)
			removed++
		}
	}
	return removed
}

// MatchRoleByGoal 根据目标匹配角色定义。
// 算法：对每个候选 Definition 计算 matchScore，取最高分。
// 用途：MetaAgent 拿到用户 goal 后，先尝试匹配已有固定/动态角色，命中则复用，
//
//	避免每次都让 LLM 重新造角色。
//
// 参数：goal 用户原始目标文本。
// 返回：最佳匹配 Definition；无任何命中返回 nil。
// 并发安全：持读锁。
func (r *RoleRegistry) MatchRoleByGoal(goal string) *types.RoleDefinition {
	goalLower := strings.ToLower(goal) // 大小写不敏感匹配
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

// CanCall 检查实例caller是否可以调用角色定义callee。
// 权限模型（三层架构的核心约束）：
//   - Domain/SubDomain → 可调用任何 Assistant（fixed/dynamic/subdomain）。
//   - Meta → 可调用 Domain/SubDomain。
//   - 固定角色 → 委托给配置层 cfg.CanCall 查 Parents 关系。
//
// 参数：
//   - callerInstID：调用方实例 ID。
//   - calleeDefID：被调用方角色定义 ID。
//
// 返回：允许 true / 拒绝 false。
// 并发安全：依赖 GetInstance / GetRoleDef（均持读锁）。
func (r *RoleRegistry) CanCall(callerInstID, calleeDefID string) bool {
	caller := r.GetInstance(callerInstID)
	if caller == nil {
		return false // 调用方实例不存在 → 拒绝
	}
	callee := r.GetRoleDef(calleeDefID)
	if callee == nil {
		return false // 被调用方定义不存在 → 拒绝
	}
	// DomainAgent/SubDomainAgent可以调用任何助手
	if caller.Type == enums.RoleTypeDomain || caller.Type == enums.RoleTypeSubDomain {
		return callee.Type == enums.RoleTypeFixed || callee.Type == enums.RoleTypeDynamic || callee.Type == enums.RoleTypeSubDomain
	}
	// MetaAgent可以调用DomainAgent/SubDomainAgent
	if caller.Type == enums.RoleTypeMeta {
		return callee.Type == enums.RoleTypeDomain || callee.Type == enums.RoleTypeSubDomain
	}
	// 固定角色：委托给配置检查Parents等
	return r.cfg.CanCall(caller.RoleDefID, calleeDefID)
}

// matchScore 计算角色定义与目标文本的匹配分数。
// 评分规则：
//   - 关键词命中：+10/个（关键词列表是路由的主要信号）。
//   - 描述包含目标：+5（弱信号）。
//   - 目标包含角色名：+8（中信号）。
//
// 返回：累计分数；0 表示无任何命中。
func matchScore(def *types.RoleDefinition, goalLower string) int {
	score := 0
	for _, kw := range def.Keywords {
		if strings.Contains(goalLower, strings.ToLower(kw)) {
			score += 10 // 关键词命中权重最高
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
