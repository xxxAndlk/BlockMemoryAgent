package subagent

// context_manifest.go TaskContextManifest v2 装配与确定性裁决（U-02 首批）。
//
// 目标（23 号文档 §4）：
//   - 统一登记实际注入来源（source/source_id/revision/trust/scope）；
//   - 同 scope、同 conflict_key 的条目做确定性裁决（去重/优先级/未决）；
//   - required 完整性检查，超预算不静默截断；
//   - shadow 模式只对拍审计，模型输入不变；enforce 模式在 required 丢失时拒派。
//
// 本文件不渲染模型文本：渲染仍走既有 buildSharedPrefix/prefix 拼装，Manifest 只读同一批
// 源数据，避免“审计一套、模型读另一套”。项目有效记忆由 dispatcher 的 MemoryContextItem
// 提供器给出结构化条目（bootstrap 接 MemoryLifecycle.EffectiveItems）。

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/team"
)

// 上下文清单模式（agent.context_manifest_mode）。
const (
	ContextManifestOff     = "off"
	ContextManifestShadow  = "shadow"
	ContextManifestEnforce = "enforce"
)

// contextManifestRequiredCapRunes required 段硬预算：与派发 fork 硬顶同口径，
// 超限即视为“必须缩小任务或改用工件引用”，不做截断后继续。
const contextManifestRequiredCapRunes = spawnForkHardCapRunes

// contextManifestOptionalCapRunes optional 段软预算：超限按优先级保序裁剪并留裁决审计。
// 首批取值与项目偏好+召回的量级对齐（远小于 required 硬顶）。
const contextManifestOptionalCapRunes = 16000

// MemoryContextItem 项目/用户有效长期记忆的结构化条目（bootstrap 注入）。
type MemoryContextItem struct {
	ID           string
	Owner        string
	Scope        string
	ProjectKey   string
	MemoryKey    string
	Content      string
	ContentHash  string
	Origin       string
	Verification string
	Revision     int64
}

// contextSource 单条待装配来源（渲染前全量，含大文本）。
type contextSource struct {
	Source         string
	SourceID       string
	SourceRevision string
	ConflictKey    string
	Scope          string
	Trust          string
	Content        string
	ArtifactRef    string
	Required       bool
	Owner          string
	ProjectKey     string
}

// manifestProblems 装配中发现的问题；enforce 模式据此拒派。
type manifestProblems struct {
	RequiredOverflow bool
	RequiredDropped  []string // 被裁决丢弃/未决的 required 条目
	UnresolvedReq    bool
	IsolatedRequired bool
}

// normalizeContextManifestMode 归一化模式；未知值回落 shadow 并告警。
func normalizeContextManifestMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case ContextManifestOff:
		return ContextManifestOff
	case ContextManifestEnforce:
		return ContextManifestEnforce
	case "", ContextManifestShadow:
		return ContextManifestShadow
	default:
		return ContextManifestShadow
	}
}

// WithContextManifestMode 设置上下文清单模式（off|shadow|enforce）。
func (d *Dispatcher) WithContextManifestMode(mode string) *Dispatcher {
	d.contextManifestMode = normalizeContextManifestMode(mode)
	return d
}

// contextManifestModeValue 读取当前模式；未设置按 shadow。
func (d *Dispatcher) contextManifestModeValue() string {
	if d == nil || d.contextManifestMode == "" {
		return ContextManifestShadow
	}
	return d.contextManifestMode
}

// WithMemoryContextItems 注入长期记忆结构化条目回调（U-02）：
// bootstrap 接 MemoryLifecycle.EffectiveItems，返回当前项目+全局 active 有效视图。
func (d *Dispatcher) WithMemoryContextItems(fn func(ctx context.Context) []MemoryContextItem) *Dispatcher {
	d.memoryItemsFn = fn
	return d
}

