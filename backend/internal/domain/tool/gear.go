package tool

// gear.go 定义会话执行档位枚举（TODO #14 会话三档控制，2026-09-16 全手动三档）。
// 档位由用户显式选择（Web 输入栏前置选择器 / 会话内下拉 / API），不再有规则自动选档。
// 枚举放 tool 包与 trustMode 同层：HTTP 端点校验与工具级档位门共用；
// config 侧沿用字符串字面量（不反向依赖本包）。

// 档位枚举值：
//   - fast：快速档——文档助手（doc_assistant）顶层直达，文档/问答类轻任务；
//     识别出超出范围的工程任务经 escalate_gear 升档。
//   - daily：日常档（默认）——DomainAgent（domain 角色）顶层直接执行，
//     可自执行亦可自行下拆叶子；跳过 meta 专属编排观测注入；同样挂 escalate_gear。
//   - cluster：集群档——Meta 全装编排（子 Agent 派发/看板/台账全套）。
//
// 历史值 "auto"（规则自动选档）已退役：旧持久化数据读侧映射为 daily。
const (
	GearFast    = "fast"
	GearDaily   = "daily"
	GearCluster = "cluster"
)

// LegacyGearAuto 历史档位值（规则自动选档，2026-09-16 退役）：新写入一律拒绝，
// 持久化数据读侧经 NormalizeGear 映射为 daily。
const LegacyGearAuto = "auto"

// NormalizeGear 归一化持久化档位：历史 "auto" 映射 daily；其余原样返回
//（合法性由 ValidGear 判定，非法值由调用方按 fallback 兜底）。
func NormalizeGear(gear string) string {
	if gear == LegacyGearAuto {
		return GearDaily
	}
	return gear
}

// ValidGear 校验档位枚举值。
func ValidGear(gear string) bool {
	switch gear {
	case GearFast, GearDaily, GearCluster:
		return true
	}
	return false
}
