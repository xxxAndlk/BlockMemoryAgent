package agent

// memory_lifecycle.go —— 长期记忆生命周期（撤回归档即时化 + 有效视图注入 + 防复活）。
//
// 事故背景（doc/计划_长期记忆撤回与污染修复.md）：
//   - 用户画像/项目经验以 Markdown 自然语言行存储，撤回只能靠会话结束时的 LLM Merge；
//     会话取消/进程退出/合并失败时旧结论永远留在现行区，每轮继续注入；
//   - memory.write_mode=shadow/off 提前 return，把"新候选写入"和"用户明确撤回"绑在一起，
//     结果是 shadow 防住了新污染，却让已被用户推翻的旧条目一直 active；
//   - 归档小节会被容量裁剪（默认 20 条），不能兼任永久墓碑；
//   - 没有条目 ID，无法证明用户撤回的正是这一条。
//
// 本文件实现：
//   1. 当轮撤回：真实用户消息入口（sendMessage/runSession/enqueue/answerClarify）判断
//      "明确撤回 / 同主题替换 / 疑似先隔离"，立刻改条目状态（不等会话结束）；
//   2. 撤回归撤回归：撤回能力与 write_mode 解耦（独立 memory.retraction_mode）；
//   3. 有效视图：注入只读 status=active 的条目（带紧凑 ID），撤回/隔离的条目物理缺席；
//   4. 永久防复活：候选激活前查 items/events 的 tombstone（hash 相同 → 拒绝；
//      同 key 且语义近似 → resurrection_suspect，不自动 active）。
//
// 语义边界（计划 §5.3）：
//   A 可直接撤回/替换：明确 ID、精确文本、或"以后/本项目不再 X 改为 Y"且同 key 唯一；
//   B 先隔离再确认：仅语义相似、模型无法保证指哪一条 → quarantined（立即不注入）；
//   C 不改长期记忆："这次/本次/当前"、产物修改反馈（"幅度太小"）、无对象的"太差"
//     （只把本会话来源的未验证条目标 verification=rejected）。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/blockmemory/agent/backend/internal/store"
)

// 撤回模式（memory.retraction_mode）：与 write_mode 解耦——write_mode 管"新候选是否
// 自动晋升"，retraction_mode 管"用户明确撤回要不要执行"。后者不得随前者关闭。
const (
	RetractionModeEnforce = "enforce"
	RetractionModeOff     = "off"
)

// memoryKeyGroups 记忆主题键分类（同一 scope+project_key+memory_key 才允许自动替换）。
// 键的粒度：一件事一个键；通用工艺经验统一走 lesson.* 前缀，不硬塞进不相关主题。
var memoryKeyGroups = []struct {
	key     string
	markers []string
}{
	{"animation.method", []string{"抽帧", "逐帧", "帧生成", "图集", "序列帧", "关键姿态", "骨架", "拼接", "骨骼", "蒙皮", "形变", "warp", "remap", "视频生成", "动画方式", "动作资产"}},
	{"asset.source", []string{"生图", "生成图", "已有素材", "现有素材", "ai画", "素材来源", "重新生成", "拆部位", "参考图"}},
	{"asset.identity", []string{"角色合同", "角色设定", "立绘", "形象设计", "参考资产", "锚点图", "同一只角色"}},
	{"animation.anchor_approval", []string{"锚点审批", "先给我看", "审批通过", "关键姿态审批"}},
	{"response.language", []string{"中文", "英文", "语言", "reply in english"}},
	{"response.confirmation", []string{"不要问我", "别问我", "先问我", "确认后再", "向我确认"}},
	{"response.style", []string{"简洁", "详细", "结论先行", "沟通风格"}},
	{"delivery.form", []string{"交付形式", "交付成", "成品形态", "打包成"}},
	{"project.package_manager", []string{"npm", "pnpm", "yarn", "包管理器"}},
	{"project.test_runner", []string{"pytest", "jest", "测试框架", "测试运行器"}},
}

// memoryLongTermMarkers 明确长期意图（撤回/替换要落到长期记忆而不是会话）。
var memoryLongTermMarkers = []string{
	"以后", "今后", "始终", "永远", "长期", "永久", "全局", "所有项目", "记住",
}

// memoryProjectMarkers 本项目作用域标记。
var memoryProjectMarkers = []string{
	"这个项目", "本项目", "该项目", "项目里", "项目内", "当前项目", "这个仓库", "本仓库",
}

// memoryOneShotMarkers 一次性任务标记（配合无长期意图 → 只改会话决策）。
var memoryOneShotMarkers = []string{
	"这次", "本次", "当前这轮", "先这样", "暂时", "先用", "这个角色", "这张图", "这套素材",
}

// memoryRetractMarkers 明确撤回标记（强标记，避免产物反馈被误判成撤回长期记忆）。
var memoryRetractMarkers = []string{
	"以后不要", "以后不用", "以后不走", "不要再", "不再用", "不再走", "别再用", "不要再用",
	"停止使用", "撤回", "忘掉", "删掉", "作废", "失效", "是错的", "这条经验不对",
	"经验错了", "不要再提", "不许再", "别再",
}

// memoryReplaceMarkers 替换标记（"改成 Y"）。
var memoryReplaceMarkers = []string{"改成", "改为", "换成", "替换为", "改用", "改走"}

// memoryBadOutcomeMarkers "效果差"类反馈：只标 verification=rejected，不撤回无关条目。
var memoryBadOutcomeMarkers = []string{
	"太差", "做的什么玩意", "什么玩意", "垃圾", "难看", "不能用", "重做", "跑偏",
}

// memoryNegationObjectRe 匹配"不要…了/不用…了/别用…吧"这类**带对象**的否定期望。
// 单独一个"不要"不足以判撤回（"不要紧"），必须后接对象并在 12 runes 内收尾，
// 才能把"这个项目不要视频抽帧了"识别为撤回、把"不要太啰嗦"留给样式类判定。
var memoryNegationObjectRe = regexp.MustCompile(`不(要|用|走|再|能)[^，。；,;！!？?
]{0,12}(了|吧|啦)`)

