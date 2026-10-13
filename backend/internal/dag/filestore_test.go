package dag

import (
	"context"       // 测试用上下文
	"database/sql"  // 模拟 PG 未命中错误
	"errors"        // errors.Is 包装判定
	"os"            // 文件存在性检查与外部改写
	"path/filepath" // 测试文件路径拼接
	"strings"       // YAML 文本断言
	"testing"       // Go 测试框架
	"time"          // 时间构造与比较

	"gopkg.in/yaml.v3" // 校验落盘 YAML 内容
)

// fakePGStore 模拟运行态 PG 存储（dag_jobs 表语义）：
// 内存 map 保存整行；GetDAG 未命中返回 sql.ErrNoRows，与 PostgresStore 一致。
type fakePGStore struct {
	rows map[string]*DAG // id -> PG 行（定义+运行态混合，与现状一致）
}

func newFakePGStore() *fakePGStore {
	return &fakePGStore{rows: make(map[string]*DAG)}
}

// SaveDAG upsert 整行（存深拷贝，避免与调用方共享内存）。
func (f *fakePGStore) SaveDAG(_ context.Context, d *DAG) error {
	f.rows[d.ID] = cloneDAG(d)
	return nil
}

// GetDAG 按 id 取行；未命中返回 sql.ErrNoRows（模拟 row.Scan 行为）。
func (f *fakePGStore) GetDAG(_ context.Context, id string) (*DAG, error) {
	if d, ok := f.rows[id]; ok {
		return cloneDAG(d), nil
	}
	return nil, sql.ErrNoRows
}

// ListDAGs 返回全部行（深拷贝）。
func (f *fakePGStore) ListDAGs(context.Context) ([]*DAG, error) {
	out := make([]*DAG, 0, len(f.rows))
	for _, d := range f.rows {
		out = append(out, cloneDAG(d))
	}
	return out, nil
}

// DeleteDAG 按 id 删行。
func (f *fakePGStore) DeleteDAG(_ context.Context, id string) error {
	delete(f.rows, id)
	return nil
}

// newTestFileStore 在临时目录上构造 FileStore + 假 PG。
func newTestFileStore(t *testing.T) (*FileStore, *fakePGStore, string) {
	t.Helper()
	dir := t.TempDir()
	pg := newFakePGStore()
	fs, err := NewFileStore(pg, dir)
	if err != nil {
		t.Fatalf("NewFileStore failed: %v", err)
	}
	return fs, pg, dir
}

// TestFileStoreSaveWritesDefinitionOnlyYAML 验证 SaveDAG 落盘文件只含定义字段，
// 运行态（status/session_id 等）只进 PG 行进 YAML 文件。
func TestFileStoreSaveWritesDefinitionOnlyYAML(t *testing.T) {
	fs, pg, dir := newTestFileStore(t)
	start := time.Now()
	now := time.Date(2026, 10, 13, 8, 0, 0, 0, time.UTC)
	d := &DAG{
		ID:        "dag-1",
		Name:      "每日报告",
		Cron:      "0 9 * * *",
		Enabled:   true,
		CreatedAt: now,
		Tasks: []*Task{
			{ID: "main", Goal: "写日报", Status: TaskStatusRunning, SessionID: "sess-1"},
			{ID: "review", Goal: "复核日报", DependsOn: []string{"main"}},
		},
	}
	if err := fs.SaveDAG(t.Context(), d); err != nil {
		t.Fatalf("SaveDAG failed: %v", err)
	}

	// 文件必须存在
	path := filepath.Join(dir, "dag-1.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("schedule file missing: %v", err)
	}
	text := string(data)
	// 定义字段必须在文件里
	for _, want := range []string{"每日报告", "0 9 * * *", "写日报", "复核日报", "depends_on"} {
		if !strings.Contains(text, want) {
			t.Errorf("schedule file missing %q:\n%s", want, text)
		}
	}
	// 运行态字段不得出现在文件里
	for _, bad := range []string{"status", "session_id", "sess-1", "started_at", "finished_at", "updated_at"} {
		if strings.Contains(text, bad) {
			t.Errorf("schedule file must not contain runtime field %q:\n%s", bad, text)
		}
	}
	// YAML 可解析且字段正确
	var def dagDef
	if err := yaml.Unmarshal(data, &def); err != nil {
		t.Fatalf("schedule file not valid yaml: %v", err)
	}
	if def.ID != "dag-1" || def.Name != "每日报告" || def.Cron != "0 9 * * *" || !def.Enabled {
		t.Errorf("def scalar fields mismatch: %+v", def)
	}
	if len(def.Tasks) != 2 || def.Tasks[1].DependsOn[0] != "main" {
		t.Errorf("def tasks mismatch: %+v", def.Tasks)
	}

	// 运行态必须落 PG 行
	row, err := pg.GetDAG(t.Context(), "dag-1")
	if err != nil {
		t.Fatalf("pg row missing: %v", err)
	}
	if row.Tasks[0].Status != TaskStatusRunning || row.Tasks[0].SessionID != "sess-1" {
		t.Errorf("pg runtime not saved: %+v", row.Tasks[0])
	}
	// 写文件成功后 updated_at 重置为当前时间（cron 基准重排）
	if row.UpdatedAt.Before(start) {
		t.Errorf("updated_at not refreshed: %v", row.UpdatedAt)
	}
}

