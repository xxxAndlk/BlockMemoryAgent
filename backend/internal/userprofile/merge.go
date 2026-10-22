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
	"sort"
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

// ArchiveView 返回归档小节的自动行文本（只读视图，供 merge 模型做冲突判据：
// 与归档行矛盾的新增量直接丢弃、现行行不得复活归档内容——2026-10-19 实证
// "移除掉落物系统"进归档后"击杀掉落无保底"被重新写回现行小节的活矛盾）。
func (s *Store) ArchiveView() []string {
	doc := ParseDocument(s.Current().Content)
	sec := findSection(doc, s.archiveSec)
	if sec == nil {
		return nil
	}
	out := make([]string, 0, len(sec.Auto))
	for _, a := range sec.Auto {
		out = append(out, a.Text)
	}
	return out
}

// StripSection 从全文渲染中剔除指定小节（注入侧用：归档小节只做审计、不进上下文，
// 避免已废止的历史行占注入配额并误导模型——与"经验归档/反馈记录"同款语义）。
func StripSection(content, title string) string {
	if strings.TrimSpace(title) == "" {
		return content
	}
	doc := ParseDocument(content)
	kept := make([]*DocSection, 0, len(doc.Sections))
	for _, sec := range doc.Sections {
		if sec.Title != title {
			kept = append(kept, sec)
		}
	}
	if len(kept) == len(doc.Sections) {
		return content
	}
	doc.Sections = kept
	return RenderDocument(doc)
}

// BudgetSections 按小节预算渲染注入视图（2026-10-22 上下文污染修复）：
//   - skip 小节（归档/反馈记录）整节剔除——已废止内容只做磁盘审计，不进模型上下文；
//   - 每小节均分 maxRunes，自动行从末尾（最新）往前保留，最旧的省略并标注行数；
//   - 人工行（无时间戳）优先保留，但整体仍受 maxRunes 硬上限约束。
//
// 旧实现从头截断：既会把归档小节原样注入（已否决内容反复污染），又会砍掉文件尾部
// 最新写入的偏好（技术栈/反馈记录整节消失）。本函数是注入侧唯一截断口径，
// Agent 画像注入与 Dispatcher 项目偏好前缀共用。
func BudgetSections(content string, maxRunes int, skip ...string) string {
	if maxRunes <= 0 {
		return content
	}
	skipSet := make(map[string]bool, len(skip))
	for _, s := range skip {
		if t := strings.TrimSpace(s); t != "" {
			skipSet[t] = true
		}
	}
	doc := ParseDocument(content)
	kept := make([]*DocSection, 0, len(doc.Sections))
	for _, sec := range doc.Sections {
		if skipSet[sec.Title] {
			continue
		}
		kept = append(kept, sec)
	}
	doc.Sections = kept
	if out := RenderDocument(doc); len([]rune(out)) <= maxRunes {
		return out
	}
	if len(doc.Sections) == 0 {
		return hardTruncateRunes(content, maxRunes)
	}
	budget := maxRunes - len([]rune(doc.Head)) - 160 // 省略标记留量
	if budget < 300 {
		budget = maxRunes / 2
	}
	per := budget / len(doc.Sections)
	if per < 60 {
		per = 60
	}
	for _, sec := range doc.Sections {
		humanRunes := 0
		for _, h := range sec.Human {
			humanRunes += len([]rune(h)) + 1
		}
		remain := per - humanRunes
		if remain < 0 {
			remain = 0
		}
		used := 0
		var tail []AutoLine
		for i := len(sec.Auto) - 1; i >= 0; i-- {
			n := len([]rune(sec.Auto[i].Text)) + len([]rune(sec.Auto[i].Stamp)) + 8
			if used+n > remain && len(tail) > 0 {
				break
			}
			tail = append([]AutoLine{sec.Auto[i]}, tail...)
			used += n
		}
		dropped := len(sec.Auto) - len(tail)
		sec.Auto = tail
		if dropped > 0 {
			sec.Human = append(sec.Human, fmt.Sprintf("…（本小节较早 %d 行因注入预算省略）", dropped))
		}
	}
	out := RenderDocument(doc)
	if len([]rune(out)) > maxRunes {
		out = hardTruncateRunes(out, maxRunes)
	}
	return out
}

