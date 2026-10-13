// filestore.go 定时任务定义文件化：dag.Store 的文件+PG 双写实现。
//
// 拆分原则：
//   - 定义字段（id/name/cron/enabled/tasks[].id/goal/depends_on + created_at）：
//     唯一活源是 schedules/<id>.yaml（与 config 同目录的 schedules/ 下，每任务一个
//     文件），可手工编辑，读路径按 mtime/size 增量重读实现热生效。
//   - 运行态字段（tasks[].status/session_id/started_at/finished_at + updated_at）：
//     仍在 Postgres dag_jobs 表（PG 行演变为运行态载体，表结构不变）。
//
// 读写语义：
//   - SaveDAG：定义字段与磁盘文件比对，有变化（或文件缺失/损坏）才原子写文件
//     （临时文件+rename），写成功后把 d.UpdatedAt 重置为 now（保持"保存即重排
//     cron 基准"的现状语义）；运行态字段随整行 upsert 进 PG。纯运行态回写
//     （调度器 reap/dispatch）定义不变，不动文件、不动 updated_at。
//   - GetDAG/ListDAGs：以文件定义为基，按 id overlay PG 运行态行；PG 里无对应
//     文件的孤儿运行态行直接忽略（定义已不在磁盘，留待人工清理）。
//   - 已知边界：DAG 运行窗口内（Trigger 到末次 reap 之间）手工改文件，可能被
//     运行实例的回写覆盖；窗口结束后文件重新成为唯一活源。
package dag

import (
	"context"       // 上下文，用于取消与超时传递
	"database/sql"  // 识别 PG 侧"行不存在"
	"errors"        // errors.Is 包装判定
	"fmt"           // 格式化错误信息
	"log"           // 未注入 logger 时的回退输出
	"os"            // 文件读写与原子 rename
	"path/filepath" // 路径拼接
	"sort"          // 列表稳定排序
	"strings"       // 文件后缀与 id 校验
	"sync"          // 缓存互斥锁
	"time"          // mtime 基线与时间截断

	"gopkg.in/yaml.v3" // 任务定义文件序列化（项目既有依赖）

	"github.com/blockmemory/agent/backend/internal/logger" // 结构化日志器
)

// scheduleFileHeader 每个 schedules/*.yaml 文件头部的说明注释，
// 方便人工编辑时理解文件语义（YAML 注释不影响解析）。
const scheduleFileHeader = "# BMA 定时任务定义：本文件是任务定义的唯一活源，可手工编辑，保存后自动热生效。\n" +
	"# 运行态（任务状态/会话/最近触发时间）保存在 Postgres dag_jobs 表，不写入本文件。\n"

// taskDef 是任务节点在 YAML 定义文件中的形态（仅定义字段，无运行态）。
type taskDef struct {
	ID        string   `yaml:"id"`                   // 任务唯一标识
	Goal      string   `yaml:"goal"`                 // 任务目标描述
	DependsOn []string `yaml:"depends_on,omitempty"` // 依赖的任务 ID 列表
}

// dagDef 是 DAG 在 YAML 定义文件中的形态（仅定义字段）。
type dagDef struct {
	ID        string    `yaml:"id"`         // DAG 唯一标识（与文件名一致，加载时以文件名为准）
	Name      string    `yaml:"name"`       // DAG 名称
	Cron      string    `yaml:"cron"`       // cron 表达式（标准 5 段或 Ns/Nm/Nh）
	Enabled   bool      `yaml:"enabled"`    // 是否启用
	CreatedAt time.Time `yaml:"created_at"` // 创建时间
	Tasks     []taskDef `yaml:"tasks"`      // 任务节点定义列表
}

// fileEntry 是单个定义文件的解析缓存：以 (missing, modTime, size) 为基线，
// 基线不变时直接复用 def，避免每次读都重新解析 YAML。
type fileEntry struct {
	scanned bool      // 是否已建立基线
	missing bool      // 基线对应的文件缺失状态
	modTime time.Time // 基线对应的文件 mtime
	size    int64     // 基线对应的文件大小
	def     *dagDef   // 解析出的定义；nil 表示文件缺失或损坏
}

// FileStore 实现 Store 接口：定义落 YAML 文件，运行态包裹 PG store。
//
// 并发说明：全部公开方法经 mu 串行化文件缓存操作；PG 调用在锁外执行。
type FileStore struct {
	pg      Store                 // 运行态载体（Postgres dag_jobs）
	dir     string                // schedules/ 目录绝对路径
	log     *logger.Logger        // 结构化日志器；nil 时回退标准库 log
	mu      sync.Mutex            // 保护 entries 缓存
	entries map[string]*fileEntry // id -> 文件解析缓存
}

