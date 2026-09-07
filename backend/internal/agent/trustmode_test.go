package agent

// trustmode_test.go 验证会话级信任模式（TODO 第10⑥）服务层接线：
//   - SetDefaultTrustMode 注入 config 默认档，createSession 取为初始值；
//   - SetSessionTrustMode 切换既有会话（枚举校验），原子生效；
//   - runSession/resumeSession 经 WithTrustModeFunc 注入读取器——切换下一工具调用生效
//   （读取器→Registry 链路在 tool/trust_mode_test.go 已覆盖）。

import "testing"

// TestReactService_TrustModeLifecycle 全生命周期：默认档 → 会话初值 → 中途切换 → 读取。
func TestReactService_TrustModeLifecycle(t *testing.T) {
	svc := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())

	// 默认档注入：非法值 fail-fast。
	if err := svc.SetDefaultTrustMode("bogus"); err == nil {
		t.Fatal("SetDefaultTrustMode must reject invalid mode")
	}
	if err := svc.SetDefaultTrustMode("suggest"); err != nil {
		t.Fatalf("SetDefaultTrustMode: %v", err)
	}

	sess := svc.store.createSession("信任模式测试", "")
	if got := sess.currentTrustMode(); got != "suggest" {
		t.Fatalf("createSession should inherit default trust mode, got %q", got)
	}

	// 未配置默认档：新会话 trustMode 为空（Registry 回退现网语义）。
	svc2 := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())
	sess2 := svc2.store.createSession("无默认档", "")
	if got := sess2.currentTrustMode(); got != "" {
		t.Fatalf("unset default should leave session trust mode empty, got %q", got)
	}

	// 中途切换：枚举校验 + 会话原子生效 + 读取器同步可见。
	if err := svc.SetSessionTrustMode(sess.ID, "bogus"); err == nil {
		t.Fatal("SetSessionTrustMode must reject invalid mode")
	}
	if err := svc.SetSessionTrustMode(sess.ID, "auto-edit"); err != nil {
		t.Fatalf("SetSessionTrustMode: %v", err)
	}
	if got := svc.SessionTrustMode(sess.ID); got != "auto-edit" {
		t.Fatalf("SessionTrustMode should reflect switch, got %q", got)
	}
	// 读取器返回切换后的值（runCtx 注入的就是这个方法值）。
	svc.store.mu.RLock()
	s := svc.store.sessions[sess.ID]
	svc.store.mu.RUnlock()
	if got := s.currentTrustMode(); got != "auto-edit" {
		t.Fatalf("getter must return switched mode, got %q", got)
	}

	// 不存在的会话报错。
	if err := svc.SetSessionTrustMode("session-none", "full-auto"); err == nil {
		t.Fatal("SetSessionTrustMode must fail for unknown session")
	}
}
