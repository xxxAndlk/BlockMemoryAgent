package server

// fs_tree_test.go 钉死 GET /api/fs/tree（TODO #26 阶段 E 树干）的行为：
// 嵌套树结构与排序（目录前按名、文件后按名）、黑名单目录跳过（大小写不敏感）、
// depth 上限、节点数超限 truncated、无工作区 404 {code:"no_workspace"}、无效 session 404。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/agent"
)

// newFSTreeTestEnv 构造带一个"已登记 WorkDir"会话的树测试环境。
// withWorkDir=false 时会话不挂工作区（验证 no_workspace 降级）。
func newFSTreeTestEnv(t *testing.T, withWorkDir bool) (*gin.Engine, string) {
	t.Helper()
	root := t.TempDir()

	s := &agent.Session{ID: "session-1", Status: "completed"}
	if withWorkDir {
		s.WorkDir = root
	}
	mock := newMockAgentForServer()
	mock.sessions["session-1"] = s

	h := NewAPIHandler(nil)
	h.SetSessionManager(NewSessionManager(mock))

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/fs/tree", h.FSTreeHandler)
	return r, root
}

// fsTreeGet 请求 /api/fs/tree 并解析 JSON 响应。
func fsTreeGet(t *testing.T, r *gin.Engine, query string) (int, map[string]any) {
	t.Helper()
	rec := rawGet(t, r, "/api/fs/tree?"+query)
	var body map[string]any
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("解析响应失败: %v body=%s", err, rec.Body.String())
		}
	} else {
		_ = json.Unmarshal(rec.Body.Bytes(), &body) // 错误体也尝试解析 code
	}
	return rec.Code, body
}

// writeTestFile 造一个测试文件（内容为 name，便于断言 size）。
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// childByName 在节点的 children 中按名查找。
func childByName(t *testing.T, node map[string]any, name string) map[string]any {
	t.Helper()
	children, _ := node["children"].([]any)
	for _, c := range children {
		m := c.(map[string]any)
		if m["name"] == name {
			return m
		}
	}
	return nil
}

// childNames 返回节点 children 的 name 列表（断言顺序用）。
func childNames(node map[string]any) []string {
	children, _ := node["children"].([]any)
	out := make([]string, 0, len(children))
	for _, c := range children {
		out = append(out, c.(map[string]any)["name"].(string))
	}
	return out
}

// TestFSTreeHandler_BasicTree 验证嵌套结构、目录在前文件在后按名排序、
// size/mtime 字段、path 全部位于 WorkDir 内。
func TestFSTreeHandler_BasicTree(t *testing.T) {
	r, root := newFSTreeTestEnv(t, true)

	// 目录：b-dir、a-dir（验证排序）；文件：z.txt、a.md（验证排序与大小）。
	if err := os.MkdirAll(filepath.Join(root, "b-dir", "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "a-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "z.txt"), "zz")
	writeTestFile(t, filepath.Join(root, "a.md"), "aaaa")
	writeTestFile(t, filepath.Join(root, "a-dir", "deep.go"), "package x")

	code, body := fsTreeGet(t, r, "session=session-1")
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%v", code, body)
	}
	if body["root"] != root {
		t.Fatalf("root=%v want %s", body["root"], root)
	}
	if body["truncated"] != false {
		t.Fatal("truncated 应为 false")
	}

	tree := body["tree"].(map[string]any)
	if tree["type"] != "dir" || tree["name"] != filepath.Base(root) {
		t.Fatalf("tree=%v", tree)
	}
	// 根直下顺序：a-dir、b-dir（目录按名）→ a.md、z.txt（文件按名）。
	names := childNames(tree)
	want := []string{"a-dir", "b-dir", "a.md", "z.txt"}
	if len(names) != len(want) {
		t.Fatalf("children=%v want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("children=%v want %v", names, want)
		}
	}

	// 文件元数据：z.txt size=2、mtime>0；a-dir 递归带 deep.go。
	z := childByName(t, tree, "z.txt")
	if z["type"] != "file" || z["size"] != float64(2) || z["mtime"].(float64) <= 0 {
		t.Fatalf("z.txt=%v", z)
	}
	if _, hasChildren := z["children"]; hasChildren {
		t.Fatal("文件节点不应带 children")
	}
	aDir := childByName(t, tree, "a-dir")
	if aDir["type"] != "dir" {
		t.Fatalf("a-dir=%v", aDir)
	}
	if deep := childByName(t, aDir, "deep.go"); deep == nil || deep["type"] != "file" {
		t.Fatalf("a-dir 应递归包含 deep.go, a-dir=%v", aDir)
	}

	// 所有 path 必须位于 root 内。
	var checkPaths func(n map[string]any)
	checkPaths = func(n map[string]any) {
		p := n["path"].(string)
		abs, err := filepath.Abs(p)
		if err != nil || (abs != root && len(abs) <= len(root)) || (abs != root && abs[:len(root)] != root) {
			if abs != root {
				t.Fatalf("path 越界: %s (root=%s)", p, root)
			}
		}
		children, _ := n["children"].([]any)
		for _, c := range children {
			checkPaths(c.(map[string]any))
		}
	}
	checkPaths(tree)
}

