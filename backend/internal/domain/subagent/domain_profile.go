package subagent

// domain_profile.go 领域注册表派发匹配（TODO #17 领域注册表 T24）：
// 跨会话冷复活——上个会话沉淀的领域档案（正名/别名/常改文件/既有结论链）在派发时
// 经名字（精确/别名）或路径（任务文本 + spec 文件清单 ∩ 档案文件清单）匹配命中后，
// 把 domain 归一化到档案正名、并把种子段拼进任务文本，子 Agent 先读现状再动手。
//
// 顺序约束：热驻隐式复用（resolveIdleSiblingReuse）与同名活跃查重必须先于档案匹配——
// 档案只管跨会话冷复活，会话内已有热驻槽/活跃实例时不掺和（callSubAgentTool.Execute 已保证）。
//
// 回调与存储解耦：dispatcher 不 import store；bootstrap 注入档案快照与记忆链两个只读回调。

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// DomainProfile 领域档案快照（回调返回值，dispatcher 不感知存储实现）。
type DomainProfile struct {
	Domain      string   // 档案正名（归一化目标；即历史会话用的中文领域名）
	DisplayName string   // 用户可见展示名
	Aliases     []string // 别名（历史变体名）
	Files       []string // 常改文件清单（相对工作目录）
	Subproject  string   // 子项目归属（可空）
	Summary     string   // 档案摘要（最近职责/结论概述）
}

// DomainProfileUpdate 领域档案增量写入（TODO #17 T25）：块记忆收尾旁路。
// Files 与档案现有清单并集合并（幂等去重）；Summary 非空时接管档案摘要
//（仅成功 outcome 才带，档案摘要始终反映最近一次成功工作）。
type DomainProfileUpdate struct {
	Domain  string
	Files   []string
	Summary string
}

// SetDomainProfileSink 注入领域档案增量写入回调（TODO #17 T25）。
// bootstrap 接 KnowledgeStore.UpsertDomainProfile；nil 时旁路关闭。
func (d *Dispatcher) SetDomainProfileSink(fn func(ctx context.Context, up DomainProfileUpdate)) *Dispatcher {
	d.domainProfileSinkFn = fn
	return d
}

// bumpDomainProfile 块记忆收尾旁路（TODO #17 T25）：taskDomain 非空时把本次
// files_modified 并进领域档案文件清单。失败仅日志（best-effort，不阻塞收尾）。
func (d *Dispatcher) bumpDomainProfile(ctx context.Context, taskDomain string, filesModified []string, summary string) {
	if d.domainProfileSinkFn == nil {
		return
	}
	td := strings.TrimSpace(taskDomain)
	if td == "" {
		return
	}
	d.domainProfileSinkFn(ctx, DomainProfileUpdate{Domain: td, Files: filesModified, Summary: summary})
}

// profilePathMinHits 路径命中最小重叠数：≥2 处不同文件重叠才算同域
//（单文件重叠太容易撞车——README/main.go 这类通用名遍布各域，误归一化比不归一化更糟）。
const profilePathMinHits = 2

// profileSeedFileCap 种子段文件清单上限（超出截断，防种子膨胀挤占任务正文）。
const profileSeedFileCap = 8

// profileSeedMemoryCap 种子段既有结论链条数。
const profileSeedMemoryCap = 3

// profilePathRe 任务文本路径样 token：带常见源码/文档扩展名的连续段（容忍反斜杠）。
var profilePathRe = regexp.MustCompile(`[\w\-.\\/]+\.(?:go|js|mjs|cjs|ts|tsx|jsx|py|java|kt|c|h|cpp|hpp|cs|rs|rb|php|vue|svelte|css|scss|less|html|htm|json|yaml|yml|toml|md|sql|sh|bat|ps1|proto|gradle|xml|ini|txt)\b`)

// profileDirPathRe 任务文本目录样 token：至少一级 "/" 且各段为词字符（src/components）。
var profileDirPathRe = regexp.MustCompile(`\b[\w\-.]+(?:/[\w\-.]+)+\b`)

// SetDomainProfileHook 注入领域档案快照回调（TODO #17 T24）：bootstrap 接
// KnowledgeStore.ListDomainProfiles 并转快照；nil 时档案匹配整体关闭（零注入，行为同前）。
func (d *Dispatcher) SetDomainProfileHook(fn func(ctx context.Context) []*DomainProfile) *Dispatcher {
	d.domainProfilesFn = fn
	return d
}

// SetDomainMemoryHook 注入领域记忆链回调（TODO #17 T24）：按 task_domain 取最近 n 条
// 块记忆正文（新→旧）。仅在档案命中后调用（惰性，未命中零查询）。
func (d *Dispatcher) SetDomainMemoryHook(fn func(ctx context.Context, domain string, n int) []string) *Dispatcher {
	d.domainMemoriesFn = fn
	return d
}

// applyDomainProfileSeed 领域档案冷复活匹配 + 种子注入（TODO #17 T24）。
// 返回（可能归一化的）task 与 domain；未命中或回调未接线时原样返回。
// 经 callSubAgentTool.Execute 在热驻/同名复用未命中后调用。
func (d *Dispatcher) applyDomainProfileSeed(ctx context.Context, parentID, domain, task string) (string, string) {
	if d.domainProfilesFn == nil {
		return task, domain
	}
	profiles := d.domainProfilesFn(ctx)
	if len(profiles) == 0 {
		return task, domain
	}
	prof, via := matchDomainProfile(profiles, domain, task, d.dispatchSpecFiles(ctx, parentID, domain))
	if prof == nil {
		return task, domain
	}
	// 归一化：别名/路径命中映射回档案正名（正名即历史会话的中文领域名，展示纪律不破）。
	if via != "name" {
		domain = prof.Domain
	}
	if seed := d.domainProfileSeedText(ctx, prof, via); seed != "" {
		task += seed
	}
	return task, domain
}

