package userprofile

// project_store.go 项目偏好存储（2026-09-02 设计 §5，S2 每会话工作目录）：
// 构造时只记录 workDir 根，目标文件 <workDir>/.bma/project_preferences.md 在
// 读写方法内经 tool.WorkDirFromContext 按 ctx 会话目录解析；ctx 未注入时回落构造目录。
//
// 每个解析路径缓存一个 *Store（首次访问时 Load），读写语义（RCU 快照/人工行保护/
// Merge 归档）与用户画像完全一致，不复制代码。

import (
	"context"
	"path/filepath"
	"sync"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// ProjectStore 项目偏好存储：按 ctx 会话目录解析 project_preferences.md。
type ProjectStore struct {
	workDir string // 构造工作目录（ctx 未注入会话目录时的回落）
	mu      sync.Mutex
	stores  map[string]*Store // 解析路径 -> 该路径的 Store（懒加载缓存）
}

// NewProjectStore 创建项目偏好存储（per workDir .bma/project_preferences.md）。
// 仅记录 workDir 根，不触发 IO；调用方需显式 Load（缺失文件容忍）。
func NewProjectStore(workDir string) *ProjectStore {
	return &ProjectStore{workDir: workDir, stores: make(map[string]*Store)}
}

// pathFor 解析项目偏好文件路径：ctx 会话目录优先。
func (s *ProjectStore) pathFor(ctx context.Context) string {
	wd := s.workDir
	if v := tool.WorkDirFromContext(ctx); v != "" {
		wd = v
	}
	return filepath.Join(wd, ".bma", "project_preferences.md")
}

// storeFor 返回 ctx 解析路径对应的 *Store：首次访问创建并 Load（缺失容忍），
// 之后命中缓存保持 RCU 读语义。解析/加载失败返回错误（不落缓存，下次重试）。
func (s *ProjectStore) storeFor(ctx context.Context) (*Store, error) {
	path := s.pathFor(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.stores[path]; ok {
		return st, nil
	}
	st := newStore(path, ProjectTemplate, ProjectArchiveSection, defaultArchiveCap)
	if err := st.Load(); err != nil {
		return nil, err
	}
	s.stores[path] = st
	return st, nil
}

// ForContext 返回 ctx 解析路径对应的底层 *Store（供 Merge 整理等复用 Store 语义的流程）。
// 解析/加载失败返回 nil。
func (s *ProjectStore) ForContext(ctx context.Context) *Store {
	st, err := s.storeFor(ctx)
	if err != nil {
		return nil
	}
	return st
}

// Load 读取 ctx 解析路径的偏好文件并发布快照（缺失容忍，见 Store.Load）。
func (s *ProjectStore) Load(ctx context.Context) error {
	_, err := s.storeFor(ctx)
	return err
}

// Reload 显式重载 ctx 解析路径的偏好文件（用户编辑后热更新）。
func (s *ProjectStore) Reload(ctx context.Context) error {
	st, err := s.storeFor(ctx)
	if err != nil {
		return err
	}
	return st.Reload()
}

// Current 返回 ctx 解析路径的当前快照；未加载/加载失败返回空画像（非 nil）。
func (s *ProjectStore) Current(ctx context.Context) *Profile {
	st, err := s.storeFor(ctx)
	if err != nil {
		return &Profile{Path: s.pathFor(ctx)}
	}
	return st.Current()
}

// Append 结构化追加一条项目偏好记录（语义同 Store.Append）。
func (s *ProjectStore) Append(ctx context.Context, section, text string) error {
	st, err := s.storeFor(ctx)
	if err != nil {
		return err
	}
	return st.Append(section, text)
}

// Save 全量覆盖 ctx 解析路径的项目偏好（HTTP PUT 用户手动编辑）。
func (s *ProjectStore) Save(ctx context.Context, content string) error {
	st, err := s.storeFor(ctx)
	if err != nil {
		return err
	}
	return st.Save(content)
}

// MergeView 返回 ctx 解析路径目标小节的自动行视图（语义同 Store.MergeView）。
func (s *ProjectStore) MergeView(ctx context.Context, targets []string) MergeView {
	st, err := s.storeFor(ctx)
	if err != nil {
		return MergeView{}
	}
	return st.MergeView(targets)
}

// ApplyMerge 应用一次合并计划到 ctx 解析路径（语义同 Store.ApplyMerge）。
func (s *ProjectStore) ApplyMerge(ctx context.Context, plan MergePlan, targets []string) error {
	st, err := s.storeFor(ctx)
	if err != nil {
		return err
	}
	return st.ApplyMerge(plan, targets)
}

// ArchiveSection 返回归档小节名（与路径无关，无需 ctx）。
func (s *ProjectStore) ArchiveSection() string { return ProjectArchiveSection }