// hardTruncateRunes 按 rune 硬截断（最后防线，正常路径不会走到）。
func hardTruncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// ApplyMerge 应用一次合并计划：重写目标小节的自动行、归档旧行（保留最近 archiveCap 条）。
// 人工行（含其他小节）逐字节不动；归档小节是唯一允许增长的额外小节。
// 计划中出现的未知小节名被忽略（模型越界输出不落盘）。
// 小节行数硬上限 maxAutoLinesPerSection：超出按时间戳最旧优先移入归档（注入面有界）。
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
	// 防复活（2026-10-19 实证）：merged 中与归档小节现有行同文本的行直接丢弃
	//（历史已废止内容不得借合并复活进现行小节；语义近重复仍靠 merge 模型自律）。
	archiveTexts := map[string]bool{}
	for _, sec := range doc.Sections {
		if sec.Title == s.archiveSec {
			for _, a := range sec.Auto {
				archiveTexts[a.Text] = true
			}
		}
	}
	var overflow []AutoLine // 超硬上限被移入归档的行（时间戳最旧优先，可审计）。
	for _, sec := range doc.Sections {
		if !targetSet[sec.Title] {
			continue
		}
		merged, ok := plan.Merged[sec.Title]
		if !ok {
			continue
		}
		if len(archiveTexts) > 0 {
			kept := merged[:0]
			for _, m := range merged {
				if !archiveTexts[strings.TrimSpace(m)] {
					kept = append(kept, m)
				}
			}
			merged = kept
		}
		sec.Auto = stampLines(merged, oldStamps, now)
		if len(sec.Auto) > maxAutoLinesPerSection {
			// 按时间戳升序淘汰最旧行（模型输出的行序不保证新旧，以时间戳为准）。
			order := make([]int, len(sec.Auto))
			for i := range order {
				order[i] = i
			}
			sort.SliceStable(order, func(a, b int) bool { return sec.Auto[order[a]].Stamp < sec.Auto[order[b]].Stamp })
			drop := map[int]bool{}
			for _, idx := range order[:len(sec.Auto)-maxAutoLinesPerSection] {
				drop[idx] = true
			}
			kept := sec.Auto[:0]
			for i, a := range sec.Auto {
				if drop[i] {
					overflow = append(overflow, a)
				} else {
					kept = append(kept, a)
				}
			}
			sec.Auto = kept
		}
	}
	for _, t := range targets {
		merged, ok := plan.Merged[t]
		if !ok || len(merged) == 0 {
			continue
		}
		if findSection(doc, t) == nil {
			if len(archiveTexts) > 0 {
				kept := merged[:0]
				for _, m := range merged {
					if !archiveTexts[strings.TrimSpace(m)] {
						kept = append(kept, m)
					}
				}
				merged = kept
			}
			if len(merged) == 0 {
				continue
			}
			doc.Sections = append(doc.Sections, &DocSection{Title: t, Auto: stampLines(merged, oldStamps, now)})
		}
	}

	// 归档：archived 文本匹配旧时间戳则保留原时间戳，否则当前时间。
	// 去重（2026-10-19 实证：同文本被反复归档 4 次——rejected 多次命中/模型重复输出
	// 而 append 无去重）：与现有归档行同文本的直接跳过。
	if len(plan.Archived) > 0 || len(overflow) > 0 {
		arc := findOrCreateSection(&doc, s.archiveSec)
		arc.Auto = append(arc.Auto, overflow...)
		existing := map[string]bool{}
		for _, a := range arc.Auto {
			existing[a.Text] = true
		}
		for _, text := range plan.Archived {
			text = strings.TrimSpace(text)
			if text == "" || existing[text] {
				continue
			}
			existing[text] = true
			stamp := now
			if old, ok := oldStamps[text]; ok && old != "" {
				stamp = old
			}
			arc.Auto = append(arc.Auto, AutoLine{Text: text, Stamp: stamp})
		}
	}

	// 归档小节整体按文本去重（保留最后出现的时间戳）+ 封顶。独立运行（不依赖本次
	// 是否有 archived 增量）——只要 ApplyMerge 被调用就顺带自愈历史脏数据。
	if arc := findSection(doc, s.archiveSec); arc != nil {
		if len(arc.Auto) > 1 {
			seenText := map[string]bool{}
			dedup := make([]AutoLine, 0, len(arc.Auto))
			for i := len(arc.Auto) - 1; i >= 0; i-- {
				if seenText[arc.Auto[i].Text] {
					continue
				}
				seenText[arc.Auto[i].Text] = true
				dedup = append(dedup, arc.Auto[i])
			}
			for i, j := 0, len(dedup)-1; i < j; i, j = i+1, j-1 {
				dedup[i], dedup[j] = dedup[j], dedup[i]
			}
			arc.Auto = dedup
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
