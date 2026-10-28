package agent

// memory_lifecycle_test.go 覆盖长期记忆撤回与污染修复的核心语义
//（doc/计划_长期记忆撤回与污染修复.md §11.1，P0/P1 门槛）。

import (
	"context"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/store"
)

// newLifecycleFixture 构造内存仓储 + 生命周期服务 + 若干条目。
func newLifecycleFixture(t *testing.T, mode string) (*MemoryLifecycle, *store.MemoryLongTermStore, MemoryScopeContext) {
	t.Helper()
	repo := store.NewMemoryLongTermStore()
	lc := NewMemoryLifecycle(repo, mode)
	sc := MemoryScopeContext{ProjectKey: store.NormalizeProjectKey(`D:\proj\moying`), SessionID: "sess-1"}
	return lc, repo, sc
}

// addActive 插入一条 active 条目。
func addActive(t *testing.T, repo *store.MemoryLongTermStore, item store.MemoryItem) store.MemoryItem {
	t.Helper()
	if item.Status == "" {
		item.Status = store.MemoryStatusActive
	}
	if item.Verification == "" {
		item.Verification = store.MemoryVerificationUnverified
	}
	if item.Origin == "" {
		item.Origin = store.MemoryOriginEvolver
	}
	created, err := repo.InsertItem(context.Background(), item)
	if err != nil {
		t.Fatalf("insert item: %v", err)
	}
	return created
}

// TestLifecycleRetractByTopicKey 「以后不要单图 warp」→ 同作用域同 key 条目当轮撤回，
// 且有效视图里不再出现（注入侧只看 active）。
func TestLifecycleRetractByTopicKey(t *testing.T) {
	lc, repo, sc := newLifecycleFixture(t, RetractionModeEnforce)
	warp := addActive(t, repo, store.MemoryItem{
		Owner: "", Scope: store.MemoryScopeUser, MemoryKey: "animation.method",
		Content: "四足奔跑默认使用单底图 warp 形变",
	})
	keep := addActive(t, repo, store.MemoryItem{
		Owner: "", Scope: store.MemoryScopeUser, MemoryKey: "response.language",
		Content: "以后都用中文回复",
	})

	report := lc.HandleUserMessage(context.Background(), sc,
		"以后不要单图 warp 做奔跑了，这条经验是错的", "msg-1")
	if !report.Handled {
		t.Fatalf("应命中撤回，report=%+v", report)
	}
	got, _ := repo.GetItem(context.Background(), "", warp.ID)
	if got.Status != store.MemoryStatusRetracted {
		t.Fatalf("warp 条目应 retracted，got %s", got.Status)
	}
	keepGot, _ := repo.GetItem(context.Background(), "", keep.ID)
	if keepGot.Status != store.MemoryStatusActive {
		t.Fatalf("无关条目（回复语言）不得被撤回，got %s", keepGot.Status)
	}
	block := lc.RenderScope(context.Background(), "", "", store.MemoryScopeUser, 2000)
	if strings.Contains(block, "warp") {
		t.Fatalf("已完成撤回的条目不得出现在有效视图:\n%s", block)
	}
	if !strings.Contains(block, "中文") {
		t.Fatalf("无关的有效偏好应保留:\n%s", block)
	}
	// events 必须留痕（可追溯）。
	events, _ := repo.ListEvents(context.Background(), "", warp.ID, 10)
	if len(events) == 0 || events[len(events)-1].EventType != store.MemoryEventRetracted {
		t.Fatalf("应追加 retracted 事件，got %+v", events)
	}
}