// TestFileStoreGetOverlaysRuntime 验证 GetDAG 以文件定义为基、按 id overlay PG 运行态：
// PG 行里的定义字段（name/cron）再新也不生效，updated_at 取 PG 行。
func TestFileStoreGetOverlaysRuntime(t *testing.T) {
	fs, pg, _ := newTestFileStore(t)
	created := time.Date(2026, 10, 13, 8, 0, 0, 0, time.UTC)
	d := &DAG{
		ID:        "dag-ovl",
		Name:      "文件定义名",
		Cron:      "30m",
		Enabled:   true,
		CreatedAt: created,
		Tasks:     []*Task{{ID: "a", Goal: "goal a"}},
	}
	if err := fs.SaveDAG(t.Context(), d); err != nil {
		t.Fatalf("SaveDAG failed: %v", err)
	}
	// 模拟运行态推进：直接改 PG 行（定义字段故意写脏，验证文件优先）
	later := created.Add(2 * time.Hour)
	if err := pg.SaveDAG(t.Context(), &DAG{
		ID:        "dag-ovl",
		Name:      "pg-stale-name",
		Cron:      "0 0 * * *",
		Enabled:   false,
		CreatedAt: created,
		UpdatedAt: later,
		Tasks:     []*Task{{ID: "a", Goal: "goal a", Status: TaskStatusCompleted, SessionID: "sess-x"}},
	}); err != nil {
		t.Fatalf("pg SaveDAG failed: %v", err)
	}

	got, err := fs.GetDAG(t.Context(), "dag-ovl")
	if err != nil {
		t.Fatalf("GetDAG failed: %v", err)
	}
	// 定义以文件为准
	if got.Name != "文件定义名" || got.Cron != "30m" || !got.Enabled {
		t.Errorf("definition must come from file: %+v", got)
	}
	// 运行态 overlay 自 PG
	if got.Tasks[0].Status != TaskStatusCompleted || got.Tasks[0].SessionID != "sess-x" {
		t.Errorf("runtime not overlaid: %+v", got.Tasks[0])
	}
	// updated_at 取 PG 运行态行（cron 基准），不是文件 mtime
	if !got.UpdatedAt.Equal(later) {
		t.Errorf("UpdatedAt = %v, want %v (from pg row)", got.UpdatedAt, later)
	}
}

// TestFileStoreGetNotFound 验证文件缺失时 GetDAG 返回包装 sql.ErrNoRows 的错误
// （与 PostgresStore 未命中行为一致，HTTP 层据此 404）。
func TestFileStoreGetNotFound(t *testing.T) {
	fs, _, _ := newTestFileStore(t)
	_, err := fs.GetDAG(t.Context(), "no-such")
	if err == nil {
		t.Fatal("expected error for missing dag")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("error must wrap sql.ErrNoRows, got %v", err)
	}
}

