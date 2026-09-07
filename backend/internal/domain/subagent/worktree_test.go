package subagent

// worktree_test.go 验证 worktree 隔离派发与合并门（TODO 第9⑤/#10⑤）：
//   - 副本创建 + 隔离性（副本写入主目录不可见）；
//   - 成功收尾 patch 产出 + 合并门 merge 全流程（apply 后主目录可见、副本与分支清理）；
//   - base 漂移拒绝（他域已合入场景）；
//   - 跨域契约违例拦截（patch 触及契约文件才跑的收窄逻辑）；
//   - reject 驳回经 mailbox 回信；
//   - 非 git 目录拒绝（不静默降级）与开关关闭拒绝。
//
// fixture：t.TempDir 里 git init + 初始提交（本地 user 配置，无网络依赖）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/domain/role"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// newWorktreeRepo 构造 git 仓库 fixture：init + 本地身份 + base.txt 初始提交。
func newWorktreeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := gitOut(dir, "init"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	// 提交身份用仓库本地配置，不依赖全局 gitconfig。
	if _, err := gitOut(dir, "config", "user.email", "bma-test@example.com"); err != nil {
		t.Fatalf("git config email: %v", err)
	}
	if _, err := gitOut(dir, "config", "user.name", "bma-test"); err != nil {
		t.Fatalf("git config name: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0644); err != nil {
		t.Fatalf("write base.txt: %v", err)
	}
	if _, err := gitOut(dir, "add", "-A"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := gitOut(dir, "commit", "-m", "init"); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	return dir
}

// newWorktreeTestEnv 构造 worktree 测试用 Dispatcher（worktree 开启）+ 邮箱 + 权威树。
func newWorktreeTestEnv(t *testing.T) (*Dispatcher, *mailbox.Mailbox, *orchestrator.Tree) {
	t.Helper()
	cfg := &config.RoleConfigFile{
		MetaAgent:   config.MetaAgentConfig{SystemPrompt: "meta", ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		DomainAgent: config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "mock"}, SystemPrompt: "domain"},
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true, SystemPrompt: "code"},
		},
	}
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mb := mailbox.New()
	d := NewDispatcher(reg, &mockModelFactory{provider: &mockProvider{text: "done"}}, toolsReg, mb, agent.NopMemoryPipeline{})
	d.WithWorktreeEnabled(true)
	tr := orchestrator.NewTree("s1", nil)
	d.WithTree(func(string) *orchestrator.Tree { return tr })
	return d, mb, tr
}

// worktreeCtx 构造带父 Agent ID + 会话工作目录（指向 git 仓库）的派发上下文。
func worktreeCtx(repoDir string) context.Context {
	return tool.WithWorkDir(agent.WithAgentID(context.Background(), "s1"), repoDir)
}

