package agent

// pinned_plugins_test.go 验证会话级指定插件（KIMI 式插件选择）服务层接线：
//   - SetSessionPinnedPlugins：resolver 严格校验（无效整体拒绝）、工具预挂/差集卸载、
//     会话原子生效、事件留痕；
//   - MetaMemory 持久化往返：sessionMetaMemory 携带 pinned_plugins，
//     pinnedPluginsFromMetaMemory 读回（含 JSONB 往返后的 []any 形态）；
//   - CreateRequest.PinnedPlugins 创建路径从宽（无效 ID 丢弃不阻断）；
//   - pinnedPluginBlock 提示块注入/零注入。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// stubPinTool 最小插件工具桩（注册进 Registry 供挂载校验）。
type stubPinTool struct{ name string }

func (s *stubPinTool) Name() string      { return s.name }
func (s *stubPinTool) Aliases() []string { return nil }
func (s *stubPinTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	return &tool.Result{Tool: s.name, Success: true, Output: "pin-ok:" + s.name}
}

// fakePinResolver 模拟 bootstrap 注入的插件解析器：plugA/plugB 有效，其余无效。
func fakePinResolver(ids []string) (valid []PinnedPluginInfo, invalid []string) {
	known := map[string][]string{
		"plugA": {"plugA__do"},
		"plugB": {"plugB__x", "plugB__y"},
	}
	for _, id := range ids {
		if tools, ok := known[id]; ok {
			valid = append(valid, PinnedPluginInfo{ID: id, Name: "插件" + id, Tools: tools})
		} else {
			invalid = append(invalid, id)
		}
	}
	return valid, invalid
}

func newPinTestService(t *testing.T) *ReactService {
	t.Helper()
	svc := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())
	svc.SetPluginToolsResolver(fakePinResolver)
	svc.toolRegistry.Register(&stubPinTool{name: "plugA__do"})
	svc.toolRegistry.Register(&stubPinTool{name: "plugB__x"})
	svc.toolRegistry.Register(&stubPinTool{name: "plugB__y"})
	return svc
}

// TestReactService_PinnedPluginsLifecycle 全生命周期：设置 → 挂载 → 切换（差集卸载）→ 清空。
func TestReactService_PinnedPluginsLifecycle(t *testing.T) {
	svc := newPinTestService(t)
	sess := svc.store.createSession("指定插件测试", "")

	// 无效 ID 整体拒绝（ErrInvalidSessionState → HTTP 400），原状态不变。
	if err := svc.SetSessionPinnedPlugins(sess.ID, []string{"plugA", "ghost"}); !errors.Is(err, ErrInvalidSessionState) {
		t.Fatalf("无效插件应拒绝, got %v", err)
	}
	if got := svc.SessionPinnedPlugins(sess.ID); len(got) != 0 {
		t.Fatalf("拒绝后不应改动, got %v", got)
	}

	// 合法设置：工具预挂进会话 scope，读取器同步可见。
	if err := svc.SetSessionPinnedPlugins(sess.ID, []string{"plugA"}); err != nil {
		t.Fatalf("SetSessionPinnedPlugins: %v", err)
	}
	if got := svc.SessionPinnedPlugins(sess.ID); len(got) != 1 || got[0] != "plugA" {
		t.Fatalf("读取器应回显 plugA, got %v", got)
	}
	if mounted := svc.toolRegistry.MountedTools(sess.ID); !mounted["plugA__do"] {
		t.Fatalf("plugA__do 应预挂, mounted=%v", mounted)
	}

	// 切换 plugA → plugB：旧工具卸载、新工具预挂。
	if err := svc.SetSessionPinnedPlugins(sess.ID, []string{"plugB"}); err != nil {
		t.Fatalf("切换 plugB: %v", err)
	}
	mounted := svc.toolRegistry.MountedTools(sess.ID)
	if mounted["plugA__do"] {
		t.Fatalf("plugA__do 应已卸载, mounted=%v", mounted)
	}
	if !mounted["plugB__x"] || !mounted["plugB__y"] {
		t.Fatalf("plugB 工具应预挂, mounted=%v", mounted)
	}

	// 清空（合法）：全部卸载。
	if err := svc.SetSessionPinnedPlugins(sess.ID, nil); err != nil {
		t.Fatalf("清空: %v", err)
	}
	if got := svc.SessionPinnedPlugins(sess.ID); len(got) != 0 {
		t.Fatalf("清空后应为空, got %v", got)
	}
	if mounted := svc.toolRegistry.MountedTools(sess.ID); len(mounted) != 0 {
		t.Fatalf("清空后挂载集应为空, mounted=%v", mounted)
	}

	// 不存在的会话报错（ErrSessionNotFound → HTTP 404）。
	if err := svc.SetSessionPinnedPlugins("session-none", []string{"plugA"}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("未知会话应 ErrSessionNotFound, got %v", err)
	}

	// 事件留痕：至少含一条"指定插件切换"。
	found := false
	for _, ev := range sess.Events {
		if strings.Contains(ev.Message, "指定插件切换") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("应留痕指定插件切换事件")
	}
}