// memoryExplicitIDRe 匹配用户消息里显式引用的条目 ID（M-123 / M-1792...）。
var memoryExplicitIDRe = regexp.MustCompile(`\bM-\d{6,}\b`)

// memoryItemIDPattern 便于外呼校验 ID 格式。
func memoryItemIDPattern(id string) bool { return memoryExplicitIDRe.MatchString(id) }

// MemoryMatcherFunc 可选语义匹配器（轻量模型）：给候选条目与用户原话，返回结构化操作。
// 模型不允许输出 SQL/Markdown 整段；Go 侧逐条校验 scope/key/ID/revision 后再执行。
type MemoryMatcherFunc func(ctx context.Context, req MemoryMatchRequest) (*MemoryMatchResult, error)

// MemoryMatchRequest 语义匹配输入。
type MemoryMatchRequest struct {
	Text       string            // 用户原话（只取真实用户消息）
	ScopeHint  string            // user | project | ""
	ProjectKey string            // 项目键（scope=project 时）
	Candidates []MemoryCandidate // 候选条目（active/quarantined）
}

// MemoryCandidate 供模型判断的紧凑条目视图。
type MemoryCandidate struct {
	ID        string `json:"id"`
	Scope     string `json:"scope"`
	MemoryKey string `json:"memory_key"`
	Content   string `json:"content"`
}

// MemoryMatchResult 模型输出的结构化操作（不含任何文本替换权）。
type MemoryMatchResult struct {
	// Ops 允许的动作：retract / quarantine / supersede（需 NewContent）。
	Ops []MemoryMatchOp `json:"ops"`
	// Uncertain 模型认为相关但不敢确认的条目 ID → 走"先隔离"。
	Uncertain []string `json:"uncertain"`
	Note      string   `json:"note"`
}

// MemoryMatchOp 单条操作建议。
type MemoryMatchOp struct {
	Action     string `json:"action"`
	ItemID     string `json:"item_id"`
	NewContent string `json:"new_content"`
	MemoryKey  string `json:"memory_key"`
	Reason     string `json:"reason"`
}

// MemoryScopeContext 一次记忆操作的作用域上下文。
type MemoryScopeContext struct {
	Owner      string // 多租户预留（当前单租户为空串）
	ProjectKey string // 规范化工作目录
	SessionID  string
}

// MemoryReport 当轮撤回处理结果（用于事件留痕与终答提示，失败不得假报成功）。
type MemoryReport struct {
	Handled           bool     `json:"handled"`
	Lines             []string `json:"lines"`              // 人类可读结果（"已撤回 M-123：…"）
	ReconcileRequired bool     `json:"reconcile_required"` // 明确要撤回但无法定位目标
	Revision          int64    `json:"revision"`           // 处理后有效视图版本
}

// String 渲染为注入/事件用的紧凑文本。
func (r MemoryReport) String() string {
	if len(r.Lines) == 0 {
		return ""
	}
	return strings.Join(r.Lines, "；")
}

// MemoryLifecycle 长期记忆生命周期服务。
//
// 并发：ops 串行化（mu），有效视图读取走 repo（PG 实现自带一致性），
// 本地操作后递增 revision，使注入侧缓存失效。
type MemoryLifecycle struct {
	mu            sync.Mutex
	repo          store.MemoryRepo
	retractMode   string
	matcher       MemoryMatcherFunc
	revision      int64
	lastRefresh   time.Time
	owner         string
	blockCache    map[string]cachedMemoryBlock
	importedPaths map[string]bool
}

type cachedMemoryBlock struct {
	revision int64
	text     string
	at       time.Time
}

// NewMemoryLifecycle 构造服务。repo 为 nil 时全部方法退化为零行为（测试/未装配）。
func NewMemoryLifecycle(repo store.MemoryRepo, retractionMode string) *MemoryLifecycle {
	mode := strings.ToLower(strings.TrimSpace(retractionMode))
	if mode == "" {
		mode = RetractionModeEnforce
	}
	return &MemoryLifecycle{
		repo:          repo,
		retractMode:   mode,
		blockCache:    map[string]cachedMemoryBlock{},
		importedPaths: map[string]bool{},
	}
}

// SetMatcher 注入可选语义匹配器（bootstrap 接轻量模型）。
func (lc *MemoryLifecycle) SetMatcher(fn MemoryMatcherFunc) { lc.matcher = fn }

// SetOwner 设置默认 owner（当前单租户空串，保留多租户接口）。
func (lc *MemoryLifecycle) SetOwner(owner string) { lc.owner = owner }

// SetRetractionMode 热切换撤回模式（config 热更新/TODO 回滚口）。
func (lc *MemoryLifecycle) SetRetractionMode(mode string) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	m := strings.ToLower(strings.TrimSpace(mode))
	if m == "" {
		m = RetractionModeEnforce
	}
	lc.retractMode = m
}

// Repo 暴露仓储（管理 API/测试用）。
func (lc *MemoryLifecycle) Repo() store.MemoryRepo {
	if lc == nil {
		return nil
	}
	return lc.repo
}

// Enabled 撤回服务是否可用（repo 已装配且模式未关闭）。
func (lc *MemoryLifecycle) Enabled() bool {
	if lc == nil || lc.repo == nil {
		return false
	}
	lc.mu.Lock()
	defer lc.mu.Unlock()
	return lc.retractMode != RetractionModeOff
}

// scopeOf 返回 owner（空则用默认 owner）。
func (lc *MemoryLifecycle) scopeOf(sc MemoryScopeContext) MemoryScopeContext {
	if sc.Owner == "" {
		sc.Owner = lc.owner
	}
	// 项目键一律规范化（大小写/斜杠差异不得串项目，计划 §7.3）。
	sc.ProjectKey = store.NormalizeProjectKey(sc.ProjectKey)
	return sc
}

// ---------------------------------------------------------------- 当轮撤回

