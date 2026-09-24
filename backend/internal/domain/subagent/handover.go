package subagent

// handover.go 提供非正常终止子 Agent 的结构化遗产清单（第二层）：
// 成功写入文件（仅 WriteFile/EditFile Success=true）+ 看板在办步骤 + 打捞摘要，
// 组装为【遗产清单】段随失败消息送达父 Agent，文案明示"可直接作为续建 spec 骨架"。
// 实证（2026-08-25 水果忍者）：domain-2 被连读守卫杀时 plan_execute 仅 1/6、
// 脚手架已建（index.html/style.css/game.js 骨架 132 行），5 步未做靠 meta 人肉盘点再派。

import (
	"context"
	"fmt"
	"strings"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/board"
)

// legacyListMaxFiles 遗产清单列出的最大文件数（防超长历史文件清单撑爆消息）。
const legacyListMaxFiles = 30

// renderLegacyList 组装【遗产清单】结构化段。空段返回空串（无写入且无在办步骤）。
func (d *Dispatcher) renderLegacyList(ctx context.Context, parentID, domain string, result agent.ReactResult) string {
	files := agent.FilesWrittenFromHistory(result.History)
	var steps []string
	if b := d.boardFor(parentID); b != nil {
		for _, id := range b.FindAllByDomain(strings.TrimSpace(domain)) {
			snap := b.Snapshot()
			for _, t := range snap.Tasks {
				if t.ID == id && (t.Status == board.TaskInProgress || t.Status == board.TaskPending || t.Status == board.TaskBlocked) {
					steps = append(steps, fmt.Sprintf("[%s] %s (%s)", t.Status, t.Title, t.ID))
				}
			}
		}
	}
	if len(files) == 0 && len(steps) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("【遗产清单】（dispatcher 结构化组装，可直接作为续建 spec 骨架素材）\n")
	if len(files) > 0 {
		b.WriteString(fmt.Sprintf("成功写入文件（WriteFile/EditFile Success=true，%d 个）:\n", len(files)))
		shown := files
		if len(shown) > legacyListMaxFiles {
			shown = shown[:legacyListMaxFiles]
		}
		for _, f := range shown {
			b.WriteString("- ")
			b.WriteString(f)
			b.WriteString("\n")
		}
		if len(files) > legacyListMaxFiles {
			b.WriteString(fmt.Sprintf("- …另有 %d 个（截断）\n", len(files)-legacyListMaxFiles))
		}
	}
	if len(steps) > 0 {
		b.WriteString("看板在办步骤（未完成，续建须覆盖）:\n")
		for _, s := range steps {
			b.WriteString("- ")
			b.WriteString(s)
			b.WriteString("\n")
		}
	}
	b.WriteString("未在上述清单中的工作均为未落盘（探索/思考/失败写入），续建 spec 请以本清单为基线。")
	return b.String()
}

// boardFor 按 parentID 推导 sessionID 并取看板（renderLegacyList 内部用）。
func (d *Dispatcher) boardFor(parentID string) *board.TaskBoard {
	if d.boardFn == nil {
		return nil
	}
	sid := ""
	if i := strings.Index(parentID, "/"); i > 0 {
		sid = parentID[:i]
	}
	if sid == "" {
		sid = parentID
	}
	return d.boardFn(sid)
}
