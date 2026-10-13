// store.go 界面部件（Widget）目录包管理：widgets/<id>/ 下 widget.json 清单 +
// Agent 自写的渲染文件（index.html/js/css/图片）。
//
// 定位：与 plugins.d/、schedules/ 平级的"Agent 补系统功能"自留地（如微信二维码
// 实时画布、定时新闻美化卡片）——不并入插件体系：插件是"给 Agent 加工具"，
// 部件是"给用户加界面"，生命周期由 Agent 经 widget_save 工具全权维护。
//
// 文件即唯一事实源：清单（name/version/description/entry/enabled）全部落在
// widget.json，可手工编辑；读路径按 (missing, mtime, size) 基线增量热载
//（直接仿 internal/dag/filestore.go 的 refreshLocked 模式），Save 走
// 临时目录 + rename 原子替换，避免读侧看到写了一半的目录。
package widgets

import (
	"context"       // 结构化日志上下文
	"encoding/json" // widget.json 序列化
	"fmt"           // 格式化错误信息
	"log"           // 未注入 logger 时的回退输出
	"os"            // 文件读写与原子 rename
	"path/filepath" // 路径拼接
	"regexp"        // id 合法性校验
	"sort"          // 列表稳定排序
	"strings"       // 路径越界判定
	"sync"          // 缓存互斥锁
	"time"          // mtime 基线与 updated_at

	"github.com/blockmemory/agent/backend/internal/logger" // 结构化日志器
)

// manifestFile 是每个部件目录里的清单文件名（目录名即部件 id）。
const manifestFile = "widget.json"

// defaultEntry 是清单缺省渲染入口：Agent 写部件时 99% 就是单页 index.html。
const defaultEntry = "index.html"

// widgetIDPattern 限定部件 id 形态：可直接当目录名与 URL 段使用，
// 杜绝路径穿越与奇异字符（与 plugins.d/ 目录命名同口径）。
var widgetIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// 保存校验上限：部件是"一张卡片"级别的产物，不允许膨胀成资源仓库。
const (
	maxWidgetFiles      = 32        // 单部件文件数上限
	maxWidgetTotalBytes = 1 << 20   // 单部件总大小上限（1MB）
	maxManifestBytes    = 64 * 1024 // widget.json 本身的大小上限（防御手改塞巨型文件）
)

// Manifest 是 widget.json 的磁盘形态（id 不落盘，目录名即 id）。
type Manifest struct {
	Name        string `json:"name"`                  // 展示名（缺省回落 id）
	Version     string `json:"version,omitempty"`     // 版本号（自由文本，展示用）
	Description string `json:"description,omitempty"` // 一句话说明
	Entry       string `json:"entry"`                 // 渲染入口文件（相对部件目录，缺省 index.html）
	Enabled     bool   `json:"enabled"`               // 启停开关（文件即唯一事实源，可手改热生效）
}

// Info 是部件对外视图（HTTP API 与 widget_list 工具的线型）。
type Info struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Description string    `json:"description"`
	Entry       string    `json:"entry"`
	Enabled     bool      `json:"enabled"`
	UpdatedAt   time.Time `json:"updated_at"` // 取 widget.json 的 mtime（保存/手改的时间戳）
	Files       []string  `json:"files"`      // 部件文件相对路径列表（正斜杠，不含 widget.json）
}

// FileInput 是 Save 的单个待写文件。
type FileInput struct {
	Path    string // 相对部件目录的路径（正斜杠）
	Content string // 文件内容（文本；二进制部件素材不在本机制范围）
}

// fileEntry 是单个部件清单的解析缓存：以 (missing, mtime, size) 为基线，
// 基线不变时直接复用 info，避免每次 List 都重新读盘解析。
type fileEntry struct {
	scanned bool      // 是否已建立基线
	missing bool      // 基线对应的清单缺失状态
	modTime time.Time // 基线对应的清单 mtime
	size    int64     // 基线对应的清单大小
	info    *Info     // 解析出的部件信息；nil 表示清单缺失或损坏
}

// Manager 管理 widgets/ 目录下的全部部件包。
//
// 并发说明：全部公开方法经 mu 串行化缓存与写盘操作（与 dag.FileStore 同款）。
type Manager struct {
	dir     string                // widgets/ 目录绝对路径
	log     *logger.Logger        // 结构化日志器；nil 时回退标准库 log
	mu      sync.Mutex            // 保护 entries 缓存
	entries map[string]*fileEntry // id -> 清单解析缓存
}

