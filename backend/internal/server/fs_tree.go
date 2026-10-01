package server

// fs_tree.go GET /api/fs/tree —— 会话工作区整棵目录树（TODO #26 阶段 E 树干）。
//
// 安全边界：root 取会话 WorkDir（登记字符串），经 Clean+Abs 规范化后遍历；
// 遍历只从 root 出发逐层 ReadDir，天然不可能产出 WorkDir 之外的 path，
// 与 /api/files/raw 的 WriteFile 白名单不同——本端点是「工作区内任意路径可读元数据」，
// 这正是树面板的语义（预览/编辑的实际字节仍走 raw/content 各自边界）。
//
// 防爆：depth 默认 8（上限 12）+ 节点总数硬上限 5000（超出 truncated=true 并截断）+
// 黑名单目录（大小写不敏感）直接跳过不进入；单个目录读取失败（权限等）跳过该目录继续。

import (
	"net/http"      // HTTP 状态码
	"os"            // ReadDir/Stat
	"path/filepath" // Clean/Abs/Join
	"sort"          // 节点排序（目录前、按名）
	"strconv"       // depth 参数解析
	"strings"       // 黑名单大小写比较

	"github.com/gin-gonic/gin"
)

// fsTree 默认/上限参数。
const (
	fsTreeDefaultDepth = 8  // 默认递归深度（root 为第 0 层）
	fsTreeMaxDepth     = 12 // depth 可调上限
)

// fsTreeMaxNodes 节点总数硬上限（var 而非 const：测试可临时调小验证截断）。
var fsTreeMaxNodes = 5000

// fsTreeDirBlacklist 遍历时直接跳过、不进入的目录名（大小写不敏感）。
var fsTreeDirBlacklist = map[string]struct{}{
	".git":         {},
	"node_modules": {},
	"dist":         {},
	"__pycache__":  {},
	".next":        {},
	".cache":       {},
	"vendor":       {},
	"target":       {},
}

// fsTreeNode 是目录树中的单个节点。目录恒带 children（可能为空数组）；
// 文件不带 children 字段（前端按 type 判断）。
type fsTreeNode struct {
	Name     string        `json:"name"`
	Path     string        `json:"path"`
	Type     string        `json:"type"` // "dir" | "file"
	Size     int64         `json:"size,omitempty"`
	Mtime    int64         `json:"mtime"` // Unix 毫秒
	Children []*fsTreeNode `json:"children,omitempty"`
}

// FSTreeHandler 处理 GET /api/fs/tree?session=<id>[&depth=N]。
//   - session 缺失/无效 → 404 {code:"no_session"}；
//   - 会话无 WorkDir 或 WorkDir 在磁盘上不存在 → 404 {code:"no_workspace"}；
//   - 正常 → 200 {root, truncated, tree}；tree 目录按名排序在前、文件按名排序在后。
func (h *APIHandler) FSTreeHandler(c *gin.Context) {
	sessionID := c.Query("session")
	if sessionID == "" || h.sessionMgr == nil {
		c.JSON(http.StatusNotFound, gin.H{"code": "no_session"})
		return
	}
	s := h.sessionMgr.GetSession(sessionID)
	if s == nil {
		c.JSON(http.StatusNotFound, gin.H{"code": "no_session"})
		return
	}
	if s.WorkDir == "" {
		c.JSON(http.StatusNotFound, gin.H{"code": "no_workspace"})
		return
	}
	root, err := filepath.Abs(filepath.Clean(s.WorkDir))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": "no_workspace"})
		return
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		c.JSON(http.StatusNotFound, gin.H{"code": "no_workspace"})
		return
	}

	depth := fsTreeDefaultDepth
	if raw := c.Query("depth"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			if n > fsTreeMaxDepth {
				n = fsTreeMaxDepth
			}
			depth = n
		}
	}

	w := newFSTreeWalker(depth, fsTreeMaxNodes)
	tree := w.walk(root, 0)

	c.JSON(http.StatusOK, gin.H{
		"root":      root,
		"truncated": w.truncated,
		"tree":      tree,
	})
}

// fsTreeWalker 携带遍历预算（剩余节点数）与 truncated 标记。
type fsTreeWalker struct {
	maxDepth  int
	nodesLeft int
	truncated bool
}

// walk 递归构建 dir 下的树节点；当前层 = depth（root 传 0）。
// 到达深度上限/预算耗尽时不再展开子级（目录节点仍以无 children 形式返回）。
// 单个目录 ReadDir 失败（权限等）→ 该目录以空 children 返回，不整体失败。
func (w *fsTreeWalker) walk(dir string, depth int) *fsTreeNode {
	info, err := os.Stat(dir)
	if err != nil {
		info = nil // mtime/size 缺省为零值，结构仍返回
	}
	node := &fsTreeNode{
		Name:  filepath.Base(dir),
		Path:  dir,
		Type:  "dir",
		Mtime: modTimeMilli(info),
	}
	if w.nodesLeft <= 0 {
		w.truncated = true
		return node
	}
	w.nodesLeft--

	if depth >= w.maxDepth {
		return node
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return node // 权限等：跳过该目录的子级，继续
	}

	dirs := make([]os.DirEntry, 0, len(entries))
	files := make([]os.DirEntry, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			if _, black := fsTreeDirBlacklist[strings.ToLower(e.Name())]; black {
				continue // 黑名单目录不进入、不出现在树里
			}
			dirs = append(dirs, e)
		} else {
			files = append(files, e)
		}
	}
	// os.ReadDir 已按文件名排序；目录在前、文件在后分桶后顺序即"目录按名在前、文件按名在后"。
	sort.SliceStable(dirs, func(i, j int) bool { return dirs[i].Name() < dirs[j].Name() })
	sort.SliceStable(files, func(i, j int) bool { return files[i].Name() < files[j].Name() })

	node.Children = make([]*fsTreeNode, 0, len(dirs)+len(files))
	for _, e := range dirs {
		node.Children = append(node.Children, w.walk(filepath.Join(dir, e.Name()), depth+1))
	}
	for _, e := range files {
		if w.nodesLeft <= 0 {
			w.truncated = true
			break
		}
		w.nodesLeft--
		fi, err := e.Info() // 失败时以零值元数据返回，不中断整树
		var size int64
		var mt int64
		if err == nil {
			size = fi.Size()
			mt = fi.ModTime().UnixMilli()
		}
		node.Children = append(node.Children, &fsTreeNode{
			Name:  e.Name(),
			Path:  filepath.Join(dir, e.Name()),
			Type:  "file",
			Size:  size,
			Mtime: mt,
		})
	}
	return node
}

// modTimeMilli 取文件 mtime 的 Unix 毫秒；info 为 nil 时返回 0。
func modTimeMilli(info os.FileInfo) int64 {
	if info == nil {
		return 0
	}
	return info.ModTime().UnixMilli()
}

// 初始化遍历预算（每请求一个 walker，构造处设定节点预算）。
// 独立函数便于测试用更小预算验证截断行为。
func newFSTreeWalker(maxDepth, maxNodes int) *fsTreeWalker {
	return &fsTreeWalker{maxDepth: maxDepth, nodesLeft: maxNodes}
}