// TestFileStoreListSkipsOrphanRuntime 验证 PG 里无对应文件的孤儿运行态行被忽略。
func TestFileStoreListSkipsOrphanRuntime(t *testing.T) {
	fs, pg, _ := newTestFileStore(t)
	for _, id := range []string{"dag-a", "dag-b"} {
		if err := fs.SaveDAG(t.Context(), &DAG{
			ID: id, Name: id, Enabled: true,
			CreatedAt: time.Now(),
			Tasks:     []*Task{{ID: "main", Goal: "g"}},
		}); err != nil {
			t.Fatalf("SaveDAG %s failed: %v", id, err)
		}
	}
	// 孤儿运行态行：PG 有、磁盘无文件
	if err := pg.SaveDAG(t.Context(), &DAG{ID: "ghost", Name: "ghost", Tasks: []*Task{{ID: "m", Goal: "g"}}}); err != nil {
		t.Fatalf("pg SaveDAG failed: %v", err)
	}
	list, err := fs.ListDAGs(t.Context())
	if err != nil {
		t.Fatalf("ListDAGs failed: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListDAGs len = %d, want 2 (orphan must be ignored)", len(list))
	}
	for _, d := range list {
		if d.ID == "ghost" {
			t.Error("orphan runtime row leaked into list")
		}
	}
}

// TestFileStoreDeleteRemovesBoth 验证 DeleteDAG 同时删除文件与 PG 行。
func TestFileStoreDeleteRemovesBoth(t *testing.T) {
	fs, pg, dir := newTestFileStore(t)
	d := &DAG{ID: "dag-del", Name: "d", Enabled: true, Tasks: []*Task{{ID: "m", Goal: "g"}}}
	if err := fs.SaveDAG(t.Context(), d); err != nil {
		t.Fatalf("SaveDAG failed: %v", err)
	}
	if err := fs.DeleteDAG(t.Context(), "dag-del"); err != nil {
		t.Fatalf("DeleteDAG failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dag-del.yaml")); !os.IsNotExist(err) {
		t.Errorf("schedule file not removed: %v", err)
	}
	if _, err := pg.GetDAG(t.Context(), "dag-del"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("pg row not removed: %v", err)
	}
	if _, err := fs.GetDAG(t.Context(), "dag-del"); err == nil {
		t.Error("GetDAG after delete must fail")
	}
	// 重复删除幂等
	if err := fs.DeleteDAG(t.Context(), "dag-del"); err != nil {
		t.Errorf("second DeleteDAG failed: %v", err)
	}
}

// TestFileStoreHotReloadOnExternalEdit 验证外部手改文件后 List 反映新值
// （mtime/size 变化触发增量重读，无需重启）。
func TestFileStoreHotReloadOnExternalEdit(t *testing.T) {
	fs, _, dir := newTestFileStore(t)
	d := &DAG{ID: "dag-hot", Name: "旧名", Cron: "30m", Enabled: true, Tasks: []*Task{{ID: "m", Goal: "g"}}}
	if err := fs.SaveDAG(t.Context(), d); err != nil {
		t.Fatalf("SaveDAG failed: %v", err)
	}
	// 先读一次建立缓存基线
	if _, err := fs.ListDAGs(t.Context()); err != nil {
		t.Fatalf("ListDAGs failed: %v", err)
	}
	// 模拟用户手改：直接写文件（内容长度变化 + 显式拨动 mtime，兼容低精度文件系统）
	path := filepath.Join(dir, "dag-hot.yaml")
	edited := "id: dag-hot\nname: 新名字变了\ncron: 0 6 * * *\nenabled: false\ncreated_at: 2026-10-13T08:00:00Z\ntasks:\n  - id: m\n    goal: 新目标\n"
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatalf("external edit failed: %v", err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("Chtimes failed: %v", err)
	}
	list, err := fs.ListDAGs(t.Context())
	if err != nil {
		t.Fatalf("ListDAGs after edit failed: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListDAGs len = %d, want 1", len(list))
	}
	got := list[0]
	if got.Name != "新名字变了" || got.Cron != "0 6 * * *" || got.Enabled {
		t.Errorf("hot reload did not pick up edits: %+v", got)
	}
	if got.Tasks[0].Goal != "新目标" {
		t.Errorf("task goal not reloaded: %+v", got.Tasks[0])
	}
}