// NewManager 创建 Manager；目录不存在时自动创建。
func NewManager(dir string) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create widgets dir %s: %w", dir, err)
	}
	return &Manager{dir: dir, entries: make(map[string]*fileEntry)}, nil
}

// SetLogger 注入结构化日志器；未注入时损坏清单等告警回退标准库 log。
func (m *Manager) SetLogger(l *logger.Logger) { m.log = l }

// Dir 返回 widgets/ 根目录（HTTP 文件服务拼接用）。
func (m *Manager) Dir() string { return m.dir }

// logWarnf 记录告警日志；与 dag.FileStore 同款回退风格。
func (m *Manager) logWarnf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if m.log != nil {
		m.log.Warn(context.Background(), msg)
		return
	}
	log.Printf("%s", msg)
}

// ValidateID 校验部件 id 可安全映射为目录名与 URL 段。
func ValidateID(id string) error {
	if !widgetIDPattern.MatchString(id) {
		return fmt.Errorf("部件 id 非法 %q（须匹配 [a-z0-9][a-z0-9-]{0,63}）", id)
	}
	return nil
}

// dirOf 返回指定部件的目录路径（调用方须先通过 ValidateID）。
func (m *Manager) dirOf(id string) string {
	return filepath.Join(m.dir, id)
}

// manifestPathOf 返回指定部件的清单文件路径。
func (m *Manager) manifestPathOf(id string) string {
	return filepath.Join(m.dirOf(id), manifestFile)
}

// refreshLocked 按 (missing, mtime, size) 基线增量重读部件清单（调用方持锁）。
//
// 清单缺失/损坏时 info 置 nil：缺失视为"部件不存在"，损坏记告警并跳过，
// 不拖垮整个 List（手改坏的文件不吞错也不拖垮列表，与 dag.FileStore 同策略）。
// 清单内容变化（手改/重写）即重扫文件列表——文件列表本身不进基线，
// 附属文件（js/css）的增删只在清单重写时体现，属可接受粒度（Agent 改代码必走
// widget_save 重写清单，手工调文件属运维动作，以清单为准）。
func (m *Manager) refreshLocked(id string) *fileEntry {
	e := m.entries[id]
	if e == nil {
		e = &fileEntry{}
		m.entries[id] = e
	}
	path := m.manifestPathOf(id)
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 基线已是"缺失"则直接复用
			if e.scanned && e.missing {
				return e
			}
			e.scanned, e.missing = true, true
			e.modTime, e.size, e.info = time.Time{}, 0, nil
			return e
		}
		// stat 失败：保留旧缓存，记告警
		m.logWarnf("[widgets] stat manifest failed: %s: %v", path, err)
		return e
	}
	// 基线未变：直接复用缓存
	if e.scanned && !e.missing && fi.ModTime().Equal(e.modTime) && fi.Size() == e.size {
		return e
	}
	data, err := os.ReadFile(path)
	if err != nil {
		m.logWarnf("[widgets] read manifest failed: %s: %v", path, err)
		return e
	}
	if len(data) > maxManifestBytes {
		m.logWarnf("[widgets] manifest too large, skipped: %s (%d bytes)", path, len(data))
		e.scanned, e.missing = true, false
		e.modTime, e.size = fi.ModTime(), fi.Size()
		e.info = nil
		return e
	}
	var mf Manifest
	if err := json.Unmarshal(data, &mf); err != nil {
		m.logWarnf("[widgets] manifest corrupted, skipped: %s: %v", path, err)
		e.scanned, e.missing = true, false
		e.modTime, e.size = fi.ModTime(), fi.Size()
		e.info = nil
		return e
	}
	if mf.Entry == "" {
		mf.Entry = defaultEntry
	}
	name := mf.Name
	if name == "" {
		name = id
	}
	e.scanned, e.missing = true, false
	e.modTime, e.size = fi.ModTime(), fi.Size()
	e.info = &Info{
		ID:          id,
		Name:        name,
		Version:     mf.Version,
		Description: mf.Description,
		Entry:       mf.Entry,
		Enabled:     mf.Enabled,
		UpdatedAt:   fi.ModTime(),
		Files:       m.listFilesLocked(id),
	}
	return e
}

// listFilesLocked 扫部件目录收集文件相对路径列表（正斜杠，不含清单与隐藏临时文件；
// 调用方持锁）。目录不可读时返回 nil（信息缺失不视为错误，文件服务会 404）。
func (m *Manager) listFilesLocked(id string) []string {
	root := m.dirOf(id)
	var files []string
	_ = filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil // 单点失败跳过，不拖垮整树
		}
		if fi.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		// 清单不进文件列表（它是元数据）；隐藏文件/目录是原子写的临时残留，跳过
		if rel == manifestFile || strings.HasPrefix(filepath.Base(rel), ".") {
			return nil
		}
		files = append(files, rel)
		return nil
	})
	sort.Strings(files)
	return files
}

