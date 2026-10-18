package server

// learned_skills_delete_test.go 钉死 DELETE /api/skills/learned/:name 的行为：
// 删 PG 行 + 技能池退池 + evolution_log（skill_delete）；重点校验文件删除的
// 路径安全——content_path 解析结果落在技能目录外时跳过文件删除（防穿越），
// 目录式技能（<name>/SKILL.md）删除整个技能目录（含 scripts/）。
// 存储层用记录型 fake SQL driver（无真实 PG，同 store 包测试手法）。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/internal/skill"
	"github.com/blockmemory/agent/backend/internal/store"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// lsdRecorder 全局记录器：预设 Get 返回行，捕获执行的 SQL。本文件测试串行执行。
var lsdRecorder = struct {
	mu      sync.Mutex
	rows    [][]driver.Value // 预设 learned_skills 行（空 = sql.ErrNoRows）
	queries []string
}{}

func init() {
	sql.Register("learned_skills_delete_test_driver", &lsdDriver{})
}

// lsdCols 与 scanLearnedSkill 的列序一致（含 015 的 last_used_at）。
var lsdCols = []string{
	"name", "title", "when_to_use", "content_path", "enabled", "use_count",
	"has_tools", "source_session", "outcome", "created_at", "updated_at", "last_used_at",
}

func lsdReset(rows [][]driver.Value) {
	lsdRecorder.mu.Lock()
	defer lsdRecorder.mu.Unlock()
	lsdRecorder.rows = rows
	lsdRecorder.queries = nil
}

// lsdOpenDB 打开 fake 连接并重置记录器。
func lsdOpenDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("learned_skills_delete_test_driver", "")
	if err != nil {
		t.Fatalf("open fake db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func lsdRecordedQueries() []string {
	lsdRecorder.mu.Lock()
	defer lsdRecorder.mu.Unlock()
	return append([]string{}, lsdRecorder.queries...)
}

type lsdDriver struct{}

func (d *lsdDriver) Open(name string) (driver.Conn, error) { return &lsdConn{}, nil }

type lsdConn struct{}

func (c *lsdConn) Prepare(query string) (driver.Stmt, error) {
	return nil, errors.New("lsdConn: Prepare not supported")
}
func (c *lsdConn) Close() error              { return nil }
func (c *lsdConn) Begin() (driver.Tx, error) { return nil, errors.New("lsdConn: Begin not supported") }

func (c *lsdConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	lsdRecorder.mu.Lock()
	defer lsdRecorder.mu.Unlock()
	lsdRecorder.queries = append(lsdRecorder.queries, query)
	return lsdResult(1), nil
}

func (c *lsdConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	lsdRecorder.mu.Lock()
	defer lsdRecorder.mu.Unlock()
	lsdRecorder.queries = append(lsdRecorder.queries, query)
	return &lsdRows{cols: lsdCols, rows: lsdRecorder.rows}, nil
}

type lsdResult int64

func (r lsdResult) LastInsertId() (int64, error) { return 0, nil }
func (r lsdResult) RowsAffected() (int64, error) { return int64(r), nil }

type lsdRows struct {
	cols []string
	rows [][]driver.Value
	pos  int
}

func (r *lsdRows) Columns() []string { return r.cols }
func (r *lsdRows) Close() error      { return nil }
func (r *lsdRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.pos])
	r.pos++
	return nil
}

// lsdSkillRow 造一行 learned_skills 记录（content_path 由调用方指定）。
func lsdSkillRow(name, contentPath string) []driver.Value {
	now := time.Now()
	return []driver.Value{
		name, name, "适用场景", contentPath, true, int64(3),
		false, nil, nil, now, now, nil,
	}
}

// newLearnedSkillDeleteRouter 构造挂 fake 存储 + 真实技能池的 DELETE 测试路由。
func newLearnedSkillDeleteRouter(t *testing.T, skillsDir string, pool *skill.Pool) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := NewAPIHandler(nil)
	h.SetRuntime(&runtime.Runtime{Skills: pool})
	h.SetLearnedSkills(store.NewLearnedSkillStore(lsdOpenDB(t), nil))
	h.SetLearnedSkillsDir(skillsDir)
	r := gin.New()
	r.DELETE("/api/skills/learned/:name", h.DeleteLearnedSkillHandler)
	return r
}

