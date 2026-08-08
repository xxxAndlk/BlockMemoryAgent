package tool

// sandbox_role_test.go 验证 Layer 4 角色级写沙箱 enforceRoleWritePath。
// dormant opt-in：角色无 Sandbox 配置时不限制，配了 allowed_write_paths 才限制。

import (
	"context"
	"path/filepath"
	"testing"
)

// TestEnforceRoleWritePath_AllowsWithinAllowed 验证角色配 ["src/"] 时，
// 写 src/ 下通过、写其他目录被拒。
func TestEnforceRoleWritePath_AllowsWithinAllowed(t *testing.T) {
	workDir := "/wd"
	e := NewExecutor(workDir)
	e.SetRoleWritePathResolver(func(roleID string) []string {
		if roleID == "code_assistant" {
			return []string{"src/"}
		}
		return nil
	})
	ctx := WithRoleID(context.Background(), "code_assistant")

	cases := map[string]bool{
		filepath.Join(workDir, "src", "a.go"): true,  // 在 src/ 下，允许
		filepath.Join(workDir, "doc", "b.md"): false, // 在 doc/ 下，拒绝
		filepath.Join(workDir, "c.txt"):       false, // workDir 根级，拒绝（不在 src/ 下）
	}
	for absPath, wantOK := range cases {
		err := e.enforceRoleWritePath(ctx, absPath)
		if wantOK && err != nil {
			t.Errorf("enforceRoleWritePath(%q) = err %v, want nil", absPath, err)
		}
		if !wantOK && err == nil {
			t.Errorf("enforceRoleWritePath(%q) = nil, want error", absPath)
		}
	}
}

// TestEnforceRoleWritePath_EmptyNoRestriction 验证角色无 Sandbox（resolver 返空）时不限制。
func TestEnforceRoleWritePath_EmptyNoRestriction(t *testing.T) {
	e := NewExecutor("/wd")
	e.SetRoleWritePathResolver(func(roleID string) []string { return nil })
	ctx := WithRoleID(context.Background(), "code_assistant")

	// 任意路径都应通过（resolver 返空切片 -> 不限制）。
	for _, absPath := range []string{"/wd/src/a.go", "/wd/doc/b.md", "/wd/c.txt"} {
		if err := e.enforceRoleWritePath(ctx, absPath); err != nil {
			t.Errorf("enforceRoleWritePath(%q) = err %v, want nil (empty allowed = unrestricted)", absPath, err)
		}
	}
}

// TestEnforceRoleWritePath_NoRoleInCtx 验证 ctx 无 roleID（如顶层未注入）时不限制。
func TestEnforceRoleWritePath_NoRoleInCtx(t *testing.T) {
	e := NewExecutor("/wd")
	e.SetRoleWritePathResolver(func(roleID string) []string {
		if roleID == "code_assistant" {
			return []string{"src/"}
		}
		return nil
	})
	// 无 roleID ctx（MetaAgent 顶层路径或未注入）-> 跳过角色级校验。
	if err := e.enforceRoleWritePath(context.Background(), "/wd/doc/b.md"); err != nil {
		t.Errorf("enforceRoleWritePath with no roleID = err %v, want nil", err)
	}
}

// TestEnforceRoleWritePath_NoResolver 验证未注入 resolver（nil）时不限制。
func TestEnforceRoleWritePath_NoResolver(t *testing.T) {
	e := NewExecutor("/wd")
	ctx := WithRoleID(context.Background(), "code_assistant")
	// 无 resolver -> 跳过。
	if err := e.enforceRoleWritePath(ctx, "/wd/doc/b.md"); err != nil {
		t.Errorf("enforceRoleWritePath with no resolver = err %v, want nil", err)
	}
}

// TestEnforceRoleWritePath_AbsoluteAllowedPath 验证 allowed_write_paths 用绝对路径也生效。
func TestEnforceRoleWritePath_AbsoluteAllowedPath(t *testing.T) {
	workDir := t.TempDir() // 真绝对路径，跨平台
	absAllowed := filepath.Join(workDir, "artifacts", "code")
	e := NewExecutor(workDir)
	e.SetRoleWritePathResolver(func(roleID string) []string {
		return []string{absAllowed}
	})
	ctx := WithRoleID(context.Background(), "code_assistant")

	if err := e.enforceRoleWritePath(ctx, filepath.Join(absAllowed, "x.go")); err != nil {
		t.Errorf("absolute allowed path under config = err %v, want nil", err)
	}
	if err := e.enforceRoleWritePath(ctx, filepath.Join(workDir, "other", "y.go")); err == nil {
		t.Errorf("path outside absolute allowed = nil, want error")
	}
}