// NewFileStore 创建 FileStore；目录不存在时自动创建。
//
// 参数：
//   - pg：运行态 PG 存储（dag.Store 实现，通常为 *store.PostgresStore），不可为 nil。
//   - dir：定义文件目录（BMA 安装目录下 schedules/）。
//
// 返回：初始化后的 *FileStore；目录创建失败或 pg 为 nil 时返回 error。
func NewFileStore(pg Store, dir string) (*FileStore, error) {
	if pg == nil {
		return nil, fmt.Errorf("dag file store: pg store is nil")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create schedules dir %s: %w", dir, err)
	}
	return &FileStore{pg: pg, dir: dir, entries: make(map[string]*fileEntry)}, nil
}

// SetLogger 注入结构化日志器；未注入时损坏文件等告警回退标准库 log。
func (f *FileStore) SetLogger(l *logger.Logger) { f.log = l }

// logWarnf 记录告警日志；未注入 logger 时回退标准库 log，保持与 Scheduler 一致的风格。
func (f *FileStore) logWarnf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if f.log != nil {
		f.log.Warn(context.Background(), msg)
		return
	}
	log.Printf("%s", msg)
}

// pathOf 返回指定 DAG id 的定义文件路径。
func (f *FileStore) pathOf(id string) string {
	return filepath.Join(f.dir, id+".yaml")
}

// validateScheduleID 校验 id 可安全映射为文件名，防止路径穿越写出 schedules/ 目录。
func validateScheduleID(id string) error {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("invalid dag id for schedule file: %q", id)
	}
	return nil
}

// refreshLocked 按 (missing, mtime, size) 基线增量重读定义文件（调用方持锁）。
//
// 文件缺失/损坏时 def 置 nil：缺失视为"未定义"，损坏记告警日志并跳过，
// 不拖垮整个 List。文件内 id 与文件名不一致时以文件名为准并告警。
func (f *FileStore) refreshLocked(id string) *fileEntry {
	e := f.entries[id]
	if e == nil {
		e = &fileEntry{}
		f.entries[id] = e
	}
	path := f.pathOf(id)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 基线已是"缺失"则直接复用
			if e.scanned && e.missing {
				return e
			}
			e.scanned, e.missing = true, true
			e.modTime, e.size, e.def = time.Time{}, 0, nil
			return e
		}
		// stat 失败：保留旧缓存，记告警
		f.logWarnf("[dag] stat schedule file failed: %s: %v", path, err)
		return e
	}
	// 基线未变：直接复用缓存
	if e.scanned && !e.missing && info.ModTime().Equal(e.modTime) && info.Size() == e.size {
		return e
	}
	data, err := os.ReadFile(path)
	if err != nil {
		f.logWarnf("[dag] read schedule file failed: %s: %v", path, err)
		return e
	}
	var def dagDef
	if err := yaml.Unmarshal(data, &def); err != nil {
		// 手改坏的文件不吞错也不拖垮列表：记告警、按缺失处理
		f.logWarnf("[dag] schedule file corrupted, skipped: %s: %v", path, err)
		e.scanned, e.missing = true, false
		e.modTime, e.size = info.ModTime(), info.Size()
		e.def = nil
		return e
	}
	if def.ID != "" && def.ID != id {
		f.logWarnf("[dag] schedule file id mismatch: file=%s id=%s, use filename", path, def.ID)
	}
	def.ID = id
	e.scanned, e.missing = true, false
	e.modTime, e.size = info.ModTime(), info.Size()
	e.def = &def
	return e
}

