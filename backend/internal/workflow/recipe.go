package workflow

// recipe.go 配方（recipe）的加载/校验/落盘。真相源 = 工作区 `.bma/workflows/*.yaml`
//（随项目持久、git 可版本化），不落配方库表；run 启动时快照配方全文进
// workflow_runs.recipe_yaml，"这批是哪个标准下做的"不依赖配方文件历史（开发文档 §3）。

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Recipe 是"标准"的物化形态：交付契约 + 阶段 + 锚点 + 不变量 + 变量槽 + 校验单 + 参数。
type Recipe struct {
	ID          string            `yaml:"id" json:"id"`
	Version     int               `yaml:"version" json:"version"`
	Title       string            `yaml:"title" json:"title"`
	Tool        string            `yaml:"tool" json:"tool"` // 生成工具名（含插件前缀，按 tool.Registry 实际注册名）
	Deliverable Deliverable       `yaml:"deliverable" json:"deliverable"`
	Stages      []*Stage          `yaml:"stages" json:"stages"`
	Anchors     map[string]string `yaml:"anchors" json:"anchors"`
	Invariants  string            `yaml:"invariants" json:"invariants"`
	Slots       []string          `yaml:"slots" json:"slots"`
	Params      Params            `yaml:"params" json:"params"`
	Check       CheckSpec         `yaml:"check" json:"check"`
	Escalate    *EscalateSpec     `yaml:"escalate,omitempty" json:"escalate,omitempty"`
}

// Deliverable 交付契约：程序校验直接按此判，能程序判的绝不叫模型判。
type Deliverable struct {
	// Type 交付形态：sprite_sheet | image | video（v1 程序校验全实现到 sprite_sheet/image）。
	Type string `yaml:"type" json:"type"`
	// Frames 帧数（sprite_sheet 网格帧数；阶段可覆写）。
	Frames int `yaml:"frames" json:"frames"`
	// Cell 单元尺寸 "WxH"（sprite_sheet 单帧格子，用来推整张网格尺寸）。
	Cell string `yaml:"cell" json:"cell"`
	// Alpha 是否需要透明通道（*bool 三态：未声明=不校验）。
	Alpha *bool `yaml:"alpha" json:"alpha"`
	// Format 交付格式（png/jpg/mp4...；空按 type 默认）。
	Format string `yaml:"format" json:"format"`
	// Path 路径模板（{{id}}/{{stage}} 等槽位替换）；以 "/" 结尾视为目录，自动拼默认文件名。
	Path string `yaml:"path" json:"path"`
}

// Stage 件内一个阶段（件内串行；anchor_out 采纳件进锚点池供后续阶段/件引用）。
type Stage struct {
	ID          string   `yaml:"id" json:"id"`
	AnchorIn    []string `yaml:"anchor_in,omitempty" json:"anchor_in,omitempty"`
	AnchorOut   bool     `yaml:"anchor_out,omitempty" json:"anchor_out,omitempty"`
	Frames      int      `yaml:"frames,omitempty" json:"frames,omitempty"`           // 覆写 deliverable.frames
	Instruction string   `yaml:"instruction,omitempty" json:"instruction,omitempty"` // 可选阶段指令（合成提示词用）
}

// Params 生成参数。
type Params struct {
	Size        int    `yaml:"size" json:"size"`               // 生成尺寸（透传/提示用，v1 不强校验）
	Reroll      int    `yaml:"reroll" json:"reroll"`           // 校验不过的重摇次数上限
	SeedPolicy  string `yaml:"seed_policy" json:"seed_policy"` // reroll_jitter（重摇微抖动）
	Concurrency int    `yaml:"concurrency" json:"concurrency"` // 覆写 run 件间并发
}

// CheckSpec 三层校验单：程序（免费）/ 模型评委（便宜）/ 人审（批量级联系表）。
type CheckSpec struct {
	Program []string `yaml:"program" json:"program"`
	Judge   []string `yaml:"judge" json:"judge"`
	Human   []string `yaml:"human" json:"human"`
}

// EscalateSpec 升级通道（可选段）：重摇耗尽后派真子 Agent 处理例外。
type EscalateSpec struct {
	RoleID string `yaml:"role" json:"role"`     // 子 Agent 角色 ID（按 role.Registry 存在角色）
	Domain string `yaml:"domain" json:"domain"` // 中文领域展示名（派发事件用）
}

// programCheckKeywords 是 v1 内置程序校验器关键字（开发文档 §6.1）。
var programCheckKeywords = map[string]bool{
	"exists": true, "path_template": true, "format": true, "dimensions": true,
}

// deliverableTypes 是 v1 支持的交付形态。
var deliverableTypes = map[string]bool{
	"sprite_sheet": true, "image": true, "video": true,
}

