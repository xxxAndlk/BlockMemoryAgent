// Package agent EnsureProjectDoc 异步化测试（TODO #14 T9）。
// 背景：LLM 领域分区是秒级调用，同步等会把"创建会话→首字"延迟整段加在用户头上；
// 异步化后 createSession 必须立即返回，PROJECT.md 就绪信号经 projectDocReady 交付。
// 启发式路径（classifier 为 nil，测试默认）保持同步——纯文件扫描毫秒级，异步只会
// 引入"临时目录清理撞上在写 goroutine"的竞态。
package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/project"
)

// blockingClassifier 阻塞的领域分区器：Partition 挂起直到 release 关闭（模拟 LLM
// 分区调用的秒级耗时），ctx 取消时退出。
type blockingClassifier struct{ release chan struct{} }

func (c blockingClassifier) Partition(ctx context.Context, root string, files []string) ([]project.DomainPartition, error) {
	select {
	case <-c.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestCreateSession_AsyncProjectDoc 验证：LLM 分区器阻塞时 createSession 立即返回；
// 放行后 goroutine 收尾、projectDocReady 关闭。
func TestCreateSession_AsyncProjectDoc(t *testing.T) {
	release := make(chan struct{})
	s := newDedupTestService(t, &mockReactModelProvider{})
	s.store.setDomainClassifier(blockingClassifier{release: release})

	// 放一个源文件，保证 scanProject 会真正走到 Partition（空目录可能被跳过）。
	if err := os.WriteFile(filepath.Join(s.store.workDir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	start := time.Now()
	sess, err := s.CreateSession(context.Background(), CreateRequest{Goal: "翻译层测试"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("createSession blocked %v on LLM classifier; must return immediately", elapsed)
	}

	// 放行分区器，等 goroutine 收尾。
	close(release)
	s.store.mu.RLock()
	internal := s.store.sessions[sess.ID]
	ready := internal.projectDocReady
	s.store.mu.RUnlock()
	if ready == nil {
		t.Fatal("classifier set: projectDocReady must be armed")
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("projectDocReady not closed after classifier released")
	}
}
