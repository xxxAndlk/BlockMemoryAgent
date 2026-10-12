package store

// role_evolution_test.go 角色自进化准则存储的 SQL 语句级单测（无真实 PG）：
// 用记录型 fake 驱动捕获执行的 SQL/参数并预设返回行，校验建表 DDL、Upsert 的
// ON CONFLICT 语义、Get 的 (nil, nil) 不存在语义与 List 的稳定排序/行扫描。
import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// revRecorder 全局记录器：捕获执行的 SQL 与参数，并预设查询返回行。
// 本文件测试不并发（无 t.Parallel），包级变量串行使用安全。
var revRecorder = struct {
	mu      sync.Mutex
	queries []string
	args    [][]driver.NamedValue
	cols    []string
	rows    [][]driver.Value
}{}

func init() {
	sql.Register("role_evolution_test_driver", &revDriver{})
}

func revReset() {
	revRecorder.mu.Lock()
	defer revRecorder.mu.Unlock()
	revRecorder.queries = nil
	revRecorder.args = nil
	revRecorder.cols = nil
	revRecorder.rows = nil
}

// revSetResult 预设下一次查询返回的列与行（Get/List 用）。
func revSetResult(cols []string, rows [][]driver.Value) {
	revRecorder.mu.Lock()
	defer revRecorder.mu.Unlock()
	revRecorder.cols = cols
	revRecorder.rows = rows
}

func revRecord(query string, args []driver.NamedValue) {
	revRecorder.mu.Lock()
	defer revRecorder.mu.Unlock()
	revRecorder.queries = append(revRecorder.queries, query)
	revRecorder.args = append(revRecorder.args, args)
}

// revLast 返回最近一次记录的 SQL 与参数。
func revLast(t *testing.T) (string, []driver.NamedValue) {
	t.Helper()
	revRecorder.mu.Lock()
	defer revRecorder.mu.Unlock()
	if len(revRecorder.queries) == 0 {
		t.Fatal("expected at least one recorded query, got none")
	}
	i := len(revRecorder.queries) - 1
	return revRecorder.queries[i], revRecorder.args[i]
}