// HandleUserMessage 处理一条**真实用户消息**的长期记忆撤回/替换/隔离。
// 返回处理结果；未命中长期记忆操作时 Handled=false（零副作用）。
//
// 只应由用户消息入口调用——Agent 文本、工具结果、压缩摘要没有撤回权。
func (lc *MemoryLifecycle) HandleUserMessage(ctx context.Context, sc MemoryScopeContext, text, sourceMessage string) MemoryReport {
	if !lc.Enabled() {
		return MemoryReport{}
	}
	sc = lc.scopeOf(sc)
	text = strings.TrimSpace(text)
	if text == "" {
		return MemoryReport{}
	}
	// C 档：一次性/产物反馈 → 不改长期记忆。
	intent := classifyMemoryIntent(text)
	if intent.oneShot {
		return MemoryReport{Handled: false}
	}
	if intent.action == "" {
		if intent.badOutcome {
			return lc.rejectUnverifiedSessionItems(ctx, sc, sourceMessage)
		}
		return MemoryReport{}
	}

	items, err := lc.listScopeItems(ctx, sc, intent.scope)
	if err != nil {
		log.Printf("[memory-lifecycle] list items failed: %v", err)
		return MemoryReport{ReconcileRequired: true, Lines: []string{"记忆撤回服务暂时不可用：本轮未修改长期记忆，旧条目仍在注入视图（请稍后重试）"}}
	}
	report := lc.applyDeterministic(ctx, sc, intent, items, text, sourceMessage)
	if report.Handled || report.ReconcileRequired {
		report.Revision = lc.bumpRevision()
		return report
	}
	// 确定性路径未命中 → 可选语义匹配器（无法定位则留 reconcile 提示，不假报成功）。
	return lc.applySemantic(ctx, sc, intent, items, text, sourceMessage)
}