// TestPinnedPluginsMetaMemoryRoundTrip 验证 MetaMemory 持久化往返。
func TestPinnedPluginsMetaMemoryRoundTrip(t *testing.T) {
	svc := newPinTestService(t)
	sess := svc.store.createSession("持久化测试", "")
	if err := svc.SetSessionPinnedPlugins(sess.ID, []string{"plugA", "plugB"}); err != nil {
		t.Fatalf("SetSessionPinnedPlugins: %v", err)
	}

	meta := sessionMetaMemory(sess)
	if len(meta) != 1 {
		t.Fatalf("sessionMetaMemory 应携带 pinned_plugins, got %+v", meta)
	}
	got := pinnedPluginsFromMetaMemory(meta)
	if len(got) != 2 || got[0] != "plugA" || got[1] != "plugB" {
		t.Fatalf("pinnedPluginsFromMetaMemory 应读回 [plugA plugB], got %v", got)
	}

	// JSONB 往返形态（[]any）也能读回。
	asJSON := []map[string]any{{"pinned_plugins": []any{"plugA"}}}
	if got := pinnedPluginsFromMetaMemory(asJSON); len(got) != 1 || got[0] != "plugA" {
		t.Fatalf("[]any 形态应读回, got %v", got)
	}

	// 未指定：空切片（旧写入形态不变），读回落 nil。
	empty := sessionMetaMemory(svc.store.createSession("无指定", ""))
	if len(empty) != 0 {
		t.Fatalf("无指定插件时应为空切片, got %+v", empty)
	}
	if got := pinnedPluginsFromMetaMemory(empty); got != nil {
		t.Fatalf("缺键应回落 nil, got %v", got)
	}
	// 脏数据（非字符串元素）跳过。
	dirty := []map[string]any{{"pinned_plugins": []any{"plugA", 42, ""}}}
	if got := pinnedPluginsFromMetaMemory(dirty); len(got) != 1 || got[0] != "plugA" {
		t.Fatalf("脏数据应跳过, got %v", got)
	}
}

// TestPinnedPluginBlock 验证提示块注入：选中时含插件名与工具，未选中零注入。
func TestPinnedPluginBlock(t *testing.T) {
	svc := newPinTestService(t)
	sess := svc.store.createSession("提示块测试", "")

	if block := svc.pinnedPluginBlock(sess); block != "" {
		t.Fatalf("未指定时应零注入, got %q", block)
	}
	if err := svc.SetSessionPinnedPlugins(sess.ID, []string{"plugA"}); err != nil {
		t.Fatalf("SetSessionPinnedPlugins: %v", err)
	}
	block := svc.pinnedPluginBlock(sess)
	if !strings.Contains(block, "【用户指定插件】") || !strings.Contains(block, "插件plugA") || !strings.Contains(block, "plugA__do") {
		t.Fatalf("提示块应含插件名与工具, got %q", block)
	}

	// resolver 未接线：零注入（不 panic）。
	svc2 := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())
	sess2 := svc2.store.createSession("无 resolver", "")
	sess2.setPinnedPlugins([]string{"plugA"})
	if block := svc2.pinnedPluginBlock(sess2); block != "" {
		t.Fatalf("resolver 未接线应零注入, got %q", block)
	}
}

// TestReactService_CreateSessionPinnedPlugins 验证 CreateRequest.PinnedPlugins：
// 有效 ID 入会话，无效 ID 丢弃不阻断（创建路径从宽）。
func TestReactService_CreateSessionPinnedPlugins(t *testing.T) {
	svc := newPinTestService(t)
	ctx := context.Background()

	created, err := svc.CreateSession(ctx, CreateRequest{Goal: "创建带指定插件", PinnedPlugins: []string{"plugA", "ghost"}})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if got := svc.SessionPinnedPlugins(created.ID); len(got) != 1 || got[0] != "plugA" {
		t.Fatalf("有效插件应入会话（无效丢弃）, got %v", got)
	}

	// 缺省：不指定。
	plain, err := svc.CreateSession(ctx, CreateRequest{Goal: "不带指定插件"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if got := svc.SessionPinnedPlugins(plain.ID); len(got) != 0 {
		t.Fatalf("缺省应为空, got %v", got)
	}
}