// openRevDB 打开记录型 fake 连接并重置记录器。
func openRevDB(t *testing.T) *sql.DB {
	t.Helper()
	revReset()
	db, err := sql.Open("role_evolution_test_driver", "")
	if err != nil {
		t.Fatalf("open fake db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// revDriver 记录型 fake SQL 驱动。
type revDriver struct{}

func (d *revDriver) Open(name string) (driver.Conn, error) { return &revConn{}, nil }

// revConn 实现 driver.Conn + ExecerContext/QueryerContext（database/sql 优先直连，不走 Prepare）。
type revConn struct{}

func (c *revConn) Prepare(query string) (driver.Stmt, error) {
	return nil, errors.New("revConn: Prepare not supported")
}
func (c *revConn) Close() error              { return nil }
func (c *revConn) Begin() (driver.Tx, error) { return nil, errors.New("revConn: Begin not supported") }

func (c *revConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	revRecord(query, args)
	return driver.ResultNoRows, nil
}

func (c *revConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	revRecord(query, args)
	revRecorder.mu.Lock()
	defer revRecorder.mu.Unlock()
	return &revRows{cols: revRecorder.cols, rows: revRecorder.rows}, nil
}

// revRows 按预设行返回，耗尽后返回 io.EOF（database/sql 据此为 QueryRow 生成 sql.ErrNoRows）。
type revRows struct {
	cols []string
	rows [][]driver.Value
	pos  int
}

func (r *revRows) Columns() []string { return r.cols }
func (r *revRows) Close() error      { return nil }
func (r *revRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.pos])
	r.pos++
	return nil
}

// TestEnsureRoleEvolutionSchema 验证建表 DDL 幂等语句被下发。
func TestEnsureRoleEvolutionSchema(t *testing.T) {
	db := openRevDB(t)
	if err := EnsureRoleEvolutionSchema(context.Background(), db); err != nil {
		t.Fatalf("EnsureRoleEvolutionSchema: %v", err)
	}
	q, _ := revLast(t)
	if !strings.Contains(q, "CREATE TABLE IF NOT EXISTS role_evolution_prompts") {
		t.Fatalf("schema DDL 应幂等建 role_evolution_prompts，got %q", q)
	}
}

// TestRoleEvolutionStoreGetNotFound 验证角色不存在时返回 (nil, nil) 而不是错误。
func TestRoleEvolutionStoreGetNotFound(t *testing.T) {
	db := openRevDB(t)
	s := NewRoleEvolutionStore(db)
	// 未预设任何行：驱动返回空结果集，database/sql 映射为 sql.ErrNoRows。
	rec, err := s.Get(context.Background(), "ghost")
	if err != nil || rec != nil {
		t.Fatalf("Get 不存在 = (%v, %v), want (nil, nil)", rec, err)
	}
}

// TestRoleEvolutionStoreGetFound 验证命中行的字段映射与 NULL source_session 归一。
func TestRoleEvolutionStoreGetFound(t *testing.T) {
	db := openRevDB(t)
	s := NewRoleEvolutionStore(db)
	now := time.Now()
	revSetResult(
		[]string{"role_id", "content", "source_session", "updated_at"},
		[][]driver.Value{{"coder", "保持简洁", "sess-1", now}},
	)
	rec, err := s.Get(context.Background(), "coder")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec == nil || rec.RoleID != "coder" || rec.Content != "保持简洁" ||
		rec.SourceSession != "sess-1" || !rec.UpdatedAt.Equal(now) {
		t.Fatalf("Get 字段映射错误: %+v", rec)
	}
	// source_session 为 NULL 时归一为空串。
	revSetResult(
		[]string{"role_id", "content", "source_session", "updated_at"},
		[][]driver.Value{{"coder", "保持简洁", nil, now}},
	)
	rec, err = s.Get(context.Background(), "coder")
	if err != nil || rec == nil || rec.SourceSession != "" {
		t.Fatalf("NULL source_session 应归一为空串: (%+v, %v)", rec, err)
	}
}

// TestRoleEvolutionStoreUpsert 验证全量替换语义：ON CONFLICT (role_id) DO UPDATE + updated_at=now()。
func TestRoleEvolutionStoreUpsert(t *testing.T) {
	db := openRevDB(t)
	s := NewRoleEvolutionStore(db)
	rec := &RoleEvolutionPrompt{RoleID: "coder", Content: "新准则", SourceSession: "sess-2"}
	if err := s.Upsert(context.Background(), rec); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	q, args := revLast(t)
	for _, want := range []string{
		"INSERT INTO role_evolution_prompts",
		"ON CONFLICT (role_id) DO UPDATE",
		"content = EXCLUDED.content",
		"source_session = EXCLUDED.source_session",
		"updated_at = now()",
	} {
		if !strings.Contains(q, want) {
			t.Fatalf("Upsert SQL 缺少 %q，got %q", want, q)
		}
	}
	if len(args) != 3 || args[0].Value != "coder" || args[1].Value != "新准则" || args[2].Value != "sess-2" {
		t.Fatalf("Upsert 参数错误: %+v", args)
	}
}

// TestRoleEvolutionStoreList 验证按 role_id 稳定排序的查询与行扫描。
func TestRoleEvolutionStoreList(t *testing.T) {
	db := openRevDB(t)
	s := NewRoleEvolutionStore(db)
	now := time.Now()
	revSetResult(
		[]string{"role_id", "content", "source_session", "updated_at"},
		[][]driver.Value{
			{"coder", "准则A", "sess-1", now},
			{"writer", "准则B", nil, now},
		},
	)
	list, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	q, _ := revLast(t)
	if !strings.Contains(q, "ORDER BY role_id") {
		t.Fatalf("List SQL 应按 role_id 稳定排序，got %q", q)
	}
	if len(list) != 2 {
		t.Fatalf("List 行数 = %d, want 2", len(list))
	}
	if list[0].RoleID != "coder" || list[0].Content != "准则A" || list[0].SourceSession != "sess-1" {
		t.Fatalf("List[0] 字段映射错误: %+v", list[0])
	}
	if list[1].RoleID != "writer" || list[1].SourceSession != "" {
		t.Fatalf("List[1] NULL source_session 应归一为空串: %+v", list[1])
	}
}
