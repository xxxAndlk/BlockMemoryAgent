package agent

// pinned_skills_test.go 验证会话级指定技能（技能选择器）服务层接线：
//   - SetSessionPinnedSkills：技能池严格校验（无效整体拒绝）、规范化名称、事件留痕、
//     新增 learned 技能提升使用权重（use_count+1 回调）；
//   - MetaMemory 持久化往返：sessionMetaMemory 携带 pinned_skills，
//     pinnedSkillsFromMetaMemory 读回（含 JSONB 往返后的 []any 形态）；
//   - CreateRequest.PinnedSkills 创建路径从宽（无效名称丢弃不阻断）；
//   - pinnedSkillBlock 提示块：全文直载（learned Content / yaml ToolRef 兜底）/零注入。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/skill"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// newSkillPinTestService 构造带技能池与提权回调的测试服务。
func newSkillPinTestService(t *testing.T, bumped *[]string) *ReactService {
	t.Helper()
	svc := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())
	svc.SetSkillCatalog(skill.NewPoolFromSkills([]*types.Skill{
		{SkillID: "pdf_extract", Name: "pdf_extract", Description: "提取 PDF 表格与文本",
			Content: "## 步骤\n1. 用 ReadFile 读取\n2. 解析表格", Source: "learned"},
		{SkillID: "read_file", Name: "读文件", Description: "读取本地文件",
			ToolRef: "ReadFile", Source: "yaml"},
	}))
	svc.SetSkillUsageBumper(func(names []string) {
		*bumped = append(*bumped, names...)
	})
	return svc
}

// TestReactService_PinnedSkillsLifecycle 全生命周期：设置 → 切换 → 清空 → 未知会话。
func TestReactService_PinnedSkillsLifecycle(t *testing.T) {
	var bumped []string
	svc := newSkillPinTestService(t, &bumped)
	sess := svc.store.createSession("指定技能测试", "")

	// 无效名称整体拒绝（ErrInvalidSessionState → HTTP 400），原状态不变。
	if err := svc.SetSessionPinnedSkills(sess.ID, []string{"pdf_extract", "ghost"}); !errors.Is(err, ErrInvalidSessionState) {
		t.Fatalf("无效技能应拒绝, got %v", err)
	}
	if got := svc.SessionPinnedSkills(sess.ID); len(got) != 0 {
		t.Fatalf("拒绝后不应改动, got %v", got)
	}
	if len(bumped) != 0 {
		t.Fatalf("拒绝时不应提权, got %v", bumped)
	}

	// 合法设置：learned 技能提权一次。
	if err := svc.SetSessionPinnedSkills(sess.ID, []string{"pdf_extract"}); err != nil {
		t.Fatalf("SetSessionPinnedSkills: %v", err)
	}
	if got := svc.SessionPinnedSkills(sess.ID); len(got) != 1 || got[0] != "pdf_extract" {
		t.Fatalf("读取器应回显 pdf_extract, got %v", got)
	}
	if len(bumped) != 1 || bumped[0] != "pdf_extract" {
		t.Fatalf("learned 技能应提权一次, got %v", bumped)
	}

	// 重复设置同一技能：不重复提权（只算新增项）。
	if err := svc.SetSessionPinnedSkills(sess.ID, []string{"pdf_extract"}); err != nil {
		t.Fatalf("重复设置: %v", err)
	}
	if len(bumped) != 1 {
		t.Fatalf("已勾选项不应重复提权, got %v", bumped)
	}

	// 切换到 yaml 技能（无 use_count）：只改集合，不提权。
	if err := svc.SetSessionPinnedSkills(sess.ID, []string{"read_file"}); err != nil {
		t.Fatalf("切换 read_file: %v", err)
	}
	if got := svc.SessionPinnedSkills(sess.ID); len(got) != 1 || got[0] != "读文件" {
		t.Fatalf("规范化名称应回显读文件, got %v", got)
	}
	if len(bumped) != 1 {
		t.Fatalf("yaml 技能不应提权, got %v", bumped)
	}

	// 清空（合法）。
	if err := svc.SetSessionPinnedSkills(sess.ID, nil); err != nil {
		t.Fatalf("清空: %v", err)
	}
	if got := svc.SessionPinnedSkills(sess.ID); len(got) != 0 {
		t.Fatalf("清空后应为空, got %v", got)
	}

	// 不存在的会话报错（ErrSessionNotFound → HTTP 404）。
	if err := svc.SetSessionPinnedSkills("session-none", []string{"pdf_extract"}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("未知会话应 ErrSessionNotFound, got %v", err)
	}

	// 事件留痕：至少含一条"指定技能切换"。
	found := false
	for _, ev := range sess.Events {
		if strings.Contains(ev.Message, "指定技能切换") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("应留痕指定技能切换事件")
	}
}

