package store

import (
	"context"      // 上下文，控制测试生命周期
	"database/sql" // 标准库 SQL 抽象层
	"database/sql/driver"
	"errors"  // 构造测试用错误
	"fmt"     // 包装错误以测试 errors.As
	"strings" // 断言错误消息内容
	"testing" // Go 测试框架
	"time"    // 构造时间戳

	"github.com/blockmemory/agent/backend/pkg/enums" // 知识类型枚举
	"github.com/blockmemory/agent/backend/pkg/types" // 领域模型
	"github.com/lib/pq"                              // PostgreSQL 错误码
)

// init 在包加载时注册一个最小化的 fake SQL 驱动。
// 目的：构造非 nil 的 *sql.DB 而无需真实数据库，KnowledgeStore 测试中只校验序列化失败路径。
func init() {
	// 注册 fake 驱动到 database/sql 全局注册表
	sql.Register("fake_store_test_driver", &fakeDriver{})
}

// fakeDriver 是一个不连接真实数据库的驱动桩。
type fakeDriver struct{}

// Open 返回一个伪造连接；name 参数在此桩中不使用。
func (d *fakeDriver) Open(name string) (driver.Conn, error) {
	return &fakeConn{}, nil
}

// fakeConn 实现 driver.Conn 及上下文相关扩展接口。
type fakeConn struct{}

// Prepare 返回空语句桩。
func (c *fakeConn) Prepare(query string) (driver.Stmt, error) { return &fakeStmt{}, nil }

// Close 无任何资源需要释放。
func (c *fakeConn) Close() error { return nil }

// Begin 返回空事务桩。
func (c *fakeConn) Begin() (driver.Tx, error) { return &fakeTx{}, nil }

// ExecContext 假装执行成功，返回无影响行。
func (c *fakeConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return driver.ResultNoRows, nil
}

// QueryContext 假装查询成功，返回空结果集。
func (c *fakeConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return &fakeRows{}, nil
}

// fakeStmt 实现 driver.Stmt。
type fakeStmt struct{}

// Close 关闭空语句。
func (s *fakeStmt) Close() error { return nil }

// NumInput 返回 -1 表示不校验参数数量。
func (s *fakeStmt) NumInput() int { return -1 }

// Exec 假装执行成功。
func (s *fakeStmt) Exec(args []driver.Value) (driver.Result, error) { return driver.ResultNoRows, nil }

// Query 假装查询成功，返回空结果集。
func (s *fakeStmt) Query(args []driver.Value) (driver.Rows, error) { return &fakeRows{}, nil }

// fakeRows 实现 driver.Rows，模拟空结果。
type fakeRows struct{}

// Columns 返回空列定义。
func (r *fakeRows) Columns() []string { return nil }

// Close 关闭空结果集。
func (r *fakeRows) Close() error { return nil }

// Next 始终返回“无更多行”，模拟空结果。
func (r *fakeRows) Next(dest []driver.Value) error { return errors.New("no rows") }

// fakeTx 实现 driver.Tx。
type fakeTx struct{}

// Commit 假装提交成功。
func (t *fakeTx) Commit() error { return nil }

// Rollback 假装回滚成功。
func (t *fakeTx) Rollback() error { return nil }

// TestEpisodeStore_IsDuplicateError_Wrapped 验证 IsDuplicateError 能识别被 fmt.Errorf 包装的 pq.Error 23505。
func TestEpisodeStore_IsDuplicateError_Wrapped(t *testing.T) {
	// 构造零值 EpisodeStore，仅用于调用 IsDuplicateError 方法
	s := &EpisodeStore{}

	// 23505 是 PostgreSQL 唯一约束冲突的错误码
	duplicate := &pq.Error{Code: "23505"}
	// 用 fmt.Errorf 包装，模拟实际 SQL 执行后的错误链路
	wrapped := fmt.Errorf("insert episode: %w", duplicate)
	if !s.IsDuplicateError(wrapped) {
		t.Fatal("expected IsDuplicateError to recognize wrapped pq.Error 23505")
	}

	// 非 pq 错误应返回 false
	if s.IsDuplicateError(errors.New("some other error")) {
		t.Fatal("expected IsDuplicateError to return false for non-pq error")
	}

	// 其他 pq 错误码（23503 外键约束）不应被误判为重复
	otherPq := &pq.Error{Code: "23503"}
	if s.IsDuplicateError(fmt.Errorf("wrapped: %w", otherPq)) {
		t.Fatal("expected IsDuplicateError to return false for non-23505 pq error")
	}
}

