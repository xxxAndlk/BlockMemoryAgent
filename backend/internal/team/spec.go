package team

// spec.go 团队定义与运行实例模型（C-01/C-02 首版）：
//   - Spec 是可复用、版本化的团队定义（配置文件加载 + 确定性校验）；
//   - Run 是一次运行实例，绑定 spec_version 与预算账本；
//   - 首版只做契约与校验，不替代现有 Dispatcher；接入派发属于后续适配工作。

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// DefaultMaxDepth 团队最大嵌套深度（含根团队）。
	DefaultMaxDepth = 3
	// DefaultMaxNodes 团队最大角色实例总数。
	DefaultMaxNodes = 32
)

// Spec 是版本化团队定义。
type Spec struct {
	ID            string              `yaml:"id" json:"id"`
	Version       string              `yaml:"version" json:"version"`
	Roles         []RoleSpec          `yaml:"roles" json:"roles"`
	Policies      Policies            `yaml:"policies" json:"policies"`
	DefaultBudget int64               `yaml:"default_budget" json:"default_budget"`
	DoD           []string            `yaml:"dod" json:"dod"`
	MemoryScopes  []string            `yaml:"memory_scopes" json:"memory_scopes"`
	Verification  *VerificationPolicy `yaml:"verification" json:"verification,omitempty"`
}

// RoleSpec 是团队内的角色包声明：角色本身仍由 role.Registry 定义，
// 这里只声明本团队如何使用它（工具、可派发关系、验收与实例上限）。
type RoleSpec struct {
	ID           string              `yaml:"id" json:"id"`
	RoleID       string              `yaml:"role_id" json:"role_id"`
	Tools        []string            `yaml:"tools" json:"tools"`
	CanCall      []string            `yaml:"can_call" json:"can_call"`
	DoD          []string            `yaml:"dod" json:"dod"`
	MaxInstances int                 `yaml:"max_instances" json:"max_instances"`
	Verification *VerificationPolicy `yaml:"verification" json:"verification,omitempty"` // 只允许收紧
}

// Policies 团队级策略。
type Policies struct {
	MaxDepth           int    `yaml:"max_depth" json:"max_depth"`
	MaxNodes           int    `yaml:"max_nodes" json:"max_nodes"`
	RequireIntegration bool   `yaml:"require_integration" json:"require_integration"`
	Approval           string `yaml:"approval" json:"approval"` // development|shared|production
}

// Run 是一次团队运行实例。
type Run struct {
	TeamID       string `json:"team_id"`
	SpecVersion  string `json:"spec_version"`
	SessionID    string `json:"session_id"`
	RootAgent    string `json:"root_agent"`
	Status       string `json:"status"` // pending|running|paused|done|failed|cancelled
	BudgetLedger int64  `json:"budget_ledger"`
}

// LoadFile 从 YAML 文件加载团队定义并校验。
func LoadFile(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("team spec read %s: %w", path, err)
	}
	return Parse(data)
}

// Parse 解析 YAML 团队定义并校验。
func Parse(data []byte) (*Spec, error) {
	var s Spec
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("team spec parse: %w", err)
	}
	if err := s.Normalize(); err != nil {
		return nil, err
	}
	return &s, nil
}

// Normalize 归一化并填充默认值；不合法值返回错误。
func (s *Spec) Normalize() error {
	s.ID = strings.TrimSpace(s.ID)
	s.Version = strings.TrimSpace(s.Version)
	if s.ID == "" {
		return fmt.Errorf("team spec: id is required")
	}
	if s.Version == "" {
		return fmt.Errorf("team spec %s: version is required", s.ID)
	}
	if len(s.Roles) == 0 {
		return fmt.Errorf("team spec %s: roles is required", s.ID)
	}
	if s.Policies.MaxDepth <= 0 {
		s.Policies.MaxDepth = DefaultMaxDepth
	}
	if s.Policies.MaxDepth > DefaultMaxDepth {
		return fmt.Errorf("team spec %s: max_depth %d exceeds hard cap %d", s.ID, s.Policies.MaxDepth, DefaultMaxDepth)
	}
	if s.Policies.MaxNodes <= 0 {
		s.Policies.MaxNodes = DefaultMaxNodes
	}
	if s.DefaultBudget < 0 {
		return fmt.Errorf("team spec %s: default_budget must be >= 0", s.ID)
	}
	switch s.Policies.Approval {
	case "":
		s.Policies.Approval = "development"
	case "development", "shared", "production":
	default:
		return fmt.Errorf("team spec %s: invalid approval %q", s.ID, s.Policies.Approval)
	}
	// 验证策略（B-03）：默认 R2；production 审批最低 R3，不允许因“任务小”降档。
	if s.Verification == nil {
		s.Verification = &VerificationPolicy{}
	}
	if err := normalizeVerificationPolicy(s.Verification); err != nil {
		return fmt.Errorf("team spec %s: %w", s.ID, err)
	}
	if s.Policies.Approval == "production" && RiskRank(s.Verification.Risk) < 3 {
		s.Verification.Risk = RiskR3
		s.Verification.RequiredLevels = append([]string(nil), riskLevels[RiskR3]...)
		if err := normalizeVerificationPolicy(s.Verification); err != nil {
			return fmt.Errorf("team spec %s: %w", s.ID, err)
		}
	}

	seen := make(map[string]bool, len(s.Roles))
	for i := range s.Roles {
		r := &s.Roles[i]
		r.ID = strings.TrimSpace(r.ID)
		r.RoleID = strings.TrimSpace(r.RoleID)
		if r.ID == "" || r.RoleID == "" {
			return fmt.Errorf("team spec %s: roles[%d] id/role_id required", s.ID, i)
		}
		if seen[r.ID] {
			return fmt.Errorf("team spec %s: duplicate role id %q", s.ID, r.ID)
		}
		seen[r.ID] = true
		if r.MaxInstances < 0 {
			return fmt.Errorf("team spec %s: role %s max_instances must be >= 0", s.ID, r.ID)
		}
		if r.Verification != nil {
			if err := normalizeVerificationPolicy(r.Verification); err != nil {
				return fmt.Errorf("team spec %s: role %s: %w", s.ID, r.ID, err)
			}
		}
	}
	return nil
}

