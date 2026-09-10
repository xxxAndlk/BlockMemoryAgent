package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// models.json：模型注册表——LLM 连接参数（provider/model/api_key/base_url）与
// 角色绑定（role_bindings）的持久化文件，与 roles.yaml 同目录。
// 设计意图：连接参数与角色解耦。roles.yaml 角色以 model_ref 引用 models[].id；
// 换 key/换端点只改本文件，ModelFactory 通过 mtime 检查热更新（免重启）；
// 运行时切换角色模型（TUI /model、Web 选择器）持久化到 role_bindings。
// 本文件可手改：保存走临时文件+rename 原子写；程序外编辑后下次访问自动重载。

// RoleBinding 单个角色的模型绑定记录（运行时切换的持久化形态）。
type RoleBinding struct {
	// ModelID 绑定的模型条目 ID（models[].id）。
	ModelID string `json:"model_id"`
	// Thinking 思考强度覆盖（off/low/medium/high）；空 = 跟随角色 roles.yaml 配置。
	Thinking string `json:"thinking,omitempty"`
	// AppliedAt 应用时间（记录用，不参与逻辑）。
	AppliedAt *time.Time `json:"applied_at,omitempty"`
}

// ModelsRegistryFile models.json 的文件结构。
type ModelsRegistryFile struct {
	// Models 可选模型清单。
	Models []types.ModelEntry `json:"models"`
	// RoleBindings roleID → 绑定记录。roleID 取值与 ModelFactory 缓存键一致：
	// "meta"/"domain"/"lightweight" 或 fixed_roles[].id。
	RoleBindings map[string]RoleBinding `json:"role_bindings,omitempty"`
}

// LoadModelsRegistry 从指定路径加载模型注册表。
//
// 文件不存在返回 (nil, nil)（角色走 model_ref/内联配置时无影响，仅绑定不可用）；
// JSON 损坏或校验失败返回 error（model_ref 引用缺失会导致角色不可解析，应暴露）。
// model/api_key/base_url 过 resolveEnv 展开（${ENV} / ${ENV:"default"}）。
func LoadModelsRegistry(path string) (*ModelsRegistryFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read models registry %s: %w", path, err)
	}
	var f ModelsRegistryFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("unmarshal models registry %s: %w", path, err)
	}
	for i := range f.Models {
		e := &f.Models[i]
		e.Model = resolveEnv(e.Model)
		e.APIKey = resolveEnv(e.APIKey)
		e.BaseURL = resolveEnv(e.BaseURL)
	}
	if err := f.Validate(); err != nil {
		return nil, fmt.Errorf("invalid models registry %s: %w", path, err)
	}
	return &f, nil
}

// Validate 校验条目完整性：id 唯一非空、provider/model 非空。
// api_key 允许为空（切换到该条目时由 SwitchModel 拒绝并提示）。
func (f *ModelsRegistryFile) Validate() error {
	seen := make(map[string]bool, len(f.Models))
	for i, e := range f.Models {
		if e.ID == "" {
			return fmt.Errorf("models[%d]: id 为空", i)
		}
		if seen[e.ID] {
			return fmt.Errorf("models[%d]: id %q 重复", i, e.ID)
		}
		seen[e.ID] = true
		if e.Provider == "" {
			return fmt.Errorf("model %q: provider 为空", e.ID)
		}
		if e.Model == "" {
			return fmt.Errorf("model %q: model 为空", e.ID)
		}
		if e.MaxOutputTokens < 0 {
			return fmt.Errorf("model %q: max_output_tokens 为负 (%d)", e.ID, e.MaxOutputTokens)
		}
	}
	for roleID, b := range f.RoleBindings {
		if roleID == "" {
			return fmt.Errorf("role_bindings: 角色 ID 为空")
		}
		if b.ModelID == "" {
			return fmt.Errorf("role_bindings[%s]: model_id 为空", roleID)
		}
	}
	return nil
}

// Save 原子写入注册表文件（临时文件 + rename，Windows 下 rename 前先移除旧文件，0600）。
func (f *ModelsRegistryFile) Save(path string) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal models registry: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".models-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("chmod temp file: %w", err)
	}
	// Windows 的 os.Rename 不覆盖已存在文件，先移除旧文件。
	if _, err := os.Stat(path); err == nil {
		if err := os.Remove(path); err != nil {
			os.Remove(tmpName)
			return fmt.Errorf("remove old registry: %w", err)
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename models registry: %w", err)
	}
	return nil
}

// RegistryStore 注册表运行时持有者：内存数据 + mtime 热更新检查。
// 并发安全：全部方法走内部 RWMutex。由 bootstrap 构造注入 ModelFactory。
type RegistryStore struct {
	mu        sync.RWMutex
	path      string
	lastMod   time.Time
	lastSize  int64
	loaded    bool
	file      *ModelsRegistryFile
	bindings  map[string]RoleBinding
	byID      map[string]types.ModelEntry
}

