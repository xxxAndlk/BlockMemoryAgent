package subagent

// worktree.go 实现 git worktree 隔离派发（TODO 第9项⑤）与 diff 交付/合并门（TODO 第10项⑤）。
//
// 设计（doc/TODO.md #9⑤/#10⑤ + 批准方案批次 G）：
//   - 派发侧：call_sub_agent 携带 worktree=true 时，Dispatcher 在主仓库
//     `.bma/worktrees/<session>-<domain>-<ts>` 建独立 worktree（新分支自 HEAD），
//     子 Agent 的 WithWorkDir/沙箱基线整体切到副本，主目录零写入。
//   - 交付侧：子 Agent 成功收尾时 `git add -A` + `git diff --cached <base>` 产出
//     全量 patch 落盘（副本同目录 .patch 文件）+ --stat 摘要，notify 摘要附路径。
//   - 合并门（meta 工具 merge_worktree）：review 返回逐文件 diff；merge 先做 base
//     漂移检测（主仓库 HEAD ≠ 建副本时 base → 拒绝，提示人工 rebase/重派），再对
//     worktree 副本跑跨域契约静态检查（patch 触及契约文件才跑，防对其他域范围误报），
//     通过后 `git apply --check` + `git apply` + 清理副本与分支，全链幂等；
//     reject 把修改意见经 mailbox 回该域（热驻槽存活则续改，已销毁提示重派）。
//   - 清理：会话终态（软停销毁/硬取消/Shutdown）经 CleanupSessionWorktrees
//     best-effort 移除残留副本。
//
// git 缺失/非 git 仓库 → createWorktree 返回明确错误，派发侧按 validation_rejected
// 拒绝（不降级为主目录派发——静默降级会让"隔离"名存实亡）。

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/mailbox"
)

// worktreeHandle 是一次 worktree 隔离派发的登记句柄。
// 合并门与清理按 AgentID 寻址；Merged/Removed 后句柄保留供审计列表展示终态。
type worktreeHandle struct {
	AgentID    string // 子 Agent 实例 ID（合并门寻址键）
	SessionID  string // 所属会话（清理/列表按会话过滤）
	ParentID   string // 派发父 Agent（契约检查取父 spec 记录用）
	Domain     string // 领域名（叶子派发为角色 ID）
	Path       string // worktree 副本绝对路径
	Branch     string // 专属分支名 bma/<name>
	BaseCommit string // 建副本时的主仓库 HEAD（base 漂移检测基准）
	RepoRoot   string // 主仓库根目录
	PatchPath  string // 交付 patch 落盘路径（成功收尾后非空）
	PatchStat  string // --stat 摘要（merge_worktree review 的头部信息）
	CreatedAt  time.Time
	Merged     bool // 已合入主仓库（幂等标记）
	Removed    bool // 副本已删除（merge/reject 丢弃/清理后置位）
}

// WorktreeView 是 worktree 句柄的 HTTP 线型 DTO（agent 包定义，避免 server 依赖
// subagent；Dispatcher 方法直接返回该类型经 agent.Query 透出）。
type WorktreeView = agent.WorktreeView

// WithWorktreeEnabled 注入 worktree 派发开关（bootstrap 从 config agent.worktree_enabled
// 读取）。false 时 call_sub_agent 携带 worktree=true 按 validation_rejected 拒绝。
func (d *Dispatcher) WithWorktreeEnabled(enabled bool) *Dispatcher {
	d.worktreeEnabled = enabled
	return d
}

// MergeWorktree 合并门入口（导出）：agent.WorktreeOperator 接口实现，
// service 层 Control 通道与 merge_worktree 工具经此调用。
func (d *Dispatcher) MergeWorktree(ctx context.Context, sessionID, agentID string) (string, error) {
	return d.mergeWorktree(ctx, sessionID, agentID)
}

// RejectWorktree 驳回交付入口（导出）：agent.WorktreeOperator 接口实现，
// comments 经 mailbox 回该域。
func (d *Dispatcher) RejectWorktree(ctx context.Context, sessionID, agentID, comments string) (string, error) {
	return d.rejectWorktree(ctx, sessionID, agentID, comments)
}