// buildTaskContextManifest 从已收集来源构建不可变清单。
// 返回清单 + 问题集合；shadow 只记录，enforce 由调用方按问题拒派。
func (d *Dispatcher) buildTaskContextManifest(sessionID, roleID string, sources []contextSource, isolated bool) (*team.TaskContextManifest, manifestProblems) {
	var probs manifestProblems
	entries, decisions := arbitrateContextSources(sources, &probs)

	m := &team.TaskContextManifest{
		Version:       team.ContextManifestVersion,
		SessionID:     sessionID,
		RoleID:        roleID,
		ExecutionPath: team.ExecutionPathLegacyDelegate,
		Constraints:   decisions,
		FrozenAt:      time.Now(),
	}
	var requiredRunes int
	for _, e := range entries {
		if e.Source == team.SourceUserInstruction || e.Required {
			m.Required = append(m.Required, e)
			requiredRunes += entryRuneCount(e)
			continue
		}
		m.Optional = append(m.Optional, e)
	}
	// 项目/owner/水位从结构化记忆来源推导（无记忆时为空，不影响其它来源）。
	for _, s := range sources {
		if s.ProjectKey != "" {
			m.ProjectKey = s.ProjectKey
		}
		if s.Owner != "" {
			m.Owner = s.Owner
		}
		if s.Source == team.SourceTaskSpec && m.SpecVersion == "" {
			m.SpecVersion = s.SourceRevision
		}
	}
	m.Optional, decisions = trimOptionalBudget(m.Optional, contextManifestOptionalCapRunes, decisions)
	m.Constraints = decisions
	if requiredRunes > contextManifestRequiredCapRunes {
		probs.RequiredOverflow = true
	}
	if isolated {
		for _, e := range m.Required {
			probs.RequiredDropped = append(probs.RequiredDropped, e.EntryID)
		}
		if len(probs.RequiredDropped) > 0 {
			probs.IsolatedRequired = true
		}
	}
	m.ContentHash = m.ComputeContentHash()
	m.ManifestID = team.NewManifestID(m.ContentHash, m.FrozenAt)
	return m, probs
}

// trimOptionalBudget 按来源优先级保序裁剪 optional，超预算条目记 dropped_budget。
// 保留顺序与原注入顺序一致；只决定“若 enforce 将注入哪些”，shadow 不改模型输入。
func trimOptionalBudget(entries []team.ContextEntry, capRunes int, decisions []team.ConstraintDecision) ([]team.ContextEntry, []team.ConstraintDecision) {
	if capRunes <= 0 || len(entries) == 0 {
		return entries, decisions
	}
	order := make([]int, len(entries))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ea, eb := entries[order[a]], entries[order[b]]
		ra, rb := team.SourceRank(ea.Source), team.SourceRank(eb.Source)
		if ra != rb {
			return ra < rb
		}
		ta, tb := team.TrustRank(ea.Trust), team.TrustRank(eb.Trust)
		if ta != tb {
			return ta < tb
		}
		return ea.EntryID < eb.EntryID
	})
	keep := make(map[int]bool, len(entries))
	used := 0
	for _, i := range order {
		n := entryRuneCount(entries[i])
		if used+n > capRunes {
			decisions = append(decisions, team.ConstraintDecision{
				ConflictKey: entries[i].ConflictKey, Scope: entries[i].Scope,
				LoserIDs: []string{entries[i].EntryID}, Action: team.DecisionDropped,
				Reason: "optional context over budget",
			})
			continue
		}
		keep[i] = true
		used += n
	}
	out := make([]team.ContextEntry, 0, len(keep))
	for i, e := range entries {
		if keep[i] {
			out = append(out, e)
		}
	}
	return out, decisions
}

func entryRuneCount(e team.ContextEntry) int {
	if e.RuneBudget > 0 {
		return e.RuneBudget
	}
	return len([]rune(e.Content))
}