// TestLifecycleReplaceSameKeyOnly 「改成 Y」只替换同 key，不动其他 key。
func TestLifecycleReplaceSameKeyOnly(t *testing.T) {
	lc, repo, sc := newLifecycleFixture(t, RetractionModeEnforce)
	old := addActive(t, repo, store.MemoryItem{
		Owner: "", Scope: store.MemoryScopeProject, ProjectKey: sc.ProjectKey,
		MemoryKey: "animation.method", Content: "四足奔跑使用视频抽帧",
	})
	other := addActive(t, repo, store.MemoryItem{
		Owner: "", Scope: store.MemoryScopeProject, ProjectKey: sc.ProjectKey,
		MemoryKey: "asset.identity", Content: "角色合同以原始参考图为准",
	})

	report := lc.HandleUserMessage(context.Background(), sc,
		"这个项目以后不走视频抽帧，改成关键姿态逐帧生成", "msg-2")
	if !report.Handled {
		t.Fatalf("应命中替换，report=%+v", report)
	}
	oldGot, _ := repo.GetItem(context.Background(), "", old.ID)
	if oldGot.Status != store.MemoryStatusSuperseded {
		t.Fatalf("旧动画方式条目应 superseded，got %s", oldGot.Status)
	}
	otherGot, _ := repo.GetItem(context.Background(), "", other.ID)
	if otherGot.Status != store.MemoryStatusActive {
		t.Fatalf("其他 key 条目不得被替换，got %s", otherGot.Status)
	}
	// 新条目 active 且同 key。
	list, _ := repo.ListItems(context.Background(), store.MemoryQuery{
		Owner: "", Scope: store.MemoryScopeProject, ProjectKey: sc.ProjectKey,
		Statuses: []string{store.MemoryStatusActive}, MemoryKeys: []string{"animation.method"},
	})
	if len(list) != 1 || !strings.Contains(list[0].Content, "关键姿态逐帧生成") {
		t.Fatalf("应写入同 key 的新 active 条目，got %+v", list)
	}
}

// TestLifecycleProjectScopeIsolation 「这个项目不要 X」不影响全局与其他项目。
func TestLifecycleProjectScopeIsolation(t *testing.T) {
	lc, repo, sc := newLifecycleFixture(t, RetractionModeEnforce)
	global := addActive(t, repo, store.MemoryItem{
		Owner: "", Scope: store.MemoryScopeUser, MemoryKey: "animation.method",
		Content: "动画优先用逐帧生图",
	})
	projA := addActive(t, repo, store.MemoryItem{
		Owner: "", Scope: store.MemoryScopeProject, ProjectKey: sc.ProjectKey,
		MemoryKey: "animation.method", Content: "本项目使用视频抽帧",
	})
	otherKey := store.NormalizeProjectKey(`D:\proj\other`)
	projB := addActive(t, repo, store.MemoryItem{
		Owner: "", Scope: store.MemoryScopeProject, ProjectKey: otherKey,
		MemoryKey: "animation.method", Content: "另一个项目使用视频抽帧",
	})

	report := lc.HandleUserMessage(context.Background(), sc,
		"这个项目不要视频抽帧了，改用逐帧生成", "msg-3")
	if !report.Handled {
		t.Fatalf("应命中项目级替换，report=%+v", report)
	}
	if got, _ := repo.GetItem(context.Background(), "", projA.ID); got.Status == store.MemoryStatusActive {
		t.Fatalf("本项目旧条目应被替换，got %s", got.Status)
	}
	if got, _ := repo.GetItem(context.Background(), "", global.ID); got.Status != store.MemoryStatusActive {
		t.Fatalf("全局条目不得被项目级指令影响，got %s", got.Status)
	}
	if got, _ := repo.GetItem(context.Background(), "", projB.ID); got.Status != store.MemoryStatusActive {
		t.Fatalf("其他项目条目不得被影响，got %s", got.Status)
	}
}

// TestLifecycleOneShotDoesNotTouchLongTerm 「这次不要 X」只改会话，不动长期记忆。
func TestLifecycleOneShotDoesNotTouchLongTerm(t *testing.T) {
	lc, repo, sc := newLifecycleFixture(t, RetractionModeEnforce)
	item := addActive(t, repo, store.MemoryItem{
		Owner: "", Scope: store.MemoryScopeProject, ProjectKey: sc.ProjectKey,
		MemoryKey: "animation.method", Content: "本项目使用视频抽帧",
	})
	report := lc.HandleUserMessage(context.Background(), sc, "这次不要视频抽帧了", "msg-4")
	if report.Handled {
		t.Fatalf("一次性选择不得改动长期记忆，report=%+v", report)
	}
	if got, _ := repo.GetItem(context.Background(), "", item.ID); got.Status != store.MemoryStatusActive {
		t.Fatalf("长期条目应保持 active，got %s", got.Status)
	}
}

