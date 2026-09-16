package agent

// gear_test.go 验证会话执行档位（TODO #14 会话三档控制）服务层接线：
//   - SetDefaultGear 注入 config 默认档，createSession 取为初始值；
//   - SetSessionGear 切换既有会话（枚举校验），原子生效；
//   - MetaMemory 持久化往返：sessionMetaMemory 携带 gear，gearFromMetaMemory 读回
//   （落库/恢复全链路在集成测试覆盖，此处验证纯函数与字段接线）。

import (
	"context"
	"testing"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// TestReactService_GearLifecycle 全生命周期：默认档 → 会话初值 → 中途切换 → 读取。
func TestReactService_GearLifecycle(t *testing.T) {
	svc := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())

	// 默认档注入：非法值 fail-fast。
	if err := svc.SetDefaultGear("bogus"); err == nil {
		t.Fatal("SetDefaultGear must reject invalid gear")
	}
	if err := svc.SetDefaultGear(tool.GearFast); err != nil {
		t.Fatalf("SetDefaultGear: %v", err)
	}

	sess := svc.store.createSession("档位测试", "")
	if got := sess.currentGear(); got != tool.GearFast {
		t.Fatalf("createSession should inherit default gear, got %q", got)
	}

	// 未配置默认档：新会话 gear 为空（runSession 按集群档兜底）。
	svc2 := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())
	sess2 := svc2.store.createSession("无默认档", "")
	if got := sess2.currentGear(); got != "" {
		t.Fatalf("unset default should leave session gear empty, got %q", got)
	}

	// 中途切换：枚举校验 + 会话原子生效 + 读取器同步可见。
	if err := svc.SetSessionGear(sess.ID, "bogus"); err == nil {
		t.Fatal("SetSessionGear must reject invalid gear")
	}
	if err := svc.SetSessionGear(sess.ID, tool.GearCluster); err != nil {
		t.Fatalf("SetSessionGear: %v", err)
	}
	if got := svc.SessionGear(sess.ID); got != tool.GearCluster {
		t.Fatalf("SessionGear should reflect switch, got %q", got)
	}
	// 读取器返回切换后的值（runSession 起跑读取的就是这个方法值）。
	svc.store.mu.RLock()
	s := svc.store.sessions[sess.ID]
	svc.store.mu.RUnlock()
	if got := s.currentGear(); got != tool.GearCluster {
		t.Fatalf("getter must return switched gear, got %q", got)
	}

	// 不存在的会话报错。
	if err := svc.SetSessionGear("session-none", tool.GearFast); err == nil {
		t.Fatal("SetSessionGear must fail for unknown session")
	}
}

// TestGearMetaMemoryRoundTrip 验证 MetaMemory 持久化往返（TODO #14）：
// 有档位携带 {"gear":...}；无档位保持空切片（旧写入形态）；非法值/缺键回落 fallback。
func TestGearMetaMemoryRoundTrip(t *testing.T) {
	svc := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())
	sess := svc.store.createSession("持久化测试", "")
	svc.SetSessionGear(sess.ID, tool.GearFast)

	meta := sessionMetaMemory(sess)
	if len(meta) != 1 || meta[0]["gear"] != tool.GearFast {
		t.Fatalf("sessionMetaMemory should carry gear, got %+v", meta)
	}
	if got := gearFromMetaMemory(meta, tool.GearDaily); got != tool.GearFast {
		t.Fatalf("gearFromMetaMemory should read back fast, got %q", got)
	}

	// 无档位：空切片（旧行为不变），读回落 fallback。
	empty := sessionMetaMemory(svc.store.createSession("无档", ""))
	if len(empty) != 0 {
		t.Fatalf("sessionMetaMemory without gear should be empty slice, got %+v", empty)
	}
	if got := gearFromMetaMemory(empty, tool.GearCluster); got != tool.GearCluster {
		t.Fatalf("gearFromMetaMemory fallback, got %q", got)
	}

	// 旧记录缺键/非法值：回落 fallback。
	bogus := []map[string]any{{"gear": "warp"}, {"other": 1}}
	if got := gearFromMetaMemory(bogus, "daily"); got != "daily" {
		t.Fatalf("invalid stored gear must fall back, got %q", got)
	}

	// 历史值 "auto"（规则自动选档，2026-09-16 退役）：读侧映射 daily。
	legacy := []map[string]any{{"gear": "auto"}}
	if got := gearFromMetaMemory(legacy, tool.GearCluster); got != tool.GearDaily {
		t.Fatalf("legacy auto should map to daily, got %q", got)
	}
}

