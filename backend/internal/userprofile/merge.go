package userprofile

// merge.go 实现画像/项目偏好的 Merge 整理（2026-09-02 偏好与自进化期 1，设计 §4）：
// 会话结束提取到偏好增量后，由轻量模型对目标小节做合并重写——去重、
// 新偏好与旧自动行冲突时新的生效、旧行移入归档小节（不直接删，可审计）。
//
// 人工行 vs 自动行（设计 §4）：无 `（YYYY-MM-DD HH:MM）` 时间戳后缀的行视为人工行，
// 自动整理永不删除/改写；带后缀的自动行可被合并、去重、重写、归档。

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// autoStampRe 匹配行尾的全角括号时间戳（程序经 Append 写入的自动行标志）。
var autoStampRe = regexp.MustCompile(`（(\d{4}-\d{2}-\d{2} \d{2}:\d{2})）$`)

// AutoLine 自动行：文本 + 时间戳（由 Append 写入，可被 Merge 改写/归档）。
type AutoLine struct {
	Text  string
	Stamp string
}

// DocSection 小节解析结果：Title 之外的行按「自动行 / 其余内容」二分。
// 其余内容（人工列表行、自由文本、空行）原样保留，Merge 永不改写。
type DocSection struct {
	Title string
	Human []string  // 非自动行（含空行，保持原顺序）
	Auto  []AutoLine
}

// Document 全文解析结果：Head 是首个 ## 小节之前的全部内容（含 # 标题与引言）。
type Document struct {
	Head     string
	Sections []*DocSection
}

// ParseDocument 把全文解析为头部 + 小节序列。解析失败无意义（纯行扫描，永不失败）。
func ParseDocument(content string) Document {
	lines := strings.Split(content, "\n")
	var doc Document
	var head []string
	var cur *DocSection
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "## ") {
			cur = &DocSection{Title: strings.TrimSpace(trimmed[3:])}
			doc.Sections = append(doc.Sections, cur)
			continue
		}
		if cur == nil {
			head = append(head, ln)
			continue
		}
		if stamp := autoStampRe.FindStringSubmatch(trimmed); stamp != nil {
			text := strings.TrimSuffix(trimmed, stamp[0])
			text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "-"))
			if text == "" {
				cur.Human = append(cur.Human, ln)
				continue
			}
			cur.Auto = append(cur.Auto, AutoLine{Text: text, Stamp: stamp[1]})
			continue
		}
		cur.Human = append(cur.Human, ln)
	}
	doc.Head = strings.Join(head, "\n")
	return doc
}

// RenderDocument 重建全文。小节渲染顺序：人工行（原序）在前，自动行（时间序）在后——
// 自动行是程序管理区，统一挪到小节末尾；人工内容的位置与措辞逐字节保留。
func RenderDocument(doc Document) string {
	var b strings.Builder
	b.WriteString(strings.TrimRight(doc.Head, "\n"))
	if strings.TrimSpace(doc.Head) != "" {
		b.WriteString("\n\n")
	}
	for _, sec := range doc.Sections {
		fmt.Fprintf(&b, "## %s\n", sec.Title)
		for _, h := range sec.Human {
			b.WriteString(h)
			b.WriteString("\n")
		}
		for _, a := range sec.Auto {
			fmt.Fprintf(&b, "- %s（%s）\n", a.Text, a.Stamp)
		}
	}
	return b.String()
}

// MergePlan 一次合并的产物（由轻量模型产出，代码侧校验拼接）。
type MergePlan struct {
	// Merged 小节名 -> 合并后的自动行文本列表（含去重后的旧行与新增量；
	// 与旧行文本一致的行保留原时间戳，新行取当前时间）。
	Merged map[string][]string
	// Archived 被新表述替换/删除的旧自动行文本（移入归档小节，可审计）。
	Archived []string
}

// MergeView 供轻量模型读取的当前自动行视图：目标小节名 -> 现有自动行文本。
type MergeView map[string][]string