// TestFileStoreRuntimeSaveKeepsFile 验证纯运行态回写（定义不变）不重写文件、
// 不重置 updated_at（reapRunning 路径的 cron 基准不变性）。
func TestFileStoreRuntimeSaveKeepsFile(t *testing.T) {
	fs, pg, dir := newTestFileStore(t)
	triggerAt := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	d := &DAG{
		ID: "dag-rt", Name: "rt", Enabled: true,
		CreatedAt: triggerAt.Add(-time.Hour),
		UpdatedAt: triggerAt,
		Tasks:     []*Task{{ID: "m", Goal: "g"}},
	}
	if err := fs.SaveDAG(t.Context(), d); err != nil {
		t.Fatalf("SaveDAG failed: %v", err)
	}
	path := filepath.Join(dir, "dag-rt.yaml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read schedule file failed: %v", err)
	}

	// 模拟 reapRunning 回写：定义不变，只推进运行态
	rt := cloneDAG(d)
	rt.Tasks[0].Status = TaskStatusCompleted
	rt.Tasks[0].SessionID = "sess-9"
	rt.UpdatedAt = triggerAt // 运行实例携带的是触发时刻的 updated_at
	if err := fs.SaveDAG(t.Context(), rt); err != nil {
		t.Fatalf("runtime SaveDAG failed: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read schedule file failed: %v", err)
	}
	if string(before) != string(after) {
		t.Error("runtime-only save must not rewrite definition file")
	}
	// updated_at 不得被重置（入参原样落到 PG）
	if !rt.UpdatedAt.Equal(triggerAt) {
		t.Errorf("runtime-only save reset UpdatedAt: %v", rt.UpdatedAt)
	}
	// 运行态仍进 PG
	row, err := pg.GetDAG(t.Context(), "dag-rt")
	if err != nil {
		t.Fatalf("pg row missing: %v", err)
	}
	if row.Tasks[0].Status != TaskStatusCompleted || !row.UpdatedAt.Equal(triggerAt) {
		t.Errorf("pg runtime mismatch: %+v updated_at=%v", row.Tasks[0], row.UpdatedAt)
	}
}

// TestFileStoreMigrateIdempotent 验证存量迁移：PG 有定义、磁盘无文件的行被导出；
// 再跑一次不覆盖已存在文件。
func TestFileStoreMigrateIdempotent(t *testing.T) {
	fs, pg, dir := newTestFileStore(t)
	created := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	// 造 PG 存量行
	for _, id := range []string{"dag-old-1", "dag-old-2"} {
		if err := pg.SaveDAG(t.Context(), &DAG{
			ID: id, Name: "存量-" + id, Cron: "1h", Enabled: true,
			CreatedAt: created, UpdatedAt: created,
			Tasks: []*Task{{ID: "main", Goal: "存量任务"}},
		}); err != nil {
			t.Fatalf("seed pg failed: %v", err)
		}
	}
	n, err := fs.Migrate(t.Context())
	if err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}
	if n != 2 {
		t.Fatalf("Migrate n = %d, want 2", n)
	}
	for _, id := range []string{"dag-old-1", "dag-old-2"} {
		if _, err := os.Stat(filepath.Join(dir, id+".yaml")); err != nil {
			t.Errorf("migrated file missing: %v", err)
		}
	}
	// 迁移后定义可读
	got, err := fs.GetDAG(t.Context(), "dag-old-1")
	if err != nil {
		t.Fatalf("GetDAG migrated failed: %v", err)
	}
	if got.Name != "存量-dag-old-1" || got.Cron != "1h" {
		t.Errorf("migrated def mismatch: %+v", got)
	}
	// 手改其中一个文件，再迁移不得覆盖
	path := filepath.Join(dir, "dag-old-1.yaml")
	edited := strings.Repeat("# 手改\n", 3) + "id: dag-old-1\nname: 手改的名\ncron: 2h\nenabled: true\ntasks: []\n"
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatalf("external edit failed: %v", err)
	}
	n, err = fs.Migrate(t.Context())
	if err != nil {
		t.Fatalf("second Migrate failed: %v", err)
	}
	if n != 0 {
		t.Errorf("second Migrate n = %d, want 0 (idempotent)", n)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "手改的名") {
		t.Error("second Migrate overwrote existing file")
	}
}

