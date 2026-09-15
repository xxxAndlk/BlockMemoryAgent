package tool

// gear.go 定义会话执行档位枚举（TODO #14 会话三档控制）。
// 档位决定一次会话的执行形态：auto 按任务信号自动选，fast 走轻量对话角色秒回，
// cluster 走 Meta 全装编排（现行为）。枚举放 tool 包与 trustMode 同层：HTTP 端点
// 校验与后续工具级档位门共用；config 侧沿用字符串字面量（不反向依赖本包）。

// 档位枚举值：
//   - auto：自动选档（默认）——明确闲聊信号 → fast，其余一律 cluster（零回归）；
//     自动升档只升不降（fast→cluster 允许，反向拒绝）。
//   - fast：固定快速档——轻量对话角色（tools≈空、thinking 低、快模型），闲聊/快问快答。
//   - cluster：固定集群档——Meta 全装编排（现行为，子 Agent 派发/看板/台账全套）。
//   - explore 为设计预留档（TODO #14 表格工具包方向），暂不入枚举不入校验，
//     前端选择器禁用展示"即将上线"。
const (
	GearAuto    = "auto"
	GearFast    = "fast"
	GearCluster = "cluster"
)

// ValidGear 校验档位枚举值。
func ValidGear(gear string) bool {
	switch gear {
	case GearAuto, GearFast, GearCluster:
		return true
	}
	return false
}
