// Package checkpoint 提供工作区内容寻址检查点（Harness 根治计划 WP2 §6.4）。
//
// 设计取舍（与计划的显式偏差，需在文档里说明）：
//   - 计划建议"shadow git（独立 git-dir）"。本实现改用**内容寻址快照**（blob 目录 +
//     JSON 索引），原因：Windows 上对用户工作区调 `git --work-tree` 需要 git 可执行
//     文件、要处理 index.lock/CRLF/filter 语义，且在脏工作区上仍可能写用户 .git；
//     内容寻址方案零外部依赖、零用户 git 副作用，二进制/权限位/行尾天然按字节保真。
//   - 不变量要求全部满足：不碰用户 `.git`、包含 dirty 与 untracked、保留二进制与
//     权限位、恢复走"仅回退本 Action 的变化 + 外部漂移检测"，绝不 `git reset --hard`
//     或整目录覆盖。
//   - 排除项必须可审计：快照自带 Excluded 列表与 Degraded 原因，调用方可据此拒绝
//     "在 enforce 模式下静默降低安全等级"。
//
// 本包不 import tool / agent / server（避免循环与层级倒挂）：只剩文件系统与哈希。
package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// defaultExcludes 是默认排除的目录/文件名（BMA 自身产物与常见巨型依赖目录）。
// 排除项会写入快照的 Excluded 字段，便于审计"这次快照少了什么"。
var defaultExcludes = []string{
	".git", ".bma", "node_modules", "dist", "build", ".next", ".nuxt",
	"vendor", "target", "__pycache__", ".venv", "venv", ".idea", ".vscode",
	".cache", "coverage", ".pytest_cache",
}

// 默认文件数上限（超限即 degraded，enforce 模式据此 fail closed）。
const defaultMaxFiles = 20000

// FileEntry 单个文件的内容指纹与元数据。
type FileEntry struct {
	// Hash 内容 sha256（hex）。
	Hash string `json:"hash"`
	// Size 字节数。
	Size int64 `json:"size"`
	// Mode 权限位（os.FileMode & 0o777）。
	Mode uint32 `json:"mode"`
	// Symlink 是否为符号链接（true 时 Hash 为链接目标文本的 hash，不跟随后写入）。
	Symlink bool `json:"symlink,omitempty"`
}

// Snapshot 一次工作区快照（相对路径 → 指纹）。
type Snapshot struct {
	ID        string               `json:"id"`
	Root      string               `json:"root"`
	Files     map[string]FileEntry `json:"files"`
	Excluded  []string             `json:"excluded"`
	Degraded  bool                 `json:"degraded,omitempty"`
	Reason    string               `json:"reason,omitempty"`
	CreatedAt time.Time            `json:"created_at"`
	// TotalBytes 快照覆盖的字节数（性能/容量报表用）。
	TotalBytes int64 `json:"total_bytes"`
}

// MutationManifest 一次 Action 的实际文件变更（以文件系统 diff 为准，采信事实而非模型声明）。
type MutationManifest struct {
	Added       []string          `json:"added"`
	Modified    []string          `json:"modified"`
	Deleted     []string          `json:"deleted"`
	ModeChanged []string          `json:"mode_changed"`
	Semantic    []string          `json:"semantic_changes,omitempty"`
	BeforeID    string            `json:"before_checkpoint"`
	AfterID     string            `json:"after_checkpoint"`
	ContentHash map[string]string `json:"content_hashes,omitempty"`
	Excluded    []string          `json:"excluded,omitempty"`
	Degraded    bool              `json:"degraded,omitempty"`
	// ExternalDrift 是"快照后又被外部修改"的文件（恢复时不得覆盖）。
	ExternalDrift []string `json:"external_drift,omitempty"`
}