// MergeView 返回目标小节的自动行视图（缺失小节给空列表，让模型看到完整小节结构）。
func (s *Store) MergeView(targets []string) MergeView {
	view := MergeView{}
	doc := ParseDocument(s.Current().Content)
	for _, t := range targets {
		view[t] = nil
	}
	for _, sec := range doc.Sections {
		if _, ok := view[sec.Title]; !ok {
			continue
		}
		for _, a := range sec.Auto {
			view[sec.Title] = append(view[sec.Title], a.Text)
		}
	}
	return view
}

// ApplyMerge 应用一次合并计划：重写目标小节的自动行、归档旧行（保留最近 archiveCap 条）。
// 人工行（含其他小节）逐字节不动；归档小节是唯一允许增长的额外小节。
// 计划中出现的未知小节名被忽略（模型越界输出不落盘）。
func (s *Store) ApplyMerge(plan MergePlan, targets []string) error {
	if len(plan.Merged) == 0 && len(plan.Archived) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	content := s.Current().Content
	if strings.TrimSpace(content) == "" {
		content = s.template
	}
	doc := ParseDocument(content)
	targetSet := map[string]bool{}
	for _, t := range targets {
		targetSet[t] = true
	}
	now := time.Now().Format("2006-01-02 15:04")

	// 旧自动行文本 -> 时间戳（合并后按文本回填原时间戳）。
	oldStamps := map[string]string{}
	for _, sec := range doc.Sections {
		for _, a := range sec.Auto {
			oldStamps[a.Text] = a.Stamp
		}
	}

	// 重写目标小节自动行；目标小节缺失但计划有内容时补建到文档末尾。
	for _, sec := range doc.Sections {
		if !targetSet[sec.Title] {
			continue
		}
		merged, ok := plan.Merged[sec.Title]
		if !ok {
			continue
		}
		sec.Auto = stampLines(merged, oldStamps, now)
	}
	for _, t := range targets {
		merged, ok := plan.Merged[t]
		if !ok || len(merged) == 0 {
			continue
		}
		if findSection(doc, t) == nil {
			doc.Sections = append(doc.Sections, &DocSection{Title: t, Auto: stampLines(merged, oldStamps, now)})
		}
	}

	// 归档：archived 文本匹配旧时间戳则保留原时间戳，否则当前时间。
	if len(plan.Archived) > 0 {
		arc := findOrCreateSection(&doc, s.archiveSec)
		for _, text := range plan.Archived {
			stamp := now
			if old, ok := oldStamps[strings.TrimSpace(text)]; ok && old != "" {
				stamp = old
			}
			arc.Auto = append(arc.Auto, AutoLine{Text: strings.TrimSpace(text), Stamp: stamp})
		}
		if len(arc.Auto) > s.archiveCap {
			arc.Auto = arc.Auto[len(arc.Auto)-s.archiveCap:]
		}
	}

	return s.writeLocked(RenderDocument(doc))
}

// stampLines 把合并后的文本列表转为自动行：文本与旧行一致回填原时间戳，新行取 now。
func stampLines(texts []string, oldStamps map[string]string, now string) []AutoLine {
	out := make([]AutoLine, 0, len(texts))
	seen := map[string]bool{}
	for _, raw := range texts {
		text := strings.TrimSpace(raw)
		if text == "" || seen[text] {
			continue
		}
		seen[text] = true
		stamp := now
		if old, ok := oldStamps[text]; ok && old != "" {
			stamp = old
		}
		out = append(out, AutoLine{Text: text, Stamp: stamp})
	}
	return out
}

func findSection(doc Document, title string) *DocSection {
	for _, sec := range doc.Sections {
		if sec.Title == title {
			return sec
		}
	}
	return nil
}

func findOrCreateSection(doc *Document, title string) *DocSection {
	if sec := findSection(*doc, title); sec != nil {
		return sec
	}
	sec := &DocSection{Title: title}
	doc.Sections = append(doc.Sections, sec)
	return sec
}