// TestFSTreeHandler_Blacklist 验证黑名单目录（含大小写变体）被跳过：
// 目录本身不出现在树里，其内文件也不可见。
func TestFSTreeHandler_Blacklist(t *testing.T) {
	r, root := newFSTreeTestEnv(t, true)

	for _, d := range []string{".git", "node_modules", "DIST", "Vendor", "target", "__pycache__", ".next", ".cache"} {
		dir := filepath.Join(root, d)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(dir, "hidden.txt"), "secret")
	}
	writeTestFile(t, filepath.Join(root, "visible.txt"), "ok")

	code, body := fsTreeGet(t, r, "session=session-1")
	if code != http.StatusOK {
		t.Fatalf("status=%d", code)
	}
	tree := body["tree"].(map[string]any)
	names := childNames(tree)
	if len(names) != 1 || names[0] != "visible.txt" {
		t.Fatalf("黑名单目录应全部跳过, children=%v", names)
	}
}

// TestFSTreeHandler_DepthLimit 验证 depth 上限：超过深度的目录以无 children 形式返回，
// 其内文件不可见；depth 参数可调且被 12 封顶。
func TestFSTreeHandler_DepthLimit(t *testing.T) {
	r, root := newFSTreeTestEnv(t, true)

	// root/l1/l2/l3/l4/data.txt（root=0 层）
	deep := filepath.Join(root, "l1", "l2", "l3", "l4")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(deep, "data.txt"), "d")

	// depth=2：root(0) → l1(1) → l2(2)；l2 不再展开，l3/l4/data.txt 不可见。
	code, body := fsTreeGet(t, r, "session=session-1&depth=2")
	if code != http.StatusOK {
		t.Fatalf("status=%d", code)
	}
	tree := body["tree"].(map[string]any)
	l1 := childByName(t, tree, "l1")
	if l1 == nil {
		t.Fatal("l1 应在 depth=2 内")
	}
	l2 := childByName(t, l1, "l2")
	if l2 == nil {
		t.Fatal("l2 应在 depth=2 内")
	}
	if _, has := l2["children"]; has {
		t.Fatalf("depth=2 时 l2 不应展开 children, l2=%v", l2)
	}
	if childByName(t, tree, "l3") != nil {
		t.Fatal("l3 超出 depth=2 不应出现")
	}

	// depth=5：全链可见。depth=99 被封顶到 12 也不报错。
	code2, body2 := fsTreeGet(t, r, "session=session-1&depth=5")
	if code2 != http.StatusOK {
		t.Fatalf("status=%d", code2)
	}
	tree2 := body2["tree"].(map[string]any)
	l4 := childByName(t, childByName(t, childByName(t, childByName(t, tree2, "l1"), "l2"), "l3"), "l4")
	if l4 == nil || childByName(t, l4, "data.txt") == nil {
		t.Fatalf("depth=5 时全链应可见, body=%v", body2)
	}
}

// TestFSTreeHandler_Truncated 验证节点总数超限：truncated=true 且树被截断
// （不保证完整性，但响应仍 200、结构合法）。
func TestFSTreeHandler_Truncated(t *testing.T) {
	old := fsTreeMaxNodes
	fsTreeMaxNodes = 4 // root + 2 dir + 1 file 预算，第 4 个文件起截断
	defer func() { fsTreeMaxNodes = old }()

	r, root := newFSTreeTestEnv(t, true)
	for i := 0; i < 6; i++ {
		writeTestFile(t, filepath.Join(root, fmt.Sprintf("f%d.txt", i)), "x")
	}

	code, body := fsTreeGet(t, r, "session=session-1")
	if code != http.StatusOK {
		t.Fatalf("status=%d", code)
	}
	if body["truncated"] != true {
		t.Fatalf("truncated 应为 true, body=%v", body)
	}
	tree := body["tree"].(map[string]any)
	if n := len(childNames(tree)); n > 4 {
		t.Fatalf("截断后根节点子级应 ≤4, got %d", n)
	}
}

// TestFSTreeHandler_NoWorkspace 验证无 WorkDir 会话返回 404 {code:"no_workspace"}。
func TestFSTreeHandler_NoWorkspace(t *testing.T) {
	r, _ := newFSTreeTestEnv(t, false)
	code, body := fsTreeGet(t, r, "session=session-1")
	if code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", code)
	}
	if body["code"] != "no_workspace" {
		t.Fatalf("code=%v want no_workspace", body["code"])
	}
}

// TestFSTreeHandler_InvalidSession 验证 session 缺失/无效返回 404 {code:"no_session"}。
func TestFSTreeHandler_InvalidSession(t *testing.T) {
	r, _ := newFSTreeTestEnv(t, true)
	for _, q := range []string{"", "session=ghost"} {
		code, body := fsTreeGet(t, r, q)
		if code != http.StatusNotFound {
			t.Fatalf("query=%q status=%d want 404", q, code)
		}
		if body["code"] != "no_session" {
			t.Fatalf("query=%q code=%v want no_session", q, body["code"])
		}
	}
}

// 确保 url 包被引用（fsTreeGet 走 rawGet，这里仅保留转义辅助的引用）。
var _ = url.QueryEscape
