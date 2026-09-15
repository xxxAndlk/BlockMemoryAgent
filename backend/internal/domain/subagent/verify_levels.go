package subagent

// verify_levels.go 提供验收分层（TODO #59）的 runtime/visual 层证据判定与
// acceptance 逐项计分（TODO #68/#69/#67/#75）：
//   - runtimeProbeRetryMessage：runtime 层缺探针证据的 1 轮反馈重试指令；
//   - visualEvidenceCheck：visual 层场景化判定（scenes 非空时内容去重 + 数量 + 邻接）；
//   - scoreAcceptance / renderAcceptanceScore：结构化验收条目逐项挂机器证据、
//     dispatcher 计算 N/M（"自述不算证据"从纪律变机制）。

import (
	"fmt"
	"strings"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// runtimeProbeRetryMessage 是 runtime 层缺探针证据时的 1 轮反馈重试指令（TODO #67）。
// probes 为 spec 声明的探针序列（人读提示，透传给子 Agent 按声明补跑）。
func runtimeProbeRetryMessage(probes []string) string {
	var b strings.Builder
	b.WriteString("【运行时探针证据要求】本任务验收层级含 runtime：终答前必须经 tool_catalog 挂载 ui_preview，" +
		"完成完整探针序列——① ui_preview__browser_navigate 打开页面；② ui_preview__browser_evaluate 执行至少一次行为断言" +
		"（如 实体生成/状态变更/资源加载 getImage('x')!==null）；③ ui_preview__browser_console_messages 回读控制台且无 error/severe 级条目。" +
		"三者缺任一或 console 有 error 均判未验证（dispatcher 机器扫描，自述不算证据）。")
	if len(probes) > 0 {
		b.WriteString("\nspec 声明的探针序列：\n")
		for _, p := range probes {
			b.WriteString("- ")
			b.WriteString(strings.TrimSpace(p))
			b.WriteString("\n")
		}
	}
	return b.String()
}

// visualEvidenceCheck visual 层证据判定（TODO #69）：
//   - scenes 非空：截图按内容指纹去重后数量 ≥ len(scenes)，且有 navigate/evaluate
//     → screenshot 时序邻接证据（同图连拍充数无效——实证 2026-08-25 水果忍者
//     10 张截图 7 张字节相同菜单图，从未拍到游玩画面）；
//   - scenes 空：退回 HasScreenshotEvidence 单截图判定（零行为变化）。
//
// 返回 (是否通过, 缺证据时的重试指令文案)。
func visualEvidenceCheck(history []agent.ReactMessage, scenes []string) (bool, string) {
	if len(scenes) == 0 {
		if agent.HasScreenshotEvidence(history) {
			return true, ""
		}
		return false, visualRetryMessage
	}
	rep := agent.HasSceneEvidence(history, len(scenes))
	if rep.DistinctShots >= len(scenes) && rep.HasAdjacent {
		return true, ""
	}
	var missing []string
	if rep.DistinctShots < len(scenes) {
		missing = append(missing, fmt.Sprintf("去重后截图数 %d < 场景数 %d（同图连拍只计 1）", rep.DistinctShots, len(scenes)))
	}
	if !rep.HasAdjacent {
		missing = append(missing, "无 navigate/evaluate → screenshot 时序邻接证据（截图须对应真实页面状态）")
	}
	msg := "【视觉证据不足】场景化截图清单未覆盖（dispatcher 按内容去重后核对）：\n- " +
		strings.Join(missing, "\n- ") +
		"\n须覆盖的场景：" + strings.Join(scenes, "、") +
		"\n每个场景分别 navigate/evaluate 到对应状态后截图（如 游玩中须先交互进入游玩画面再截），同图连拍不算新场景。"
	return false, msg
}

// acceptanceScore 是 acceptance 逐项计分结果（TODO #68）。
type acceptanceScore struct {
	total           int      // 结构化条目总数（非 manual 之外的机器可核条目；纯 manual spec total=0 不计分）
	passed          int      // 机器证据通过条目数
	qualityMissing  int      // quality 层缺证据条目数
	qualityEvidence int      // quality 层声明了机器可核证据类型的条目数（manual 的 quality 不算）
	unverified      []string // 缺证据条目清单（编号+文本+证据类型）
}

// scoreAcceptance 按条目 evidence 类型挂机器证据并计分（TODO #68 dispatcher 逐项计分）。
// acceptance 行格式（tool.FormatAcceptanceLine）：`文本 [evidence:X]` 或 `文本 [evidence:X layer:Y]`；
// 无标记行（存量纯字符串）按 manual 处理、不计入 total（保持旧 spec 零行为变化）。
// 证据挂接：
//   - command → HasExecutableVerification（验证类命令成功证据，全条目共享同一口径）；
//   - screenshot → visualEvidenceCheck（scenes 空时单截图）；
//   - probe → HasRuntimeProbeEvidence（navigate+evaluate+console 无 error）；
//   - file → FilesModifiedFromHistory 非空（本子 Agent 实际写入过文件）；
//   - manual → 不机器核，不计分。
func scoreAcceptance(history []agent.ReactMessage, acceptance []string, rec *parentSpecRecord) acceptanceScore {
	var sc acceptanceScore
	for i, line := range acceptance {
		text, evidence, layer := tool.ParseAcceptanceLine(line)
		if evidence == "" || evidence == "manual" {
			continue // 存量纯字符串 / 纸面标准：机器不核
		}
		sc.total++
		var ok bool
		switch evidence {
		case "command":
			ok = agent.HasExecutableVerification(history)
		case "screenshot":
			ok, _ = visualEvidenceCheck(history, rec.scenes)
		case "probe":
			ok = agent.HasRuntimeProbeEvidence(history)
		case "file":
			ok = len(agent.FilesModifiedFromHistory(history)) > 0
		}
		if ok {
			sc.passed++
			continue
		}
		sc.unverified = append(sc.unverified, fmt.Sprintf("#%d [%s] %s", i+1, evidence, text))
		if layer == "quality" {
			sc.qualityMissing++
			sc.qualityEvidence++
		}
	}
	return sc
}

// renderAcceptanceScore 渲染计分报告（【机器校验】段，meta 只读不自算）。
func renderAcceptanceScore(sc acceptanceScore) string {
	if sc.total == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "【机器校验】验收条目计分（dispatcher 计算，非 agent 自述）: %d/%d 条机器证据通过", sc.passed, sc.total)
	if sc.qualityMissing > 0 {
		fmt.Fprintf(&b, "；quality 层缺证据 %d 条（整体不得标绿）", sc.qualityMissing)
	}
	b.WriteString("\n")
	if len(sc.unverified) > 0 {
		b.WriteString("未验证条目（缺机器证据，不计入通过数）:\n")
		for _, u := range sc.unverified {
			b.WriteString("- ")
			b.WriteString(u)
			b.WriteString("\n")
		}
	} else {
		b.WriteString("全部机器可核条目均有证据。\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
