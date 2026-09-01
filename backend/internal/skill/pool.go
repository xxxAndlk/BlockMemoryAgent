// Package skill 管理 Skill 池的加载与查询。
//
// 技能来源三类，启动时合并进同一个 Pool：
//   - skills.yaml（工具别名 skill，ToolRef 绑定已有工具）
//   - 主流 Agent 工具约定目录的 SKILL.md（.claude/.codex/.agents/.cursor/.gemini/.agent）
//   - 插件 bundle 注入（plugins/manager Reload）
//
// 内容按渐进披露下发：仅元数据（Name+Description）经 MetadataBlock 进
// 系统提示，正文（Content）经 load_skill 工具按需获取，整池永不注入上下文。
package skill

import (
	"fmt"
	"os"
	"sort"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// Pool 全局 Skill 池
//
// 职责：以 skill_id 为键保存所有可用 Skill，提供注册/查询/名称匹配能力。
// 并发安全：所有读写均经 mu 保护。
type Pool struct {
	mu     sync.RWMutex            // 读写锁：保护 skills map
	skills map[string]*types.Skill // skill_id -> skill
}

// NewPool 创建空 Skill 池
func NewPool() *Pool {
	return &Pool{skills: make(map[string]*types.Skill)}
}

// NewPoolFromSkills 从已有 Skill 列表构建（常用于启动加载）；
// nil 项与缺 SkillID 的项会被 Register 忽略。
func NewPoolFromSkills(skills []*types.Skill) *Pool {
	p := NewPool()
	for _, s := range skills {
		p.Register(s)
	}
	return p
}

// Register 注册一个 Skill；同 ID 覆盖。nil 或 SkillID 为空时静默忽略。
func (p *Pool) Register(s *types.Skill) {
	if s == nil || s.SkillID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.skills[s.SkillID] = s
}

// Get 按 SkillID 取出 Skill；未命中返回 nil。
func (p *Pool) Get(id string) *types.Skill {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.skills[id]
}

// FindByNameOrID 按 SkillID 精确匹配，未命中再按 Name 精确匹配。
// 用于派发参数与角色固定集的名称解析；未命中返回 nil。
func (p *Pool) FindByNameOrID(key string) *types.Skill {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if s, ok := p.skills[key]; ok {
		return s
	}
	for _, s := range p.skills {
		if s.Name == key {
			return s
		}
	}
	return nil
}

// Remove 摘除一个 Skill；不存在时静默（幂等）。
// 供 bundle 插件包 reload 时清理消失目录的 skill。
func (p *Pool) Remove(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.skills, id)
}

// All 返回当前所有 Skill（指针切片共享，调用方不应修改 Skill 字段）。
func (p *Pool) All() []*types.Skill {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*types.Skill, 0, len(p.skills))
	for _, s := range p.skills {
		out = append(out, s)
	}
	return out
}

// Names 返回池内全部 Skill 的 Name（缺 Name 的回退 SkillID），按字典序排序
// 保证输出稳定（提示缓存友好）。同名去重。
func (p *Pool) Names() []string {
	p.mu.RLock()
	seen := make(map[string]struct{}, len(p.skills))
	names := make([]string, 0, len(p.skills))
	for _, s := range p.skills {
		n := s.Name
		if n == "" {
			n = s.SkillID
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		names = append(names, n)
	}
	p.mu.RUnlock()
	sort.Strings(names)
	return names
}

// LoadFromYAML 从 YAML 文件加载 Skill（工具别名类），Source 标记为 "yaml"。
//
// 文件格式（顶层 skills 数组）：
//
//	skills:
//	  - skill_id: read_file
//	    name: 读取文件
//	    description: 读取本地文件返回文本
//	    domain: code,doc
//	    tool_ref: ReadFile
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
	for _, s := range raw.Skills {
		if s != nil && s.Source == "" {
			s.Source = "yaml"
		}
	}
	return NewPoolFromSkills(raw.Skills), nil
}

// BuiltinPool 返回内置 Skill 集（无外部配置时的开箱兜底），Source 标记为 "builtin"。
func BuiltinPool() *Pool {
	return NewPoolFromSkills([]*types.Skill{
		{SkillID: "read_file", Name: "读文件", Description: "读取本地文件并返回文本内容", Domain: "code,doc,*", ToolRef: "ReadFile", Tags: []string{"io", "fs"}, Source: "builtin"},
		{SkillID: "write_file", Name: "写文件", Description: "把文本内容写入本地文件", Domain: "code,doc,*", ToolRef: "WriteFile", Tags: []string{"io", "fs"}, Source: "builtin"},
		{SkillID: "list_dir", Name: "列目录", Description: "列出目录下文件与子目录", Domain: "code,*", ToolRef: "ListDir", Tags: []string{"io", "fs"}, Source: "builtin"},
		{SkillID: "run_command", Name: "执行命令", Description: "在沙箱内执行 shell 命令并返回输出", Domain: "code,ops,*", ToolRef: "RunCommand", Tags: []string{"shell"}, Source: "builtin"},
		{SkillID: "search_in_files", Name: "代码搜索", Description: "在工作目录中按关键词搜索文件内容", Domain: "code,doc,*", ToolRef: "SearchInFiles", Tags: []string{"search"}, Source: "builtin"},
		{SkillID: "http_get", Name: "HTTP GET", Description: "对指定 URL 发起 GET 请求并返回 JSON/文本", Domain: "ops,api,*", ToolRef: "HTTPGet", Tags: []string{"http"}, Source: "builtin"},
		{SkillID: "http_post", Name: "HTTP POST", Description: "对指定 URL 发起 POST 请求（带 JSON Body）", Domain: "ops,api,*", ToolRef: "HTTPPost", Tags: []string{"http"}, Source: "builtin"},
	})
}
