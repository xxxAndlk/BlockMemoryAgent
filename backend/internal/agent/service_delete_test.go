// Package agent DeleteSession 硬删除测试：内存摘除 + ctx 取消 + 迟到落库守卫 + 404 语义。
package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/blockmemory/agent/backend/pkg/enums"
)

// TestDeleteSession_RemovesAndGuards 验证：删除后会话从内存摘除、运行 ctx 被取消、
// stillLive 守卫翻 false（迟到收尾落库被拦截）、Get/再次删除均报 ErrSessionNotFound。
func TestDeleteSession_RemovesAndGuards(t *testing.T) {
	s := newDedupTestService(t, nil)
	// 直塞 store（同 service_create_dedup_test 的确定性做法）：只建对象不起 runSession goroutine。
	sess := s.store.createSession("删除目标", "")
	stCtx := sess.ctx

	if err := s.DeleteSession(context.Background(), sess.ID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	s.store.mu.RLock()
	_, exists := s.store.sessions[sess.ID]
	s.store.mu.RUnlock()
	if exists {
		t.Fatalf("session should be removed from memory map")
	}
	if stCtx.Err() != context.Canceled {
		t.Errorf("session ctx should be canceled, got %v", stCtx.Err())
	}
	if s.store.stillLive(sess) {
		t.Errorf("stillLive must be false after delete (late persist guard)")
	}
	if _, err := s.Get(context.Background(), sess.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("Get after delete: want ErrSessionNotFound, got %v", err)
	}
	if err := s.DeleteSession(context.Background(), sess.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("second DeleteSession: want ErrSessionNotFound, got %v", err)
	}
}

// TestDeleteSession_NotFound 未知会话（内存与 PG 均无）返回 ErrSessionNotFound。
func TestDeleteSession_NotFound(t *testing.T) {
	s := newDedupTestService(t, nil)
	if err := s.DeleteSession(context.Background(), "session-unknown"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("want ErrSessionNotFound, got %v", err)
	}
}

// TestStillLive_PointerIdentity 守卫按指针身份判定：同 ID 不同实例（重启恢复替换的副本）
// 或已摘除的实例都不视为存活，避免误放行旧实例的落库覆盖新会话数据。
func TestStillLive_PointerIdentity(t *testing.T) {
	s := newDedupTestService(t, nil)
	orig := &reactInternalSession{ID: "sess-x", Status: enums.SessionStatusCompleted}
	s.store.mu.Lock()
	s.store.sessions[orig.ID] = orig
	s.store.mu.Unlock()

	if !s.store.stillLive(orig) {
		t.Fatalf("live pointer should report live")
	}
	imposter := &reactInternalSession{ID: "sess-x"}
	if s.store.stillLive(imposter) {
		t.Errorf("different pointer with same ID must not report live")
	}
	s.store.mu.Lock()
	delete(s.store.sessions, orig.ID)
	s.store.mu.Unlock()
	if s.store.stillLive(orig) {
		t.Errorf("removed session must not report live")
	}
	if s.store.stillLive(nil) {
		t.Errorf("nil session must not report live")
	}
}