// TestFileStoreCorruptFileSkipped 验证损坏文件跳过并记告警，不拖垮整个 List。
func TestFileStoreCorruptFileSkipped(t *testing.T) {
	fs, _, dir := newTestFileStore(t)
	d := &DAG{ID: "dag-good", Name: "good", Enabled: true, Tasks: []*Task{{ID: "m", Goal: "g"}}}
	if err := fs.SaveDAG(t.Context(), d); err != nil {
		t.Fatalf("SaveDAG failed: %v", err)
	}
	// 写入损坏的 YAML 文件
	if err := os.WriteFile(filepath.Join(dir, "dag-bad.yaml"), []byte("{{{not yaml"), 0o600); err != nil {
		t.Fatalf("write corrupt file failed: %v", err)
	}
	list, err := fs.ListDAGs(t.Context())
	if err != nil {
		t.Fatalf("ListDAGs must tolerate corrupt file: %v", err)
	}
	if len(list) != 1 || list[0].ID != "dag-good" {
		t.Errorf("corrupt file must be skipped, got %+v", list)
	}
	// 单点 Get 损坏文件视为不存在
	if _, err := fs.GetDAG(t.Context(), "dag-bad"); err == nil {
		t.Error("GetDAG on corrupt file must fail as not found")
	}
}

// TestFileStoreRejectsUnsafeID 验证路径穿越 id 被拒绝，不会写出 schedules/ 目录。
func TestFileStoreRejectsUnsafeID(t *testing.T) {
	fs, _, dir := newTestFileStore(t)
	d := &DAG{ID: "../evil", Name: "x", Tasks: []*Task{{ID: "m", Goal: "g"}}}
	if err := fs.SaveDAG(t.Context(), d); err == nil {
		t.Fatal("SaveDAG with traversal id must fail")
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "evil.yaml")); !os.IsNotExist(err) {
		t.Error("file escaped schedules dir")
	}
	if err := fs.DeleteDAG(t.Context(), "../evil"); err == nil {
		t.Error("DeleteDAG with traversal id must fail")
	}
	if _, err := fs.GetDAG(t.Context(), "../evil"); err == nil {
		t.Error("GetDAG with traversal id must fail")
	}
}

// TestFileStoreSchedulerRoundTrip 调度器兼容检查：Scheduler 跑在 FileStore 上，
// Trigger → 会话完成 → reap 回写，定义文件不被运行态污染，PG 行拿到终态。
func TestFileStoreSchedulerRoundTrip(t *testing.T) {
	fs, pg, dir := newTestFileStore(t)
	d := &DAG{
		ID: "dag-sched", Name: "sched", Enabled: false, // 禁用 cron，只手动触发
		CreatedAt: time.Now(),
		Tasks:     []*Task{{ID: "a", Goal: "goal sched"}},
	}
	if err := fs.SaveDAG(t.Context(), d); err != nil {
		t.Fatalf("SaveDAG failed: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "dag-sched.yaml"))
	if err != nil {
		t.Fatalf("read schedule file failed: %v", err)
	}

	launcher := newStubLauncher()
	s := NewScheduler(fs, launcher, time.Second)
	ctx := t.Context()
	if err := s.Trigger(ctx, "dag-sched"); err != nil {
		t.Fatalf("Trigger failed: %v", err)
	}
	if len(launcher.goals) != 1 {
		t.Fatalf("expected 1 launch, got %v", launcher.goals)
	}
	// 会话完成 → reap 收尾回写 → DAG 从 running 清除
	launcher.setStatus(launcher.sessionOf("goal sched"), "completed")
	s.tick(ctx)
	if _, ok := s.Snapshot()["dag-sched"]; ok {
		t.Error("finished dag should be removed from running map")
	}
	// 定义文件不被运行态回写污染
	after, err := os.ReadFile(filepath.Join(dir, "dag-sched.yaml"))
	if err != nil {
		t.Fatalf("read schedule file failed: %v", err)
	}
	if string(before) != string(after) {
		t.Error("scheduler runtime save must not rewrite definition file")
	}
	// PG 运行态行拿到终态
	row, err := pg.GetDAG(ctx, "dag-sched")
	if err != nil {
		t.Fatalf("pg row missing: %v", err)
	}
	if row.Tasks[0].Status != TaskStatusCompleted {
		t.Errorf("pg runtime status = %q, want completed", row.Tasks[0].Status)
	}
}
