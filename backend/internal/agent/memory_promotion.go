package agent

// memory_promotion.go C-05 组织学习闭环（首版）：
// 复用 long_term_memory_items/events 与撤回/防复活，不新建第二套长期记忆。
// 团队经验只允许 project 作用域候选；默认 promotion_mode=shadow，不自动激活。

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/store"
)

// 晋升模式（memory.promotion_mode）。
const (
	PromotionModeOff     = "off"
	PromotionModeShadow  = "shadow"
	PromotionModeEnforce = "enforce"
)

// candidateSecretPattern 常见凭据形态（OpenAI/Anthropic key、Bearer、api_key=）。
var candidateSecretPattern = regexp.MustCompile(`(?i)(sk-[a-z0-9_-]{16,}|bearer\s+[a-z0-9._-]{16,}|(?:api[_-]?key|token|password|secret)\s*[:=]\s*\S{8,})`)

// ErrEvaluationUnavailable 未接线同预算 eval 时不得自动晋升。
var ErrEvaluationUnavailable = fmt.Errorf("candidate evaluation unavailable")

// TeamCandidate 团队经验候选（证据来自 B-03 done 结果与通过报告）。
type TeamCandidate struct {
	Owner                      string
	ProjectKey                 string
	MemoryKey                  string
	Content                    string
	RoleID                     string
	TaskType                   string
	SourceSession              string
	SourceTask                 string
	SourceAttempt              string
	SourceResultID             string
	SourceVerificationReportID string
	SpecVersion                string
	ContextManifestHash        string
	SourceStatus               string // 必须 completed/done
	VerificationStatus         string // 必须 passed
}

// PromotionConfig 晋升配置。
type PromotionConfig struct {
	Mode           string
	AutoActivate   bool
	MinEvalTasks   int
	MinEvalRepeats int
}

// MemoryPromotionService 候选/评估/晋升/反馈。
type MemoryPromotionService struct {
	lc  *MemoryLifecycle
	cfg PromotionConfig
	// Evaluator 可选同预算 baseline/treatment 评估器；nil 时晋升必须人工验证，不自动。
	Evaluator CandidateEvaluator
}

// CandidateEvaluator 同预算 shadow 评估接口（由专项 eval runner 实现）。
type CandidateEvaluator interface {
	Evaluate(ctx context.Context, item store.MemoryItem) (string, error)
}

// NewMemoryPromotionService 构造服务。
func NewMemoryPromotionService(lc *MemoryLifecycle, cfg PromotionConfig) *MemoryPromotionService {
	if cfg.Mode == "" {
		cfg.Mode = PromotionModeShadow
	}
	return &MemoryPromotionService{lc: lc, cfg: cfg}
}

// SubmitTeamCandidate 只创建 candidate + 证据事件，不进入正常注入。
func (s *MemoryPromotionService) SubmitTeamCandidate(ctx context.Context, c TeamCandidate) (*store.MemoryItem, error) {
	if s == nil || s.lc == nil || s.lc.repo == nil {
		return nil, fmt.Errorf("memory lifecycle not wired")
	}
	if s.cfg.Mode == PromotionModeOff {
		return nil, fmt.Errorf("promotion_mode=off: team candidates disabled")
	}
	content := strings.TrimSpace(c.Content)
	if content == "" {
		return nil, fmt.Errorf("team candidate: content required")
	}
	// 候选不得把密钥写进长期记忆；命中常见凭据形态直接脱敏。
	content = candidateSecretPattern.ReplaceAllString(content, "[redacted]")
	if len([]rune(content)) > 2000 {
		return nil, fmt.Errorf("team candidate: content exceeds 2000 runes")
	}
	if store.NormalizeProjectKey(c.ProjectKey) == "" {
		return nil, fmt.Errorf("team candidate: project_key required (project scope only)")
	}
	if c.SourceStatus != "completed" && c.SourceStatus != "done" {
		return nil, fmt.Errorf("team candidate: source result status=%q (want completed/done)", c.SourceStatus)
	}
	if c.VerificationStatus != "passed" {
		return nil, fmt.Errorf("team candidate: verification status=%q (want passed)", c.VerificationStatus)
	}
	item := store.MemoryItem{
		Owner: c.Owner, Scope: store.MemoryScopeProject,
		ProjectKey: store.NormalizeProjectKey(c.ProjectKey),
		MemoryKey:  strings.TrimSpace(c.MemoryKey), Content: content,
		Status: store.MemoryStatusCandidate, Origin: store.MemoryOriginTeamEvolver,
		Verification:  store.MemoryVerificationUnverified,
		SourceSession: c.SourceSession, RoleID: c.RoleID, TaskType: c.TaskType,
		SourceResultID: c.SourceResultID, SourceVerificationReportID: c.SourceVerificationReportID,
	}
	if item.MemoryKey == "" {
		item.MemoryKey = "team.experience"
	}
	item.ContentHash = store.MemoryContentHash(item.Content)
	blocked, reason, err := s.lc.CheckResurrectionStrict(ctx, item)
	if err != nil {
		return nil, fmt.Errorf("team candidate resurrection check failed (fail-closed): %w", err)
	}
	if blocked {
		return nil, fmt.Errorf("team candidate rejected (resurrection): %s", reason)
	}
	created, err := s.lc.repo.InsertItem(ctx, item)
	if err != nil {
		return nil, fmt.Errorf("insert team candidate: %w", err)
	}
	_ = s.lc.repo.AppendEvent(ctx, store.MemoryEvent{
		Owner: created.Owner, MemoryID: created.ID, EventType: "candidate_submitted",
		Actor: "team_evolver", SourceSession: c.SourceSession, Reason: c.SourceTask,
		Payload: promotionJSON(map[string]any{
			"role_id": c.RoleID, "task_type": c.TaskType, "result_id": c.SourceResultID,
			"verification_report_id": c.SourceVerificationReportID,
			"spec_version":           c.SpecVersion, "context_manifest_hash": c.ContextManifestHash,
		}),
	})
	s.lc.bumpRevision()
	return &created, nil
}

