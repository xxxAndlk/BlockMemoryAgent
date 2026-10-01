package tool

// sandbox_skill_scripts_test.go TODO 25 阶段 C2：经验技能 scripts/ 目录的沙箱放行验证。
//
// 背景：技能脚本执行发生在会话内 agent 的 RunCommand（只过命令黑名单，不做路径校验），
// 但执行前 agent 常用 ReadFile/写入类工具接触 <skills_learned>/<name>/scripts/——该目录在
// 会话工作目录之外，默认被 resolvePathWithSandbox 拦截。bootstrap 启动时把 skills_learned
// 并入 SandboxConfig.AllowedPaths（与 tool_sandbox_allowed_paths 同机制），本测试锁定
// 该前缀放行语义：目录内可读、目录外仍被拒。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSandboxAllowedPathsPermitSkillScripts skills_learned 前缀经 AllowedPaths 放行读。
func TestSandboxAllowedPathsPermitSkillScripts(t *testing.T) {
	workDir := t.TempDir()
	skillsDir := t.TempDir() // 模拟 config/skills_learned（工作目录之外）

	// 造一个带脚本的经验技能目录。
	scriptPath := filepath.Join(skillsDir, "img-dedup", "scripts", "dedup.py")
	if err := os.MkdirAll(filepath.Dir(scriptPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scriptPath, []byte("print('ok')"), 0o644); err != nil {
		t.Fatal(err)
	}

	e := NewExecutor(workDir)
	cfg := DefaultSandboxConfig()
	cfg.AllowedPaths = []string{skillsDir} // bootstrap C2 注入 skills_learned 后的形态
	e.SetSandboxConfig(&cfg)

	// 技能脚本可读（agent 执行前查看脚本内容）。
	if _, err := e.resolvePathWithSandbox(context.Background(), scriptPath); err != nil {
		t.Fatalf("skill script read must be allowed: %v", err)
	}
	// 技能目录内其他文件（SKILL.md）同样可读。
	skillMD := filepath.Join(skillsDir, "img-dedup", "SKILL.md")
	if err := os.WriteFile(skillMD, []byte("---\nname: x\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.resolvePathWithSandbox(context.Background(), skillMD); err != nil {
		t.Fatalf("SKILL.md read must be allowed: %v", err)
	}
	// 写校验同前缀放行（审计/整理场景写回技能目录）。
	if err := e.sanitizeWritePath(context.Background(), skillMD); err != nil {
		t.Fatalf("skill dir write must be allowed: %v", err)
	}
	// 目录外路径仍被拦截（白名单不扩散）。
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.resolvePathWithSandbox(context.Background(), outside); err == nil ||
		!strings.Contains(err.Error(), "path escapes sandbox") {
		t.Fatalf("outside path must be rejected with sandbox error, got %v", err)
	}
}
