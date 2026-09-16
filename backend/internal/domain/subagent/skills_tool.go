package subagent

// skills_tool.go 实现 list_skills / load_skill 工具（技能渐进披露）：
//   - list_skills：列出调用方持有集（meta = 全池）的名称+一句话描述目录；
//   - load_skill：返回指定技能的全文（SKILL.md 正文），供按需展开。
//
// 范围判定与派发分发共用 heldSkills 权威持有集：子 Agent 只能 load/list
// 自己持有的技能（角色固定集 ∪ 父派发 skills 参数下放集），防止越权取用。
// skillPool 未注入（nil）时两工具返回"技能未启用"提示，零副作用。

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/skill"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/google/jsonschema-go/jsonschema"
)

// RegisterSkillTools 将 list_skills / load_skill 工具安装到传入的工具注册表。
// 角色可见性由 role.Registry 的 meta/domain 内置工具白名单 + roles.yaml tools 控制；
// skillPool 未注入（WithSkillPool 未调用）时工具仍注册但执行时提示未启用，
// 保持角色白名单与工具注册解耦（测试可不注入池）。
func (d *Dispatcher) RegisterSkillTools(r *tool.Registry) {
	r.Register(&listSkillsTool{dispatcher: d})
	r.Register(&loadSkillTool{dispatcher: d})
}

// listSkillsTool 实现 list_skills 工具。
type listSkillsTool struct {
	dispatcher *Dispatcher
}

// Name 返回工具名称。
func (t *listSkillsTool) Name() string { return "list_skills" }

// Aliases 返回工具别名列表，当前无别名。
func (t *listSkillsTool) Aliases() []string { return nil }

// Description 返回 LLM 可见的工具描述。
func (t *listSkillsTool) Description() string {
	return "列出你可用的技能（名称 + 一句话描述）。你系统提示中的【可用技能】块即本工具的范围；" +
		"对某个技能需要详细操作指引时，用 load_skill(名称) 获取全文。" +
		"技能多时用 query 关键词过滤（匹配名称/标题/描述）；经验技能（自进化沉淀）系统提示只列常用若干，" +
		"剩余用本工具检索。" +
		"派发子 Agent 时可把其中技能经 call_sub_agent 的 skills 参数下放给子 Agent。"
}

// listSkillsMaxOutput 单次 list_skills 输出的技能条数上限（超限只列前 N 并提示用 query 过滤），
// 防技能库膨胀后工具输出灌爆上下文。
const listSkillsMaxOutput = 40

// InputSchema 返回工具入参 JSON Schema：可选 query 关键词过滤。
func (t *listSkillsTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"query": {Type: "string", Description: "可选关键词：按名称/标题/描述过滤技能（大小写不敏感子串匹配）"},
		},
	}
}

// Execute 执行 list_skills：渲染调用方持有集的元数据目录（可 query 过滤 + 输出条数封顶）。
func (t *listSkillsTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	d := t.dispatcher
	if d.skillPool == nil {
		return &tool.Result{Tool: "list_skills", Success: true, Output: "技能功能未启用（未注入技能池）。"}
	}
	agentID := agent.AgentIDFromContext(ctx)
	if agentID == "" {
		return &tool.Result{Tool: "list_skills", Error: "missing caller agent context"}
	}
	// meta 持全池（可派发任意技能）；其他 Agent 只列自己持有集。
	var names []string
	if roleIDFromAgentID(agentID) == "meta" {
		names = d.skillPool.Names()
	} else if v, ok := d.heldSkills.Load(agentID); ok {
		names = v.([]string)
	} else {
		if roleDef := d.registry.Get(roleIDFromAgentID(agentID)); roleDef != nil {
			names = fixedSkillNames(d.skillPool, roleDef)
		}
	}
	query, _ := args["query"].(string)
	query = strings.ToLower(strings.TrimSpace(query))
	total := 0
	var kept []string
	for _, n := range names {
		s := d.skillPool.FindByNameOrID(n)
		if s == nil {
			continue
		}
		if query != "" && !skillMatchesQuery(s, query) {
			continue
		}
		total++
		if len(kept) < listSkillsMaxOutput {
			kept = append(kept, n)
		}
	}
	block := skill.MetadataBlock(d.skillPool, kept)
	switch {
	case block == "" && query != "":
		return &tool.Result{Tool: "list_skills", Success: true, Output: fmt.Sprintf("没有匹配 %q 的技能（可省略 query 查看全部）。", query)}
	case block == "":
		return &tool.Result{Tool: "list_skills", Success: true, Output: "你当前没有持有任何技能。"}
	case total > len(kept):
		return &tool.Result{Tool: "list_skills", Success: true, Output: fmt.Sprintf("%s\n（共 %d 个匹配，只列前 %d 个；用 query 关键词缩小范围。）", block, total, len(kept))}
	}
	return &tool.Result{Tool: "list_skills", Success: true, Output: block}
}