// writeDefLocked 原子写定义文件（临时文件 + rename，0600；调用方持锁），
// 成功后刷新缓存基线与 def。Windows 的 os.Rename 不覆盖已存在文件，先移除旧文件。
func (f *FileStore) writeDefLocked(id string, def *dagDef, e *fileEntry) error {
	body, err := yaml.Marshal(def)
	if err != nil {
		return fmt.Errorf("marshal schedule def %s: %w", id, err)
	}
	data := append([]byte(scheduleFileHeader), body...)
	tmp, err := os.CreateTemp(f.dir, ".schedule-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp schedule file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write temp schedule file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp schedule file: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("chmod temp schedule file: %w", err)
	}
	path := f.pathOf(id)
	if _, err := os.Stat(path); err == nil {
		if err := os.Remove(path); err != nil {
			os.Remove(tmpName)
			return fmt.Errorf("remove old schedule file: %w", err)
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename schedule file: %w", err)
	}
	// 刷新缓存基线，避免下一次读重复解析
	if info, err := os.Stat(path); err == nil {
		e.scanned, e.missing = true, false
		e.modTime, e.size = info.ModTime(), info.Size()
		e.def = def
	}
	return nil
}

// defOfDAG 从完整 DAG 提取定义字段。CreatedAt 截断到秒，保证 YAML 往返后
// 相等性判定稳定（避免每次 Save 都因纳秒精度差被误判为"定义变化"）。
func defOfDAG(d *DAG) *dagDef {
	def := &dagDef{
		ID:        d.ID,
		Name:      d.Name,
		Cron:      d.Cron,
		Enabled:   d.Enabled,
		CreatedAt: d.CreatedAt.Truncate(time.Second),
		Tasks:     make([]taskDef, 0, len(d.Tasks)),
	}
	for _, t := range d.Tasks {
		def.Tasks = append(def.Tasks, taskDef{ID: t.ID, Goal: t.Goal, DependsOn: t.DependsOn})
	}
	return def
}

// equalDef 比较两个定义是否语义相等；depends_on 的 nil 与空切片视为相等。
func equalDef(a, b *dagDef) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.ID != b.ID || a.Name != b.Name || a.Cron != b.Cron || a.Enabled != b.Enabled {
		return false
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return false
	}
	if len(a.Tasks) != len(b.Tasks) {
		return false
	}
	for i := range a.Tasks {
		ta, tb := a.Tasks[i], b.Tasks[i]
		if ta.ID != tb.ID || ta.Goal != tb.Goal {
			return false
		}
		if len(ta.DependsOn) != len(tb.DependsOn) {
			return false
		}
		for j := range ta.DependsOn {
			if ta.DependsOn[j] != tb.DependsOn[j] {
				return false
			}
		}
	}
	return true
}

// dagFromDef 由定义构造完整 DAG；任务状态缺省为 pending，切片深拷贝避免与缓存共享内存。
func dagFromDef(def *dagDef) *DAG {
	d := &DAG{
		ID:        def.ID,
		Name:      def.Name,
		Cron:      def.Cron,
		Enabled:   def.Enabled,
		CreatedAt: def.CreatedAt,
		// 无 PG 运行态行时以 created_at 兜底 cron 基准
		UpdatedAt: def.CreatedAt,
		Tasks:     make([]*Task, 0, len(def.Tasks)),
	}
	for _, td := range def.Tasks {
		t := &Task{ID: td.ID, Goal: td.Goal, Status: TaskStatusPending}
		if td.DependsOn != nil {
			t.DependsOn = make([]string, len(td.DependsOn))
			copy(t.DependsOn, td.DependsOn)
		}
		d.Tasks = append(d.Tasks, t)
	}
	return d
}

// overlayRuntime 把 PG 运行态行叠加到文件定义上：updated_at 取 PG 行
// （cron 基准的来源，绝不取文件 mtime）；任务运行态按 task id 匹配填充；
// PG 行里存在而文件里已删除的任务直接忽略。
func overlayRuntime(d *DAG, row *DAG) {
	d.UpdatedAt = row.UpdatedAt
	if d.CreatedAt.IsZero() {
		d.CreatedAt = row.CreatedAt
	}
	byID := make(map[string]*Task, len(row.Tasks))
	for _, t := range row.Tasks {
		byID[t.ID] = t
	}
	for _, t := range d.Tasks {
		if rt, ok := byID[t.ID]; ok {
			t.Status = rt.Status
			t.SessionID = rt.SessionID
			t.StartedAt = rt.StartedAt
			t.FinishedAt = rt.FinishedAt
		}
	}
}

// SaveDAG 实现 Store：定义字段写 YAML 文件，运行态随整行 upsert 进 PG。
//
// 定义与磁盘文件一致时跳过文件写（纯运行态回写不动文件、不重置 updated_at，
// 保证 reapRunning/dispatchReady 的回写不改变 cron 基准）；定义变化或文件
// 缺失/损坏时原子写文件，并把 d.UpdatedAt 重置为 now（保持"保存即重排 cron
// 基准"的现状语义）。
func (f *FileStore) SaveDAG(ctx context.Context, d *DAG) error {
	if d == nil {
		return fmt.Errorf("nil dag")
	}
	if err := validateScheduleID(d.ID); err != nil {
		return err
	}
	incoming := defOfDAG(d)
	f.mu.Lock()
	e := f.refreshLocked(d.ID)
	if !equalDef(incoming, e.def) {
		if err := f.writeDefLocked(d.ID, incoming, e); err != nil {
			f.mu.Unlock()
			return err
		}
		// 写文件成功后重置 cron 基准
		d.UpdatedAt = time.Now()
	}
	f.mu.Unlock()
	return f.pg.SaveDAG(ctx, d)
}