// arbitrateContextSources 确定性裁决并物化为条目。
// 规则（23 号文档 §4.3）：
//  1. 同 scope+project+conflict_key 且内容 hash 相同 → 去重（duplicate）；
//  2. 优先级（来源 rank，其次 trust，再次 revision）最高者胜出；
//  3. rank 与 trust 完全相同但内容不同 → unresolved；
//  4. required 条目被丢弃 → 记入问题（enforce 拒派）。
func arbitrateContextSources(sources []contextSource, probs *manifestProblems) ([]team.ContextEntry, []team.ConstraintDecision) {
	keep := make([]bool, len(sources))
	for i := range keep {
		keep[i] = true
	}
	var decisions []team.ConstraintDecision

	groups := map[string][]int{}
	for i, s := range sources {
		if strings.TrimSpace(s.ConflictKey) == "" {
			continue
		}
		key := s.Scope + "\x00" + s.ProjectKey + "\x00" + s.ConflictKey
		groups[key] = append(groups[key], i)
	}

	for key, idxs := range groups {
		if len(idxs) < 2 {
			continue
		}
		scope, _, conflictKey := splitGroupKey(key)
		// 同 hash 去重：保留首个，其余 duplicate。
		firstByHash := map[string]int{}
		for _, i := range idxs {
			h := team.HashContextContent(sources[i].Content)
			j, ok := firstByHash[h]
			if !ok {
				firstByHash[h] = i
				continue
			}
			// 同内容去重保留更高优先级副本，避免“先到者”误伤权威来源。
			winner, loser := j, i
			if sourceOutranks(sources[i], sources[j]) {
				winner, loser = i, j
				firstByHash[h] = i
			}
			keep[loser] = false
			decisions = append(decisions, team.ConstraintDecision{
				ConflictKey: conflictKey, Scope: scope,
				WinnerID: entryIDFor(sources[winner]), LoserIDs: []string{entryIDFor(sources[loser])},
				Action: team.DecisionDuplicate, Reason: "identical content hash",
			})
		}
		// 收集仍然存活的条目。
		var live []int
		for _, i := range idxs {
			if keep[i] {
				live = append(live, i)
			}
		}
		if len(live) < 2 {
			continue
		}
		// 排序：来源 rank 升序 → trust 升序 → revision 高优先 → 输入序稳定。
		best := live[0]
		for _, i := range live[1:] {
			if sourceOutranks(sources[i], sources[best]) {
				best = i
			}
		}
		// 与最优同 rank+trust 但内容不同 → unresolved。
		var ties []int
		for _, i := range live {
			if i == best {
				continue
			}
			if team.SourceRank(sources[i].Source) == team.SourceRank(sources[best].Source) &&
				team.TrustRank(sources[i].Trust) == team.TrustRank(sources[best].Trust) &&
				team.HashContextContent(sources[i].Content) != team.HashContextContent(sources[best].Content) {
				ties = append(ties, i)
			}
		}
		if len(ties) > 0 {
			var losers []string
			for _, i := range live {
				keep[i] = false
				losers = append(losers, entryIDFor(sources[i]))
				if sources[i].Required {
					probs.UnresolvedReq = true
					probs.RequiredDropped = append(probs.RequiredDropped, entryIDFor(sources[i]))
				}
			}
			decisions = append(decisions, team.ConstraintDecision{
				ConflictKey: conflictKey, Scope: scope,
				LoserIDs: losers, Action: team.DecisionUnresolved,
				Reason: "same rank/trust with different content",
			})
			continue
		}
		var losers []string
		for _, i := range live {
			if i == best {
				continue
			}
			keep[i] = false
			losers = append(losers, entryIDFor(sources[i]))
			if sources[i].Required {
				probs.RequiredDropped = append(probs.RequiredDropped, entryIDFor(sources[i]))
			}
		}
		decisions = append(decisions, team.ConstraintDecision{
			ConflictKey: conflictKey, Scope: scope,
			WinnerID: entryIDFor(sources[best]), LoserIDs: losers,
			Action: team.DecisionSuperseded, Reason: "lower source priority",
		})
	}

	var out []team.ContextEntry
	for i, s := range sources {
		if !keep[i] {
			continue
		}
		out = append(out, materializeEntry(s))
	}
	return out, decisions
}

func splitGroupKey(key string) (scope, projectKey, conflictKey string) {
	parts := strings.SplitN(key, "\x00", 3)
	for len(parts) < 3 {
		parts = append(parts, "")
	}
	return parts[0], parts[1], parts[2]
}

// sourceOutranks 判断 a 是否优于 b（数字越小优先级越高）。
func sourceOutranks(a, b contextSource) bool {
	ra, rb := team.SourceRank(a.Source), team.SourceRank(b.Source)
	if ra != rb {
		return ra < rb
	}
	ta, tb := team.TrustRank(a.Trust), team.TrustRank(b.Trust)
	if ta != tb {
		return ta < tb
	}
	if a.SourceRevision != b.SourceRevision {
		return a.SourceRevision > b.SourceRevision
	}
	return false
}

// entryIDFor 生成稳定条目 ID（同来源同内容重复构建一致）。
func entryIDFor(s contextSource) string {
	h := team.HashContextContent(s.Source + "|" + s.Scope + "|" + s.SourceID + "|" + s.ConflictKey + "|" + s.Content)
	return "e-" + h[:12]
}