// AllPaths 返回 manifest 中所有变更路径（有序去重）。
func (m *MutationManifest) AllPaths() []string {
	if m == nil {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(m.Added)+len(m.Modified)+len(m.Deleted)+len(m.ModeChanged))
	for _, group := range [][]string{m.Added, m.Modified, m.Deleted, m.ModeChanged} {
		for _, p := range group {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Empty 判定本次 Action 是否零文件变更。
func (m *MutationManifest) Empty() bool {
	return m == nil || len(m.AllPaths()) == 0
}

// Store 内容寻址检查点存储。
type Store struct {
	dir      string
	excludes []string
	maxFiles int
}

// Option 构造选项。
type Option func(*Store)

// WithExcludes 追加排除项（除默认项之外）。
func WithExcludes(patterns ...string) Option {
	return func(s *Store) { s.excludes = append(s.excludes, patterns...) }
}

// WithMaxFiles 设置单次快照文件数上限（<=0 用默认 20000）。
func WithMaxFiles(n int) Option {
	return func(s *Store) {
		if n > 0 {
			s.maxFiles = n
		}
	}
}

// NewStore 构造检查点存储。dir 为空时返回 nil（调用方按"未启用"处理）。
func NewStore(dir string, opts ...Option) *Store {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	s := &Store{
		dir:      dir,
		excludes: append([]string{}, defaultExcludes...),
		maxFiles: defaultMaxFiles,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s
}

// Dir 暴露存储目录（审计/报表用）。
func (s *Store) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

// Excluded 返回当前排除清单（写入快照便于审计）。
func (s *Store) Excluded() []string {
	if s == nil {
		return nil
	}
	return append([]string{}, s.excludes...)
}

func (s *Store) blobPath(hash string) string {
	if len(hash) < 2 {
		return filepath.Join(s.dir, "blobs", hash)
	}
	return filepath.Join(s.dir, "blobs", hash[:2], hash)
}

func (s *Store) indexPath(id string) string {
	return filepath.Join(s.dir, "index", id+".json")
}

func isExcluded(rel string, excludes []string) bool {
	parts := strings.Split(rel, "/")
	for _, ex := range excludes {
		if ex == "" {
			continue
		}
		for _, p := range parts {
			if p == ex {
				return true
			}
		}
		if strings.HasPrefix(rel, ex+"/") {
			return true
		}
	}
	return false
}

// Capture 对 root 做一次快照：写入 blob 存储 + 索引 JSON。
//
// 行为约定：
//   - 包含 dirty 与 untracked（不做 git 状态过滤，直接扫盘）；
//   - 二进制按原始字节存 blob（不做行尾/编码转换）；
//   - 符号链接只记录目标文本，不跟随（防止越界把工作区外文件纳入快照）；
//   - 超 maxFiles 时返回 degraded 快照 + 明确 reason（不静默声称"完整快照"）；
//   - 目录不可读时返回 degraded + reason（不中断快照，但要能被上层看见）。
func (s *Store) Capture(root string) (*Snapshot, error) {
	if s == nil {
		return nil, errors.New("checkpoint: store not configured")
	}
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("checkpoint: root required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: resolve root: %w", err)
	}
	snap := &Snapshot{
		Root:      filepath.ToSlash(abs),
		Files:     map[string]FileEntry{},
		Excluded:  s.Excluded(),
		CreatedAt: time.Now(),
	}
	var degradedReasons []string
	walkErr := filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			degradedReasons = append(degradedReasons, "walk: "+err.Error())
			return nil // 单点不可读不中断整次快照
		}
		rel, relErr := filepath.Rel(abs, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if isExcluded(rel, s.excludes) {
				return fs.SkipDir
			}
			return nil
		}
		if isExcluded(rel, s.excludes) {
			return nil
		}
		if len(snap.Files) >= s.maxFiles {
			degradedReasons = append(degradedReasons, fmt.Sprintf("file limit reached (%d)", s.maxFiles))
			return fs.SkipAll
		}
		entry, size, entryErr := s.storeFile(abs, rel, path, d)
		if entryErr != nil {
			degradedReasons = append(degradedReasons, rel+": "+entryErr.Error())
			return nil
		}
		snap.Files[rel] = entry
		snap.TotalBytes += size
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("checkpoint: walk %s: %w", abs, walkErr)
	}
	if len(degradedReasons) > 0 {
		snap.Degraded = true
		snap.Reason = strings.Join(degradedReasons, "; ")
	}
	id, err := s.writeIndex(snap)
	if err != nil {
		return nil, err
	}
	snap.ID = id
	return snap, nil
}

// storeFile 把一个文件写入 blob 并返回指纹（size 为文件字节数）。
func (s *Store) storeFile(root, rel, path string, d fs.DirEntry) (FileEntry, int64, error) {
	info, err := d.Info()
	if err != nil {
		return FileEntry{}, 0, err
	}
	// 符号链接：只记录目标文本（不跟随，避免把工作区外文件纳入快照）。
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return FileEntry{}, 0, err
		}
		sum := sha256.Sum256([]byte(target))
		hash := hex.EncodeToString(sum[:])
		if err := s.writeBlob(hash, []byte(target)); err != nil {
			return FileEntry{}, 0, err
		}
		return FileEntry{Hash: hash, Size: int64(len(target)), Mode: uint32(info.Mode().Perm()), Symlink: true}, int64(len(target)), nil
	}
	if !info.Mode().IsRegular() {
		// FIFO/设备文件等不纳入快照（记 degraded 由调用层决定是否继续）。
		return FileEntry{}, 0, fmt.Errorf("unsupported file type %s", info.Mode().Type())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return FileEntry{}, 0, err
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if err := s.writeBlob(hash, data); err != nil {
		return FileEntry{}, 0, err
	}
	return FileEntry{Hash: hash, Size: int64(len(data)), Mode: uint32(info.Mode().Perm())}, int64(len(data)), nil
}