// TestSessionThinkingRoundTrip 验证会话级思考强度（2026-09-16）：
// 切换校验、MetaMemory 持久化往返（gear+thinking 同 map）、缺键回落空串、CreateRequest 覆盖。
func TestSessionThinkingRoundTrip(t *testing.T) {
	svc := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())
	ctx := context.Background()

	sess := svc.store.createSession("思考强度测试", "")
	if err := svc.SetSessionThinking(sess.ID, "bogus"); err == nil {
		t.Fatal("SetSessionThinking must reject invalid thinking")
	}
	// 大写/空白归一化（HTTP 层已先行校验，服务层宽容归一）。
	if err := svc.SetSessionThinking(sess.ID, " High "); err != nil {
		t.Fatalf("SetSessionThinking: %v", err)
	}
	if got := svc.SessionThinking(sess.ID); got != "high" {
		t.Fatalf("SessionThinking should normalize to high, got %q", got)
	}

	// 空串 = 跟随角色默认（合法清除）。
	if err := svc.SetSessionThinking(sess.ID, ""); err != nil {
		t.Fatalf("SetSessionThinking(empty): %v", err)
	}
	if got := svc.store.sessions[sess.ID].currentThinking(); got != "" {
		t.Fatalf("empty thinking should clear override, got %q", got)
	}

	// 不存在的会话报错。
	if err := svc.SetSessionThinking("session-none", "low"); err == nil {
		t.Fatal("SetSessionThinking must fail for unknown session")
	}

	// 持久化往返：gear+thinking 同 map 携带；缺键回落空串。
	svc.SetSessionGear(sess.ID, tool.GearDaily)
	svc.SetSessionThinking(sess.ID, "medium")
	meta := sessionMetaMemory(svc.store.sessions[sess.ID])
	if len(meta) != 1 || meta[0]["gear"] != tool.GearDaily || meta[0]["thinking"] != "medium" {
		t.Fatalf("sessionMetaMemory should carry gear+thinking, got %+v", meta)
	}
	if got := thinkingFromMetaMemory(meta); got != "medium" {
		t.Fatalf("thinkingFromMetaMemory should read back medium, got %q", got)
	}
	if got := thinkingFromMetaMemory([]map[string]any{{"gear": "daily"}}); got != "" {
		t.Fatalf("missing thinking key should fall back empty, got %q", got)
	}
	if got := thinkingFromMetaMemory([]map[string]any{{"thinking": "warp"}}); got != "" {
		t.Fatalf("invalid stored thinking should fall back empty, got %q", got)
	}

	// CreateRequest 覆盖：合法值入会话，非法值忽略。
	created, err := svc.CreateSession(ctx, CreateRequest{Goal: "创建带思考强度", Thinking: "low"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if got := svc.SessionThinking(created.ID); got != "low" {
		t.Fatalf("CreateRequest.Thinking 应入会话, got %q", got)
	}
	bogus, err := svc.CreateSession(ctx, CreateRequest{Goal: "创建非法思考强度", Thinking: "warp"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if got := svc.SessionThinking(bogus.ID); got != "" {
		t.Fatalf("非法 thinking 应回落空串, got %q", got)
	}
}

// TestReactService_CreateSessionGearOverride 验证 CreateRequest.Gear（TODO #14 新会话页
// 选档）：合法枚举覆盖 store 默认档；非法/空值回落默认（HTTP 层已先行 400，服务层宽容兜底）。
func TestReactService_CreateSessionGearOverride(t *testing.T) {
	svc := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())
	if err := svc.SetDefaultGear(tool.GearCluster); err != nil {
		t.Fatalf("SetDefaultGear: %v", err)
	}
	ctx := context.Background()

	fast, err := svc.CreateSession(ctx, CreateRequest{Goal: "显式快速档", Gear: tool.GearFast})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if got := svc.SessionGear(fast.ID); got != tool.GearFast {
		t.Fatalf("显式 gear 应覆盖默认档, got %q", got)
	}

	fallback, err := svc.CreateSession(ctx, CreateRequest{Goal: "非法值回落", Gear: "warp"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if got := svc.SessionGear(fallback.ID); got != tool.GearCluster {
		t.Fatalf("非法 gear 应回落默认 cluster, got %q", got)
	}
}