// EvaluateCandidate 调用同预算评估器；未接线返回 ErrEvaluationUnavailable。
func (s *MemoryPromotionService) EvaluateCandidate(ctx context.Context, owner, id string) (string, error) {
	if s == nil || s.Evaluator == nil {
		return "", ErrEvaluationUnavailable
	}
	item, err := s.lc.repo.GetItem(ctx, owner, id)
	if err != nil {
		return "", err
	}
	report, err := s.Evaluator.Evaluate(ctx, *item)
	if err != nil {
		return "", err
	}
	_ = s.lc.repo.AppendEvent(ctx, store.MemoryEvent{
		Owner: owner, MemoryID: id, EventType: "candidate_eval", Actor: "evaluator",
		Reason: "baseline/treatment 同预算评估", Payload: report,
	})
	return report, nil
}

// ExposureStats 聚合候选的曝光/结果反馈（只统计，不推断因果）。
func (s *MemoryPromotionService) ExposureStats(ctx context.Context, owner, id string) (map[string]int, error) {
	if s == nil || s.lc == nil || s.lc.repo == nil {
		return nil, fmt.Errorf("memory lifecycle not wired")
	}
	events, err := s.lc.repo.ListEvents(ctx, owner, id, 5000)
	if err != nil {
		return nil, err
	}
	stats := map[string]int{}
	for _, ev := range events {
		if ev.EventType != "exposure_outcome" {
			continue
		}
		var payload struct {
			Outcome string `json:"outcome"`
		}
		if json.Unmarshal([]byte(ev.Payload), &payload) == nil && payload.Outcome != "" {
			stats[payload.Outcome]++
		}
	}
	return stats, nil
}

// VerifyCandidate 人工/评估通过后置 verified（仍不激活）。
func (s *MemoryPromotionService) VerifyCandidate(ctx context.Context, owner, id string, expectedRevision int64, actor, reason string) (*store.MemoryItem, error) {
	if s == nil || s.lc == nil || s.lc.repo == nil {
		return nil, fmt.Errorf("memory lifecycle not wired")
	}
	it, err := s.lc.repo.GetItem(ctx, owner, id)
	if err != nil {
		return nil, err
	}
	if it.Status != store.MemoryStatusCandidate {
		return nil, fmt.Errorf("verify candidate: status=%s (want candidate)", it.Status)
	}
	if expectedRevision > 0 && it.Revision != expectedRevision {
		return nil, store.ErrMemoryRevisionConflict
	}
	verified, err := s.lc.repo.SetVerification(ctx, owner, id, store.MemoryVerificationVerified, it.Revision)
	if err != nil {
		return nil, err
	}
	_ = s.lc.repo.AppendEvent(ctx, store.MemoryEvent{
		Owner: owner, MemoryID: id, EventType: "candidate_verified", Actor: actor,
		Reason: reason, Payload: "{}",
	})
	s.lc.bumpRevision()
	return verified, nil
}