// writeBlob 内容寻址写 blob（同 hash 只写一次）。
func (s *Store) writeBlob(hash string, data []byte) error {
	path := s.blobPath(hash)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("checkpoint: mkdir blob: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("checkpoint: write blob: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("checkpoint: commit blob: %w", err)
	}
	return nil
}

// writeIndex 写索引 JSON，返回快照 ID。
// ID 只由内容（root + 文件指纹 + 排除清单）决定，不含 CreatedAt/字节总量等可变量：
// 同一工作区重复快照必须得到同一 ID（方便审计去重与"快照没变就不落盘"）。
func (s *Store) writeIndex(snap *Snapshot) (string, error) {
	idPayload, err := json.Marshal(struct {
		Root     string               `json:"root"`
		Files    map[string]FileEntry `json:"files"`
		Excluded []string             `json:"excluded"`
	}{Root: snap.Root, Files: snap.Files, Excluded: snap.Excluded})
	if err != nil {
		return "", fmt.Errorf("checkpoint: marshal index id: %w", err)
	}
	sum := sha256.Sum256(idPayload)
	id := hex.EncodeToString(sum[:])[:24]
	// 先把 ID 回填到快照再落盘：读回的索引必须自带 ID（否则 Load 后 ID 为空）。
	snap.ID = id
	payload, err := json.Marshal(snap)
	if err != nil {
		return "", fmt.Errorf("checkpoint: marshal index: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(s.dir, "index"), 0o755); err != nil {
		return "", fmt.Errorf("checkpoint: mkdir index: %w", err)
	}
	path := s.indexPath(id)
	if _, err := os.Stat(path); err == nil {
		return id, nil
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		return "", fmt.Errorf("checkpoint: write index: %w", err)
	}
	return id, nil
}

// Load 读取已落盘的快照索引。
func (s *Store) Load(id string) (*Snapshot, error) {
	if s == nil {
		return nil, errors.New("checkpoint: store not configured")
	}
	data, err := os.ReadFile(s.indexPath(id))
	if err != nil {
		return nil, fmt.Errorf("checkpoint: load %s: %w", id, err)
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("checkpoint: decode %s: %w", id, err)
	}
	return &snap, nil
}

// Diff 计算两个快照之间的文件级变更（before 可为 nil，表示全新增）。
func (s *Store) Diff(before, after *Snapshot) *MutationManifest {
	m := &MutationManifest{ContentHash: map[string]string{}}
	if after == nil {
		return m
	}
	m.AfterID = after.ID
	m.Degraded = after.Degraded
	m.Excluded = after.Excluded
	if before != nil {
		m.BeforeID = before.ID
	}
	beforeFiles := map[string]FileEntry{}
	if before != nil {
		beforeFiles = before.Files
	}
	for path, a := range after.Files {
		b, ok := beforeFiles[path]
		if !ok {
			m.Added = append(m.Added, path)
			m.ContentHash[path] = a.Hash
			continue
		}
		if a.Hash != b.Hash || a.Symlink != b.Symlink {
			m.Modified = append(m.Modified, path)
			m.ContentHash[path] = a.Hash
		}
		if a.Mode != b.Mode {
			m.ModeChanged = append(m.ModeChanged, path)
		}
	}
	for path := range beforeFiles {
		if _, ok := after.Files[path]; !ok {
			m.Deleted = append(m.Deleted, path)
		}
	}
	sort.Strings(m.Added)
	sort.Strings(m.Modified)
	sort.Strings(m.Deleted)
	sort.Strings(m.ModeChanged)
	return m
}

// RestoreReport 回滚结果（供"Restore Files / Restore Task"审计）。
type RestoreReport struct {
	Restored    []string `json:"restored"`
	Skipped     []string `json:"skipped"`
	Drifted     []string `json:"drifted"`
	Failed      []string `json:"failed"`
	Unavailable []string `json:"unavailable"`
}

// RestoreOptions 回滚选项。
type RestoreOptions struct {
	// Only 限定只回滚这些路径（空=manifest 全部）。用于"只回退本 Action 的变化"。
	Only []string
	// DetectDrift 检测"快照 after 之后又被外部修改"的文件并跳过（默认开启）。
	DetectDrift bool
	// DryRun 只计算结果不落盘（shadow 模式/审计用）。
	DryRun bool
}

// Restore 把 before 快照中的内容选择性恢复到工作区。
//
// 语义（I-EXEC-4）：
//   - 只处理 before 相对 after 的差异路径，绝不整目录覆盖；
//   - 恢复前比对当前磁盘内容与 after 快照：不一致 = 外部漂移，跳过并计入 Drifted
//     （不得覆盖用户/其他 Agent 的新修改）；
//   - 删除的文件按 before 的 blob 重建，新增的文件删除，权限位按 before 恢复。
func (s *Store) Restore(root string, before, after *Snapshot, opts RestoreOptions) (RestoreReport, error) {
	var rep RestoreReport
	if s == nil {
		return rep, errors.New("checkpoint: store not configured")
	}
	if before == nil {
		return rep, errors.New("checkpoint: before snapshot required")
	}
	root = strings.TrimSpace(root)
	if root == "" {
		return rep, errors.New("checkpoint: root required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return rep, fmt.Errorf("checkpoint: resolve root: %w", err)
	}
	manifest := s.Diff(before, after)
	targets := manifest.AllPaths()
	if len(opts.Only) > 0 {
		only := map[string]bool{}
		for _, p := range opts.Only {
			only[filepath.ToSlash(filepath.Clean(p))] = true
		}
		filtered := targets[:0]
		for _, p := range targets {
			if only[p] {
				filtered = append(filtered, p)
			}
		}
		targets = filtered
	}
	detectDrift := opts.DetectDrift
	afterFiles := map[string]FileEntry{}
	if after != nil {
		afterFiles = after.Files
	}
	for _, rel := range targets {
		path := filepath.Join(abs, filepath.FromSlash(rel))
		want, inBefore := before.Files[rel]
		cur, inAfter := afterFiles[rel]
		// 外部漂移检测：当前磁盘内容与 after 指纹不一致（且该文件在 after 中存在）→ 跳过。
		if detectDrift {
			if curInfo, statErr := os.Lstat(path); statErr == nil {
				actualHash, hashErr := hashFile(path, curInfo)
				if hashErr == nil && inAfter && actualHash != cur.Hash {
					rep.Drifted = append(rep.Drifted, rel)
					continue
				}
				if !inAfter && statErr == nil {
					// after 中不存在（本次新增后又出现）：视为外部漂移。
					rep.Drifted = append(rep.Drifted, rel)
					continue
				}
			}
		}
		if !inBefore {
			// 本 Action 新增 → 删除（DryRun 只登记）。
			if opts.DryRun {
				rep.Restored = append(rep.Restored, rel)
				continue
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				rep.Failed = append(rep.Failed, rel)
				continue
			}
			rep.Restored = append(rep.Restored, rel)
			continue
		}
		// 存在于 before → 恢复内容（含删除后重建）。
		data, readErr := os.ReadFile(s.blobPath(want.Hash))
		if readErr != nil {
			rep.Unavailable = append(rep.Unavailable, rel)
			continue
		}
		if opts.DryRun {
			rep.Restored = append(rep.Restored, rel)
			continue
		}
		if want.Symlink {
			_ = os.Remove(path)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				rep.Failed = append(rep.Failed, rel)
				continue
			}
			if err := os.Symlink(string(data), path); err != nil {
				rep.Failed = append(rep.Failed, rel)
				continue
			}
			rep.Restored = append(rep.Restored, rel)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			rep.Failed = append(rep.Failed, rel)
			continue
		}
		mode := fs.FileMode(want.Mode)
		if mode == 0 {
			mode = 0o644
		}
		if err := os.WriteFile(path, data, mode); err != nil {
			rep.Failed = append(rep.Failed, rel)
			continue
		}
		// 写回后按 owner 期望恢复权限位（WriteFile 会按 umask 修正）。
		_ = os.Chmod(path, mode)
		rep.Restored = append(rep.Restored, rel)
	}
	sort.Strings(rep.Restored)
	sort.Strings(rep.Drifted)
	sort.Strings(rep.Failed)
	sort.Strings(rep.Unavailable)
	return rep, nil
}

// hashFile 计算当前磁盘文件指纹（与 Capture 同口径）。
func hashFile(path string, info fs.FileInfo) (string, error) {
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256([]byte(target))
		return hex.EncodeToString(sum[:]), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
