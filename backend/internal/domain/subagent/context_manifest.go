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
	var requiredRunes, optionalRunes int
	for _, e := range entries {
		if e.Source == team.SourceUserInstruction {
			m.Required = append(m.Required, e)
			requiredRunes += entryRuneCount(e)
			continue
		}
		if e.Required {
			m.Required = append(m.Required, e)
			requiredRunes += entryRuneCount(e)
		} else {
			m.Optional = append(m.Optional, e)
			optionalRunes += entryRuneCount(e)
		}
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
	_ = optionalRunes
	m.ContentHash = m.ComputeContentHash()
	m.ManifestID = team.NewManifestID(m.ContentHash, m.FrozenAt)
	return m, probs
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
			if j, ok := firstByHash[h]; ok {
				keep[i] = false
				decisions = append(decisions, team.ConstraintDecision{
					ConflictKey: conflictKey, Scope: scope,
					WinnerID: entryIDFor(sources[j]), LoserIDs: []string{entryIDFor(sources[i])},
					Action: team.DecisionDuplicate, Reason: "identical content hash",
				})
				continue
			}
			firstByHash[h] = i
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
		trust := team.TrustVerified
		rank := team.SourceRank(team.SourceProjectMemory)
		if it.Scope == "user" {
			source = team.SourceUserMemory
			scope = team.ScopeUser
			trust = team.TrustVerified
			rank = team.SourceRank(team.SourceUserMemory)
		}
		if strings.TrimSpace(it.Verification) != "verified" {
			trust = team.TrustUntrusted
			rank = team.SourceRank(team.SourceBlockRecall)
		}
		_ = rank
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
