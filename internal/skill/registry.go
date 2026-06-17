package skill

import (
	"fmt"
	"os"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/blockmemory/agent/pkg/types"
)

// Registry 持有 Pool 与 Agent->SkillSet 装配映射
type Registry struct {
	pool *Pool

	mu    sync.RWMutex
	owned map[string]*types.SkillSet // owner agent instance id -> skill set
}

// NewRegistry 创建 Skill 注册表
func NewRegistry(pool *Pool) *Registry {
	return &Registry{
		pool:  pool,
		owned: make(map[string]*types.SkillSet),
	}
}

// Pool 暴露底层池（仅读）
func (r *Registry) Pool() *Pool { return r.pool }

// Bind 将 SkillSet 绑定到某 Agent 实例
func (r *Registry) Bind(set *types.SkillSet) {
	if set == nil || set.OwnerAgent == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.owned[set.OwnerAgent] = set
}

// GetForAgent 取出指定 Agent 已装配的 SkillSet
func (r *Registry) GetForAgent(agentID string) *types.SkillSet {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.owned[agentID]
}

// AddSkillToAgent 在已存在的 SkillSet 中追加（v3 §5.3 扩展技能）
func (r *Registry) AddSkillToAgent(agentID, skillID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.owned[agentID]
	if set == nil {
		return false
	}
	for _, s := range set.Skills {
		if s.SkillID == skillID {
			return true
		}
	}
	sk := r.pool.Get(skillID)
	if sk == nil {
		return false
	}
	set.Skills = append(set.Skills, sk)
	return true
}

// LoadFromYAML 从 YAML 文件加载 Skill 池
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
		Skills []*types.Skill `yaml:"skills"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse skill yaml: %w", err)
	}
	return NewPoolFromSkills(raw.Skills), nil
}

// BuiltinPool 返回内置 Skill 集（覆盖当前工具执行器与常用动作）
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