// listScopeItems 读取候选条目：intent.scope=project 只取该项目；user 只取全局；
// 空 = 两者都取（先项目后全局）。
func (lc *MemoryLifecycle) listScopeItems(ctx context.Context, sc MemoryScopeContext, scope string) ([]store.MemoryItem, error) {
	statuses := []string{store.MemoryStatusActive, store.MemoryStatusQuarantined}
	var out []store.MemoryItem
	if scope == "" || scope == store.MemoryScopeProject {
		if sc.ProjectKey != "" {
			list, err := lc.repo.ListItems(ctx, store.MemoryQuery{
				Owner: sc.Owner, Scope: store.MemoryScopeProject,
				ProjectKey: sc.ProjectKey, Statuses: statuses,
			})
			if err != nil {
				return nil, err
			}
			out = append(out, list...)
		}
	}
	if scope == "" || scope == store.MemoryScopeUser {
		list, err := lc.repo.ListItems(ctx, store.MemoryQuery{
			Owner: sc.Owner, Scope: store.MemoryScopeUser, Statuses: statuses,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, list...)
	}
	return out, nil
}

// applyDeterministic A 档：明确 ID / 精确文本 / 同 key 唯一 → 撤回或替换；
// 多候选且无法区分 → 先隔离（B 档，已确认撤回意图但目标不明）。
func (lc *MemoryLifecycle) applyDeterministic(ctx context.Context, sc MemoryScopeContext, intent memoryIntent, items []store.MemoryItem, text, sourceMessage string) MemoryReport {
	var report MemoryReport

	// 1) 显式条目 ID：用户直接点名，最高确定性。
	if ids := memoryExplicitIDRe.FindAllString(text, -1); len(ids) > 0 {
		for _, id := range uniqueStrings(ids) {
			item := findItemByID(items, id)
			if item == nil {
				// ID 不在候选里：可能已撤回或不属于该作用域——如实报告，不假装成功。
				report.Lines = append(report.Lines, fmt.Sprintf("%s 不在当前有效/候选中（可能已撤回或属于其他作用域）", id))
				report.ReconcileRequired = true
				continue
			}
			if lc.transition(ctx, sc, *item, store.MemoryStatusRetracted, store.MemoryActorUser, text, sourceMessage) == nil {
				report.Handled = true
				report.Lines = append(report.Lines, fmt.Sprintf("已撤回 %s（%s）：%s", item.ID, item.MemoryKey, truncateMemoryText(item.Content, 80)))
			}
		}
		if report.Handled {
			return report
		}
	}

	// 2) 精确文本匹配：用户复述了某条原文（≥6 runes，避免"不要视频"误配短条目）。
	if hit := matchItemByExactContent(items, text); hit != nil {
		if lc.transition(ctx, sc, *hit, store.MemoryStatusRetracted, store.MemoryActorUser, text, sourceMessage) == nil {
			report.Handled = true
			report.Lines = append(report.Lines, fmt.Sprintf("已撤回 %s（%s）：%s", hit.ID, hit.MemoryKey, truncateMemoryText(hit.Content, 80)))
			return report
		}
	}

	// 3) 同 key 匹配（用户说"以后不要 X"这类，X 落在已登记的主题键上）。
	keys := classifyMemoryKeys(text)
	candidates := filterItemsByKeys(items, keys)
	if len(candidates) == 0 {
		// key 未命中 → 用相似度兜底（≥0.35 的唯一最高分）。
		candidates = items
	}
	ranked := rankItemsBySimilarity(candidates, text)

	switch intent.action {
	case memoryActionRetract:
		if len(ranked) == 1 || (len(ranked) > 1 && ranked[0].score >= 0.45 && ranked[0].score >= ranked[1].score+0.15) {
			item := ranked[0].item
			if lc.transition(ctx, sc, item, store.MemoryStatusRetracted, store.MemoryActorUser, text, sourceMessage) == nil {
				report.Handled = true
				report.Lines = append(report.Lines, fmt.Sprintf("已撤回 %s（%s）：%s", item.ID, item.MemoryKey, truncateMemoryText(item.Content, 80)))
			}
			return report
		}
		if len(ranked) > 1 {
			// B 档：确认要撤回，但无法确定是哪一条 → 全部先隔离（立即不注入），交给用户确认。
			var ids []string
			for _, r := range ranked {
				if lc.transition(ctx, sc, r.item, store.MemoryStatusQuarantined, store.MemoryActorSystem, "撤回目标不唯一，先隔离："+truncateMemoryText(text, 60), sourceMessage) == nil {
					ids = append(ids, r.item.ID)
				}
			}
			if len(ids) > 0 {
				report.Handled = true
				report.Lines = append(report.Lines, fmt.Sprintf("撤回目标不唯一，已暂停注入 %s（可回复条目 ID 精确撤回，或说明要保留哪条）", strings.Join(ids, "、 ")))
			}
			return report
		}
		report.ReconcileRequired = true
		report.Lines = append(report.Lines, "未找到与本次撤回对应的长期记忆条目（可能已撤回，或只属于当前会话决策）")
		return report

	case memoryActionReplace:
		// 同 key 全部 supersede + 新建 active（新表述）；无 key 时退回相似度最高者。
		targetKey := ""
		if len(keys) > 0 {
			targetKey = keys[0]
		} else if len(ranked) > 0 {
			targetKey = ranked[0].item.MemoryKey
		}
		if targetKey == "" {
			report.ReconcileRequired = true
			report.Lines = append(report.Lines, "收到替换指令，但无法确定要替换哪条长期记忆（未匹配到主题或旧条目）")
			return report
		}
		for _, it := range items {
			if it.MemoryKey != targetKey || it.Scope != intent.scopeOrDefault() {
				continue
			}
			if it.Status != store.MemoryStatusActive {
				continue
			}
			_ = lc.transition(ctx, sc, it, store.MemoryStatusSuperseded, store.MemoryActorUser, "被新指令替换："+truncateMemoryText(text, 80), sourceMessage)
		}
		newItem := store.MemoryItem{
			Owner: sc.Owner, Scope: intent.scopeOrDefault(), ProjectKey: projectKeyFor(intent.scopeOrDefault(), sc),
			MemoryKey: targetKey, Content: intent.newContent, Status: store.MemoryStatusActive,
			Origin: store.MemoryOriginExplicitTool, Verification: store.MemoryVerificationVerified,
			SourceSession: sc.SessionID, SourceMessage: sourceMessage,
		}
		created, blocked, reason := lc.insertChecked(ctx, newItem)
		if blocked {
			report.Lines = append(report.Lines, "替换指令命中了已撤回的旧结论（resurrection_suspect），新表述先记为候选，未进入现行记忆："+reason)
			report.Handled = true
			return report
		}
		report.Handled = true
		extra := ""
		if created.ID != "" {
			extra = fmt.Sprintf("；新条目 %s（%s）", created.ID, targetKey)
		}
		report.Lines = append(report.Lines, fmt.Sprintf("已替换 %s 轴上的旧条目%s：%s", targetKey, extra, truncateMemoryText(intent.newContent, 80)))
		return report
	}
	return report
}

// applySemantic 确定性路径失败时的可选语义匹配（模型只建议，Go 校验后执行）。
func (lc *MemoryLifecycle) applySemantic(ctx context.Context, sc MemoryScopeContext, intent memoryIntent, items []store.MemoryItem, text, sourceMessage string) MemoryReport {
	if lc.matcher == nil || len(items) == 0 {
		return MemoryReport{}
	}
	cands := make([]MemoryCandidate, 0, len(items))
	for _, it := range items {
		cands = append(cands, MemoryCandidate{ID: it.ID, Scope: it.Scope, MemoryKey: it.MemoryKey, Content: it.Content})
	}
	mctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	res, err := lc.matcher(mctx, MemoryMatchRequest{
		Text: text, ScopeHint: intent.scope, ProjectKey: sc.ProjectKey, Candidates: cands,
	})
	if err != nil || res == nil {
		log.Printf("[memory-lifecycle] matcher failed/empty: %v", err)
		return MemoryReport{}
	}
	var report MemoryReport
	byID := map[string]store.MemoryItem{}
	for _, it := range items {
		byID[it.ID] = it
	}
	for _, id := range uniqueStrings(res.Uncertain) {
		if it, ok := byID[id]; ok {
			if lc.transition(ctx, sc, it, store.MemoryStatusQuarantined, store.MemoryActorSystem, "语义匹配不确定，先隔离："+truncateMemoryText(text, 60), sourceMessage) == nil {
				report.Handled = true
				report.Lines = append(report.Lines, fmt.Sprintf("疑似过期，已暂停注入 %s（%s）：%s", it.ID, it.MemoryKey, truncateMemoryText(it.Content, 60)))
			}
		}
	}
	for _, op := range res.Ops {
		it, ok := byID[op.ItemID]
		if !ok {
			continue // 模型编造的 ID：忽略，不采信
		}
		action := strings.ToLower(strings.TrimSpace(op.Action))
		switch action {
		case "retract":
			if lc.transition(ctx, sc, it, store.MemoryStatusRetracted, store.MemoryActorUser, op.Reason, sourceMessage) == nil {
				report.Handled = true
				report.Lines = append(report.Lines, fmt.Sprintf("已撤回 %s（%s）：%s", it.ID, it.MemoryKey, truncateMemoryText(it.Content, 60)))
			}
		case "quarantine":
			if lc.transition(ctx, sc, it, store.MemoryStatusQuarantined, store.MemoryActorSystem, op.Reason, sourceMessage) == nil {
				report.Handled = true
				report.Lines = append(report.Lines, fmt.Sprintf("已隔离 %s（%s）：%s", it.ID, it.MemoryKey, truncateMemoryText(it.Content, 60)))
			}
		case "supersede":
			newContent := strings.TrimSpace(op.NewContent)
			key := strings.TrimSpace(op.MemoryKey)
			if key == "" {
				key = it.MemoryKey
			}
			if newContent == "" || key != it.MemoryKey {
				continue // 只允许同主题替换（跨主题必须走确定性路径）
			}
			if lc.transition(ctx, sc, it, store.MemoryStatusSuperseded, store.MemoryActorUser, op.Reason, sourceMessage) == nil {
				created, blocked, reason := lc.insertChecked(ctx, store.MemoryItem{
					Owner: sc.Owner, Scope: it.Scope, ProjectKey: it.ProjectKey, MemoryKey: key,
					Content: newContent, Status: store.MemoryStatusActive, Origin: store.MemoryOriginExplicitTool,
					Verification: store.MemoryVerificationVerified, SourceSession: sc.SessionID, SourceMessage: sourceMessage,
				})
				report.Handled = true
				msg := fmt.Sprintf("已替换 %s（%s）为：%s", it.ID, key, truncateMemoryText(newContent, 60))
				if blocked {
					msg = fmt.Sprintf("已替换 %s，但新表述命中已撤回内容（%s），记候选未激活", it.ID, reason)
				} else if created.ID != "" {
					msg += "（新条目 " + created.ID + "）"
				}
				report.Lines = append(report.Lines, msg)
			}
		}
	}
	report.Revision = lc.bumpRevision()
	return report
}

// rejectUnverifiedSessionItems C 档"效果太差"：只把**本会话来源、未验证**的条目标
// verification=rejected（不按关键词清空全部项目经验）。
func (lc *MemoryLifecycle) rejectUnverifiedSessionItems(ctx context.Context, sc MemoryScopeContext, sourceMessage string) MemoryReport {
	if sc.SessionID == "" {
		return MemoryReport{}
	}
	items, err := lc.listScopeItems(ctx, sc, "")
	if err != nil {
		return MemoryReport{}
	}
	var report MemoryReport
	for _, it := range items {
		if it.SourceSession != sc.SessionID || it.Verification != store.MemoryVerificationUnverified {
			continue
		}
		if _, err := lc.repo.SetVerification(ctx, it.Owner, it.ID, store.MemoryVerificationRejected, it.Revision); err != nil {
			continue
		}
		_ = lc.repo.AppendEvent(ctx, store.MemoryEvent{
			Owner: it.Owner, MemoryID: it.ID, EventType: store.MemoryEventVerificationSet,
			Actor: store.MemoryActorUser, SourceSession: sc.SessionID, SourceMessage: sourceMessage,
			Reason: "用户反馈成果不达标：本会话来源的未验证条目标 rejected（不撤回无关经验）",
		})
		report.Handled = true
		report.Lines = append(report.Lines, fmt.Sprintf("已标记 %s 为未验证通过（verification=rejected）", it.ID))
	}
	return report
}

// RecordExplicitItem 把一条用户显式写入（remember_preference / 项目约定工具）落成条目。
// status=active，但先过防复活检查：命中已撤回结论则落 candidate（不进入注入视图）。
func (lc *MemoryLifecycle) RecordExplicitItem(ctx context.Context, sc MemoryScopeContext, scope, origin, content string) {
	if lc == nil || lc.repo == nil || strings.TrimSpace(content) == "" {
		return
	}
	sc = lc.scopeOf(sc)
	if scope != store.MemoryScopeProject {
		scope = store.MemoryScopeUser
	}
	key := ""
	if keys := classifyMemoryKeys(content); len(keys) > 0 {
		key = keys[0]
	} else {
		key = "lesson.general"
	}
	pk := projectKeyFor(scope, sc)
	item := store.MemoryItem{
		Owner: sc.Owner, Scope: scope, ProjectKey: pk, MemoryKey: key,
		Content: strings.TrimSpace(content), Status: store.MemoryStatusActive,
		Origin: origin, Verification: store.MemoryVerificationVerified,
		SourceSession: sc.SessionID, ContentHash: store.MemoryContentHash(content),
	}
	created, blocked, reason := lc.insertChecked(ctx, item)
	if blocked {
		log.Printf("[memory-lifecycle] explicit item blocked by tombstone (kept candidate): %s", reason)
		return
	}
	if created.ID != "" {
		lc.bumpRevision()
	}
}

// transition 状态迁移 + 事件（同一逻辑事务：先状态后事件，失败只告警）。
func (lc *MemoryLifecycle) transition(ctx context.Context, sc MemoryScopeContext, item store.MemoryItem, status, actor, reason, sourceMessage string) error {
	updated, err := lc.repo.SetStatus(ctx, item.Owner, item.ID, status, item.Revision)
	if err != nil {
		log.Printf("[memory-lifecycle] set status %s → %s failed: %v", item.ID, status, err)
		return err
	}
	eventType := store.MemoryEventQuarantined
	switch status {
	case store.MemoryStatusRetracted:
		eventType = store.MemoryEventRetracted
	case store.MemoryStatusSuperseded:
		eventType = store.MemoryEventSuperseded
	case store.MemoryStatusActive:
		eventType = store.MemoryEventRestored
	}
	payload, _ := json.Marshal(map[string]any{
		"from": item.Status, "to": status, "memory_key": item.MemoryKey,
		"scope": item.Scope, "project_key": item.ProjectKey, "revision": updated.Revision,
	})
	_ = lc.repo.AppendEvent(ctx, store.MemoryEvent{
		Owner: item.Owner, MemoryID: item.ID, EventType: eventType, Actor: actor,
		SourceSession: sc.SessionID, SourceMessage: sourceMessage,
		Reason: truncateMemoryText(reason, 240), Payload: string(payload),
	})
	return nil
}

// insertChecked 插入条目并做防复活检查：命中 tombstone → 降级为 candidate（不注入）。
func (lc *MemoryLifecycle) insertChecked(ctx context.Context, item store.MemoryItem) (store.MemoryItem, bool, string) {
	if blocked, reason := lc.CheckResurrection(ctx, item); blocked {
		item.Status = store.MemoryStatusCandidate
		item.Origin = store.MemoryOriginEvolver
		created, err := lc.repo.InsertItem(ctx, item)
		_ = lc.repo.AppendEvent(ctx, store.MemoryEvent{
			Owner: item.Owner, MemoryID: created.ID, EventType: store.MemoryEventResurrectionHit,
			Actor: store.MemoryActorSystem, SourceSession: item.SourceSession, SourceMessage: item.SourceMessage,
			Reason:  truncateMemoryText(reason, 240),
			Payload: `{"blocked_status":"active"}`,
		})
		if err != nil {
			log.Printf("[memory-lifecycle] insert candidate failed: %v", err)
			return store.MemoryItem{}, true, reason
		}
		return created, true, reason
	}
	created, err := lc.repo.InsertItem(ctx, item)
	if err != nil {
		log.Printf("[memory-lifecycle] insert item failed: %v", err)
		return store.MemoryItem{}, false, ""
	}
	_ = lc.repo.AppendEvent(ctx, store.MemoryEvent{
		Owner: created.Owner, MemoryID: created.ID, EventType: store.MemoryEventActivated,
		Actor: store.MemoryActorUser, SourceSession: created.SourceSession, SourceMessage: created.SourceMessage,
		Reason: "激活新条目（" + created.Origin + "）",
	})
	return created, false, ""
}

// ---------------------------------------------------------------- 防复活

// CheckResurrection 候选激活前的永久防复活检查（查 items/events，不查会被裁剪的 Markdown 归档）：
//  1. 同 hash：与已撤回/已替换/隔离条目规范化后完全一致 → 拒绝激活；
//  2. 同 key + 语义近似（Jaccard ≥ 0.6）→ resurrection_suspect，拒绝激活；
//  3. 归档不是权威：只看数据库。
func (lc *MemoryLifecycle) CheckResurrection(ctx context.Context, item store.MemoryItem) (bool, string) {
	if lc == nil || lc.repo == nil {
		return false, ""
	}
	hash := item.ContentHash
	if hash == "" {
		hash = store.MemoryContentHash(item.Content)
	}
	dead, err := lc.repo.ListItems(ctx, store.MemoryQuery{
		Owner: item.Owner, Scope: item.Scope, ProjectKey: item.ProjectKey,
		Statuses: []string{store.MemoryStatusRetracted, store.MemoryStatusSuperseded, store.MemoryStatusQuarantined},
	})
	if err != nil {
		log.Printf("[memory-lifecycle] resurrection check failed (fail-open): %v", err)
		return false, ""
	}
	for _, it := range dead {
		if it.ContentHash == hash {
			return true, fmt.Sprintf("与已 %s 条目 %s 内容完全相同（换排版/标点不算新结论）", it.Status, it.ID)
		}
		if it.MemoryKey == item.MemoryKey {
			if sim := store.MemoryTextSimilarity(it.Content, item.Content); sim >= 0.6 {
				return true, fmt.Sprintf("与已 %s 条目 %s 语义高度近似（%.2f），疑似旧结论换措辞复活", it.Status, it.ID, sim)
			}
		}
	}
	return false, ""
}

// ScreenCandidates 供 SessionEvolver 在 enforce 激活前批量过滤候选（防复活）。
// 返回被拦截的候选索引与原因（调用方只保留未拦截者）。
func (lc *MemoryLifecycle) ScreenCandidates(ctx context.Context, sc MemoryScopeContext, scope string, candidates []string, memoryKey string) (keep []string, blocked []string) {
	if !lc.Enabled() || scope == "" {
		return candidates, nil
	}
	sc = lc.scopeOf(sc)
	pk := projectKeyFor(scope, sc)
	for _, c := range candidates {
		blockedThis, reason := lc.CheckResurrection(ctx, store.MemoryItem{
			Owner: sc.Owner, Scope: scope, ProjectKey: pk, MemoryKey: memoryKey, Content: c,
			ContentHash: store.MemoryContentHash(c),
		})
		if blockedThis {
			blocked = append(blocked, c+" ← "+reason)
			continue
		}
		keep = append(keep, c)
	}
	return keep, blocked
}

// ---------------------------------------------------------------- 有效视图

// RenderScope 渲染指定作用域的有效视图（只读 active；带紧凑 ID 供用户点名撤回）。
// projectKey 为空时只渲染全局条目。返回空串表示无内容（零注入）。
func (lc *MemoryLifecycle) RenderScope(ctx context.Context, owner, projectKey, scope string, maxRunes int) string {
	if lc == nil || lc.repo == nil {
		return ""
	}
	if maxRunes <= 0 {
		maxRunes = 1800
	}
	// 注入视图每轮都要构建（MetaAgent/子 Agent 各自一次），给 3s 短缓存：
	// 撤回/替换会 bumpRevision 立刻失效，跨进程写入最多迟 3s（可接受）。
	cacheKey := fmt.Sprintf("scope|%s|%s|%s|%d", owner, projectKey, scope, maxRunes)
	if hit, ok := lc.cachedBlock(cacheKey); ok {
		return hit
	}
	items, err := lc.repo.ListItems(ctx, store.MemoryQuery{
		Owner: owner, Scope: scope, ProjectKey: projectKey,
		Statuses: []string{store.MemoryStatusActive},
	})
	if err != nil {
		log.Printf("[memory-lifecycle] render scope %s failed: %v", scope, err)
		return ""
	}
	if scope == store.MemoryScopeProject {
		items = dedupeByMemoryKey(items)
	}
	out := renderMemoryItems(items, scope, maxRunes)
	lc.storeBlock(cacheKey, out)
	return out
}

// EffectiveBlock 渲染合并后的有效视图（项目条目覆盖同 key 的全局条目）。
// 用于需要一份统一记忆块的注入点（子 Agent 前缀）。
func (lc *MemoryLifecycle) EffectiveBlock(ctx context.Context, owner, projectKey string, maxRunes int) string {
	if lc == nil || lc.repo == nil {
		return ""
	}
	if maxRunes <= 0 {
		maxRunes = 2200
	}
	cacheKey := fmt.Sprintf("effective|%s|%s|%d", owner, projectKey, maxRunes)
	if hit, ok := lc.cachedBlock(cacheKey); ok {
		return hit
	}
	var all []store.MemoryItem
	for _, scope := range []string{store.MemoryScopeProject, store.MemoryScopeUser} {
		pk := ""
		if scope == store.MemoryScopeProject {
			pk = projectKey
		}
		items, err := lc.repo.ListItems(ctx, store.MemoryQuery{
			Owner: owner, Scope: scope, ProjectKey: pk,
			Statuses: []string{store.MemoryStatusActive},
		})
		if err != nil {
			log.Printf("[memory-lifecycle] effective block %s failed: %v", scope, err)
			return ""
		}
		all = append(all, dedupeByMemoryKey(items)...)
	}
	// 项目条目优先（更具体），同 key 的项目条目把全局条目挤掉。
	out := renderMemoryItems(all, "effective", maxRunes)
	lc.storeBlock(cacheKey, out)
	return out
}

// cachedBlock 读取注入视图缓存（revision 变化或超过 3s 即失效）。
func (lc *MemoryLifecycle) cachedBlock(key string) (string, bool) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	c, ok := lc.blockCache[key]
	if !ok || c.revision != lc.revision || time.Since(c.at) > 3*time.Second {
		return "", false
	}
	return c.text, true
}

