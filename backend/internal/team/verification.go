package team

// verification.go 分风险 DoD 策略（B-03 契约层）：
// TeamSpec / RoleSpec 可声明验证风险档与所需证据层级；子团队只能持平或收紧，
// 不能通过 RoleSpec 降低父团队风险或绕过人工签核。运行时 enforce（board done /
// 解锁依赖）仍受 agent.verification_policy_mode 控制，本文件只做确定性与嵌套校验。

import (
	"fmt"
	"strings"
)

// 风险档。
const (
	RiskR0 = "R0" // 只读检索/解释
	RiskR1 = "R1" // 文档/测试辅助/低风险单文件
	RiskR2 = "R2" // 默认代码行为修改/前后端集成
	RiskR3 = "R3" // 数据迁移/权限/安全/生产配置/外部副作用
)

// VerificationPolicy 默认完成要求。
type VerificationPolicy struct {
	Risk                string   `yaml:"risk" json:"risk"` // R0/R1/R2/R3
	RequiredLevels      []string `yaml:"required_levels" json:"required_levels"`
	AllowManual         bool     `yaml:"allow_manual" json:"allow_manual"`
	RequireHumanSignoff bool     `yaml:"require_human_signoff" json:"require_human_signoff"`
	MaxRepairRounds     int      `yaml:"max_repair_rounds" json:"max_repair_rounds"`
}

// riskLevels 各档默认证据层级（23 号文档 §5.2）。
var riskLevels = map[string][]string{
	RiskR0: nil,
	RiskR1: {"existence", "static"},
	RiskR2: {"existence", "static", "integration"},
	RiskR3: {"existence", "static", "integration", "runtime"},
}

// validVerificationLevels 允许的证据层级。
var validVerificationLevels = map[string]bool{
	"existence": true, "static": true, "integration": true, "runtime": true, "visual": true,
}

// RiskRank 返回风险档序号（R0=0 … R3=3）；未知按 R2 处理（保守默认）。
func RiskRank(risk string) int {
	switch strings.ToUpper(strings.TrimSpace(risk)) {
	case RiskR0:
		return 0
	case RiskR1:
		return 1
	case RiskR3:
		return 3
	default:
		return 2
	}
}

// normalizeVerificationPolicy 填充默认值并校验；risk 为空按 R2。
func normalizeVerificationPolicy(p *VerificationPolicy) error {
	if p == nil {
		return nil
	}
	p.Risk = strings.ToUpper(strings.TrimSpace(p.Risk))
	if p.Risk == "" {
		p.Risk = RiskR2
	}
	if _, ok := riskLevels[p.Risk]; !ok {
		return fmt.Errorf("verification policy: invalid risk %q (want R0/R1/R2/R3)", p.Risk)
	}
	if len(p.RequiredLevels) == 0 {
		p.RequiredLevels = append([]string(nil), riskLevels[p.Risk]...)
	}
	seen := make(map[string]bool, len(p.RequiredLevels))
	for _, lv := range p.RequiredLevels {
		lv = strings.ToLower(strings.TrimSpace(lv))
		if !validVerificationLevels[lv] {
			return fmt.Errorf("verification policy: invalid level %q", lv)
		}
		seen[lv] = true
	}
	// 去重保序。
	out := p.RequiredLevels[:0]
	for _, lv := range p.RequiredLevels {
		lv = strings.ToLower(strings.TrimSpace(lv))
		if seen[lv] {
			out = append(out, lv)
			seen[lv] = false
		}
	}
	p.RequiredLevels = out
	if p.MaxRepairRounds < 0 {
		return fmt.Errorf("verification policy: max_repair_rounds must be >= 0")
	}
	if p.MaxRepairRounds == 0 {
		p.MaxRepairRounds = 1
	}
	return nil
}

// stricterThan 校验 child 不弱于 parent（风险不降档、层级不少、签核不取消）。
func stricterThan(parent, child *VerificationPolicy) error {
	if parent == nil || child == nil {
		return nil
	}
	if RiskRank(child.Risk) < RiskRank(parent.Risk) {
		return fmt.Errorf("verification risk %s lower than parent %s", child.Risk, parent.Risk)
	}
	for _, lv := range parent.RequiredLevels {
		found := false
		for _, cl := range child.RequiredLevels {
			if cl == lv {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("verification level %q missing from child (parent requires it)", lv)
		}
	}
	if !parent.AllowManual && child.AllowManual {
		return fmt.Errorf("verification allow_manual cannot be relaxed in child")
	}
	if parent.RequireHumanSignoff && !child.RequireHumanSignoff {
		return fmt.Errorf("verification require_human_signoff cannot be removed in child")
	}
	return nil
}
