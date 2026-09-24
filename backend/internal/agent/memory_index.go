package agent

// memory_index.go 记忆索引槽（TODO #20③+#22③）：会话启动注入"一行式沉淀索引"——
// CC auto memory 两级结构（MEMORY.md 索引 + 正文按需）的 BMA 版：索引槽有配额
// （memory_index_max_lines / memory_index_max_runes），详情行走向量召回不动。
// 超限不静默截断——渲染"重写指令"逼整理（CC memory-index 超限返错逼重写同款语义）。
// 信任分层（#22③）：untrusted 内容（WrapUntrusted 围栏标记）结构性禁止进索引与
// curated 记忆层；被召回内容同款围栏标记，提取侧丢弃（防"召回→转述→再沉淀"循环）。

import (
	"fmt"
	"strings"
)

// MemoryIndexEntry 一条沉淀索引行（一行式：类型 + 内容摘要）。
type MemoryIndexEntry struct {
	Type    string // 沉淀类型（用户偏好/项目经验/技能/…）
	Summary string // 一行摘要（已截断）
}

// RenderMemoryIndex 渲染【沉淀索引】块：maxLines 行 / maxRunes 字双配额。
// 超限时返回的文本带"重写指令"（不静默截断）：写侧/模型应整理合并旧沉淀后重写索引，
// 超限部分本次不注入（CC 语义：超限部分下次加载丢弃，但必须显式告知）。
// 返回 (块文本, 是否超限)；空 entries 返回空串。
func RenderMemoryIndex(entries []MemoryIndexEntry, maxLines, maxRunes int) (string, bool) {
	if len(entries) == 0 {
		return "", false
	}
	if maxLines <= 0 {
		maxLines = 200
	}
	if maxRunes <= 0 {
		maxRunes = 25000
	}
	over := len(entries) > maxLines
	var sb strings.Builder
	sb.WriteString("【沉淀索引】（本会话相关沉淀的一行式索引；详情按需向量召回，勿全文臆测）\n")
	used := 0
	shown := 0
	for _, e := range entries {
		if shown >= maxLines {
			over = true
			break
		}
		line := fmt.Sprintf("- [%s] %s", e.Type, e.Summary)
		cost := len([]rune(line)) + 1
		if used+cost > maxRunes {
			over = true
			break
		}
		sb.WriteString(line)
		sb.WriteString("\n")
		used += cost
		shown++
	}
	if over {
		// 重写指令（不静默截断）：写侧收到的是"请整理"，不是"悄悄丢"。
		fmt.Fprintf(&sb, "【记忆索引超限】沉淀共 %d 条，本次仅注入 %d 条（配额 %d 行/%d runes）。"+
			"请整理合并旧沉淀条目后重写索引（删除/归档过期条目），超限条目本次未注入。\n",
			len(entries), shown, maxLines, maxRunes)
	}
	return strings.TrimRight(sb.String(), "\n"), over
}

// untrustedFenceMarker 是 WrapUntrusted 的围栏标记（tool 包渲染）：内容被它包裹即
// 来自不可信源（MCP/HTTP/召回转述）。提取/索引侧见到即丢——结构性信任分层（#22③）。
const untrustedFenceMarker = "<untrusted_data"

// ContainsUntrustedFence 报告文本是否携带不可信围栏标记（召回循环防护 + provenance 门）。
// 提取/索引侧见到即丢：untrusted 结构性禁止进 curated 层与自动注入，被召回内容
// （renderRecalledMemory 已包围栏）不再反向提取为新记忆（防"召回→转述→再沉淀"循环）。
func ContainsUntrustedFence(s string) bool {
	return strings.Contains(s, untrustedFenceMarker)
}
