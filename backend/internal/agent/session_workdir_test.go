package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-kratos/blades"

	"github.com/blockmemory/agent/backend/internal/project"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// recordingModelProvider 捕获首轮 LLM 请求的系统提示词(Instruction),随即返回 done 结束会话。
// 供终审修复 I-1 测试断言 MetaAgent 系统提示词的工作目录来源。
type recordingModelProvider struct {
	mu  sync.Mutex
	sys string
}

// Generate 实现 blades.ModelProvider 接口:记录 Instruction 文本,返回 done 让会话立即完成。
func (m *recordingModelProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	m.mu.Lock()
	if m.sys == "" && req != nil && req.Instruction != nil {
		m.sys = systemText(req)
	}
	m.mu.Unlock()
	return &blades.ModelResponse{Message: blades.AssistantMessage("done")}, nil
}

// Name 返回 provider 名称,用于日志与调试。
func (m *recordingModelProvider) Name() string { return "recording" }

// systemPromptOf 创建会话并等其完成,返回 MetaAgent 首轮系统提示词。
func systemPromptOf(t *testing.T, rec *recordingModelProvider, s *ReactService, workDir string) string {
	t.Helper()
	sess, err := s.CreateSession(context.Background(), CreateRequest{Goal: "g", WorkDir: workDir})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := s.Get(context.Background(), sess.ID)
		if err != nil {
			t.Fatalf("Get error: %v", err)
		}
		if got.Status == string(enums.SessionStatusCompleted) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.sys == "" {
		t.Fatal("provider 未被调用或系统提示词为空")
	}
	return rec.sys
}

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

// TestRunSession_SystemPromptPerSessionWorkDir 验证终审修复 I-1:自定义 workDir 会话的
// MetaAgent 系统提示词——工作目录行与【项目概览】(PROJECT.md)均取自会话目录,
// 与 createSession 在该目录 EnsureProjectDoc 生成的 PROJECT.md 对上;
// 未设 workDir 的会话回落进程默认目录。
func TestRunSession_SystemPromptPerSessionWorkDir(t *testing.T) {
	dirA := t.TempDir() // 进程默认目录
	dirB := t.TempDir() // 会话目录
	// 会话目录预写带哨兵的 PROJECT.md:createSession 的 EnsureProjectDoc 见已存在不覆盖。
	if err := os.MkdirAll(filepath.Join(dirB, ".bma"), 0o755); err != nil {
		t.Fatal(err)
	}
	const sentinel = "SENTINEL-SESSION-PROJECT-DOC"
	doc := project.ManagedBegin + "\n" + sentinel + "\n" + project.ManagedEnd + "\n"
	if err := os.WriteFile(filepath.Join(dirB, ".bma", "PROJECT.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	// 自定义 workDir 会话:工作目录行与 PROJECT.md 均取自 dirB。
	rec := &recordingModelProvider{}
	s := newReactServiceForTest(rec, dirA)
	sys := systemPromptOf(t, rec, s, dirB)
	if !strings.Contains(sys, "- 工作目录: "+dirB) {
		t.Fatalf("系统提示词工作目录行未取会话目录 %q:\n%s", dirB, sys)
	}
	if !strings.Contains(sys, sentinel) {
		t.Fatalf("系统提示词项目概览未取会话目录 PROJECT.md:\n%s", sys)
	}

	// 空 workDir 会话:回落进程默认目录 dirA。
	rec2 := &recordingModelProvider{}
	s2 := newReactServiceForTest(rec2, dirA)
	sys2 := systemPromptOf(t, rec2, s2, "")
	if !strings.Contains(sys2, "- 工作目录: "+dirA) {
		t.Fatalf("空 workDir 会话应回落进程目录 %q:\n%s", dirA, sys2)
	}
	s.Shutdown(context.Background())
	s2.Shutdown(context.Background())
}
