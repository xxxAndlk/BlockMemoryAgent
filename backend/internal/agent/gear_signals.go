package agent

// gear_signals.go 档位使用隐性信号采集（TODO #14 T18）：档位全手动化（2026-09-16）后
// 作为"档位选得合不合适"的观测面（快速档收任务、集群档被秒停）。只落事件不改行为——
// 信号仅供后续统计，不触发任何档位变更。
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

// actionVerbs 工程/行动动词表（中英，子串匹配）：出现任一即视为任务型输入。
// 供档位信号判定复用（原选档器词表；选档器随 auto 档退役后迁至此处）。
var actionVerbs = []string{
	// 中文：写改修删建 + 构建运维链路
	"写", "改", "修", "删", "建", "实现", "开发", "重构", "修复", "构建", "编译",
	"部署", "发布", "运行", "执行", "测试", "排查", "诊断", "调试", "分析", "生成",
	"添加", "新增", "迁移", "升级", "接入", "对接", "安装", "配置", "爬取", "抓取",
	"翻译", "总结", "搜索", "检索", "统计", "画", "做", "调", "改一下", "优化",
	// 英文：子串匹配（误命中如 pruning 视为任务，安全向）
	"write", "fix", "build", "compile", "deploy", "run", "test", "refactor",
	"implement", "create", "add ", "delete", "remove", "update", "migrate",
	"install", "generate", "analyze", "parse", "debug", "search", "translate",
	"summarize", "optimiz", "config",
}

// hasActionVerb 判定文本是否含工程/行动动词（子串匹配）。
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