// TestLifecycleRetractionIndependentOfWriteMode shadow/off 下用户明确撤回仍然生效。
func TestLifecycleRetractionIndependentOfWriteMode(t *testing.T) {
	for _, writeMode := range []string{MemoryWriteModeOff, MemoryWriteModeShadow} {
		lc, repo, _ := newLifecycleFixture(t, RetractionModeEnforce)
		svc := &ReactService{store: newReactSessionStore(nil)}
		svc.SetMemoryWriteMode(writeMode)
		svc.SetMemoryLifecycle(lc)
		sess := &reactInternalSession{ID: "sess-1"}
		sess.setWorkDir(`D:\proj\moying`)
		item := addActive(t, repo, store.MemoryItem{
			Owner: "", Scope: store.MemoryScopeProject, ProjectKey: store.NormalizeProjectKey(sess.currentWorkDir()),
			MemoryKey: "animation.method", Content: "本项目使用视频抽帧",
		})
		report := svc.applyMemoryRetraction(context.Background(), sess, "这个项目不要视频抽帧了", "user_message")
		if !report.Handled {
			t.Fatalf("write_mode=%s 时撤回仍应生效，report=%+v", writeMode, report)
		}
		if got, _ := repo.GetItem(context.Background(), "", item.ID); got.Status == store.MemoryStatusActive {
			t.Fatalf("write_mode=%s 下撤回应先于自动沉淀闸门生效", writeMode)
		}
	}
}

// TestLifecycleAmbiguousTargetQuarantines 目标不唯一 → 先隔离（立即不注入），不误删。
func TestLifecycleAmbiguousTargetQuarantines(t *testing.T) {
	lc, repo, sc := newLifecycleFixture(t, RetractionModeEnforce)
	a := addActive(t, repo, store.MemoryItem{
		Owner: "", Scope: store.MemoryScopeProject, ProjectKey: sc.ProjectKey,
		MemoryKey: "animation.method", Content: "奔跑使用骨架拼接",
	})
	b := addActive(t, repo, store.MemoryItem{
		Owner: "", Scope: store.MemoryScopeProject, ProjectKey: sc.ProjectKey,
		MemoryKey: "asset.source", Content: "奔跑素材用 AI 生图",
	})
	report := lc.HandleUserMessage(context.Background(), sc,
		"这个项目以后不要用骨架拼接和 AI 生图了，都错了", "msg-5")
	if !report.Handled {
		t.Fatalf("多候选时应至少隔离，report=%+v", report)
	}
	for _, it := range []struct{ id string }{{a.ID}, {b.ID}} {
		got, _ := repo.GetItem(context.Background(), "", it.id)
		if got.Status == store.MemoryStatusActive {
			t.Fatalf("目标不唯一时条目应先隔离（不注入），%s 仍 active", it.id)
		}
		if got.Status != store.MemoryStatusQuarantined {
			t.Fatalf("%s 应为 quarantined，got %s", it.id, got.Status)
		}
	}
	if block := lc.EffectiveBlock(context.Background(), "", sc.ProjectKey, 2000); strings.Contains(block, "骨架拼接") || strings.Contains(block, "视频抽帧") {
		t.Fatalf("隔离条目不得进入注入视图:\n%s", block)
	}
}

