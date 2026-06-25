package skill

import (
	"fmt" // 错误格式化
	"os"  // 读取 yaml 文件
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// Registry 持有 Pool 与 Agent->SkillSet 装配映射
//
// 职责：作为 Skill 池（只读持有）与各 Agent 实例 SkillSet 之间的中间层，
// 负责记录"哪个 Agent 当前装配了哪些 Skill"，并支持运行时扩展。
// 并发安全：owned map 经 mu 读写锁保护；pool 视为只读共享。
// 副作用：Bind/AddSkillToAgent 会修改 owned map 与 SkillSet.Skills。
type Registry struct {
	pool *Pool // 全局 Skill 池，只读引用，用于按 ID 取 Skill

	mu    sync.RWMutex
	owned map[string]*types.SkillSet // owner agent instance id -> skill set
}

// NewRegistry 创建 Skill 注册表
//
// 参数 pool：底层全局 Skill 池，后续 Bind 追加新 Skill 时会从中按 ID 取回。
// 返回：已初始化 owned map 的 *Registry。
func NewRegistry(pool *Pool) *Registry {
	return &Registry{
		pool:  pool,
		owned: make(map[string]*types.SkillSet),
	}
}

// Pool 暴露底层池（仅读）
//
// 返回：Registry 持有的 *Pool，调用方可用其做 FilterByDomain/Get 等只读操作。
// 注意：返回原始指针而非拷贝，调用方不应直接修改池内容。
func (r *Registry) Pool() *Pool { return r.pool }

// Bind 将 SkillSet 绑定到某 Agent 实例
//
// 参数 set：待绑定的 SkillSet；nil 或 OwnerAgent 为空时静默忽略。
// 副作用：以 OwnerAgent 为键写入 owned map，同 Agent 重复绑定会覆盖旧 SkillSet。
// 并发安全：加写锁。
// 设计意图：在 Agent 构造阶段完成 SkillSet 与 Agent 实例的关联，
// 后续 GetForAgent/AddSkillToAgent 均以此为入口。
func (r *Registry) Bind(set *types.SkillSet) {
	if set == nil || set.OwnerAgent == "" {
		return // 防御：无 OwnerAgent 无法建立映射
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.owned[set.OwnerAgent] = set // 同 Agent 覆盖，保证最新装配生效
}

// GetForAgent 取出指定 Agent 已装配的 SkillSet
//
// 参数 agentID：OwnerAgent 实例 ID。
// 返回：命中的 *types.SkillSet；未绑定返回 nil。
// 并发安全：加读锁。
func (r *Registry) GetForAgent(agentID string) *types.SkillSet {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.owned[agentID]
}

// AddSkillToAgent 在已存在的 SkillSet 中追加（v3 §5.3 扩展技能）
//
// 参数：
//   - agentID：目标 Agent 实例 ID
//   - skillID：待追加 Skill 的 ID
//
// 返回：成功追加返回 true；Agent 未绑定、Skill 不存在、或已存在该 Skill 时
// 分别返回 false/true（已存在视为幂等成功）。
// 副作用：命中后向 set.Skills 追加 *types.Skill 指针。
// 并发安全：加写锁，避免与 GetForAgent 并发读冲突。
// 设计意图：为"扩展技能"分支提供运行时增量入口，无需重建整个 SkillSet。
func (r *Registry) AddSkillToAgent(agentID, skillID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.owned[agentID]
	if set == nil {
		return false // Agent 尚未绑定 SkillSet，无法追加
	}
	// 幂等检查：Skill 已在 set 中则直接返回成功，避免重复注入
	for _, s := range set.Skills {
		if s.SkillID == skillID {
			return true
		}
	}
	sk := r.pool.Get(skillID) // 从全局池按 ID 取 Skill 定义
	if sk == nil {
		return false // 池中无此 Skill，扩展失败
	}
	set.Skills = append(set.Skills, sk) // 追加到 Agent 的 SkillSet
	return true
}

// LoadFromYAML 从 YAML 文件加载 Skill 池
//
// 参数 path：yaml 文件路径。
// 返回：由文件内容构建的 *Pool；读取或解析失败时返回包装后的 error。
// 副作用：仅读文件，不写；不修改任何全局状态。
// 并发安全：无共享状态访问。
//
// 文件格式：
//
//	skills:
//	  - skill_id: read_file
//	    name: 读取文件
//	    description: 读取本地文件返回文本
//	    domain: code,doc
//	    tool_ref: ReadFile
//	    tags: [io,fs]
func LoadFromYAML(path string) (*Pool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read skill yaml: %w", err)
	}
	var raw struct {
		Skills []*types.Skill `yaml:"skills"` // 仅解析顶层 skills 数组
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse skill yaml: %w", err)
	}
	return NewPoolFromSkills(raw.Skills), nil // 复用 NewPoolFromSkills 完成逐个注册
}

// BuiltinPool 返回内置 Skill 集（覆盖当前工具执行器与常用动作）
//
// 返回：含读写文件、列目录、执行命令、搜索、HTTP GET/POST 等基础 Skill 的 *Pool。
// 副作用：每次调用都新建 Pool，调用方可自由修改不影响后续调用。
// 并发安全：无共享状态。
//
// 没有外部 Skill yaml 时，框架退回到此默认集合，保证开箱可用。
func BuiltinPool() *Pool {
	return NewPoolFromSkills([]*types.Skill{
		{SkillID: "read_file", Name: "读文件", Description: "读取本地文件并返回文本内容", Domain: "code,doc,*", ToolRef: "ReadFile", Tags: []string{"io", "fs"}},
		{SkillID: "write_file", Name: "写文件", Description: "把文本内容写入本地文件", Domain: "code,doc,*", ToolRef: "WriteFile", Tags: []string{"io", "fs"}},
		{SkillID: "list_dir", Name: "列目录", Description: "列出目录下文件与子目录", Domain: "code,*", ToolRef: "ListDir", Tags: []string{"io", "fs"}},
		{SkillID: "run_command", Name: "执行命令", Description: "在沙箱内执行 shell 命令并返回输出", Domain: "code,ops,*", ToolRef: "RunCommand", Tags: []string{"shell"}},
		{SkillID: "search_in_files", Name: "代码搜索", Description: "在工作目录中按关键词搜索文件内容", Domain: "code,doc,*", ToolRef: "SearchInFiles", Tags: []string{"search"}},
		{SkillID: "http_get", Name: "HTTP GET", Description: "对指定 URL 发起 GET 请求并返回 JSON/文本", Domain: "ops,api,*", ToolRef: "HTTPGet", Tags: []string{"http"}},
		{SkillID: "http_post", Name: "HTTP POST", Description: "对指定 URL 发起 POST 请求（带 JSON Body）", Domain: "ops,api,*", ToolRef: "HTTPPost", Tags: []string{"http"}},
	})
}