// ValidateRoleIDs 校验团队引用的角色均已注册（available=role.Registry 的角色集合）。
func (s *Spec) ValidateRoleIDs(available map[string]bool) error {
	if s == nil {
		return fmt.Errorf("team spec is nil")
	}
	for _, r := range s.Roles {
		if !available[r.RoleID] {
			return fmt.Errorf("team spec %s: role %q is not registered", s.ID, r.RoleID)
		}
	}
	return nil
}

// ValidateNested 校验子团队不突破父团队边界（C-02）：
// 角色集合与工具只能收窄，预算不得超过父级，深度/节点数受父策略限制。
func ValidateNested(parent, child *Spec) error {
	if parent == nil || child == nil {
		return fmt.Errorf("nested team: parent/child required")
	}
	if err := child.Normalize(); err != nil {
		return err
	}
	if child.Policies.MaxDepth > parent.Policies.MaxDepth-1 {
		return fmt.Errorf("nested team %s: max_depth %d exceeds parent allowance %d",
			child.ID, child.Policies.MaxDepth, parent.Policies.MaxDepth-1)
	}
	if child.Policies.MaxNodes > parent.Policies.MaxNodes {
		return fmt.Errorf("nested team %s: max_nodes %d exceeds parent %d",
			child.ID, child.Policies.MaxNodes, parent.Policies.MaxNodes)
	}
	if parent.DefaultBudget > 0 && (child.DefaultBudget <= 0 || child.DefaultBudget > parent.DefaultBudget) {
		return fmt.Errorf("nested team %s: budget %d must be within parent budget %d",
			child.ID, child.DefaultBudget, parent.DefaultBudget)
	}
	parentRoles := make(map[string]RoleSpec, len(parent.Roles))
	for _, r := range parent.Roles {
		parentRoles[r.ID] = r
	}
	for _, c := range child.Roles {
		p, ok := parentRoles[c.ID]
		if !ok {
			return fmt.Errorf("nested team %s: role %q not declared in parent", child.ID, c.ID)
		}
		if len(p.Tools) > 0 && !subset(c.Tools, p.Tools) {
			return fmt.Errorf("nested team %s: role %q tools exceed parent", child.ID, c.ID)
		}
		if err := stricterThan(effectiveVerification(parent, &p), effectiveVerification(child, &c)); err != nil {
			return fmt.Errorf("nested team %s: role %q %w", child.ID, c.ID, err)
		}
	}
	if err := stricterThan(parent.Verification, child.Verification); err != nil {
		return fmt.Errorf("nested team %s: %w", child.ID, err)
	}
	return nil
}

// effectiveVerification 角色未覆盖时继承团队默认策略。
func effectiveVerification(spec *Spec, role *RoleSpec) *VerificationPolicy {
	if role != nil && role.Verification != nil {
		return role.Verification
	}
	if spec != nil {
		return spec.Verification
	}
	return nil
}

// subset 判断 child 是否 ⊆ parent（空 child 视为不限制，false）。
func subset(child, parent []string) bool {
	if len(child) == 0 {
		return true
	}
	set := make(map[string]bool, len(parent))
	for _, p := range parent {
		set[p] = true
	}
	for _, c := range child {
		if !set[c] {
			return false
		}
	}
	return true
}