// storeBlock 写入注入视图缓存。
func (lc *MemoryLifecycle) storeBlock(key, text string) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	lc.blockCache[key] = cachedMemoryBlock{revision: lc.revision, text: text, at: time.Now()}
}

// renderMemoryItems 渲染条目（带 ID/作用域，预算内按必要性裁剪，不整体硬截断）。
func renderMemoryItems(items []store.MemoryItem, scopeLabel string, maxRunes int) string {
	if len(items) == 0 {
		return ""
	}
	sort.SliceStable(items, func(i, j int) bool {
		// project 优先，其次 veirfied，再按更新时间新→旧。
		if items[i].Scope != items[j].Scope {
			return items[i].Scope == store.MemoryScopeProject
		}
		if items[i].Verification != items[j].Verification {
			return items[i].Verification == store.MemoryVerificationVerified
		}
		return items[i].UpdatedAt.After(items[j].UpdatedAt)
	})
	var b strings.Builder
	used := 0
	for _, it := range items {
		scopeTag := "user"
		if it.Scope == store.MemoryScopeProject {
			scopeTag = "project"
		}
		line := fmt.Sprintf("- [%s %s/%s] %s\n", it.ID, scopeTag, it.MemoryKey, truncateMemoryText(it.Content, 200))
		n := utf8.RuneCountInString(line)
		if used+n > maxRunes {
			continue // 超预算跳过该条（确定性：不最后整体截断，避免砍掉用户最新纠正）
		}
		b.WriteString(line)
		used += n
	}
	out := strings.TrimRight(b.String(), "\n")
	if out == "" {
		return ""
	}
	_ = scopeLabel
	return out
}