// TestKnowledgeStore_Save_MetaMarshalError 验证 Meta 包含不可序列化值时 Save 返回明确错误。
func TestKnowledgeStore_Save_MetaMarshalError(t *testing.T) {
	// 打开 fake 数据库连接，仅用于构造 KnowledgeStore
	db, err := sql.Open("fake_store_test_driver", "")
	if err != nil {
		t.Fatalf("open fake db: %v", err)
	}
	// 测试结束后关闭连接，即使后续出错也保证释放
	defer db.Close()

	// 初始化 KnowledgeStore，注入 fake DB
	s := &KnowledgeStore{db: db}
	rec := &types.KnowledgeRecord{
		KnowledgeType: enums.KnowledgeTypeBlockMemory,
		TopicID:       "topic-1",
		Content:       "content",
		// channel 不可 JSON 序列化，用于触发 json.Marshal 失败
		Meta:      map[string]any{"bad": make(chan int)},
		CreatedAt: time.Now(),
	}

	// 调用 Save，期望在序列化 Meta 时出错
	err = s.Save(context.Background(), rec)
	if err == nil {
		t.Fatal("expected error when meta JSON marshaling fails, got nil")
	}
	// 断言错误消息包含 “marshal knowledge meta”，便于调用方识别错误阶段
	if !strings.Contains(err.Error(), "marshal knowledge meta") {
		t.Fatalf("expected error wrapping 'marshal knowledge meta', got %v", err)
	}
}

// TestSanitizeUTF8 验证存储层 UTF-8 清洗：NUL 字节被剥离（Postgres text/jsonb 拒绝 0x00），
// 非法 UTF-8 序列被替换为 U+FFFD，合法文本原样保留。
func TestSanitizeUTF8(t *testing.T) {
	if got := sanitizeUTF8("a\x00b\x00c"); got != "abc" {
		t.Fatalf("NUL 字节应被剥离，got %q", got)
	}
	if got := sanitizeUTF8("hello 世界"); got != "hello 世界" {
		t.Fatalf("合法 UTF-8 不应改变，got %q", got)
	}
	if got := sanitizeUTF8(string([]byte{'a', 0xff, 'b'})); !strings.Contains(got, "�") {
		t.Fatalf("非法 UTF-8 应被替换为 U+FFFD，got %q", got)
	}
	if got := sanitizeUTF8(""); got != "" {
		t.Fatalf("空字符串应原样返回，got %q", got)
	}
}

// TestSanitizeJSONValue 验证 jsonb 写入前的递归清洗：字符串值与 map 键中的
// NUL 被剥离，嵌套结构与非字符串值保持不变，字面 "" 文本不受影响。
func TestSanitizeJSONValue(t *testing.T) {
	in := map[string]any{
		"output": "line1\x00line2",
		"ok":     true,
		"n":      42,
		"nested": map[string]any{"k\x00ey": "v\x00"},
		"list":   []any{"a\x00", 1.5},
	}
	out := sanitizeJSONValue(in).(map[string]any)
	if out["output"] != "line1line2" {
		t.Fatalf("字符串值中的 NUL 应被剥离，got %q", out["output"])
	}
	if out["ok"] != true || out["n"] != 42 {
		t.Fatal("非字符串值不应改变")
	}
	nested := out["nested"].(map[string]any)
	if nested["key"] != "v" {
		t.Fatalf("嵌套 map 的键与值都应被清洗，got %v", nested)
	}
	list := out["list"].([]any)
	if list[0] != "a" || list[1] != 1.5 {
		t.Fatalf("切片元素应被清洗，got %v", list)
	}
	// 字面 6 字符  文本是合法 ASCII，不应被破坏。
	literal := map[string]any{"s": `\u0000`}
	got := sanitizeJSONValue(literal).(map[string]any)
	if got["s"] != `\u0000` {
		t.Fatalf("字面 \u0000 文本不应被破坏，got %q", got["s"])
	}
}