func doDeleteReq(r *gin.Engine, name string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/skills/learned/"+name, nil)
	r.ServeHTTP(rec, req)
	return rec
}

// TestDeleteLearnedSkillSkipsFileOutsideDir 路径安全校验：content_path 在技能目录外
//（老格式 .md）时不删文件，仅删 PG 行 + 退池 + 审计。
func TestDeleteLearnedSkillSkipsFileOutsideDir(t *testing.T) {
	skillsDir := t.TempDir()
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "escape-skill.md")
	if err := os.WriteFile(outsideFile, []byte("do not delete me"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	lsdReset([][]driver.Value{lsdSkillRow("escape-skill", outsideFile)})
	pool := skill.NewPool()
	pool.Register(&types.Skill{SkillID: "escape-skill"})
	r := newLearnedSkillDeleteRouter(t, skillsDir, pool)

	rec := doDeleteReq(r, "escape-skill")
	if rec.Code != http.StatusOK {
		t.Fatalf("expect 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp["ok"] != true {
		t.Fatalf("resp = %s", rec.Body.String())
	}
	// 目录外文件必须原样保留。
	if data, err := os.ReadFile(outsideFile); err != nil || string(data) != "do not delete me" {
		t.Fatalf("outside file must be kept, err=%v", err)
	}
	// 技能池退池 + 审计流水（skill_delete）。
	if got := pool.All(); len(got) != 0 {
		t.Fatalf("pool must remove deleted skill, got %v", got)
	}
	joined := strings.Join(lsdRecordedQueries(), "\n")
	if !strings.Contains(joined, "DELETE FROM learned_skills") || !strings.Contains(joined, "INSERT INTO evolution_log") {
		t.Fatalf("expect delete + evolution log SQL, got:\n%s", joined)
	}
}

// TestDeleteLearnedSkillRemovesDirStyleSkill 目录式技能删除整个 <name>/ 目录（含 scripts/）。
func TestDeleteLearnedSkillRemovesDirStyleSkill(t *testing.T) {
	skillsDir := t.TempDir()
	skillDir := filepath.Join(skillsDir, "doomed-skill")
	if err := os.MkdirAll(filepath.Join(skillDir, "scripts"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: x\n---\n"), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "scripts", "run.py"), []byte("print(1)"), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	lsdReset([][]driver.Value{lsdSkillRow("doomed-skill", filepath.Join(skillDir, "SKILL.md"))})
	pool := skill.NewPool()
	pool.Register(&types.Skill{SkillID: "doomed-skill"})
	r := newLearnedSkillDeleteRouter(t, skillsDir, pool)

	rec := doDeleteReq(r, "doomed-skill")
	if rec.Code != http.StatusOK {
		t.Fatalf("expect 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(skillDir); !os.IsNotExist(err) {
		t.Fatalf("skill dir must be removed entirely, stat err=%v", err)
	}
	if got := pool.All(); len(got) != 0 {
		t.Fatalf("pool must remove deleted skill, got %v", got)
	}
}

// TestDeleteLearnedSkillNotFound 记录不存在时返回 404，不发 DELETE。
func TestDeleteLearnedSkillNotFound(t *testing.T) {
	lsdReset(nil) // 无行 → sql.ErrNoRows → Get 返回 (nil, nil)
	r := newLearnedSkillDeleteRouter(t, t.TempDir(), skill.NewPool())
	rec := doDeleteReq(r, "ghost-skill")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expect 404, got %d body=%s", rec.Code, rec.Body.String())
	}
	if joined := strings.Join(lsdRecordedQueries(), "\n"); strings.Contains(joined, "DELETE FROM learned_skills") {
		t.Fatalf("must not DELETE when record missing, got:\n%s", joined)
	}
}