// matchDomainProfile 档案匹配：名字精确 > 别名 > 路径重叠。未命中返回 (nil, "")。
// 路径匹配取重叠最多者，严格并列视为歧义不命中；阈值 profilePathMinHits。
func matchDomainProfile(profiles []*DomainProfile, domain string, task string, specFiles []string) (*DomainProfile, string) {
	domain = strings.TrimSpace(domain)
	// 1) 名字精确：档案正名直接对应，零歧义。
	for _, p := range profiles {
		if domain != "" && p.Domain == domain {
			return p, "name"
		}
	}
	// 2) 别名：历史变体名映射回正名。
	if domain != "" {
		for _, p := range profiles {
			for _, a := range p.Aliases {
				if strings.TrimSpace(a) == domain {
					return p, "alias"
				}
			}
		}
	}
	// 3) 路径重叠：任务文本 + spec 文件清单提取的路径 ∩ 档案文件清单。
	//    不依赖 domain 命中，总是尝试；取重叠最多者，严格并列视为歧义不命中。
	paths := profilePathCandidates(task, specFiles)
	if len(paths) == 0 {
		return nil, ""
	}
	best, bestHits := (*DomainProfile)(nil), 0
	tied := false
	for _, p := range profiles {
		hits := 0
		for _, pf := range p.Files {
			for _, cand := range paths {
				if pathsOverlap(cand, pf) {
					hits++
					break // 每个档案文件只计一次
				}
			}
		}
		if hits < profilePathMinHits {
			continue
		}
		if hits > bestHits {
			best, bestHits, tied = p, hits, false
		} else if hits == bestHits {
			tied = true
		}
	}
	if best != nil && !tied {
		return best, "paths"
	}
	return nil, ""
}

// profilePathCandidates 从任务文本与 spec 文件清单提取路径候选（去重保序）。
func profilePathCandidates(task string, specFiles []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		k := normPathKey(p)
		if k == "" || seen[k] {
			return
		}
		seen[k] = true
		out = append(out, p)
	}
	for _, m := range profilePathRe.FindAllString(task, -1) {
		add(m)
	}
	for _, m := range profileDirPathRe.FindAllString(task, -1) {
		add(m)
	}
	for _, f := range specFiles {
		add(f)
	}
	return out
}

// domainProfileSeedText 构建种子段（已命中才调用）：摘要 + 存活文件清单 + 记忆链。
// 文件逐个 os.Stat 核对存在性（档案里的常改文件可能已被删除/重命名），空 workdir 跳过核对。
func (d *Dispatcher) domainProfileSeedText(ctx context.Context, prof *DomainProfile, via string) string {
	var b strings.Builder
	b.WriteString("\n\n【领域档案】本任务匹配到历史领域「" + prof.DisplayName + "」（按" + profileViaName(via) + "命中），跨会话衔接既有工作：")
	if s := strings.TrimSpace(prof.Summary); s != "" {
		b.WriteString("\n- 历史摘要：" + truncateRunes(s, 300))
	}
	if prof.Subproject != "" {
		b.WriteString("\n- 子项目：" + prof.Subproject)
	}
	if files, total := existingProfileFiles(tool.WorkDirFromContext(ctx), prof.Files); len(files) > 0 {
		b.WriteString("\n- 常改文件（已核对存在，开工先读现状）：")
		for _, f := range files {
			b.WriteString("\n  - " + f)
		}
		if total > len(files) {
			b.WriteString("\n  …另有 " + strconv.Itoa(total-len(files)) + " 个（档案完整清单）")
		}
	}
	if d.domainMemoriesFn != nil {
		if mems := d.domainMemoriesFn(ctx, prof.Domain, profileSeedMemoryCap); len(mems) > 0 {
			b.WriteString("\n- 既有结论链（新→旧）：")
			for i, m := range mems {
				b.WriteString("\n  " + strconv.Itoa(i+1) + ". " + truncateRunes(strings.TrimSpace(m), 160))
			}
		}
	}
	b.WriteString("\n衔接纪律：先读上述文件与既有结论再动手，勿从零重做已完成的改动。")
	return b.String()
}

// existingProfileFiles 过滤档案文件清单中实际存在的文件（相对 workdir stat 核对）。
// workdir 为空时不核对（无法定位），原样返回；返回值 (清单按 cap 截断且排序保证文案
// 确定性, 存活总数)——总数超出 cap 时种子段标注"另有 N 个"。
func existingProfileFiles(workdir string, files []string) ([]string, int) {
	if len(files) == 0 {
		return nil, 0
	}
	seen := map[string]bool{}
	var alive []string
	for _, f := range files {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		k := normPathKey(f)
		if seen[k] {
			continue
		}
		seen[k] = true
		if workdir != "" && !filepath.IsAbs(f) {
			if _, err := os.Stat(filepath.Join(workdir, filepath.FromSlash(f))); err != nil {
				continue // 已删除/重命名：从种子剔除（绝对路径无法在 workdir 下核对，一并剔除）
			}
		}
		alive = append(alive, f)
	}
	sort.Strings(alive) // 排序保证文案确定性（多处派发种子一致）
	if len(alive) > profileSeedFileCap {
		return alive[:profileSeedFileCap], len(alive)
	}
	return alive, len(alive)
}

// profileViaName 命中方式的用户可读名。
func profileViaName(via string) string {
	switch via {
	case "name":
		return "同名"
	case "alias":
		return "别名"
	case "paths":
		return "文件路径"
	default:
		return via
	}
}