// TestPinnedSkillsMetaMemoryRoundTrip 验证 MetaMemory 持久化往返。
func TestPinnedSkillsMetaMemoryRoundTrip(t *testing.T) {
	var bumped []string
	svc := newSkillPinTestService(t, &bumped)
	sess := svc.store.createSession("持久化测试", "")
	if err := svc.SetSessionPinnedSkills(sess.ID, []string{"pdf_extract", "read_file"}); err != nil {
		t.Fatalf("SetSessionPinnedSkills: %v", err)
	}

	meta := sessionMetaMemory(sess)
	if len(meta) != 1 {
		t.Fatalf("sessionMetaMemory 应携带 pinned_skills, got %+v", meta)
	}
	got := pinnedSkillsFromMetaMemory(meta)
	if len(got) != 2 || got[0] != "pdf_extract" || got[1] != "读文件" {
		t.Fatalf("pinnedSkillsFromMetaMemory 应读回规范化名称, got %v", got)
	}

	// JSONB 往返形态（[]any）也能读回。
	asJSON := []map[string]any{{"pinned_skills": []any{"pdf_extract"}}}
	if got := pinnedSkillsFromMetaMemory(asJSON); len(got) != 1 || got[0] != "pdf_extract" {
		t.Fatalf("[]any 形态应读回, got %v", got)
	}

	// 未指定：空切片（旧写入形态不变），读回落 nil。
	empty := sessionMetaMemory(svc.store.createSession("无指定", ""))
	if len(empty) != 0 {
		t.Fatalf("无指定技能时应为空切片, got %+v", empty)
	}
	if got := pinnedSkillsFromMetaMemory(empty); got != nil {
		t.Fatalf("缺键应回落 nil, got %v", got)
	}
	// 脏数据（非字符串元素）跳过。
	dirty := []map[string]any{{"pinned_skills": []any{"pdf_extract", 42, ""}}}
	if got := pinnedSkillsFromMetaMemory(dirty); len(got) != 1 || got[0] != "pdf_extract" {
		t.Fatalf("脏数据应跳过, got %v", got)
	}
}

// TestPinnedSkillBlock 验证提示块注入：全文直载 / 工具别名兜底 / 未选中零注入。
func TestPinnedSkillBlock(t *testing.T) {
	var bumped []string
	svc := newSkillPinTestService(t, &bumped)
	sess := svc.store.createSession("提示块测试", "")

	if block := svc.pinnedSkillBlock(sess); block != "" {
		t.Fatalf("未指定时应零注入, got %q", block)
	}
	if err := svc.SetSessionPinnedSkills(sess.ID, []string{"pdf_extract"}); err != nil {
		t.Fatalf("SetSessionPinnedSkills: %v", err)
	}
	block := svc.pinnedSkillBlock(sess)
	if !strings.Contains(block, "【用户指定技能】") ||
		!strings.Contains(block, "pdf_extract") ||
		!strings.Contains(block, "## 技能全文：pdf_extract") ||
		!strings.Contains(block, "用 ReadFile 读取") {
		t.Fatalf("提示块应含技能名与全文, got %q", block)
	}

	// 工具别名类技能：无正文时回退 ToolRef 说明。
	if err := svc.SetSessionPinnedSkills(sess.ID, []string{"read_file"}); err != nil {
		t.Fatalf("切换 read_file: %v", err)
	}
	block = svc.pinnedSkillBlock(sess)
	if !strings.Contains(block, "工具 ReadFile") {
		t.Fatalf("工具别名技能应回退 ToolRef 说明, got %q", block)
	}

	// 技能池未接线：零注入（不 panic）。
	svc2 := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())
	sess2 := svc2.store.createSession("无技能池", "")
	sess2.setPinnedSkills([]string{"pdf_extract"})
	if block := svc2.pinnedSkillBlock(sess2); block != "" {
		t.Fatalf("技能池未接线应零注入, got %q", block)
	}
}

// TestReactService_CreateSessionPinnedSkills 验证 CreateRequest.PinnedSkills：
// 有效名称入会话并提权，无效名称丢弃不阻断（创建路径从宽）。
func TestReactService_CreateSessionPinnedSkills(t *testing.T) {
	var bumped []string
	svc := newSkillPinTestService(t, &bumped)
	ctx := context.Background()

	created, err := svc.CreateSession(ctx, CreateRequest{Goal: "创建带指定技能", PinnedSkills: []string{"pdf_extract", "ghost"}})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if got := svc.SessionPinnedSkills(created.ID); len(got) != 1 || got[0] != "pdf_extract" {
		t.Fatalf("有效技能应入会话（无效丢弃）, got %v", got)
	}
	if len(bumped) != 1 || bumped[0] != "pdf_extract" {
		t.Fatalf("创建时应提升 learned 技能权重, got %v", bumped)
	}

	// 缺省：不指定。
	plain, err := svc.CreateSession(ctx, CreateRequest{Goal: "不带指定技能"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if got := svc.SessionPinnedSkills(plain.ID); len(got) != 0 {
		t.Fatalf("缺省应为空, got %v", got)
	}
}
