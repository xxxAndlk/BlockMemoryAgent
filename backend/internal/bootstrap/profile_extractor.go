package bootstrap

// profile_extractor.go 实现 TODO #28 用户画像的会话完成自动提取 + 2026-09-02 期 1 Merge 整理：
// 提取：轻量模型扫用户消息提取稳定偏好增量（与 llmFactExtractor 同模式：CallLightweightWithRetry + JSON 解析）。
// 合并：轻量模型对目标小节自动行做去重/冲突归档重写，产出 MergePlan（人工行保护在 userprofile/merge.go）。

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/blockmemory/agent/backend/internal/domain/subagent"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/userprofile"
)

// extractProfilePreferences 从用户对话文本中提取 1-3 条稳定偏好。
// 提取失败/非 JSON 返回 error，调用方（ReactService.extractProfilePreferences）零副作用跳过。
func extractProfilePreferences(ctx context.Context, factory *model.ModelFactory, text string) ([]string, error) {
	prompt := fmt.Sprintf(`从以下用户消息中提取 1-3 条稳定的用户偏好（沟通风格/技术栈/确认频率/任务拆解粒度等长期偏好）。
只提取明确、可复用的陈述；一次性的具体任务指令不算偏好；不确定就不要提取。

要求:
- 每条简洁完整，不超过 60 字
- 输出 JSON 字符串数组,如 ["沟通风格: 直接给结论不要铺垫", "技术栈偏好: Go 与 VUE"]
- 不要输出任何其他文本、不要 markdown 围栏

用户消息:
%s`, truncateForPrompt(text, 4000))
	resp, err := factory.CallLightweightWithRetry(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("call lightweight: %w", err)
	}
	return subagent.ParseFactsJSON(resp)
}

// mergePreferences 把偏好增量合并进现有小节（设计 §4 Merge 流程）：
// 轻量模型读当前自动行视图 + 增量，产出 MergePlan（合并后各行 + 被替换归档行）。
// 解析失败返回 error，调用方降级直写归档小节。
func mergePreferences(ctx context.Context, factory *model.ModelFactory, view userprofile.MergeView, increments []string) (userprofile.MergePlan, error) {
	var b strings.Builder
	b.WriteString("现有档案各小节的自动行（已带时间戳的机器行；无时间戳的人工行不在其中且永不可改）：\n")
	for _, sec := range sortedViewSections(view) {
		lines := view[sec]
		b.WriteString(sec)
		b.WriteString(": ")
		if len(lines) == 0 {
			b.WriteString("（空）")
		} else {
			b.WriteString(strings.Join(lines, " | "))
		}
		b.WriteString("\n")	}
	b.WriteString("\n新增增量:\n")
	for i, inc := range increments {
		fmt.Fprintf(&b, "%d. %s\n", i+1, inc)
	}
	prompt := fmt.Sprintf(`你是偏好档案整理器。把新增增量合并进现有档案小节。

规则:
- 去重：与现有行语义重复的增量不再新增
- 冲突：新增量与现有行矛盾时（如 npm vs pnpm）新表述生效，被替换的现有行放入 archived
- 增量归属哪个小节由你判断（只能是上面列出的现有小节名）；不属于任何小节的丢弃
- 与现有内容无关的小节不要输出
- 每行不超过 60 字、不带时间戳、不带序号
- 输出 JSON 对象（不要 markdown 围栏）:
  {"merged": {"小节名": ["合并后该小节全部自动行", ...]}, "archived": ["被替换的旧行文本", ...]}
- merged 各小节是**完整替换**后的最终列表（现有行 + 新增量去重后的结果）；无变化的小节不要输出

%s

现在输出 JSON：`, b.String())
	resp, err := factory.CallLightweightWithRetry(ctx, prompt)
	if err != nil {
		return userprofile.MergePlan{}, fmt.Errorf("call lightweight: %w", err)
	}
	return parseMergePlan(resp)
}

// sortedViewSections 稳定排序视图小节名（prompt 输出确定性）。
func sortedViewSections(view userprofile.MergeView) []string {
	keys := make([]string, 0, len(view))
	for k := range view {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// parseMergePlan 解析合并计划 JSON（剥围栏/截取首尾大括号，容错与 ParseFactsJSON 同风格）。
func parseMergePlan(resp string) (userprofile.MergePlan, error) {
	cleaned := strings.TrimSpace(resp)
	cleaned = strings.TrimPrefix(cleaned, "```json")
	cleaned = strings.TrimPrefix(cleaned, "```")
	cleaned = strings.TrimSuffix(cleaned, "```")
	start := strings.Index(cleaned, "{")
	end := strings.LastIndex(cleaned, "}")
	if start < 0 || end <= start {
		return userprofile.MergePlan{}, fmt.Errorf("no JSON object in response")
	}
	var raw struct {
		Merged   map[string][]string `json:"merged"`
		Archived []string            `json:"archived"`
	}
	if err := json.Unmarshal([]byte(cleaned[start:end+1]), &raw); err != nil {
		return userprofile.MergePlan{}, fmt.Errorf("unmarshal merge plan: %w", err)
	}
	return userprofile.MergePlan{Merged: raw.Merged, Archived: raw.Archived}, nil
}
