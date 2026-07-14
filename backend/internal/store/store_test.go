package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/lib/pq"
)

func init() {
	// 注册一个最小化的 fake SQL 驱动，用于构造非 nil 的 *sql.DB 而不依赖真实数据库。
	sql.Register("fake_store_test_driver", &fakeDriver{})
}

type fakeDriver struct{}

func (d *fakeDriver) Open(name string) (driver.Conn, error) {
	return &fakeConn{}, nil
}

type fakeConn struct{}

func (c *fakeConn) Prepare(query string) (driver.Stmt, error) { return &fakeStmt{}, nil }
func (c *fakeConn) Close() error                              { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)                 { return &fakeTx{}, nil }

func (c *fakeConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return driver.ResultNoRows, nil
}

func (c *fakeConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return &fakeRows{}, nil
}

type fakeStmt struct{}

func (s *fakeStmt) Close() error                                    { return nil }
func (s *fakeStmt) NumInput() int                                   { return -1 }
func (s *fakeStmt) Exec(args []driver.Value) (driver.Result, error) { return driver.ResultNoRows, nil }
func (s *fakeStmt) Query(args []driver.Value) (driver.Rows, error)  { return &fakeRows{}, nil }

type fakeRows struct{}

func (r *fakeRows) Columns() []string              { return nil }
func (r *fakeRows) Close() error                   { return nil }
func (r *fakeRows) Next(dest []driver.Value) error { return errors.New("no rows") }

type fakeTx struct{}

func (t *fakeTx) Commit() error   { return nil }
func (t *fakeTx) Rollback() error { return nil }

func TestEpisodeStore_IsDuplicateError_Wrapped(t *testing.T) {
	s := &EpisodeStore{}

	duplicate := &pq.Error{Code: "23505"}
	wrapped := fmt.Errorf("insert episode: %w", duplicate)
	if !s.IsDuplicateError(wrapped) {
		t.Fatal("expected IsDuplicateError to recognize wrapped pq.Error 23505")
	}

	if s.IsDuplicateError(errors.New("some other error")) {
		t.Fatal("expected IsDuplicateError to return false for non-pq error")
	}

	otherPq := &pq.Error{Code: "23503"}
	if s.IsDuplicateError(fmt.Errorf("wrapped: %w", otherPq)) {
		t.Fatal("expected IsDuplicateError to return false for non-23505 pq error")
	}
}

func TestKnowledgeStore_Save_MetaMarshalError(t *testing.T) {
	db, err := sql.Open("fake_store_test_driver", "")
	if err != nil {
		t.Fatalf("open fake db: %v", err)
	}
	defer db.Close()

	s := &KnowledgeStore{db: db}
	rec := &types.KnowledgeRecord{
		KnowledgeType: enums.KnowledgeTypeBlockMemory,
		TopicID:       "topic-1",
		Content:       "content",
		Meta:          map[string]any{"bad": make(chan int)}, // channel 不可 JSON 序列化
		CreatedAt:     time.Now(),
	}

	err = s.Save(context.Background(), rec)
	if err == nil {
		t.Fatal("expected error when meta JSON marshaling fails, got nil")
	}
	if !strings.Contains(err.Error(), "marshal knowledge meta") {
		t.Fatalf("expected error wrapping 'marshal knowledge meta', got %v", err)
	}
}
