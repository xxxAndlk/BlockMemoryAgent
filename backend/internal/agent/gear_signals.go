package agent

// gear_signals.go 选档误判隐性信号采集（TODO #14 T18）：档位规则选档器（gear_selector.go）
// 的误判观测面。只落事件不改行为——信号仅供后续调参统计，不触发任何档位变更。
//
//   - 信号①（sendMessage 侧）：快速档会话在上一 run 终态后 gearSignalRecentWindow 内
//     收到含行动动词的新指令 → 疑似"工作活进了快速档"（用户把闲聊会话当工作会话续用）。
//   - 信号②（Stop 侧）：集群档会话本次 run 开始后 gearSignalEarlyStopWindow 内即被用户
//     软停 → 疑似"集群档太重"（等不及/嫌慢）。
//
// 事件统一 type=system、消息带「档位信号:」前缀（eventkind.System 无需新增前端映射），
// detail_json 携带结构化字段供离线统计。

import (
	"fmt"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// gearSignalRecentWindow 信号①的"上一 run 刚结束"窗口：终态后 5 分钟内的新指令才算续用。
const gearSignalRecentWindow = 5 * time.Minute

// gearSignalEarlyStopWindow 信号②的"刚开始就被停"窗口：run 开始 30s 内软停才算等不及。
const gearSignalEarlyStopWindow = 30 * time.Second

// hasActionVerb 判定文本是否含工程/行动动词（复用选档器词表，子串匹配）。
func hasActionVerb(text string) bool {
	lower := strings.ToLower(text)
	for _, v := range actionVerbs {
		if strings.Contains(lower, v) {
			return true
		}
	}
	return false
}

// gearSignalOnSend 计算信号①：返回非空 detailJSON 表示命中（快速档 + 上一 run 终态
// ≤5min + 行动动词）。锁外调用方负责落事件（addEvent 自身加锁）。
func gearSignalOnSend(currentGear string, endedAt *time.Time, content string) string {
	if currentGear != tool.GearFast || endedAt == nil {
		return ""
	}
	elapsed := time.Since(*endedAt)
	if elapsed > gearSignalRecentWindow || !hasActionVerb(content) {
		return ""
	}
	return fmt.Sprintf(`{"signal":"fast_task_intent","since_last_end_sec":%d}`, int(elapsed.Seconds()))
}

// gearSignalOnStop 计算信号②：返回非空 detailJSON 表示命中（集群档 + 本次 run 开始
// ≤30s 被软停）。runStartedAt 零值（旧路径未记录）不触发。
func gearSignalOnStop(currentGear string, runStartedAt time.Time) string {
	if currentGear != tool.GearCluster || runStartedAt.IsZero() {
		return ""
	}
	elapsed := time.Since(runStartedAt)
	if elapsed > gearSignalEarlyStopWindow {
		return ""
	}
	return fmt.Sprintf(`{"signal":"cluster_early_stop","run_sec":%d}`, int(elapsed.Seconds()))
}