// dedupeByMemoryKey 同 key 保留最新一条（理论上 active 唯一，兜底防历史脏数据）。
func dedupeByMemoryKey(items []store.MemoryItem) []store.MemoryItem {
	if len(items) <= 1 {
		return items
	}
	best := map[string]store.MemoryItem{}
	var order []string
	for _, it := range items {
		k := it.Scope + "\x00" + it.ProjectKey + "\x00" + it.MemoryKey
		if _, ok := best[k]; !ok {
			best[k] = it
			order = append(order, k)
			continue
		}
		if it.UpdatedAt.After(best[k].UpdatedAt) {
			best[k] = it
		}
	}
	out := make([]store.MemoryItem, 0, len(order))
	for _, k := range order {
		out = append(out, best[k])
	}
	return out
}

// bumpRevision 本地操作后刷新有效视图版本（注入侧可据此判断缓存失效）。
func (lc *MemoryLifecycle) bumpRevision() int64 {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	lc.revision++
	lc.lastRefresh = time.Now()
	lc.blockCache = map[string]cachedMemoryBlock{}
	return lc.revision
}

// Revision 当前有效视图版本（事件数兜底：多进程/重启后仍单调）。
func (lc *MemoryLifecycle) Revision(ctx context.Context, owner string) int64 {
	if lc == nil || lc.repo == nil {
		return 0
	}
	n, err := lc.repo.EventCount(ctx, owner)
	if err != nil {
		lc.mu.Lock()
		defer lc.mu.Unlock()
		return lc.revision
	}
	lc.mu.Lock()
	defer lc.mu.Unlock()
	if n > lc.revision {
		lc.revision = n
	}
	return lc.revision
}