// List 返回全部部件信息（目录扫描 + 增量重读，按 id 稳定排序）。
func (m *Manager) List() []*Info {
	dirEntries, err := os.ReadDir(m.dir)
	if err != nil {
		m.logWarnf("[widgets] read widgets dir failed: %s: %v", m.dir, err)
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Info, 0, len(dirEntries))
	for _, de := range dirEntries {
		if !de.IsDir() || strings.HasPrefix(de.Name(), ".") {
			continue // 隐藏目录是原子写的临时/备份残留
		}
		id := de.Name()
		if ValidateID(id) != nil {
			continue // 异形目录名（人工放的杂物）不参与管理
		}
		e := m.refreshLocked(id)
		if e.info == nil {
			continue // 无清单/损坏：已记告警，跳过
		}
		out = append(out, e.info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Get 返回单个部件信息；不存在（无清单/损坏/id 非法）返回 ok=false。
func (m *Manager) Get(id string) (*Info, bool) {
	if ValidateID(id) != nil {
		return nil, false
	}
	m.mu.Lock()
	e := m.refreshLocked(id)
	info := e.info
	m.mu.Unlock()
	if info == nil {
		return nil, false
	}
	return info, true
}

// Reload 清空解析缓存后全量重扫（POST /api/widgets/reload）。
func (m *Manager) Reload() []*Info {
	m.mu.Lock()
	m.entries = make(map[string]*fileEntry)
	m.mu.Unlock()
	return m.List()
}

// validateRelPath 校验部件内文件相对路径：拒绝绝对路径与 .. 越界写法。
// 返回清洗后的正斜杠相对路径。
func validateRelPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("文件路径为空")
	}
	// Windows 的 filepath.IsAbs 不认 "\abs\x.js" 这类无盘符根路径，须显式拒掉
	// 前导斜杠/反斜杠与盘符写法（"C:\x"），否则 Join 后可能逃逸出部件目录。
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) || strings.Contains(p, ":") {
		return "", fmt.Errorf("文件路径越界 %q（拒绝绝对路径与 ..）", p)
	}
	rel := filepath.Clean(filepath.FromSlash(p))
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("文件路径越界 %q（拒绝绝对路径与 ..）", p)
	}
	if strings.HasPrefix(filepath.Base(rel), ".") {
		return "", fmt.Errorf("文件路径 %q 不允许隐藏文件名（与原子写临时文件冲突）", p)
	}
	return filepath.ToSlash(rel), nil
}