// TestLifecycleRestoreKeepsHistory 恢复条目追加事件，不删除旧事件。
func TestLifecycleRestoreKeepsHistory(t *testing.T) {
	lc, repo, sc := newLifecycleFixture(t, RetractionModeEnforce)
	item := addActive(t, repo, store.MemoryItem{
		Owner: "", Scope: store.MemoryScopeUser, MemoryKey: "response.style", Content: "输出要简洁",
	})
	if _, err := lc.RetractByID(context.Background(), "", item.ID, "误撤回"); err != nil {
		t.Fatalf("retract: %v", err)
	}
	if _, err := lc.RestoreByID(context.Background(), "", item.ID, "确认恢复"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	events, _ := repo.ListEvents(context.Background(), "", item.ID, 20)
	types := make([]string, 0, len(events))
	for _, ev := range events {
		types = append(types, ev.EventType)
	}
	joined := strings.Join(types, ",")
	if !strings.Contains(joined, store.MemoryEventRetracted) || !strings.Contains(joined, store.MemoryEventRestored) {
		t.Fatalf("恢复应保留撤回事件并追加 restored，got %v", types)
	}
	if got, _ := repo.GetItem(context.Background(), "", item.ID); got.Status != store.MemoryStatusActive {
		t.Fatalf("恢复后应为 active，got %s", got.Status)
	}
	_ = sc
}

// TestLifecycleBadOutcomeRejectsOnlySameSessionUnverified 「效果太差」只标本会话来源的
// 未验证条目，不按关键词清空全部经验。
func TestLifecycleBadOutcomeRejectsOnlySameSessionUnverified(t *testing.T) {
	lc, repo, sc := newLifecycleFixture(t, RetractionModeEnforce)
	mine := addActive(t, repo, store.MemoryItem{
		Owner: "", Scope: store.MemoryScopeProject, ProjectKey: sc.ProjectKey,
		MemoryKey: "lesson.general", Content: "本会话用到的经验", SourceSession: sc.SessionID,
	})
	verified := addActive(t, repo, store.MemoryItem{
		Owner: "", Scope: store.MemoryScopeProject, ProjectKey: sc.ProjectKey,
		MemoryKey: "animation.method", Content: "已验证的动画方式", Verification: store.MemoryVerificationVerified,
	})
	other := addActive(t, repo, store.MemoryItem{
		Owner: "", Scope: store.MemoryScopeProject, ProjectKey: sc.ProjectKey,
		MemoryKey: "lesson.general2", Content: "别的会话沉淀的经验", SourceSession: "sess-other",
	})
	report := lc.HandleUserMessage(context.Background(), sc, "效果太差了，重做", "msg-6")
	if !report.Handled {
		t.Fatalf("应标记本会话未验证条目，report=%+v", report)
	}
	if got, _ := repo.GetItem(context.Background(), "", mine.ID); got.Verification != store.MemoryVerificationRejected {
		t.Fatalf("本会话未验证条目应 verification=rejected，got %s", got.Verification)
	}
	for _, id := range []string{verified.ID, other.ID} {
		got, _ := repo.GetItem(context.Background(), "", id)
		if got.Verification == store.MemoryVerificationRejected {
			t.Fatalf("%s 不应被标记 rejected", id)
		}
		if got.Status != store.MemoryStatusActive {
			t.Fatalf("%s 不应被撤回/隔离", id)
		}
	}
}

// TestLifecycleResurrectionBlocked 已撤回内容同文本/换措辞都不能重新 active。
func TestLifecycleResurrectionBlocked(t *testing.T) {
	lc, repo, sc := newLifecycleFixture(t, RetractionModeEnforce)
	oldItem := addActive(t, repo, store.MemoryItem{
		Owner: "", Scope: store.MemoryScopeProject, ProjectKey: sc.ProjectKey,
		MemoryKey: "animation.method", Content: "四足奔跑默认使用单底图 warp 形变",
	})
	if _, err := lc.RetractByID(context.Background(), "", oldItem.ID, "用户否决"); err != nil {
		t.Fatalf("retract: %v", err)
	}
	// 完全相同（换排版/标点）→ 拦截。
	blocked, reason := lc.CheckResurrection(context.Background(), store.MemoryItem{
		Owner: "", Scope: store.MemoryScopeProject, ProjectKey: sc.ProjectKey,
		MemoryKey: "animation.method", Content: "四足奔跑默认使用单底图 warp 形变。",
		ContentHash: store.MemoryContentHash("四足奔跑默认使用单底图 warp 形变。"),
	})
	if !blocked || !strings.Contains(reason, "完全相同") {
		t.Fatalf("同内容候选必须被拦截，blocked=%v reason=%s", blocked, reason)
	}
	// 近似换措辞 → resurrection_suspect。
	blocked2, reason2 := lc.CheckResurrection(context.Background(), store.MemoryItem{
		Owner: "", Scope: store.MemoryScopeProject, ProjectKey: sc.ProjectKey,
		MemoryKey: "animation.method", Content: "四足奔跑默认用单底图 warp 形变来做动作",
	})
	if !blocked2 || !strings.Contains(reason2, "近似") {
		t.Fatalf("近义候选应命中 resurrection_suspect，blocked=%v reason=%s", blocked2, reason2)
	}
	// 显式写入路径：被拦截 → 只落 candidate，不进注入视图。
	lc.RecordExplicitItem(context.Background(), sc, store.MemoryScopeProject,
		store.MemoryOriginExplicitTool, "四足奔跑默认使用单底图 warp 形变")
	block := lc.EffectiveBlock(context.Background(), "", sc.ProjectKey, 2000)
	if strings.Contains(block, "warp") {
		t.Fatalf("被拦截的候选不得进入有效视图:\n%s", block)
	}
}

// TestLifecycleExplicitNewDecisionAllowed 明确的新决定（不是旧结论换措辞）可以 active，
// 旧条目保持 superseded。
func TestLifecycleExplicitNewDecisionAllowed(t *testing.T) {
	lc, repo, sc := newLifecycleFixture(t, RetractionModeEnforce)
	oldItem := addActive(t, repo, store.MemoryItem{
		Owner: "", Scope: store.MemoryScopeProject, ProjectKey: sc.ProjectKey,
		MemoryKey: "animation.method", Content: "本项目不再使用视频抽帧",
	})
	if _, err := lc.RetractByID(context.Background(), "", oldItem.ID, "用户否决"); err != nil {
		t.Fatalf("retract: %v", err)
	}
	// 新决定：重新允许视频抽帧（语义与旧结论相反，不是复活）。
	report := lc.HandleUserMessage(context.Background(), sc,
		"这个项目以后重新允许视频抽帧，改用抽帧做宣传片", "msg-7")
	if !report.Handled {
		t.Fatalf("明确新决定应被处理，report=%+v", report)
	}
	list, _ := repo.ListItems(context.Background(), store.MemoryQuery{
		Owner: "", Scope: store.MemoryScopeProject, ProjectKey: sc.ProjectKey,
		Statuses: []string{store.MemoryStatusActive},
	})
	if len(list) == 0 {
		t.Fatal("明确的新决定应产生 active 条目")
	}
	if got, _ := repo.GetItem(context.Background(), "", oldItem.ID); got.Status == store.MemoryStatusActive {
		t.Fatal("旧条目不得重新 active")
	}
}

// TestLifecycleOwnerIsolation owner 隔离：不同 owner 查不到彼此条目。
func TestLifecycleOwnerIsolation(t *testing.T) {
	lc, repo, _ := newLifecycleFixture(t, RetractionModeEnforce)
	addActive(t, repo, store.MemoryItem{
		Owner: "owner-a", Scope: store.MemoryScopeUser, MemoryKey: "response.style", Content: "A 的偏好",
	})
	block := lc.RenderScope(context.Background(), "owner-b", "", store.MemoryScopeUser, 2000)
	if strings.Contains(block, "A 的偏好") {
		t.Fatalf("owner-b 不得看到 owner-a 的条目:\n%s", block)
	}
	if b := lc.RenderScope(context.Background(), "owner-a", "", store.MemoryScopeUser, 2000); !strings.Contains(b, "A 的偏好") {
		t.Fatalf("owner-a 应能看到自己的条目:\n%s", b)
	}
}

// TestLifecycleRevisionConflict 并发会话用旧 revision 提交失败，不覆盖新决定。
func TestLifecycleRevisionConflict(t *testing.T) {
	_, repo, _ := newLifecycleFixture(t, RetractionModeEnforce)
	item := addActive(t, repo, store.MemoryItem{
		Owner: "", Scope: store.MemoryScopeUser, MemoryKey: "response.style", Content: "输出要简洁",
	})
	// 会话 A 与 B 都持有 revision=1。
	if _, err := repo.SetStatus(context.Background(), "", item.ID, store.MemoryStatusQuarantined, item.Revision); err != nil {
		t.Fatalf("A 提交失败: %v", err)
	}
	// B 用旧 revision 提交 → 冲突。
	if _, err := repo.SetStatus(context.Background(), "", item.ID, store.MemoryStatusRetracted, item.Revision); err != store.ErrMemoryRevisionConflict {
		t.Fatalf("B 用旧 revision 提交应冲突，got %v", err)
	}
}

// TestLifecycleMissingTargetDoesNotFakeSuccess 明确要撤回但找不到对应条目时，
// 报 reconcile_required，不假报"已撤回"。
func TestLifecycleMissingTargetDoesNotFakeSuccess(t *testing.T) {
	lc, _, sc := newLifecycleFixture(t, RetractionModeEnforce)
	report := lc.HandleUserMessage(context.Background(), sc, "以后不要用某个我从未登记的做法了", "msg-8")
	if report.Handled {
		t.Fatalf("无对应条目时不得报成功，report=%+v", report)
	}
	if !report.ReconcileRequired {
		t.Fatalf("应提示需要人工对账，report=%+v", report)
	}
	if !report.ReconcileRequired {
		t.Fatalf("应提示需要人工对账，report=%+v", report)
	}
}

// TestLifecycleRetractionModeOffDisables retraction_mode=off 是紧急回滚口。
func TestLifecycleRetractionModeOffDisables(t *testing.T) {
	lc, repo, sc := newLifecycleFixture(t, RetractionModeOff)
	item := addActive(t, repo, store.MemoryItem{
		Owner: "", Scope: store.MemoryScopeUser, MemoryKey: "animation.method", Content: "使用 warp 做奔跑",
	})
	if report := lc.HandleUserMessage(context.Background(), sc, "以后不要用 warp 了", "msg-9"); report.Handled {
		t.Fatalf("off 模式不应处理撤回，report=%+v", report)
	}
	if got, _ := repo.GetItem(context.Background(), "", item.ID); got.Status != store.MemoryStatusActive {
		t.Fatalf("off 模式下条目应保持 active，got %s", got.Status)
	}
}

// TestMemoryImportMarkdown Markdown 迁移：归档区只作 tombstone、自动行标 legacy_import、
// 人工行可 active，" | " 合并行拆开。
func TestMemoryImportMarkdown(t *testing.T) {
	lc, repo, _ := newLifecycleFixture(t, RetractionModeEnforce)
	md := `# 用户画像

## 偏好

- 偏好单步完成，查完即止
- 用中文回复 | 结论先行（2026-09-07 17:09）

## 反馈记录

- 任务拆解粒度：偏好单步完成（2026-09-07）
`
	n, reason := lc.ImportMarkdown(context.Background(), "", store.MemoryScopeUser, "", md)
	if n != 4 {
		t.Fatalf("应导入 4 条（1 人工 + 2 拆开的自动 + 1 归档），got %d reason=%s", n, reason)
	}
	items, _ := repo.ListItems(context.Background(), store.MemoryQuery{Owner: "", Scope: store.MemoryScopeUser})
	var active, retracted, legacy int
	for _, it := range items {
		switch it.Status {
		case store.MemoryStatusActive:
			active++
		case store.MemoryStatusRetracted:
			retracted++
		}
		if it.Origin == store.MemoryOriginLegacyImport {
			legacy++
		}
	}
	if active != 3 || retracted != 1 {
		t.Fatalf("active=%d retracted=%d，期望 3/1", active, retracted)
	}
	if legacy < 3 {
		t.Fatalf("带时间戳/归档行应标记 legacy_import，got %d", legacy)
	}
	// 幂等：再次导入跳过。
	if n2, _ := lc.ImportMarkdown(context.Background(), "", store.MemoryScopeUser, "", md); n2 != 0 {
		t.Fatalf("重复导入应为 0，got %d", n2)
	}
	// 归档行已 tombstone：同内容候选被拦截。
	blocked, _ := lc.CheckResurrection(context.Background(), store.MemoryItem{
		Owner: "", Scope: store.MemoryScopeUser, MemoryKey: "lesson.general",
		Content: "任务拆解粒度：偏好单步完成",
	})
	if !blocked {
		t.Fatal("归档行应作为 tombstone 阻止同内容复活")
	}
}