// skillMatchesQuery 大小写不敏感子串匹配技能名称/标题/描述（query 需已小写）。
func skillMatchesQuery(s *types.Skill, query string) bool {
	hay := strings.ToLower(s.Name + "\n" + s.SkillID + "\n" + s.Description)
	return strings.Contains(hay, query)
}

// loadSkillTool 实现 load_skill 工具。
type loadSkillTool struct {
	dispatcher *Dispatcher
}

// Name 返回工具名称。
func (t *loadSkillTool) Name() string { return "load_skill" }

// Aliases 返回工具别名列表，当前无别名。
func (t *loadSkillTool) Aliases() []string { return nil }

// Description 返回 LLM 可见的工具描述。
func (t *loadSkillTool) Description() string {
	return "获取指定技能的完整操作指引（SKILL.md 正文）。参数 name 填【可用技能】块或 list_skills " +
		"输出中的技能名称。只能加载你持有的技能（系统提示【可用技能】块列出的范围）；" +
		"未持有或不存在的技能会返回错误。工具别名类技能（无正文）返回其使用示例。"
}

// InputSchema 返回工具入参 JSON Schema：name（必填字符串）。
func (t *loadSkillTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"name": {Type: "string", Description: "技能名称（来自【可用技能】块或 list_skills）"},
		},
		Required: []string{"name"},
	}
}

// Execute 执行 load_skill：按名称解析技能并返回正文。
func (t *loadSkillTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	d := t.dispatcher
	if d.skillPool == nil {
		return &tool.Result{Tool: "load_skill", Success: true, Output: "技能功能未启用（未注入技能池）。"}
	}
	agentID := agent.AgentIDFromContext(ctx)
	if agentID == "" {
		return &tool.Result{Tool: "load_skill", Error: "missing caller agent context"}
	}
	name, _ := args["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return &tool.Result{Tool: "load_skill", Error: "name is required", Category: tool.ResultCategoryValidationRejected}
	}
	s := d.skillPool.FindByNameOrID(name)
	if s == nil {
		return &tool.Result{Tool: "load_skill", Error: fmt.Sprintf("技能 %q 不存在", name), Category: tool.ResultCategoryValidationRejected}
	}
	// 越权取用校验：非 meta 只能加载自己持有的技能（meta 持全池）。
	// 例外（2026-09-02 设计 §6.5）：learned 技能（自进化经验库）全局可加载——
	// 派发前缀只注入一行提示，取全文必须走 load_skill，持有集不登记经验技能。
	if roleIDFromAgentID(agentID) != "meta" && s.Source != "learned" {
		held := map[string]bool{}
		if v, ok := d.heldSkills.Load(agentID); ok {
			for _, n := range v.([]string) {
				held[n] = true
			}
		} else if roleDef := d.registry.Get(roleIDFromAgentID(agentID)); roleDef != nil {
			for _, n := range fixedSkillNames(d.skillPool, roleDef) {
				held[n] = true
			}
		}
		canonical := s.Name
		if canonical == "" {
			canonical = s.SkillID
		}
		if !held[canonical] {
			return &tool.Result{Tool: "load_skill", Error: fmt.Sprintf("技能 %q 不在你的持有集内（仅系统提示【可用技能】块列出的技能可用）", canonical), Category: tool.ResultCategoryValidationRejected}
		}
	}
	out := s.Content
	if out == "" {
		// 工具别名类技能（yaml/builtin，无正文）：回退使用示例。
		out = s.UsageExample
		if out == "" {
			out = "(该技能无正文内容，仅为工具别名：" + s.ToolRef + "，直接调用对应工具即可。)"
		}
	}
	// SKILL.md 同目录资源文件清单（渐进披露第三层）：正文以相对路径引用脚本/模板时，
	// 附资源清单帮 Agent 定位（读取走 ReadFile，Path 为绝对路径可直接引用）。
	if s.Path != "" {
		if entries, err := os.ReadDir(filepath.Dir(s.Path)); err == nil {
			var files []string
			for _, e := range entries {
				if e.IsDir() || e.Name() == "SKILL.md" {
					continue
				}
				files = append(files, e.Name())
			}
			sort.Strings(files)
			if len(files) > 0 {
				out += "\n\n【同目录资源文件】(目录: " + filepath.Dir(s.Path) + ")\n- " + strings.Join(files, "\n- ")
			}
		} else {
			log.Printf("[subagent] load_skill resource listing failed: path=%s err=%v", s.Path, err)
		}
	}
	// learned 技能 use_count++（设计 §6.5：被 load 后计数，用于排序与审计）。
	// 回调未注入或计数失败零影响（只记日志）。
	if s.Source == "learned" && d.skillUseCounter != nil {
		if err := d.skillUseCounter(s.SkillID); err != nil {
			log.Printf("[subagent] load_skill use_count++ failed: skill=%s err=%v", s.SkillID, err)
		}
	}
	return &tool.Result{Tool: "load_skill", Success: true, Output: out}
}
