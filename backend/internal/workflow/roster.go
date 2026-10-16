package workflow

// roster.go 名册（roster）解析：workflow_run 参数内联 JSON 数组，或
// `.bma/workflows/roster_*.yaml` 文件路径；一行一件，stages 逐件裁剪（开发文档 §3）。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// rosterFileWrapper 允许名册文件用 `roster:` 顶层键包裹（设计报告 §3.2 的写法）。
type rosterFileWrapper struct {
	Roster []RosterItem `yaml:"roster" json:"roster"`
}

// ParseRoster 解析名册：raw 形如 `[...]` / `{` 视为内联 JSON；否则视为文件路径
// （相对 workdir 或绝对），支持 YAML（顶层数组或 roster: 包裹）与 JSON 文件。
func ParseRoster(raw, workdir string) ([]RosterItem, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("roster 不能为空（内联 JSON 数组或 roster 文件路径）")
	}
	if strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{") {
		return parseRosterJSON([]byte(trimmed))
	}
	return parseRosterFile(trimmed, workdir)
}

func parseRosterJSON(data []byte) ([]RosterItem, error) {
	// 兼容 {"roster":[...]} 包裹形态。
	var wrapper rosterFileWrapper
	if err := json.Unmarshal(data, &wrapper); err == nil && len(wrapper.Roster) > 0 {
		return finalizeRoster(wrapper.Roster)
	}
	var items []RosterItem
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("解析内联 roster JSON: %w", err)
	}
	return finalizeRoster(items)
}

func parseRosterFile(path, workdir string) ([]RosterItem, error) {
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(workdir, filepath.FromSlash(path))
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("读取名册文件 %s: %w", abs, err)
	}
	if strings.HasSuffix(strings.ToLower(abs), ".json") {
		return parseRosterJSON(data)
	}
	// YAML：先试顶层数组，再试 roster: 包裹。
	var items []RosterItem
	if err := yaml.Unmarshal(data, &items); err == nil && len(items) > 0 {
		return finalizeRoster(items)
	}
	var wrapper rosterFileWrapper
	if err := yaml.Unmarshal(data, &wrapper); err != nil {
		return nil, fmt.Errorf("解析名册 YAML %s: %w", abs, err)
	}
	if len(wrapper.Roster) == 0 {
		return nil, fmt.Errorf("名册文件 %s 没有条目（顶层数组或 roster: 段）", abs)
	}
	return finalizeRoster(wrapper.Roster)
}

// finalizeRoster 去重与合法性收口（名册是调度坐标，id 必须唯一）。
func finalizeRoster(items []RosterItem) ([]RosterItem, error) {
	if len(items) == 0 {
		return nil, fmt.Errorf("名册为空")
	}
	seen := make(map[string]bool, len(items))
	for i := range items {
		item := &items[i]
		item.ID = strings.TrimSpace(item.ID)
		if item.ID == "" {
			return nil, fmt.Errorf("名册第 %d 行缺少 id", i+1)
		}
		if !idPattern.MatchString(item.ID) {
			return nil, fmt.Errorf("名册 id 非法（限字母数字下划线连字符）: %q", item.ID)
		}
		if seen[item.ID] {
			return nil, fmt.Errorf("名册存在重复 id: %q", item.ID)
		}
		seen[item.ID] = true
	}
	return items, nil
}

// validateRosterAgainstRecipe 校验名册每件的 stages 子集都存在于配方阶段表
// （裁剪写错阶段名=静默漏产，宁可在建 run 前打回）。
func validateRosterAgainstRecipe(items []RosterItem, recipe *Recipe) error {
	for _, item := range items {
		for _, s := range item.Stages {
			if recipe.StageByID(strings.TrimSpace(s)) == nil {
				return fmt.Errorf("名册 %s 引用了配方中不存在的阶段 %q", item.ID, s)
			}
		}
		if len(item.StagesFor(recipe.Stages)) == 0 {
			return fmt.Errorf("名册 %s 的 stages 裁剪后没有任何阶段", item.ID)
		}
	}
	return nil
}