// Save 校验并原子替换一个部件包（widget_save 工具的写侧）。
//
// 校验：id 形态、文件路径不越界、文件数与总大小上限、entry 必须在文件列表。
// 更新已存在部件时保留原清单的 enabled 与 version（调用方未给 version 时），
// 使"改代码"不会意外翻转启停状态。
func (m *Manager) Save(id string, mf Manifest, files []FileInput) (*Info, error) {
	if err := ValidateID(id); err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("至少需要一个文件（入口 %s）", defaultEntry)
	}
	if len(files) > maxWidgetFiles {
		return nil, fmt.Errorf("文件数 %d 超过上限 %d", len(files), maxWidgetFiles)
	}
	entry := mf.Entry
	if entry == "" {
		entry = defaultEntry
	}
	entry, err := validateRelPath(entry)
	if err != nil {
		return nil, fmt.Errorf("entry 非法: %v", err)
	}
	// 逐个清洗路径 + 累计大小；entry 必须在文件列表（前端 iframe 加载的就是它）。
	seen := make(map[string]bool, len(files))
	total := 0
	entryFound := false
	for i := range files {
		rel, err := validateRelPath(files[i].Path)
		if err != nil {
			return nil, err
		}
		if rel == manifestFile {
			return nil, fmt.Errorf("文件路径 %q 与清单文件名冲突", manifestFile)
		}
		if seen[rel] {
			return nil, fmt.Errorf("文件路径重复 %q", rel)
		}
		seen[rel] = true
		files[i].Path = rel
		total += len(files[i].Content)
		if rel == entry {
			entryFound = true
		}
	}
	if total > maxWidgetTotalBytes {
		return nil, fmt.Errorf("总大小 %d 字节超过上限 %d", total, maxWidgetTotalBytes)
	}
	if !entryFound {
		return nil, fmt.Errorf("entry %q 不在文件列表中", entry)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// 更新场景保留原 enabled/version：保存语义是"改代码/改描述"，不是"改开关"。
	if e := m.refreshLocked(id); e.info != nil {
		mf.Enabled = e.info.Enabled
		if mf.Version == "" {
			mf.Version = e.info.Version
		}
	}
	mf.Entry = entry
	body, err := json.MarshalIndent(&mf, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal manifest %s: %w", id, err)
	}

	// 写临时目录 → 校验通过 → rename 原子替换：读侧永远看到完整旧版或完整新版，
	// 不会看到写了一半的目录（Windows 的 os.Rename 不覆盖已存在目录，走备份换名）。
	tmp, err := os.MkdirTemp(m.dir, ".widget-tmp-*")
	if err != nil {
		return nil, fmt.Errorf("create temp widget dir: %w", err)
	}
	rollback := func() { _ = os.RemoveAll(tmp) }
	if err := os.WriteFile(filepath.Join(tmp, manifestFile), body, 0o644); err != nil {
		rollback()
		return nil, fmt.Errorf("write manifest: %w", err)
	}
	for _, f := range files {
		abs := filepath.Join(tmp, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			rollback()
			return nil, fmt.Errorf("mkdir for %s: %w", f.Path, err)
		}
		if err := os.WriteFile(abs, []byte(f.Content), 0o644); err != nil {
			rollback()
			return nil, fmt.Errorf("write file %s: %w", f.Path, err)
		}
	}
	target := m.dirOf(id)
	backup := ""
	if _, err := os.Stat(target); err == nil {
		backup, err = os.MkdirTemp(m.dir, ".widget-old-*")
		if err != nil {
			rollback()
			return nil, fmt.Errorf("create backup dir: %w", err)
		}
		// MkdirTemp 建出来的是目录，rename 前须先移除占位
		_ = os.RemoveAll(backup)
		if err := os.Rename(target, backup); err != nil {
			rollback()
			return nil, fmt.Errorf("backup old widget dir: %w", err)
		}
	}
	if err := os.Rename(tmp, target); err != nil {
		// 回滚：恢复旧目录，清临时目录
		if backup != "" {
			_ = os.Rename(backup, target)
		}
		rollback()
		return nil, fmt.Errorf("replace widget dir: %w", err)
	}
	if backup != "" {
		_ = os.RemoveAll(backup)
	}
	// 清缓存强制下一次读重建基线（新清单 mtime/size 必变，此处防御性兜底）
	delete(m.entries, id)
	e := m.refreshLocked(id)
	return e.info, nil
}

// SetEnabled 原子改写清单的 enabled 字段（热启停；保留其余字段原样）。
func (m *Manager) SetEnabled(id string, enabled bool) (*Info, error) {
	if err := ValidateID(id); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.refreshLocked(id)
	if e.info == nil {
		return nil, fmt.Errorf("部件不存在: %s", id)
	}
	mf := Manifest{
		Name:        e.info.Name,
		Version:     e.info.Version,
		Description: e.info.Description,
		Entry:       e.info.Entry,
		Enabled:     enabled,
	}
	if mf.Name == id {
		mf.Name = "" // 缺省回落 id 的不落盘，保持清单干净
	}
	body, err := json.MarshalIndent(&mf, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal manifest %s: %w", id, err)
	}
	// 临时文件 + rename 原子改写（Windows rename 不覆盖，先删旧文件，
	// 与 dag.FileStore.writeDefLocked 同款处理）。
	tmp, err := os.CreateTemp(m.dirOf(id), ".manifest-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create temp manifest: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return nil, fmt.Errorf("write temp manifest: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return nil, fmt.Errorf("close temp manifest: %w", err)
	}
	path := m.manifestPathOf(id)
	if err := os.Remove(path); err != nil {
		os.Remove(tmpName)
		return nil, fmt.Errorf("remove old manifest: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return nil, fmt.Errorf("rename manifest: %w", err)
	}
	delete(m.entries, id)
	e = m.refreshLocked(id)
	return e.info, nil
}

// Delete 删除整个部件目录（仅 HTTP 管理面用，工具不开放删除）。
// 不存在不视为错误（幂等）。
func (m *Manager) Delete(id string) error {
	if err := ValidateID(id); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := os.RemoveAll(m.dirOf(id)); err != nil {
		return fmt.Errorf("remove widget dir %s: %w", id, err)
	}
	delete(m.entries, id)
	return nil
}