// ---------------------------------------------------------------- 管理操作

// RetractByID 管理接口/测试用：按 ID 撤回（actor=admin）。
func (lc *MemoryLifecycle) RetractByID(ctx context.Context, owner, id, reason string) (*store.MemoryItem, error) {
	return lc.setStatusByID(ctx, owner, id, store.MemoryStatusRetracted, store.MemoryActorAdmin, reason)
}

// QuarantineByID 隔离条目（立即停止注入，保留审计）。
func (lc *MemoryLifecycle) QuarantineByID(ctx context.Context, owner, id, reason string) (*store.MemoryItem, error) {
	return lc.setStatusByID(ctx, owner, id, store.MemoryStatusQuarantined, store.MemoryActorAdmin, reason)
}

// RestoreByID 恢复条目（针对具体 tombstone，不提供"清空全部墓碑"）。
func (lc *MemoryLifecycle) RestoreByID(ctx context.Context, owner, id, reason string) (*store.MemoryItem, error) {
	return lc.setStatusByID(ctx, owner, id, store.MemoryStatusActive, store.MemoryActorAdmin, reason)
}

func (lc *MemoryLifecycle) setStatusByID(ctx context.Context, owner, id, status, actor, reason string) (*store.MemoryItem, error) {
	if lc == nil || lc.repo == nil {
		return nil, fmt.Errorf("memory lifecycle not wired")
	}
	item, err := lc.repo.GetItem(ctx, owner, id)
	if err != nil {
		return nil, err
	}
	sc := MemoryScopeContext{Owner: owner, ProjectKey: item.ProjectKey}
	if err := lc.transition(ctx, sc, *item, status, actor, reason, ""); err != nil {
		return nil, err
	}
	lc.bumpRevision()
	return lc.repo.GetItem(ctx, owner, id)
}