// materializeEntry 物化条目：required 冻结短文本，optional 只留 hash（防事件膨胀）。
func materializeEntry(s contextSource) team.ContextEntry {
	hash := team.HashContextContent(s.Content)
	rev := s.SourceRevision
	if rev == "" {
		rev = hash
	}
	e := team.ContextEntry{
		EntryID:        entryIDFor(s),
		ConflictKey:    s.ConflictKey,
		Source:         s.Source,
		SourceID:       s.SourceID,
		SourceRevision: rev,
		Scope:          s.Scope,
		Trust:          s.Trust,
		ContentHash:    hash,
		ArtifactRef:    s.ArtifactRef,
		RuneBudget:     len([]rune(s.Content)),
		Required:       s.Required,
	}
	if s.Required {
		e.Content = truncateRunes(s.Content, 2000)
	}
	return e
}

// contextGuardFor 构造运行中 required 完整性检查（U-02）：
// 每轮比对 agents_md 与 task_spec 的当前内容 hash 是否与冻结 Manifest 一致。
// 不一致说明来源被撤回/覆盖/改写，返回原因即停止本 run。
func (d *Dispatcher) contextGuardFor(ctx context.Context, parentID, domain string, manifest *team.TaskContextManifest) func(context.Context) string {
	if manifest == nil {
		return nil
	}
	expectedAgents := ""
	specs := map[string]string{} // source_id -> content hash
	for _, e := range manifest.Required {
		switch e.Source {
		case team.SourceAgentsMD:
			expectedAgents = e.ContentHash
		case team.SourceTaskSpec:
			specs[e.SourceID] = e.ContentHash
		}
	}
	if expectedAgents == "" && len(specs) == 0 {
		return nil
	}
	base := ctx
	return func(callCtx context.Context) string {
		_ = callCtx
		if expectedAgents != "" {
			cur := d.projectBriefPrefix(base)
			if cur == "" {
				return "项目自述（AGENTS.md/CLAUDE.md）已失效或被删除"
			}
			if team.HashContextContent(cur) != expectedAgents {
				return "项目自述（AGENTS.md/CLAUDE.md）内容已变化，required 约束不再与冻结 Manifest 一致"
			}
		}
		if len(specs) > 0 {
			_, sources := d.buildSharedPrefixStructured(base, parentID, domain)
			cur := map[string]string{}
			for _, s := range sources {
				if s.Source == team.SourceTaskSpec {
					cur[s.SourceID] = team.HashContextContent(s.Content)
				}
			}
			for id, want := range specs {
				got, ok := cur[id]
				if !ok {
					return "任务规范 " + id + " 已在运行中失效或被移除"
				}
				if got != want {
					return "任务规范 " + id + " 内容已变化（新版本或撤回），required 约束不再有效"
				}
			}
		}
		return ""
	}
}

// contextMemorySources 把有效记忆条目转成可选来源。
func (d *Dispatcher) contextMemorySources(ctx context.Context) []contextSource {
	if d == nil || d.memoryItemsFn == nil {
		return nil
	}
	items := d.memoryItemsFn(ctx)
	out := make([]contextSource, 0, len(items))
	for _, it := range items {
		source := team.SourceProjectMemory
		scope := team.ScopeProject
		if it.Scope == "user" {
			source = team.SourceUserMemory
			scope = team.ScopeUser
		}
		// 未验证（evolver/旧导入）与块召回同档：只能作数据，不能覆盖人工/已验证事实。
		trust := team.TrustVerified
		if strings.TrimSpace(it.Verification) != "verified" {
			trust = team.TrustUntrusted
		}
		out = append(out, contextSource{
			Source: source, SourceID: it.ID, SourceRevision: strconv.FormatInt(it.Revision, 10),
			ConflictKey: "memory:" + it.MemoryKey, Scope: scope, Trust: trust,
			Content: it.Content, Required: false, Owner: it.Owner, ProjectKey: it.ProjectKey,
		})
	}
	return out
}

// verifyManifestEnforce 在 enforce 模式下判定是否必须拒派。
func verifyManifestEnforce(probs manifestProblems) error {
	if probs.RequiredOverflow {
		return fmt.Errorf("context manifest enforce: required context exceeds %d runes; shrink task or use artifact refs", contextManifestRequiredCapRunes)
	}
	if probs.IsolatedRequired {
		return fmt.Errorf("context manifest enforce: fork hard cap would drop required context (%s); dispatch refused", strings.Join(probs.RequiredDropped, ","))
	}
	if probs.UnresolvedReq || len(probs.RequiredDropped) > 0 {
		return fmt.Errorf("context manifest enforce: required context conflicted or superseded (%s); resolve before dispatch", strings.Join(probs.RequiredDropped, ","))
	}
	return nil
}
