package subagent

// relay.go 提供 TODO #76 接力熔断与终答【未验证项】强制：
//   - bumpDispatchGeneration：per-(parent, domain) 派发代数计数（含 takeover 接管链），
//     第 3 代起注入【重写评估】强制段、第 4 代起无【接力理由】声明拒派；
//   - unverifiedSectionMissing / appendUnverifiedWarning：子 Agent 完成摘要缺
//     【未验证项】段时追加机器警告行（可观测不硬拒，首版口径）。

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// bumpDispatchGeneration 递增 (parentID, domain) 派发代数并返回当前代数。
// takeover 非空时把旧 domain 的既有代数累加到新 domain（异名接管链不断档）。
// 无 domain（叶子/复用）不计数，返回 0。拒派判定由调用方按代数与 task 声明做。
func (d *Dispatcher) bumpDispatchGeneration(parentID, domain, takeover string) int {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return 0
	}
	key := parentID + "\x00" + domain
	v, _ := d.dispatchGenerations.LoadOrStore(key, new(atomic.Int64))
	gen := int(v.(*atomic.Int64).Add(1))
	// 异名接管（TODO #73）：旧 domain 的既有代数并入（链不断档），随后清零旧键
	//（旧名已死，不再计数）。
	if takeover = strings.TrimSpace(takeover); takeover != "" && takeover != domain {
		oldKey := parentID + "\x00" + takeover
		if ov, ok := d.dispatchGenerations.LoadAndDelete(oldKey); ok {
			if prev := int(ov.(*atomic.Int64).Load()); prev > 0 {
				return int(v.(*atomic.Int64).Add(int64(prev)))
			}
		}
	}
	return gen
}

// relayDeclineMessage 渲染第 N 代（>= relayHardDeclineGen）无理由声明的拒派文案。
func relayDeclineMessage(gen int, domain string) string {
	return fmt.Sprintf(
		"接力熔断：领域 %q 已是第 %d 代派发（多棒接力质量逐代劣化）。继续派发必须在 task 中写明 %s… 段落，"+
			"声明为什么继续修补而非整文件重写/换方案；无声明则拒派。若前序产出已不可救，建议整文件重写或调整验收标准。",
		domain, gen, relayContinueMarker)
}

// taskHasRelayReason 判断 task 是否含【接力理由】声明（TODO #76 第 4 代起放行条件）。
func taskHasRelayReason(task string) bool {
	return strings.Contains(task, relayContinueMarker)
}

// unverifiedSectionMarker 是子 Agent 完成摘要中的【未验证项】段标记（TODO #76）。
const unverifiedSectionMarker = "【未验证项】"

// unverifiedSectionMissing 判断完成摘要是否缺【未验证项】段。
// 摘要无该段=子 Agent 未显式声明哪些验收条目未验证（全过的也应写"无"），
// dispatcher 追加警告行入【机器校验】段（可观测不硬拒，首版口径）。
func unverifiedSectionMissing(summary string) bool {
	return !strings.Contains(summary, unverifiedSectionMarker)
}

// appendUnverifiedWarning 在 MachineCheck 段追加"未声明未验证项"警告行。
// 追加后的段随完成摘要送达父 Agent，meta 纸面对照时可见声明缺口。
func appendUnverifiedWarning(machineCheck string) string {
	warn := "【机器校验】未声明未验证项：完成摘要缺 " + unverifiedSectionMarker +
		" 段（即使全部通过也应声明\"无\"）——验收时不得按\"全过\"口径采信本摘要。"
	if strings.TrimSpace(machineCheck) == "" {
		return warn
	}
	return strings.TrimRight(machineCheck, "\n") + "\n\n" + warn
}