// idPattern 配方/名册件 id 合法形状（进文件名与路径模板，限安全字符集）。
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_\-]*$`)

// WorkflowsDir 返回工作区的配方目录（<workdir>/.bma/workflows）。
func WorkflowsDir(workdir string) string {
	return filepath.Join(workdir, ".bma", "workflows")
}

// Validate 校验配方完整性（id/version/tool/deliverable/stages/anchors/slots/check）。
func (r *Recipe) Validate() error {
	if r == nil {
		return fmt.Errorf("recipe is nil")
	}
	if strings.TrimSpace(r.ID) == "" {
		return fmt.Errorf("recipe.id is required")
	}
	if !idPattern.MatchString(r.ID) {
		return fmt.Errorf("recipe.id 非法（限字母数字下划线连字符）: %q", r.ID)
	}
	if r.Version < 0 {
		return fmt.Errorf("recipe.version 不能为负")
	}
	if strings.TrimSpace(r.Tool) == "" {
		return fmt.Errorf("recipe.tool is required（生成工具名，含插件前缀）")
	}
	if r.Deliverable.Type == "" {
		return fmt.Errorf("recipe.deliverable.type is required")
	}
	if !deliverableTypes[r.Deliverable.Type] {
		return fmt.Errorf("recipe.deliverable.type 不支持: %q（sprite_sheet|image|video）", r.Deliverable.Type)
	}
	if r.Deliverable.Path != "" {
		if err := validateRelTemplate(r.Deliverable.Path); err != nil {
			return fmt.Errorf("recipe.deliverable.path: %w", err)
		}
	}
	if len(r.Stages) == 0 {
		return fmt.Errorf("recipe.stages 不能为空（件内至少要有一个阶段）")
	}
	seen := make(map[string]bool, len(r.Stages))
	for i, st := range r.Stages {
		if st == nil || strings.TrimSpace(st.ID) == "" {
			return fmt.Errorf("recipe.stages[%d].id is required", i)
		}
		if !idPattern.MatchString(st.ID) {
			return fmt.Errorf("recipe.stages[%d].id 非法: %q", i, st.ID)
		}
		if seen[st.ID] {
			return fmt.Errorf("recipe.stages 存在重复阶段 id: %q", st.ID)
		}
		seen[st.ID] = true
		if st.Frames < 0 {
			return fmt.Errorf("recipe.stages[%d].frames 不能为负", i)
		}
	}
	for name, tpl := range r.Anchors {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("recipe.anchors 存在空锚点名")
		}
		if err := validateRelTemplate(tpl); err != nil {
			return fmt.Errorf("recipe.anchors[%s]: %w", name, err)
		}
	}
	for _, kw := range r.Check.Program {
		if !programCheckKeywords[kw] {
			return fmt.Errorf("recipe.check.program 未知校验器 %q（可用: exists/path_template/format/dimensions）", kw)
		}
	}
	if r.Params.Reroll < 0 {
		return fmt.Errorf("recipe.params.reroll 不能为负")
	}
	if r.Escalate != nil && strings.TrimSpace(r.Escalate.RoleID) == "" {
		return fmt.Errorf("recipe.escalate.role 不能为空（要么整段省略）")
	}
	return nil
}

// validateRelTemplate 校验路径模板是工作区相对路径且不逃逸（{{...}} 槽位先占位）。
func validateRelTemplate(tpl string) error {
	if strings.TrimSpace(tpl) == "" {
		return nil // 空=按默认路径
	}
	flat := regexp.MustCompile(`\{\{[^}]*\}\}`).ReplaceAllString(tpl, "x")
	flat = strings.ReplaceAll(flat, "\\", "/")
	if filepath.IsAbs(flat) || strings.HasPrefix(flat, "/") {
		return fmt.Errorf("必须是工作区相对路径: %q", tpl)
	}
	for _, seg := range strings.Split(flat, "/") {
		if seg == ".." {
			return fmt.Errorf("路径不得包含 ..: %q", tpl)
		}
	}
	return nil
}

// StageByID 按 id 查阶段（不存在返回 nil）。
func (r *Recipe) StageByID(id string) *Stage {
	for _, st := range r.Stages {
		if st.ID == id {
			return st
		}
	}
	return nil
}

// EffectiveDeliverable 返回阶段生效的交付契约（阶段 frames 覆写整件 frames）。
func (r *Recipe) EffectiveDeliverable(stage *Stage) Deliverable {
	d := r.Deliverable
	if stage != nil && stage.Frames > 0 {
		d.Frames = stage.Frames
	}
	return d
}

// RerollLimit 重摇上限（未声明默认 2，见设计报告 §3.1）。
func (r *Recipe) RerollLimit() int {
	if r.Params.Reroll > 0 {
		return r.Params.Reroll
	}
	return 2
}

// ProgramChecks 生效程序校验关键字（未声明默认 exists/path_template/format）。
func (r *Recipe) ProgramChecks() []string {
	if len(r.Check.Program) == 0 {
		return []string{"exists", "path_template", "format"}
	}
	return r.Check.Program
}

// ParseRecipe 解析配方 YAML 并校验。
func ParseRecipe(data []byte) (*Recipe, error) {
	var r Recipe
	if err := yaml.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse recipe yaml: %w", err)
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

// MarshalRecipe 序列化配方为 YAML（2 空格缩进）。
func MarshalRecipe(r *Recipe) ([]byte, error) {
	var sb strings.Builder
	enc := yaml.NewEncoder(&sb)
	enc.SetIndent(2)
	if err := enc.Encode(r); err != nil {
		return nil, fmt.Errorf("marshal recipe yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("marshal recipe yaml: %w", err)
	}
	return []byte(sb.String()), nil
}

// validateRecipeID 防路径穿越（id 直接进文件名）。
func validateRecipeID(id string) error {
	if !idPattern.MatchString(strings.TrimSpace(id)) {
		return fmt.Errorf("非法配方 id: %q", id)
	}
	return nil
}

// RecipeFilePath 返回配方文件绝对路径。
func RecipeFilePath(workdir, id string) (string, error) {
	if err := validateRecipeID(id); err != nil {
		return "", err
	}
	return filepath.Join(WorkflowsDir(workdir), id+".yaml"), nil
}

// LoadRecipe 从工作区加载配方（<workdir>/.bma/workflows/<id>.yaml）。
func LoadRecipe(workdir, id string) (*Recipe, error) {
	path, err := RecipeFilePath(workdir, id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("配方 %q 不存在（%s）", id, path)
		}
		return nil, fmt.Errorf("读取配方 %q: %w", id, err)
	}
	r, err := ParseRecipe(data)
	if err != nil {
		return nil, fmt.Errorf("配方 %s: %w", path, err)
	}
	// 文件名即 id（§3）：文件内容 id 与请求 id 不一致按内容为准但提醒（名字由落盘侧保证一致）。
	return r, nil
}

// RecipeSummary 是配方库列表的一行（id/title/version/交付契约摘要）。
type RecipeSummary struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Version int    `json:"version"`
	Tool    string `json:"tool"`
	Type    string `json:"deliverable_type"`
	Frames  int    `json:"frames"`
	Cell    string `json:"cell"`
	Stages  int    `json:"stage_count"`
	Anchors int    `json:"anchor_count"`
	Error   string `json:"error,omitempty"` // 解析失败时带原因（列表不因单文件损坏而整体失败）
}

// ListRecipes 扫描工作区配方目录，返回摘要列表（按 id 排序；目录不存在返回空列表）。
func ListRecipes(workdir string) ([]RecipeSummary, error) {
	dir := WorkflowsDir(workdir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []RecipeSummary{}, nil
		}
		return nil, fmt.Errorf("读取配方目录 %s: %w", dir, err)
	}
	out := make([]RecipeSummary, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		sum := RecipeSummary{ID: strings.TrimSuffix(e.Name(), ".yaml")}
		data, err := os.ReadFile(path)
		if err == nil {
			var r Recipe
			if err = yaml.Unmarshal(data, &r); err == nil {
				err = r.Validate()
			}
			if err == nil {
				sum.Title, sum.Version, sum.Tool = r.Title, r.Version, r.Tool
				sum.Type, sum.Frames, sum.Cell = r.Deliverable.Type, r.Deliverable.Frames, r.Deliverable.Cell
				sum.Stages, sum.Anchors = len(r.Stages), len(r.Anchors)
			} else {
				sum.Error = err.Error()
			}
		} else {
			sum.Error = err.Error()
		}
		out = append(out, sum)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// SaveRecipeYAML 落盘配方 YAML：解析 → 校验 → 版本规则（文件已存在时 version 至少现版+1，
// 保证"每次修订 +1"且回灌/冻结产物不会覆盖同版本）→ 原子写。
// 返回落盘后的配方（含生效版本号）。
func SaveRecipeYAML(workdir, yamlText string) (*Recipe, error) {
	r, err := ParseRecipe([]byte(yamlText))
	if err != nil {
		return nil, err
	}
	r.ID = strings.TrimSpace(r.ID)
	path, err := RecipeFilePath(workdir, r.ID)
	if err != nil {
		return nil, err
	}
	if existing, lerr := LoadRecipe(workdir, r.ID); lerr == nil && existing != nil {
		if r.Version <= existing.Version {
			r.Version = existing.Version + 1
		}
	} else if r.Version <= 0 {
		r.Version = 1
	}
	data, err := MarshalRecipe(r)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(WorkflowsDir(workdir), 0o755); err != nil {
		return nil, fmt.Errorf("创建配方目录: %w", err)
	}
	// 原子写：临时文件同目录 + rename（Windows rename 不覆盖，先删旧）。
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return nil, fmt.Errorf("写配方临时文件: %w", err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		os.Remove(tmp)
		return nil, fmt.Errorf("清理旧配方文件: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return nil, fmt.Errorf("落盘配方: %w", err)
	}
	return r, nil
}

// expandTemplate 展开路径模板中的 {{slot}}（id/name/stage/任意名册槽位）。
func expandTemplate(tpl string, item RosterItem, stage string) string {
	out := regexp.MustCompile(`\{\{\s*([A-Za-z0-9_]+)\s*\}\}`).ReplaceAllStringFunc(tpl, func(m string) string {
		name := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(m, "{{"), "}}"))
		if name == "stage" {
			return stage
		}
		if v := item.Slot(name); v != "" {
			return v
		}
		return m // 未命中槽位原样保留（校验/人审可见）
	})
	return strings.ReplaceAll(out, "\\", "/")
}
