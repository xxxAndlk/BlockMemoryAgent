package tool

// filemap.go 任务级常驻文件小地图：记录每个 Agent 触碰（ReadFile/WriteFile/EditFile 成功）
// 过的文件，按 mtime 缓存符号轮廓（复用 project.FileOutline 单文件抽取，与 PROJECT.md
// 同源），渲染为一条注入文本供记忆流水线在 history 尾部常驻注入——压缩循环压不掉，
// 解决"压缩后连哪个函数在哪个文件哪行都忘了"的失忆重读。
//
// 链路：Registry.Dispatch 工具成功后 touchFileMap 登记（按 Agent 隔离）->
// bootstrap 经 memory.WithFileMapProvider(FileMapText) 接线 -> Pipeline.Assemble 尾部注入。

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/blockmemory/agent/backend/internal/project" // project 包提供单文件符号轮廓抽取
)

const (
	// maxFileMapRunes 单次注入文本总上限（rune）：超出按最久未触碰裁掉。
	// 小地图是常驻注入，每轮都占 token，2000 runes ≈ 15-25 个文件的轮廓。
	maxFileMapRunes = 2000
	// maxFileMapFiles 单次注入的文件数上限：按最近触碰优先截断。
	maxFileMapFiles = 20
	// maxFileMapTracked 每 Agent 追踪集容量上限：超限淘汰最久未触碰的条目。
	// 追踪集进程生命周期内不清理（与事件流同生命周期），容量兜底防长会话无限增长。
	maxFileMapTracked = 64
)

// fileMapEntry 是单条触碰记录：mtime 缓存轮廓 + 最近触碰序号（LRU）。
type fileMapEntry struct {
	mtime    time.Time // 上次成功抽取轮廓时的文件修改时间
	outline  string    // 缓存的符号轮廓（mtime 未变直接复用，变了才重扫）
	probed   bool      // 是否已抽取过轮廓（区分零值 mtime 与未抽取）
	touchSeq uint64    // 最近触碰序号（全局递增，LRU 排序/淘汰用）
}

// fileMapTracker 按 Agent 隔离记录触碰文件集与轮廓缓存。受 mu 保护。
type fileMapTracker struct {
	mu      sync.Mutex
	seq     uint64                             // 全局触碰序号（单调递增）
	byAgent map[string]map[string]*fileMapEntry // agentID -> (path -> 条目)
}

func newFileMapTracker() *fileMapTracker {
	return &fileMapTracker{byAgent: make(map[string]map[string]*fileMapEntry)}
}

// touch 登记一次文件触碰：新增/刷新条目的 LRU 序号；追踪集超上限淘汰最久未触碰的条目。
func (t *fileMapTracker) touch(agentID, path string) {
	if agentID == "" || path == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	set := t.byAgent[agentID]
	if set == nil {
		set = make(map[string]*fileMapEntry)
		t.byAgent[agentID] = set
	}
	e := set[path]
	if e == nil {
		e = &fileMapEntry{}
		set[path] = e
	}
	e.touchSeq = t.seq
	if len(set) > maxFileMapTracked {
		var oldestPath string
		var oldestSeq uint64
		for p, e := range set {
			if oldestPath == "" || e.touchSeq < oldestSeq {
				oldestPath, oldestSeq = p, e.touchSeq
			}
		}
		delete(set, oldestPath)
	}
}

// render 渲染指定 Agent 的【本任务文件地图】注入文本；无触碰文件或全部不可读时返回空串。
// 按最近触碰优先截断文件数与总 rune 上限（被裁的是最久未触碰的）；
// mtime 未变复用缓存轮廓，变了才重扫；文件消失/读取失败降级跳过该文件，不报错。
func (t *fileMapTracker) render(agentID string) string {
	// 锁内只拷贝条目指针与序号；stat/轮廓抽取放锁外（IO 不阻塞 touch）。
	type item struct {
		path string
		seq  uint64
		e    *fileMapEntry
	}
	t.mu.Lock()
	set := t.byAgent[agentID]
	list := make([]item, 0, len(set))
	for p, e := range set {
		list = append(list, item{p, e.touchSeq, e})
	}
	t.mu.Unlock()
	if len(list) == 0 {
		return ""
	}

	// 最近触碰优先：LRU 序号降序，超限截断文件数。
	sort.Slice(list, func(i, j int) bool { return list[i].seq > list[j].seq })
	if len(list) > maxFileMapFiles {
		list = list[:maxFileMapFiles]
	}

	var lines []string
	total := 0
	for _, it := range list {
		outline := t.outlineFor(it.e, it.path)
		if outline == "" {
			continue // 文件消失/读取失败/非源码扩展名：降级跳过该文件，不报错
		}
		line := "- " + it.path + outline
		n := len([]rune(line)) + 1
		if len(lines) > 0 && total+n > maxFileMapRunes {
			break // 总量超限：裁掉其余（list 已按最近触碰排序，被裁的是最久未触碰的）
		}
		total += n
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return ""
	}
	return "【本任务文件地图】\n" +
		"本任务已触碰文件的符号轮廓（名称 L行号，() 后缀为函数），细节用 ReadFile 带 offset 精读；文件变动后轮廓自动更新。\n" +
		strings.Join(lines, "\n")
}

// outlineFor 取单文件符号轮廓：mtime 未变用缓存，变了重扫（project.FileOutline，锁外执行）；
// 文件消失/不可读返回空串，调用方降级跳过。
func (t *fileMapTracker) outlineFor(e *fileMapEntry, path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return "" // 文件消失/不可读：降级跳过
	}
	t.mu.Lock()
	if e.probed && fi.ModTime().Equal(e.mtime) {
		outline := e.outline
		t.mu.Unlock()
		return outline
	}
	t.mu.Unlock()
	// 锁外抽取（IO + 行首正则，best-effort）；非源码扩展名时 FileOutline 自身返回空串。
	outline := project.FileOutline(path)
	t.mu.Lock()
	e.mtime, e.outline, e.probed = fi.ModTime(), outline, true
	t.mu.Unlock()
	return outline
}

// touchFileMap 登记一次文件触碰（ReadFile/WriteFile/EditFile 成功后由 Dispatch 调用）。
// 按 Agent 隔离：agentID 取自 ctx（ReActAgent.Run 经 WithAgentID 注入），
// 与记忆流水线 Assemble 的 agentID 同源；ctx 无 agentID 时跳过。
func (r *Registry) touchFileMap(ctx context.Context, path string) {
	if r.fileMap == nil {
		return
	}
	r.fileMap.touch(AgentIDFromContext(ctx), filepath.Clean(path))
}

// FileMapText 渲染指定 Agent 的【本任务文件地图】注入文本，供记忆流水线尾部常驻注入
// （bootstrap 经 memory.WithFileMapProvider 接线）。无触碰文件或全部不可读时返回空串。
func (r *Registry) FileMapText(agentID string) string {
	if r == nil || r.fileMap == nil {
		return ""
	}
	return r.fileMap.render(agentID)
}