// PromoteCandidate 受控晋升：enforce + 人工（auto_activate=false）+ verified +
// 严格防复活 + 原子替换同 key active。shadow 只留事件不激活。
func (s *MemoryPromotionService) PromoteCandidate(ctx context.Context, owner, id string, expectedRevision int64, actor, reason string) (*store.MemoryItem, error) {
	if s == nil || s.lc == nil || s.lc.repo == nil {
		return nil, fmt.Errorf("memory lifecycle not wired")
	}
	if s.cfg.Mode != PromotionModeEnforce {
		_ = s.lc.repo.AppendEvent(ctx, store.MemoryEvent{
			Owner: owner, MemoryID: id, EventType: "promote_shadow", Actor: actor,
			Reason: "promotion_mode=" + s.cfg.Mode, Payload: "{}",
		})
		return nil, fmt.Errorf("promotion_mode=%s: candidate not activated (shadow recorded)", s.cfg.Mode)
	}
	if !s.cfg.AutoActivate && strings.EqualFold(strings.TrimSpace(actor), "auto") {
		return nil, fmt.Errorf("auto_activate=false: auto promotion rejected")
	}
	it, err := s.lc.repo.GetItem(ctx, owner, id)
	if err != nil {
		return nil, err
	}
	if it.Scope != store.MemoryScopeProject || it.ProjectKey == "" {
		return nil, fmt.Errorf("promotion: only project-scoped candidates may be promoted")
	}
	if it.Status != store.MemoryStatusCandidate {
		return nil, fmt.Errorf("promotion: item status=%s (want candidate)", it.Status)
	}
	if it.Verification != store.MemoryVerificationVerified {
		return nil, fmt.Errorf("promotion: candidate verification=%s (need verified)", it.Verification)
	}
	if expectedRevision > 0 && it.Revision != expectedRevision {
		return nil, store.ErrMemoryRevisionConflict
	}
	blocked, why, err := s.lc.CheckResurrectionStrict(ctx, *it)
	if err != nil {
		return nil, fmt.Errorf("promotion resurrection check failed (fail-closed): %w", err)
	}
	if blocked {
		return nil, fmt.Errorf("promotion rejected (resurrection): %s", why)
	}
	promoter, ok := s.lc.repo.(store.AtomicPromoter)
	if !ok {
		return nil, fmt.Errorf("promotion: repository lacks atomic promotion")
	}
	promoted, err := promoter.PromoteItem(ctx, owner, id, it.Revision, actor, reason)
	if err != nil {
		return nil, err
	}
	s.lc.bumpRevision()
	return promoted, nil
}

// RecordExposureOutcome 记录一次实际注入与任务结果（只记反馈，不定因果）。
func (s *MemoryPromotionService) RecordExposureOutcome(ctx context.Context, owner, id, outcome, taskID string, evidence map[string]any) error {
	if s == nil || s.lc == nil || s.lc.repo == nil {
		return fmt.Errorf("memory lifecycle not wired")
	}
	payload := map[string]any{"outcome": outcome, "task_id": taskID, "at": time.Now()}
	for k, v := range evidence {
		payload[k] = v
	}
	return s.lc.repo.AppendEvent(ctx, store.MemoryEvent{
		Owner: owner, MemoryID: id, EventType: "exposure_outcome",
		Actor: "system", Payload: promotionJSON(payload),
	})
}

// Quarantine/Retract/Rollback 复用现有生命周期（保留完整事件）。
func (s *MemoryPromotionService) Quarantine(ctx context.Context, owner, id, reason string) (*store.MemoryItem, error) {
	return s.lc.QuarantineByID(ctx, owner, id, reason)
}

func (s *MemoryPromotionService) Retract(ctx context.Context, owner, id, reason string) (*store.MemoryItem, error) {
	return s.lc.RetractByID(ctx, owner, id, reason)
}

// Rollback 撤回当前问题版本并恢复指定历史版本（显式人工操作）。
func (s *MemoryPromotionService) Rollback(ctx context.Context, owner, badID, goodID, reason string) error {
	if _, err := s.lc.RetractByID(ctx, owner, badID, reason); err != nil {
		return err
	}
	_, err := s.lc.RestoreByID(ctx, owner, goodID, "rollback: "+reason)
	return err
}

func promotionJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