// ListItems 管理接口用：按条件列条目。
func (lc *MemoryLifecycle) ListItems(ctx context.Context, q store.MemoryQuery) ([]store.MemoryItem, error) {
	if lc == nil || lc.repo == nil {
		return nil, fmt.Errorf("memory lifecycle not wired")
	}
	return lc.repo.ListItems(ctx, q)
}

// ListEvents 管理接口用：条目事件时间线。
func (lc *MemoryLifecycle) ListEvents(ctx context.Context, owner, id string, limit int) ([]store.MemoryEvent, error) {
	if lc == nil || lc.repo == nil {
		return nil, fmt.Errorf("memory lifecycle not wired")
	}
	return lc.repo.ListEvents(ctx, owner, id, limit)
}

// ---------------------------------------------------------------- 分类工具

const (
	memoryActionRetract = "retract"
	memoryActionReplace = "replace"
)

// memoryIntent 从用户原话解析出的意图。
type memoryIntent struct {
	scope      string // user | project | ""
	oneShot    bool
	action     string // retract | replace | ""
	newContent string // replace 的新表述
	badOutcome bool   // "效果太差"类
}

// scopeOrDefault 未指明作用域时默认项目（长期记忆里最常见的是项目级工艺）。
func (i memoryIntent) scopeOrDefault() string {
	if i.scope != "" {
		return i.scope
	}
	return store.MemoryScopeProject
}

// classifyMemoryIntent 解析长期记忆意图。判定保守：宁可走 B 档隔离，也不误删。
func classifyMemoryIntent(text string) memoryIntent {
	low := strings.ToLower(text)
	intent := memoryIntent{}
	hasLongTerm := containsAnyMarker(low, memoryLongTermMarkers)
	hasProject := containsAnyMarker(low, memoryProjectMarkers)
	switch {
	case hasProject:
		intent.scope = store.MemoryScopeProject
	case hasLongTerm:
		intent.scope = store.MemoryScopeUser
	}
	// 一次性标记 + 无长期意图 → 只改会话决策（"这次不用""这个角色先这样"）。
	if containsAnyMarker(low, memoryOneShotMarkers) && !hasLongTerm && !hasProject {
		intent.oneShot = true
		return intent
	}
	if containsAnyMarker(low, memoryRetractMarkers) || memoryNegationObjectRe.MatchString(text) {
		intent.action = memoryActionRetract
	}
	for _, m := range memoryReplaceMarkers {
		if i := strings.Index(text, m); i >= 0 {
			rest := strings.TrimSpace(text[i+len(m):])
			rest = strings.Trim(rest, "，,。.；;：: ")
			if rest != "" {
				intent.action = memoryActionReplace
				intent.newContent = truncateMemoryText(rest, 600)
			}
			break
		}
	}
	if containsAnyMarker(low, memoryBadOutcomeMarkers) {
		intent.badOutcome = true
	}
	return intent
}

// classifyMemoryKeys 返回文本命中的主题键（按分组顺序，去重）。
func classifyMemoryKeys(text string) []string {
	low := strings.ToLower(text)
	var keys []string
	for _, g := range memoryKeyGroups {
		if containsAnyMarker(low, g.markers) {
			keys = append(keys, g.key)
		}
	}
	return keys
}

// projectKeyFor 计算条目应落到的 project_key（user 作用域恒为空）。
func projectKeyFor(scope string, sc MemoryScopeContext) string {
	if scope != store.MemoryScopeProject {
		return ""
	}
	return store.NormalizeProjectKey(sc.ProjectKey)
}

type rankedItem struct {
	item  store.MemoryItem
	score float64
}

// rankItemsBySimilarity 按与用户原话的 Jaccard 相似度降序（确定性排序，便于阈值判定）。
func rankItemsBySimilarity(items []store.MemoryItem, text string) []rankedItem {
	out := make([]rankedItem, 0, len(items))
	for _, it := range items {
		out = append(out, rankedItem{item: it, score: store.MemoryTextSimilarity(it.Content, text)})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].item.UpdatedAt.After(out[j].item.UpdatedAt)
	})
	return out
}

// matchItemByExactContent 用户复述了某条原文（归一化后互为子串，且 ≥6 runes）。
func matchItemByExactContent(items []store.MemoryItem, text string) *store.MemoryItem {
	normText := normalizeMemoryComparable(text)
	if utf8.RuneCountInString(normText) < 6 {
		return nil
	}
	for i := range items {
		normItem := normalizeMemoryComparable(items[i].Content)
		if utf8.RuneCountInString(normItem) < 6 {
			continue
		}
		if strings.Contains(normText, normItem) || strings.Contains(normItem, normText) {
			return &items[i]
		}
	}
	return nil
}

// normalizeMemoryComparable 与 store 侧 hash 归一化口径保持一致的比较文本。
func normalizeMemoryComparable(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch r {
		case ' ', '\t', '\r', '\n', '\u3000', '.', ',', '，', '。', '、', '；', ';', '：', ':',
			'!', '?', '！', '？', '"', '\'', '`', '“', '”', '(', ')', '（', '）', '《', '》',
			'-', '_', '*', '#', '>', '~', '/', '\\', '|':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// filterItemsByKeys 只保留命中主题键的条目。
func filterItemsByKeys(items []store.MemoryItem, keys []string) []store.MemoryItem {
	if len(keys) == 0 {
		return nil
	}
	var out []store.MemoryItem
	for _, it := range items {
		for _, k := range keys {
			if it.MemoryKey == k {
				out = append(out, it)
				break
			}
		}
	}
	return out
}

func findItemByID(items []store.MemoryItem, id string) *store.MemoryItem {
	for i := range items {
		if items[i].ID == id {
			return &items[i]
		}
	}
	return nil
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// truncateMemoryText rune 安全截断（省略号，不用 truncated 字样）。
func truncateMemoryText(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + "…"
}