// TestWorktreeCreateIsolatePatchMerge 全流程：创建副本 → 副本内写入（主目录不可见）→
// patch 产出 → merge 应用主目录 → 副本与分支清理。
func TestWorktreeCreateIsolatePatchMerge(t *testing.T) {
	repoDir := newWorktreeRepo(t)
	d, _, _ := newWorktreeTestEnv(t)
	ctx := worktreeCtx(repoDir)

	h, rej := d.createWorktreeForDispatch(ctx, "s1", "s1/domain-1", "s1", "测试域", "domain")
	if rej != nil || h == nil {
		t.Fatalf("create worktree failed: %+v", rej)
	}
	// 副本分支与 base 已登记。
	if !strings.HasPrefix(h.Branch, "bma/") {
		t.Errorf("branch should be prefixed bma/, got %q", h.Branch)
	}
	if strings.TrimSpace(h.BaseCommit) == "" {
		t.Fatal("base commit should be recorded")
	}

	// 隔离性：副本内写入对主目录不可见。
	if err := os.WriteFile(filepath.Join(h.Path, "wt_only.txt"), []byte("isolated\n"), 0644); err != nil {
		t.Fatalf("write in worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoDir, "wt_only.txt")); !os.IsNotExist(err) {
		t.Fatal("main repo must not see worktree writes before merge")
	}

	// 成功收尾：产出 patch（附言 + 落盘 + stat）。
	note := d.worktreePatchNote(ctx, "s1/domain-1")
	if !strings.Contains(note, "worktree 交付") {
		t.Errorf("patch note should mention delivery, got %q", note)
	}
	if h.PatchPath == "" {
		t.Fatal("patch path should be set after produce")
	}
	if _, err := os.Stat(h.PatchPath); err != nil {
		t.Fatalf("patch file should exist: %v", err)
	}

	// 合并门：base 未漂移、无契约 → apply 成功，主目录可见副本写入。
	out, err := d.MergeWorktree(ctx, "s1", "s1/domain-1")
	if err != nil {
		t.Fatalf("merge failed: %v", err)
	}
	if !strings.Contains(out, "已合入主仓库") {
		t.Errorf("merge output should confirm, got %q", out)
	}
	data, err := os.ReadFile(filepath.Join(repoDir, "wt_only.txt"))
	// 行尾不做强断言：Windows autocrlf 下 apply 可能写 CRLF，内容一致即通过。
	if err != nil || strings.TrimSpace(string(data)) != "isolated" {
		t.Fatalf("merged file missing/incorrect in main repo: %v %q", err, data)
	}
	if _, err := os.Stat(h.Path); !os.IsNotExist(err) {
		t.Error("worktree copy should be removed after merge")
	}
	if _, err := gitOut(repoDir, "rev-parse", "--verify", h.Branch); err == nil {
		t.Error("worktree branch should be deleted after merge")
	}
}

// TestWorktreeMergeBaseDriftReject 验证 base 漂移拒绝：他域已合入（主仓库 HEAD 前进）
// 后合并被拒，副本保留待人工 rebase/重派。
func TestWorktreeMergeBaseDriftReject(t *testing.T) {
	repoDir := newWorktreeRepo(t)
	d, _, _ := newWorktreeTestEnv(t)
	ctx := worktreeCtx(repoDir)

	h, rej := d.createWorktreeForDispatch(ctx, "s1", "s1/domain-1", "s1", "测试域", "domain")
	if rej != nil || h == nil {
		t.Fatalf("create worktree failed: %+v", rej)
	}
	if err := os.WriteFile(filepath.Join(h.Path, "wt_only.txt"), []byte("x\n"), 0644); err != nil {
		t.Fatalf("write in worktree: %v", err)
	}
	if note := d.worktreePatchNote(ctx, "s1/domain-1"); note == "" {
		t.Fatal("patch should be produced")
	}

	// 主仓库前进一格 HEAD（模拟他域已合入）。
	if err := os.WriteFile(filepath.Join(repoDir, "other.txt"), []byte("other domain\n"), 0644); err != nil {
		t.Fatalf("write other.txt: %v", err)
	}
	if _, err := gitOut(repoDir, "add", "-A"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := gitOut(repoDir, "commit", "-m", "other domain merged"); err != nil {
		t.Fatalf("git commit: %v", err)
	}

	_, err := d.MergeWorktree(ctx, "s1", "s1/domain-1")
	if err == nil || !strings.Contains(err.Error(), "base 漂移") {
		t.Fatalf("merge should be rejected for base drift, got %v", err)
	}
	// 副本保留（人工 rebase/重派需要）。
	if _, statErr := os.Stat(h.Path); statErr != nil {
		t.Error("worktree copy should be kept after drift rejection")
	}
}

// TestWorktreeMergeContractViolation 验证合并门契约检查：父 spec 契约声明符号必须
// 出现在副本内的声明文件，缺失即拒绝合并（检查基准 = worktree 副本）。
func TestWorktreeMergeContractViolation(t *testing.T) {
	repoDir := newWorktreeRepo(t)
	d, _, _ := newWorktreeTestEnv(t)
	ctx := worktreeCtx(repoDir)

	h, rej := d.createWorktreeForDispatch(ctx, "s1", "s1/domain-1", "s1", "测试域", "domain")
	if rej != nil || h == nil {
		t.Fatalf("create worktree failed: %+v", rej)
	}
	// 副本写入契约声明文件但不包含契约符号。
	if err := os.WriteFile(filepath.Join(h.Path, "wt_only.txt"), []byte("no expected symbol here\n"), 0644); err != nil {
		t.Fatalf("write in worktree: %v", err)
	}
	if note := d.worktreePatchNote(ctx, "s1/domain-1"); note == "" {
		t.Fatal("patch should be produced")
	}
	// 注入父 spec 记录：契约要求 wt_only.txt 声明符号 expectedFunc。
	d.parentSpecs.Store(specRecKey{parentID: "s1", domain: "测试域"}, &parentSpecRecord{
		files: []string{filepath.Join(repoDir, "wt_only.txt")},
		contract: &tool.Contract{Symbols: []tool.ContractSymbol{{
			Symbol: "expectedFunc",
			File:   "wt_only.txt",
		}}},
	})

	_, err := d.MergeWorktree(ctx, "s1", "s1/domain-1")
	if err == nil || !strings.Contains(err.Error(), "契约检查未通过") {
		t.Fatalf("merge should be rejected by contract check, got %v", err)
	}
	// 主仓库未被写入（apply 前置）。
	if _, statErr := os.Stat(filepath.Join(repoDir, "wt_only.txt")); !os.IsNotExist(statErr) {
		t.Error("main repo must stay untouched on contract violation")
	}
}

// TestWorktreeRejectNotifiesMailbox 验证驳回回信：comments + 副本路径经 mailbox 投递
// 给该 Agent 实例。
func TestWorktreeRejectNotifiesMailbox(t *testing.T) {
	repoDir := newWorktreeRepo(t)
	d, mb, _ := newWorktreeTestEnv(t)
	ctx := worktreeCtx(repoDir)

	if _, rej := d.createWorktreeForDispatch(ctx, "s1", "s1/domain-1", "s1", "测试域", "domain"); rej != nil {
		t.Fatalf("create worktree failed: %+v", rej)
	}
	out, err := d.RejectWorktree(ctx, "s1", "s1/domain-1", "符号缺失请补齐")
	if err != nil {
		t.Fatalf("reject failed: %v", err)
	}
	if !strings.Contains(out, "驳回") {
		t.Errorf("reject output should confirm, got %q", out)
	}
	msgs := mb.Drain("s1/domain-1")
	if len(msgs) != 1 {
		t.Fatalf("want 1 mailbox message, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Body, "符号缺失请补齐") || !strings.Contains(msgs[0].Body, "worktree 驳回") {
		t.Errorf("reject message should carry comments and marker, got %q", msgs[0].Body)
	}
}

// TestWorktreeCreateNonRepoReject 验证主目录非 git 仓库时拒绝（不静默降级主目录派发）。
func TestWorktreeCreateNonRepoReject(t *testing.T) {
	d, _, _ := newWorktreeTestEnv(t)
	plain := t.TempDir() // 无 git init 的普通目录
	_, rej := d.createWorktreeForDispatch(worktreeCtx(plain), "s1", "s1/domain-1", "s1", "测试域", "domain")
	if rej == nil || !strings.Contains(rej.Error, "git 仓库") {
		t.Fatalf("non-repo dir should be rejected, got %+v", rej)
	}
}

// TestDispatchOneWorktreeDisabledReject 验证开关关闭时 worktree 派发拒绝。
func TestDispatchOneWorktreeDisabledReject(t *testing.T) {
	d, _, _ := newWorktreeTestEnv(t)
	d.WithWorktreeEnabled(false)
	_, res := d.dispatchOne(worktreeCtx(t.TempDir()), "code_assistant", "", "task", "", "", "none", nil, nil, 0, "", "", &dispatchOpts{worktree: true})
	if res == nil || !strings.Contains(res.Error, "worktree 派发未启用") {
		t.Fatalf("disabled worktree should be rejected, got %+v", res)
	}
}

// TestWorktreeDiffLiveAndFinalized 验证 review 数据源：未收尾读副本实时 diff，
// 收尾后读落盘 patch；已删除副本报错。
func TestWorktreeDiffLiveAndFinalized(t *testing.T) {
	repoDir := newWorktreeRepo(t)
	d, _, _ := newWorktreeTestEnv(t)
	ctx := worktreeCtx(repoDir)

	h, rej := d.createWorktreeForDispatch(ctx, "s1", "s1/domain-1", "s1", "测试域", "domain")
	if rej != nil || h == nil {
		t.Fatalf("create worktree failed: %+v", rej)
	}
	// 未收尾：实时 diff 可读。
	if err := os.WriteFile(filepath.Join(h.Path, "wt_only.txt"), []byte("live\n"), 0644); err != nil {
		t.Fatalf("write in worktree: %v", err)
	}
	live, err := d.WorktreeDiff("s1", "s1/domain-1")
	if err != nil || !strings.Contains(live, "wt_only.txt") {
		t.Fatalf("live diff should contain the new file, err=%v diff=%q", err, live)
	}
	// 收尾后：patch 全文可读。
	if note := d.worktreePatchNote(ctx, "s1/domain-1"); note == "" {
		t.Fatal("patch should be produced")
	}
	final, err := d.WorktreeDiff("s1", "s1/domain-1")
	if err != nil || !strings.Contains(final, "wt_only.txt") {
		t.Fatalf("finalized diff should read patch file, err=%v diff=%q", err, final)
	}
	// 清理后报错。
	d.CleanupSessionWorktrees("s1")
	if _, err := d.WorktreeDiff("s1", "s1/domain-1"); err == nil || !strings.Contains(err.Error(), "已删除") {
		t.Fatalf("diff on removed copy should error, got %v", err)
	}
}
