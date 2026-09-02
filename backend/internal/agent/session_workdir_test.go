package agent

import (
	"context"
	"path/filepath"
	"testing"
)

// TestCreateSession_PerSessionWorkDir 验证每会话工作目录（S2）：
// CreateRequest.WorkDir 非空时会话级 TempDir 基于该目录；空时回落服务默认 workDir。
func TestCreateSession_PerSessionWorkDir(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	// provider 不能传 nil：空角色配置仍会合成 meta 角色，runSession 会在
	// nil modelFactory.GetBladesProvider 上 panic（goroutine 内崩溃打爆测试二进制）。
	// mockReactModelProvider 无预设响应时首轮即返回 "done"，会话快速完成。
	s := newReactServiceForTest(&mockReactModelProvider{}, dirA)
	sess, err := s.CreateSession(context.Background(), CreateRequest{Goal: "g", WorkDir: dirB})
	if err != nil {
		t.Fatal(err)
	}
	if sess.WorkDir != dirB {
		t.Fatalf("WorkDir got %q want %q", sess.WorkDir, dirB)
	}
	wantTemp := filepath.Join(dirB, ".bma", "tmp", sess.ID)
	if sess.TempDir != wantTemp {
		t.Fatalf("TempDir got %q want %q", sess.TempDir, wantTemp)
	}
	// 空 WorkDir 回落默认
	sess2, err := s.CreateSession(context.Background(), CreateRequest{Goal: "g2"})
	if err != nil {
		t.Fatal(err)
	}
	if sess2.WorkDir != "" {
		t.Fatalf("empty WorkDir should stay empty, got %q", sess2.WorkDir)
	}
	wantTemp2 := filepath.Join(dirA, ".bma", "tmp", sess2.ID)
	if sess2.TempDir != wantTemp2 {
		t.Fatalf("fallback TempDir got %q want %q", sess2.TempDir, wantTemp2)
	}
	s.Shutdown(context.Background())
}