// gitOut 在 dir 下执行 git 子命令并返回 stdout（CombinedOutput 兼取 stderr 参与报错）。
func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), text, err)
	}
	return text, nil
}

// sanitizeWorktreeToken 把 session/domain 名清洗为文件名与分支名安全片段
//（非字母数字下划线中划线统一为中划线，超长截断）。
func sanitizeWorktreeToken(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			b.WriteRune(unicode.ToLower(r))
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "task"
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

// boolArg 解析布尔型工具参数（兼容 bool / "true" 字符串两种形态）。
func boolArg(args map[string]any, key string) bool {
	switch v := args[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	}
	return false
}

// mainWorkDir 解析主仓库基准目录：派发 ctx 的会话工作目录 > 工具注册表默认目录。
func (d *Dispatcher) mainWorkDir(ctx context.Context) string {
	if wd := tool.WorkDirFromContext(ctx); wd != "" {
		return wd
	}
	return d.subAgentWorkDir()
}

// createWorktreeForDispatch 为本次派发创建 git worktree 副本并登记句柄。
// 失败返回 validation 语义错误（派发侧按 validation_rejected 拒绝，不降级）。
func (d *Dispatcher) createWorktreeForDispatch(ctx context.Context, parentID, subAgentID, sessionID, domain, roleID string) (*worktreeHandle, *tool.Result) {
	workDir := d.mainWorkDir(ctx)
	if strings.TrimSpace(workDir) == "" {
		return nil, toolReject("worktree 派发需要会话工作目录（work_dir），当前为空")
	}
	repoRoot, err := gitOut(workDir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, toolReject(fmt.Sprintf("worktree 派发要求主目录是 git 仓库（%v）", err))
	}
	label := domain
	if strings.TrimSpace(label) == "" {
		label = roleID
	}
	name := fmt.Sprintf("%s-%s-%d", sanitizeWorktreeToken(sessionID), sanitizeWorktreeToken(label), time.Now().Unix())
	wtPath := filepath.Join(repoRoot, ".bma", "worktrees", name)
	branch := "bma/" + name
	if _, err := gitOut(repoRoot, "worktree", "add", wtPath, "-b", branch); err != nil {
		// 失败可能留下半登记状态：prune 清理元数据（目录残留 best-effort 删除）。
		_, _ = gitOut(repoRoot, "worktree", "prune")
		_ = os.RemoveAll(wtPath)
		return nil, toolReject(fmt.Sprintf("创建 worktree 失败: %v", err))
	}
	base, err := gitOut(repoRoot, "rev-parse", "HEAD")
	if err != nil {
		_, _ = gitOut(repoRoot, "worktree", "remove", "--force", wtPath)
		_, _ = gitOut(repoRoot, "branch", "-D", branch)
		return nil, toolReject(fmt.Sprintf("读取主仓库 HEAD 失败: %v", err))
	}
	h := &worktreeHandle{
		AgentID: subAgentID, SessionID: sessionID, ParentID: parentID,
		Domain: label, Path: wtPath, Branch: branch, BaseCommit: base,
		RepoRoot: repoRoot, CreatedAt: time.Now(),
	}
	d.worktrees.Store(subAgentID, h)
	log.Printf("[subagent] WORKTREE-ADD: sub=%s path=%s branch=%s base=%s", subAgentID, wtPath, branch, base[:min(8, len(base))])
	return h, nil
}

// toolReject 把文案包成 validation_rejected 语义的 tool.Result。
func toolReject(msg string) *tool.Result {
	return &tool.Result{Error: msg, Category: tool.ResultCategoryValidationRejected}
}

// worktreeHandleFor 按 AgentID 取句柄；不存在返回 nil。
func (d *Dispatcher) worktreeHandleFor(agentID string) *worktreeHandle {
	if v, ok := d.worktrees.Load(agentID); ok {
		return v.(*worktreeHandle)
	}
	return nil
}

// producePatchForAgent 在子 Agent 成功收尾时产出全量 patch：staged 全部变更
// diff <base> 落盘 + --stat 摘要。无变更/句柄缺失/已合并返回空串（零打扰）。
func (d *Dispatcher) producePatchForAgent(ctx context.Context, subAgentID string) string {
	h := d.worktreeHandleFor(subAgentID)
	if h == nil || h.Merged || h.Removed {
		return ""
	}
	if _, err := gitOut(h.Path, "add", "-A"); err != nil {
		log.Printf("[subagent] WORKTREE add failed: sub=%s err=%v", subAgentID, err)
		return ""
	}
	patch, err := gitOut(h.Path, "diff", "--cached", h.BaseCommit)
	if err != nil {
		log.Printf("[subagent] WORKTREE diff failed: sub=%s err=%v", subAgentID, err)
		return ""
	}
	if strings.TrimSpace(patch) == "" {
		h.PatchStat = "（worktree 副本无文件变更）"
		log.Printf("[subagent] WORKTREE-EMPTY: sub=%s path=%s", subAgentID, h.Path)
		return "\n\n【worktree 交付】副本 " + h.Path + " 无文件变更。"
	}
	h.PatchPath = h.Path + ".patch"
	if err := os.WriteFile(h.PatchPath, []byte(patch+"\n"), 0644); err != nil {
		log.Printf("[subagent] WORKTREE patch write failed: sub=%s path=%s err=%v", subAgentID, h.PatchPath, err)
		return ""
	}
	stat, _ := gitOut(h.Path, "diff", "--cached", "--stat", h.BaseCommit)
	h.PatchStat = stat
	log.Printf("[subagent] WORKTREE-PATCH: sub=%s patch=%s stat_lines=%d", subAgentID, h.PatchPath, len(strings.Split(stat, "\n")))
	return "\n\n【worktree 交付】变更隔离于副本（主目录零写入）：\n" +
		"patch: " + h.PatchPath + "\n" + stat +
		"\n（合并请用 merge_worktree review 先看全量 diff，确认后 merge / reject 附意见。）"
}

// ListWorktrees 返回会话全部 worktree 句柄的线型快照（HTTP 列表端点数据源）。
func (d *Dispatcher) ListWorktrees(sessionID string) []agent.WorktreeView {
	out := []agent.WorktreeView{}
	d.worktrees.Range(func(_, v any) bool {
		if h, ok := v.(*worktreeHandle); ok && h.SessionID == sessionID {
			out = append(out, agent.WorktreeView{
				AgentID: h.AgentID, Domain: h.Domain, Path: h.Path,
				Branch: h.Branch, BaseCommit: h.BaseCommit,
				PatchPath: h.PatchPath, PatchStat: h.PatchStat,
				Merged: h.Merged, Removed: h.Removed, CreatedAt: h.CreatedAt,
			})
		}
		return true
	})
	return out
}

// WorktreeDiff 返回指定句柄的 patch 全文（review 数据源）。
// patch 未产出（未收尾）时报错提示等待。
func (d *Dispatcher) WorktreeDiff(sessionID, agentID string) (string, error) {
	h := d.worktreeHandleFor(agentID)
	if h == nil || h.SessionID != sessionID {
		return "", fmt.Errorf("worktree 不存在: agent=%s", agentID)
	}
	if h.Removed {
		return "", fmt.Errorf("worktree 副本已删除: agent=%s", agentID)
	}
	if h.PatchPath == "" {
		// 未收尾：直接读副本工作区 diff（进行中任务的实时 diff）。
		if _, err := gitOut(h.Path, "add", "-A"); err != nil {
			return "", fmt.Errorf("读取 worktree 变更失败: %w", err)
		}
		patch, err := gitOut(h.Path, "diff", "--cached", h.BaseCommit)
		if err != nil {
			return "", fmt.Errorf("读取 worktree diff 失败: %w", err)
		}
		if strings.TrimSpace(patch) == "" {
			return "（副本暂无文件变更）", nil
		}
		return patch, nil
	}
	data, err := os.ReadFile(h.PatchPath)
	if err != nil {
		return "", fmt.Errorf("读取 patch 失败: %w", err)
	}
	return string(data), nil
}

// mergeWorktree 合并门：base 漂移检测 → 契约静态检查（patch 触及契约文件才跑）→
// git apply --check → apply → 清理副本与分支。返回人读结果文案；任何一步失败
// 返回 error 且主仓库保持原样（apply --check 前置保证幂等）。
func (d *Dispatcher) mergeWorktree(ctx context.Context, sessionID, agentID string) (string, error) {
	h := d.worktreeHandleFor(agentID)
	if h == nil || h.SessionID != sessionID {
		return "", fmt.Errorf("worktree 不存在: agent=%s", agentID)
	}
	if h.Merged {
		return "该 worktree 已合入主仓库（幂等）", nil
	}
	if h.Removed {
		return "", fmt.Errorf("worktree 副本已删除，无法合并: agent=%s", agentID)
	}
	// 1. base 漂移检测：主仓库 HEAD 相对建副本时已前进（他域已合入）→ 拒绝，
	//    强制人工 rebase/重派——v1 不做自动冲突解决。
	head, err := gitOut(h.RepoRoot, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("读取主仓库 HEAD 失败: %w", err)
	}
	if head != h.BaseCommit {
		return "", fmt.Errorf("base 漂移: 建副本时 HEAD=%s，当前 HEAD=%s（他域已合入）。请人工 rebase 副本分支或重派该域任务后再合并", shortSHA(h.BaseCommit), shortSHA(head))
	}
	// 2. patch 就绪：成功收尾已产出；未产出（异常路径）此处补产。
	if h.PatchPath == "" {
		if note := d.producePatchForAgent(ctx, agentID); note == "" {
			return "", fmt.Errorf("patch 产出失败，无法合并（副本保留待排查）: %s", h.Path)
		}
	}
	// 3. 契约静态检查：仅跑 patch 触及契约文件的记录，检查基准 = worktree 副本
	//（跨域引用在副本内自洽才算可合入）。
	if violations := d.worktreeContractCheck(ctx, h); violations != "" {
		return "", fmt.Errorf("契约检查未通过，合并被拒绝:\n%s", violations)
	}
	// 4. 应用前预检 + 应用（--check 幂等保证：预检不过不动主仓库）。
	if _, err := gitOut(h.RepoRoot, "apply", "--check", h.PatchPath); err != nil {
		return "", fmt.Errorf("patch 应用预检失败（主仓库未动）: %v", err)
	}
	if _, err := gitOut(h.RepoRoot, "apply", h.PatchPath); err != nil {
		return "", fmt.Errorf("patch 应用失败（主仓库未动）: %v", err)
	}
	// 5. 清理副本与专属分支（best-effort，失败不回滚已应用的 patch）。
	d.removeWorktreeCopy(h)
	h.Merged = true
	log.Printf("[subagent] WORKTREE-MERGED: sub=%s patch=%s", agentID, h.PatchPath)
	return "已合入主仓库并清理副本：" + h.PatchPath + "\n" + h.PatchStat, nil
}

// worktreeContractCheck 对 worktree 副本跑父 spec 契约静态检查。
// 只跑"契约文件与 patch 有交集"的记录（纯他域范围的契约在副本内必然文件缺失，
// 跑了只会误报）；返回违例文案（空 = 通过或无可跑契约）。
func (d *Dispatcher) worktreeContractCheck(ctx context.Context, h *worktreeHandle) string {
	recs := d.parentSpecRecordsOf(h.ParentID)
	if len(recs) == 0 {
		return ""
	}
	patchFiles := map[string]bool{}
	if names, err := gitOut(h.Path, "diff", "--cached", "--name-only", h.BaseCommit); err == nil {
		for _, f := range strings.Split(names, "\n") {
			f = strings.TrimSpace(f)
			if f != "" {
				patchFiles[f] = true
			}
		}
	}
	var b strings.Builder
	for _, rec := range recs {
		if rec == nil || rec.contract == nil || rec.contract.Empty() {
			continue
		}
		if !contractTouches(rec.contract, patchFiles) {
			continue
		}
		sub := d.runContractChecksIn(h.Path, rec.contract, rec.files)
		for _, v := range sub.violations {
			b.WriteString("- " + v.file + ": " + v.detail + "\n")
		}
	}
	return b.String()
}

// contractTouches 判断契约声明的任一文件是否出现在 patch 变更清单里。
func contractTouches(c *tool.Contract, patchFiles map[string]bool) bool {
	touched := func(f string) bool { return patchFiles[strings.TrimSpace(f)] }
	for _, s := range c.Symbols {
		if touched(s.File) {
			return true
		}
		for _, r := range s.Refs {
			if touched(r) {
				return true
			}
		}
	}
	for _, e := range c.DOMIDs {
		if touched(e.File) {
			return true
		}
	}
	for _, e := range c.Scripts {
		if touched(e.File) {
			return true
		}
	}
	for _, e := range c.Signatures {
		if touched(e.File) {
			return true
		}
	}
	return false
}

// rejectWorktree 驳回交付：comments 经 mailbox 回该域。热驻槽存活则子 Agent 续改
//（Idle/Paused 槽下次轮询 drain 邮箱读到）；槽已销毁（非热驻一次性实例）提示重派。
// 副本保留（改动还在，重派可参考 patch）。
func (d *Dispatcher) rejectWorktree(ctx context.Context, sessionID, agentID, comments string) (string, error) {
	h := d.worktreeHandleFor(agentID)
	if h == nil || h.SessionID != sessionID {
		return "", fmt.Errorf("worktree 不存在: agent=%s", agentID)
	}
	if h.Merged || h.Removed {
		return "", fmt.Errorf("该 worktree 已合并/清理，无法驳回: agent=%s", agentID)
	}
	comments = strings.TrimSpace(comments)
	if d.mailbox != nil {
		body := "【worktree 驳回】你的交付未通过合并门审查，需按意见修改后重新提交。\n"
		if comments != "" {
			body += "修改意见：\n" + comments + "\n"
		}
		body += "副本路径: " + h.Path + "（变更仍在副本内，继续在该目录工作）"
		if _, err := d.mailbox.Send(&mailbox.Message{
			From:    "dispatcher",
			To:      agentID,
			Type:    mailbox.MsgInfo,
			Subject: "worktree 交付被驳回",
			Body:    body,
		}); err != nil {
			log.Printf("[subagent] WORKTREE-REJECT notify failed: to=%s err=%v", agentID, err)
		}
	}
	// 热驻槽存活判定：Idle 槽下次唤醒 drain 邮箱即读到；Running 则主循环下轮读到。
	if d.pool != nil && d.pool.slot(sessionID, agentID) != nil {
		d.pokeParent(h.ParentID)
		return "已驳回并投递修改意见至该域邮箱（热驻槽存活，续改后重新提交 merge）。", nil
	}
	return "已驳回并投递修改意见（该 Agent 实例已销毁，意见可能无法送达——请携带意见重派该域任务，副本 patch 可作参考: " + h.Path + "）。", nil
}

// removeWorktreeCopy best-effort 删除副本目录与专属分支，并置 Removed 标记。
func (d *Dispatcher) removeWorktreeCopy(h *worktreeHandle) {
	if _, err := gitOut(h.RepoRoot, "worktree", "remove", "--force", h.Path); err != nil {
		log.Printf("[subagent] WORKTREE remove failed: path=%s err=%v", h.Path, err)
		_, _ = gitOut(h.RepoRoot, "worktree", "prune")
	}
	if _, err := gitOut(h.RepoRoot, "branch", "-D", h.Branch); err != nil {
		log.Printf("[subagent] WORKTREE branch delete failed: branch=%s err=%v", h.Branch, err)
	}
	h.Removed = true
}

// CleanupSessionWorktrees 会话终态清理：best-effort 强制移除该会话全部残留副本
//（软停销毁/硬取消/Shutdown 调用）。patch 文件保留（.bma/worktrees/*.patch）供事后审计。
func (d *Dispatcher) CleanupSessionWorktrees(sessionID string) {
	d.worktrees.Range(func(_, v any) bool {
		h, ok := v.(*worktreeHandle)
		if !ok || h.SessionID != sessionID || h.Merged || h.Removed {
			return true
		}
		d.removeWorktreeCopy(h)
		log.Printf("[subagent] WORKTREE-CLEANUP: session=%s agent=%s", sessionID, h.AgentID)
		return true
	})
}

// worktreePatchNote 成功收尾钩子：句柄存在时产出 patch 并返回摘要附言（空串零打扰）。
func (d *Dispatcher) worktreePatchNote(ctx context.Context, subAgentID string) string {
	if d.worktreeHandleFor(subAgentID) == nil {
		return ""
	}
	return d.producePatchForAgent(ctx, subAgentID)
}

// shortSHA 截断 SHA 便于人读报错。
func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