// GetDAG 实现 Store：以文件定义为基，overlay PG 运行态行。
//
// 文件缺失/损坏视为任务不存在，返回包装 sql.ErrNoRows 的错误（与
// PostgresStore 的未命中行为一致，HTTP 层据此返回 404）；PG 行缺失（新
// 建文件尚未有运行态）不视为错误。
func (f *FileStore) GetDAG(ctx context.Context, id string) (*DAG, error) {
	if err := validateScheduleID(id); err != nil {
		return nil, err
	}
	f.mu.Lock()
	e := f.refreshLocked(id)
	def := e.def
	f.mu.Unlock()
	if def == nil {
		return nil, fmt.Errorf("dag %s not found: %w", id, sql.ErrNoRows)
	}
	d := dagFromDef(def)
	row, err := f.pg.GetDAG(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// 尚无运行态行：返回纯定义
			return d, nil
		}
		return nil, fmt.Errorf("load dag runtime %s: %w", id, err)
	}
	if row != nil {
		overlayRuntime(d, row)
	}
	return d, nil
}

// ListDAGs 实现 Store：目录扫描 + 增量重读定义文件，overlay PG 运行态。
//
// PG 行里无对应文件的孤儿运行态行忽略（定义已不在磁盘）；损坏文件跳过并
// 记告警，不拖垮整个列表。返回按 updated_at 倒序，与 PG ListDAGs 行为一致。
func (f *FileStore) ListDAGs(ctx context.Context) ([]*DAG, error) {
	dirEntries, err := os.ReadDir(f.dir)
	if err != nil {
		return nil, fmt.Errorf("read schedules dir %s: %w", f.dir, err)
	}
	var ids []string
	for _, de := range dirEntries {
		if de.IsDir() {
			continue
		}
		name := de.Name()
		// 跳过非 yaml 文件与隐藏临时文件
		if !strings.HasSuffix(name, ".yaml") || strings.HasPrefix(name, ".") {
			continue
		}
		id := strings.TrimSuffix(name, ".yaml")
		if id == "" {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := f.pg.ListDAGs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list dag runtimes: %w", err)
	}
	runtimeByID := make(map[string]*DAG, len(rows))
	for _, row := range rows {
		runtimeByID[row.ID] = row
	}
	f.mu.Lock()
	out := make([]*DAG, 0, len(ids))
	for _, id := range ids {
		e := f.refreshLocked(id)
		if e.def == nil {
			// 损坏文件已记告警，跳过
			continue
		}
		d := dagFromDef(e.def)
		if row := runtimeByID[id]; row != nil {
			overlayRuntime(d, row)
		}
		out = append(out, d)
	}
	f.mu.Unlock()
	// 与 PG ListDAGs 的 updated_at DESC 排序保持一致
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// DeleteDAG 实现 Store：删定义文件 + 删 PG 运行态行。文件缺失不视为错误。
func (f *FileStore) DeleteDAG(ctx context.Context, id string) error {
	if err := validateScheduleID(id); err != nil {
		return err
	}
	f.mu.Lock()
	if err := os.Remove(f.pathOf(id)); err != nil && !os.IsNotExist(err) {
		f.mu.Unlock()
		return fmt.Errorf("remove schedule file %s: %w", f.pathOf(id), err)
	}
	delete(f.entries, id)
	f.mu.Unlock()
	return f.pg.DeleteDAG(ctx, id)
}

// Migrate 存量迁移：PG dag_jobs 里"有定义但 schedules/ 无对应文件"的行
// 导出为 YAML 定义文件。幂等：文件已存在（哪怕内容损坏，可能是人工在改）
// 一律跳过不覆盖；导出后 PG 行保留作运行态载体。
//
// 返回：本次迁移导出的条数。
func (f *FileStore) Migrate(ctx context.Context) (int, error) {
	rows, err := f.pg.ListDAGs(ctx)
	if err != nil {
		return 0, fmt.Errorf("list dag_jobs for migrate: %w", err)
	}
	n := 0
	for _, row := range rows {
		if err := validateScheduleID(row.ID); err != nil {
			f.logWarnf("[dag] migrate skipped: %v", err)
			continue
		}
		f.mu.Lock()
		e := f.refreshLocked(row.ID)
		if !e.missing {
			// 幂等：文件已存在则跳过
			f.mu.Unlock()
			continue
		}
		if err := f.writeDefLocked(row.ID, defOfDAG(row), e); err != nil {
			f.mu.Unlock()
			f.logWarnf("[dag] migrate dag %s write file failed: %v", row.ID, err)
			continue
		}
		f.mu.Unlock()
		n++
	}
	return n, nil
}
