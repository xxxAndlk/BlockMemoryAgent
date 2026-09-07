package subagent

// merge_worktree.go 实现 meta 专用工具 merge_worktree（TODO 第9项⑤/#10项⑤）：
// worktree 隔离派发的 diff 审查与合并门操作入口。
//
//   - action=review：返回该 worktree 的全量 diff（patch 未产出时读副本实时 diff），
//     超长截断（防 meta 上下文被大 patch 撑爆，完整 patch 落盘路径随附可 ReadFile）；
//   - action=merge：走合并门（base 漂移检测 → 契约静态检查 → git apply --check →
//     apply → 清理副本与分支），任何一步失败主仓库保持原样；
//   - action=reject：驳回交付，comments 经 mailbox 回该域（热驻槽续改，销毁则提示重派）。
//
// 白名单仅 meta：roles.yaml meta_agent.tools 与 domain/role/registry.go meta 表各加一项
//（meta tools 非空即整体覆盖，漏配则 meta 看不到该工具）。

import (
	"context"
	"fmt"
	"strings"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// mergeWorktreeReviewMaxRunes review 输出截断上限：全量 diff 大于该值时只回头部
// 摘要 + patch 落盘路径（meta 需要 ReadFile 精读时走文件）。
const mergeWorktreeReviewMaxRunes = 20000

// mergeWorktreeTool 实现 merge_worktree 工具。
type mergeWorktreeTool struct {
	dispatcher *Dispatcher
}

// Name 返回工具名称。
func (t *mergeWorktreeTool) Name() string { return "merge_worktree" }

// Aliases 返回工具别名，当前无别名。
func (t *mergeWorktreeTool) Aliases() []string { return nil }

// Description 返回 LLM 可见的工具描述。
func (t *mergeWorktreeTool) Description() string {
	return "worktree 隔离派发（call_sub_agent worktree=true）的交付审查与合并门。" +
		"参数 agent_id 为派发时返回的 sub_agent_id；action=review 返回该副本的全量 diff" +
		"（超长截断，完整 patch 路径随附）；action=merge 执行合并门：base 漂移检测" +
		"（他域已合入则拒绝，需人工 rebase 或重派）→ 跨域契约静态检查 → patch 应用，" +
		"失败时主仓库保持原样；action=reject 驳回交付，comments 修改意见经邮箱回该域" +
		"（热驻槽续改后重新提交；实例已销毁则需携带意见重派）。" +
		"合并前必须先 review 全量 diff，确认质量与无越界改动后再 merge。"
}

// Execute 执行 merge_worktree 工具调用。
// 参数 args：agent_id（必填）、action（review/merge/reject，必填）、comments（reject 时附意见）。
func (t *mergeWorktreeTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	const name = "merge_worktree"
	d := t.dispatcher
	agentID, _ := args["agent_id"].(string)
	action, _ := args["action"].(string)
	action = strings.TrimSpace(strings.ToLower(action))
	if agentID == "" || action == "" {
		return &tool.Result{Tool: name, Error: "agent_id and action are required（action: review|merge|reject）"}
	}
	sessionID := tool.SessionIDFromContext(ctx)
	if sessionID == "" {
		return &tool.Result{Tool: name, Error: "missing session context"}
	}
	switch action {
	case "review":
		diff, err := d.WorktreeDiff(sessionID, agentID)
		if err != nil {
			return &tool.Result{Tool: name, Error: err.Error()}
		}
		return &tool.Result{Tool: name, Success: true, Output: truncateWorktreeDiff(diff)}
	case "merge":
		out, err := d.MergeWorktree(ctx, sessionID, agentID)
		if err != nil {
			return &tool.Result{Tool: name, Error: err.Error()}
		}
		return &tool.Result{Tool: name, Success: true, Output: out}
	case "reject":
		comments, _ := args["comments"].(string)
		out, err := d.RejectWorktree(ctx, sessionID, agentID, comments)
		if err != nil {
			return &tool.Result{Tool: name, Error: err.Error()}
		}
		return &tool.Result{Tool: name, Success: true, Output: out}
	default:
		return &tool.Result{Tool: name, Error: fmt.Sprintf("未知 action %q（可选 review|merge|reject）", action)}
	}
}

// truncateWorktreeDiff review 输出截断：超限时保留头部并附完整 patch 提示。
func truncateWorktreeDiff(diff string) string {
	runes := []rune(diff)
	if len(runes) <= mergeWorktreeReviewMaxRunes {
		return diff
	}
	head := string(runes[:mergeWorktreeReviewMaxRunes])
	return head + "\n\n【截断】diff 全长 " + fmt.Sprintf("%d", len(runes)) +
		" 字符，以上为头部 " + fmt.Sprintf("%d", mergeWorktreeReviewMaxRunes) +
		" 字符。完整 patch 已落盘（见【worktree 交付】消息中的 patch 路径），可用 ReadFile 分段精读。"
}