// NewRegistryStore 构造并立即加载注册表。文件不存在时得到空注册表
// （model_ref 引用会在解析期报错，绑定操作会落盘创建文件）。
func NewRegistryStore(path string) (*RegistryStore, error) {
	s := &RegistryStore{path: path}
	if _, err := s.MaybeReload(); err != nil {
		return nil, err
	}
	return s, nil
}

// MaybeReload 检查文件 mtime/size，变化则重载并重建索引。
// 返回是否发生了重载。文件被删除视为清空（回到无注册表状态）。
func (s *RegistryStore) MaybeReload() (bool, error) {
	info, err := os.Stat(s.path)
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("stat models registry %s: %w", s.path, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exists := err == nil
	if s.loaded && exists && info.ModTime().Equal(s.lastMod) && info.Size() == s.lastSize {
		return false, nil
	}
	if s.loaded && !exists {
		// 文件被删除：清空（保持 loaded 语义，避免反复 stat 重载）。
		s.lastMod, s.lastSize = time.Time{}, 0
		s.file = &ModelsRegistryFile{}
		s.rebuildIndexLocked()
		return true, nil
	}
	file, err := LoadModelsRegistry(s.path)
	if err != nil {
		// 手改坏文件时不吞错：保留旧数据继续服务，向调用方报错。
		return false, err
	}
	if file == nil {
		file = &ModelsRegistryFile{}
	}
	if exists {
		s.lastMod, s.lastSize = info.ModTime(), info.Size()
	} else {
		s.lastMod, s.lastSize = time.Time{}, 0
	}
	s.loaded = true
	s.file = file
	s.rebuildIndexLocked()
	return true, nil
}

// rebuildIndexLocked 重建 byID 索引与 bindings 副本（调用方持写锁）。
func (s *RegistryStore) rebuildIndexLocked() {
	s.byID = make(map[string]types.ModelEntry, len(s.file.Models))
	for _, e := range s.file.Models {
		s.byID[e.ID] = e
	}
	s.bindings = make(map[string]RoleBinding, len(s.file.RoleBindings))
	for k, v := range s.file.RoleBindings {
		s.bindings[k] = v
	}
}

// ModelByID 查模型条目；未命中返回 false。
func (s *RegistryStore) ModelByID(id string) (types.ModelEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.byID[id]
	return e, ok
}

// Models 返回模型条目清单副本（按 ID 排序，稳定展示序）。
func (s *RegistryStore) Models() []types.ModelEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]types.ModelEntry, 0, len(s.file.Models))
	for _, e := range s.file.Models {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Binding 查角色绑定；未命中返回 false。
func (s *RegistryStore) Binding(roleID string) (RoleBinding, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.bindings[roleID]
	return b, ok
}

// Bindings 返回全部绑定副本。
func (s *RegistryStore) Bindings() map[string]RoleBinding {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]RoleBinding, len(s.bindings))
	for k, v := range s.bindings {
		out[k] = v
	}
	return out
}

// persistLocked 将内存态写回磁盘并刷新 mtime 基线（调用方持写锁）。
func (s *RegistryStore) persistLocked() error {
	if err := s.file.Save(s.path); err != nil {
		return err
	}
	if info, err := os.Stat(s.path); err == nil {
		s.lastMod, s.lastSize = info.ModTime(), info.Size()
	}
	s.loaded = true
	return nil
}

// SetBinding 写入/清除（modelID 为空串）角色绑定并落盘。
func (s *RegistryStore) SetBinding(roleID, modelID, thinking string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		s.file = &ModelsRegistryFile{}
	}
	if s.file.RoleBindings == nil {
		s.file.RoleBindings = make(map[string]RoleBinding)
	}
	if modelID == "" {
		delete(s.file.RoleBindings, roleID)
	} else {
		now := time.Now().UTC()
		s.file.RoleBindings[roleID] = RoleBinding{ModelID: modelID, Thinking: thinking, AppliedAt: &now}
	}
	if err := s.file.Validate(); err != nil {
		return fmt.Errorf("binding invalid, not saved: %w", err)
	}
	if err := s.persistLocked(); err != nil {
		return err
	}
	s.rebuildIndexLocked()
	return nil
}

// Add 新增模型条目并落盘；id 冲突报错。
func (s *RegistryStore) Add(entry types.ModelEntry) error {
	if entry.ID == "" || entry.Provider == "" || entry.Model == "" {
		return fmt.Errorf("新增模型需 id/provider/model 非空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		s.file = &ModelsRegistryFile{}
	}
	if _, ok := s.byID[entry.ID]; ok {
		return fmt.Errorf("模型 id %q 已存在", entry.ID)
	}
	s.file.Models = append(s.file.Models, entry)
	if err := s.file.Validate(); err != nil {
		// 回滚内存追加，避免坏状态。
		s.file.Models = s.file.Models[:len(s.file.Models)-1]
		return fmt.Errorf("新增模型校验失败: %w", err)
	}
	if err := s.persistLocked(); err != nil {
		s.file.Models = s.file.Models[:len(s.file.Models)-1]
		return err
	}
	s.rebuildIndexLocked()
	return nil
}
