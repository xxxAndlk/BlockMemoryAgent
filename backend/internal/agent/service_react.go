// Package agent 提供基于 ReAct 引擎的 Agent 服务实现，
// 负责会话生命周期管理与外部接口适配。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/blockmemory/agent/backend/internal/board"          // board 提供任务看板快照（TODO #22）
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/domain/role"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/logger"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/userprofile"
	"github.com/blockmemory/agent/backend/internal/project"
	"github.com/blockmemory/agent/backend/internal/skill"
	"github.com/blockmemory/agent/backend/internal/server/eventkind"
	"github.com/blockmemory/agent/backend/internal/store"
	pkgconfig "github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/textutil"
)

// ReactService 是基于 ReAct（Reasoning + Acting）引擎的 Agent 接口实现。
// 它持有内存中的会话存储，并将实际执行委托给 ReActAgent。
type ReactService struct {
	store        *reactSessionStore  // 内存会话存储，管理会话生命周期与事件
	roleRegistry *role.Registry      // 角色注册表，用于获取 meta 角色等配置
	modelFactory *model.ModelFactory // 模型工厂，负责构造大模型调用 provider
	toolRegistry *tool.Registry      // 工具注册表，提供 ReAct 可调用的工具
	mailbox      *mailbox.Mailbox    // 邮箱，用于跨组件消息通知
	memory       MemoryPipeline      // 记忆管道，负责会话记忆的写入与查询
	runtimeCfg   ReactRuntimeConfig  // ReAct 主循环运行时参数（轮数/超时/重试/历史滑窗）

	// pendingChecker 注入到每个 ReActAgent，用于父会话终结保护
	// （有未决子 Agent 时阻止终结，防止迟到 mailbox 消息丢失）。
	// 为 nil 时关闭保护；由 bootstrap 注入 subagent.Dispatcher 实现。
	pendingChecker PendingChildrenChecker

	// pausedChecker 注入到 MetaAgent ReActAgent，用于父终结保护 wait loop 检测
	// Paused 子 DomainAgent（触达 token 上限）。MetaAgent 无限 budget 不会自行暂停，
	// 靠此检查跳出 wait loop 返回 PausedOnChild，由上层 pauseSession 置会话暂停态。
	// 为 nil 时关闭检查；由 bootstrap 注入 subagent.Dispatcher 实现。
	pausedChecker PausedChildChecker

	// resumeDispatcher 注入 Paused DomainAgent 恢复器，sendMessage 在会话处于
	// PausedOnChild 态时调用其 ResumePaused 从 agent_messages 加载历史续跑。
	// 为 nil 时 PausedOnChild 会话回退到普通 resumeSession（不恢复暂停的 domain）。
	resumeDispatcher PausedDomainResumer

	// idleRosterProvider 可选的热驻空闲领域清单提供者（Domain 热驻），
	// 由 bootstrap 注入 subagent.Dispatcher 实现。runSession/resumeSession 构造
	// MetaAgent 时把 IdleRoster 包装进记忆管线注入【空闲领域Agent】段。
	// 为 nil 时零注入。
	idleRosterProvider IdleRosterProvider
	// activityEvidenceProvider 可选的活动证据查询器（TODO 第10项②展示面），
	// 由 bootstrap 注入 subagent.Dispatcher 实现；ListAgents 据此填充各节点的
	// ActivityKind/LastActivityAgo。为 nil 时字段留空。
	activityEvidenceProvider ActivityEvidenceProvider
	// idleTTLArmer 可选的 Idle TTL 武装器（Domain 热驻），sendMessage 在新用户
	// 消息到达时调用 ArmIdleTTLs（完成后一直热存，TTL 只在用户消息后启动）。
	// 为 nil 时零行为。
	idleTTLArmer IdleTTLArmer
	// sessionAgentWaker 可选的会话挂起唤醒器（Domain 热驻），sendMessage 在非
	// Running 态恢复时调用 ResumeSessionAgents 唤醒全部挂起 Agent（含全树叶子）。
	// 为 nil 时走旧路径（逐个 resumePausedDomain）。
	sessionAgentWaker SessionAgentWaker
	// hotResident Domain 热驻模式标记（bootstrap 按 config 注入）：Stop 语义变化
	// （叶子杀+domain 转 Idle+取消 MetaAgent 调用+停用 session 级销毁倒计时）。
	hotResident bool

	// testProvider 是包内部测试使用的钩子，
	// 允许单元测试注入 mock 的 ModelProvider，从而无需真实 API 密钥即可运行 ReAct 循环。
	testProvider ModelProvider

	// trees 按 sessionID 维护权威 Agent 树（lazy init）。
	// Dispatcher 通过 TreeFor(sid) 取得 *orchestrator.Tree 后 Register/Finish/SetCancel。
	// Snapshot/Cancel 经由 Tree()/CancelAgent() 暴露给 HTTP API。
	// treeStore 非 nil 时 TreeFor lazy init 会调 LoadFromStore 恢复历史节点(重启不丢)。
	trees sync.Map
	// createMu 串行化 CreateSession 的"查重-建会话"临界区：web 端重载/双开重发会
	// 并发触发两次 CreateSession，无锁时查重窗口内双双通过、起两个并行同任务会话。
	createMu sync.Mutex
	// treeStore 可选的 Agent 树持久化层。为 nil 时纯内存。
	// 由 bootstrap 注入 store.PostgresTreeStore;测试场景保持 nil。
	treeStore orchestrator.TreeStore
	// sharedMemoryStore 可选的共享记忆 KV,用于话题切换时写入旧话题摘要。
	// key 格式 `topic:{topicID}:summary`。为 nil 时跳过摘要写入(测试场景)。
	// MetaAgent 在新话题召回该摘要依赖步骤 4 part C(MetaAgent 根 recall 注入,未做)。
	sharedMemoryStore tool.SharedMemoryStore
	// persona 可选的人格注入器（soul.Loader 实现该接口）；为 nil 时不注入人格前缀。
	// bootstrap 注入 runtime.Soul；runSession/resumeSession 构造 MetaAgent 时调用 WithPersonaInjector。
	persona PersonaInjector
	// boardFn 按 sessionID 返回会话任务看板（TODO #22 执行计划）；nil 表示未接线。
	boardFn func(sessionID string) *board.TaskBoard

	// ledgerFn 按 sessionID 渲染【任务台账】块文本（2026-08-28 旧需求重派事故根治）；
	// 空串=无台账不注入，nil 表示未接线。由 bootstrap 注入 Dispatcher.TaskLedgerBrief。
	ledgerFn func(sessionID string) string

	// VideoOpts 用户消息视频附件（Alt+V 粘贴视频文件）抽帧参数：
	// bootstrap 从 config.Video 注入，零值字段内部回落 DefaultVideoOptions。
	VideoOpts VideoOptions
	// promptEnhance 用户输入自动提示词补全开关（TODO #36 Phase 0 规则版）。
	// 开启时 sendMessage 对命中续跑/控制/诊断意图的输入附加【系统补全】段
	// （意图标签 + 最近失败/未完成任务绑定），只增不改原文；关闭时零行为变化。
	promptEnhance bool
	// promptEnhanceLLM L2 轻量模型仲裁开关（TODO #39）：灰区输入（L1 未命中但句中有
	// 弱词信号）调仲裁器四分类；关闭时降级纯规则（Phase 0 行为）。
	promptEnhanceLLM bool
	// promptEnhanceArbiter L2 仲裁器（bootstrap 注入轻量模型实现）；nil 且开关开时
	// L2 不可用，同样降级纯规则。
	promptEnhanceArbiter IntentArbiter
	// promptEnhanceTimeout L2 单次超时（<=0 用默认 10s）。
	promptEnhanceTimeout time.Duration
	// promptEnhanceMaxRunes L0 输入形态闸门长度上限（rune；<=0 用默认 30）。
	promptEnhanceMaxRunes int
	// stopMarker 会话软停止标记器（TODO #37，subagent.Dispatcher 实现）：
	// Stop 先标记再触发子 Agent cancel，dispatcher 收尾分支据此刻意落 Paused/部分回灌。
	// nil 时 Stop 退化为仅级联取消（无暂停语义）。
	stopMarker SoftStopMarker
	// stopCountdown 软停止销毁倒计时（TODO #37）：Stop 后到期未续跑则硬销毁全部节点；
	// <=0 关闭倒计时（永久暂停，靠用户续跑）。
	stopCountdown time.Duration
	// userProfile 用户画像存储（TODO #28 第四层记忆）；nil 表示未接线（不注入不提取）。
	userProfile *userprofile.Store
	// profileExtractor 会话完成时从对话提取偏好增量的轻量模型回调；nil 跳过提取。
	profileExtractor func(ctx context.Context, text string) ([]string, error)
	// projectPrefs 项目偏好存储（2026-09-02 偏好与自进化期 1）：per workDir
	// .bma/project_preferences.md，读写按 ctx 会话目录解析（S2）；
	// nil 表示未接线（Meta/DomainAgent 均不注入）。
	projectPrefs *userprofile.ProjectStore
	// prefMerger 偏好合并回调（轻量模型对目标小节做去重/冲突归档重写）；
	// nil 时增量降级直写归档小节（v1 行为）。
	prefMerger PrefMerger
	// evolver SessionEvolver 回调（设计 §6.1）：一次轻量模型调用产出三类沉淀；
	// nil 时会话结束回退纯画像提取（extractProfilePreferences 语义）。
	evolver func(ctx context.Context, in EvolveInput) (*EvolveOutput, error)
	// skillSink 技能包落库回调（设计 §6.3/§6.4）：文件 + learned_skills PG +
	// skill_create/skill_update evolution_log；nil 时技能沉淀跳过。
	skillSink func(ctx context.Context, sessionID string, skills []EvolvedSkill, outcome string) error
	// evolutionLog 进化审计写回调（evolution_log 表）；nil 时跳过审计。
	evolutionLog func(ctx context.Context, sessionID, kind, target, summary string) error
	// skillRecall MetaAgent 侧经验技能向量预答回调（设计 §6.5）：goal -> top-3 提示行；
	// nil 时零注入。与 Dispatcher 侧召回同源（bootstrap 注入同一检索函数）。
	skillRecall func(ctx context.Context, task string) []SkillRecallHint
	// pluginVisibility 热插拔插件角色可见性回调（设计文档 §4.3）：
	// fn(roleID, toolName) -> (owned, visible)；nil 时插件工具不额外过滤
	//（白名单语义不变）。由 bootstrap 注入 plugins.Manager.ToolVisibility。
	pluginVisibility ToolVisibilityFunc
	// activityPinger 等待用户答复期间的心跳保活回调（bootstrap 注入
	// subagent.Dispatcher.PingActivity）：审批/提问阻塞时周期性刷新子 Agent 活动时间，
	// 防止心跳巡检把"等用户操作"误判假死 kill（实证 2026-08-18：三次误杀均卡在
	// Remove-Item 确认框无人答复，每次白耗 ~12 分钟 + 重派）。nil 时不保活。
	activityPinger func(agentID string)
	// skillPool 全局技能池（技能渐进披露）：nil 时不注入 MetaAgent 技能目录块。
	// bootstrap 经 SetSkillCatalog 注入与 Dispatcher 同一个池。
	skillPool *skill.Pool
}

// SetSkillCatalog 注入全局技能池：MetaAgent 会话系统提示追加全池【可用技能】目录块
// （Meta 持全集、可派发任意技能给下级）；nil 关闭（测试/未配置场景）。
func (s *ReactService) SetSkillCatalog(p *skill.Pool) {
	s.skillPool = p
}

// metaSkillBlock 渲染 MetaAgent 的全池技能目录块：Meta 持全集（无需派发即可
// load_skill 取全文，也可经 call_sub_agent 的 skills 参数下放任意技能）。
// skillPool 未注入或池为空时返回空串（零注入）。
func (s *ReactService) metaSkillBlock() string {
	if s.skillPool == nil {
		return ""
	}
	return skill.MetadataBlock(s.skillPool, s.skillPool.Names())
}

// SetActivityPinger 注入等待用户答复期间的心跳保活回调；nil 关闭（测试场景）。
func (s *ReactService) SetActivityPinger(fn func(agentID string)) {
	s.activityPinger = fn
}

// userWaitKeepalive 是等待用户答复期间的保活器，Stop 幂等。
type userWaitKeepalive struct{ done chan struct{} }

// Stop 停止保活 goroutine（幂等）。
func (k userWaitKeepalive) Stop() {
	select {
	case <-k.done:
	default:
		close(k.done)
	}
}

// startUserWaitKeepalive 启动等待用户答复期间的保活 goroutine（30s 一拍）：
// 每拍调用 activityPinger 刷新 ctx 中 Agent 的活动时间，直到 Stop 或 ctx 取消。
// pinger 未注入或 agentID 为空时返回的保活器零工作（Stop 仍安全）。
func (s *ReactService) startUserWaitKeepalive(ctx context.Context) userWaitKeepalive {
	k := userWaitKeepalive{done: make(chan struct{})}
	agentID := tool.AgentIDFromContext(ctx)
	if s.activityPinger == nil || agentID == "" {
		close(k.done)
		return k
	}
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-k.done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				s.activityPinger(agentID)
			}
		}
	}()
	return k
}

// SetUserProfileStore 注入用户画像存储（TODO #28）。
// 注入后 MetaAgent system prompt 自动带【用户画像】前缀（带 rune 上限截断），子 Agent 不下发。
// 传 nil 关闭画像功能（测试场景）。
func (s *ReactService) SetUserProfileStore(st *userprofile.Store) {
	s.userProfile = st
}

// SetProfileExtractor 注入会话完成时的偏好提取回调（轻量模型扫对话）。
// 传 nil 关闭自动提取（默认关闭）；显式写入（remember_preference / SaveProfile）不受影响。
func (s *ReactService) SetProfileExtractor(fn func(ctx context.Context, text string) ([]string, error)) {
	s.profileExtractor = fn
}

// SetPrefMerger 注入偏好合并回调（轻量模型去重/冲突归档重写，设计 §4）。
// nil（默认）时会话结束提取的增量降级直写归档小节（v1 行为）。
func (s *ReactService) SetPrefMerger(fn PrefMerger) {
	s.prefMerger = fn
}

// SetEvolver 注入 SessionEvolver（设计 §6.1）。
// nil（默认）时会话结束回退纯画像提取（旧语义，测试/未接线场景）。
func (s *ReactService) SetEvolver(fn func(ctx context.Context, in EvolveInput) (*EvolveOutput, error)) {
	s.evolver = fn
}

// SetSkillSink 注入技能包落库回调（设计 §6.3 存储分界 + §6.4 同名合并）。
// nil（默认）时会话进化产出的技能包被跳过（偏好增量照常）。
func (s *ReactService) SetSkillSink(fn func(ctx context.Context, sessionID string, skills []EvolvedSkill, outcome string) error) {
	s.skillSink = fn
}

// SetEvolutionLogger 注入进化审计写回调（evolution_log 表）。
// nil（默认）时三类沉淀不写审计流水（沉淀本身照常生效）。
func (s *ReactService) SetEvolutionLogger(fn func(ctx context.Context, sessionID, kind, target, summary string) error) {
	s.evolutionLog = fn
}

// SetSkillRecall 注入 MetaAgent 侧经验技能向量预答回调（设计 §6.5）。
// nil（默认）时新任务零提示（技能仍可 list_skills/load_skill 主动取用）。
func (s *ReactService) SetSkillRecall(fn func(ctx context.Context, task string) []SkillRecallHint) {
	s.skillRecall = fn
}

// SetProjectPreferencesStore 注入项目偏好存储（设计 §5）：
// 注入后 MetaAgent system prompt 带【项目偏好】前缀（persona 链），
// Dispatcher 派发前缀带【项目偏好】段（子 Agent 执行层遵守项目工艺）。
func (s *ReactService) SetProjectPreferencesStore(st *userprofile.ProjectStore) {
	s.projectPrefs = st
}

// ProjectPreferences 返回项目偏好全文快照（按 ctx 会话目录解析）。未接线返回空画像。
func (s *ReactService) ProjectPreferences(ctx context.Context) (*userprofile.Profile, error) {
	if s.projectPrefs == nil {
		return &userprofile.Profile{}, nil
	}
	return s.projectPrefs.Current(ctx), nil
}

// SaveProjectPreferences 全量覆盖项目偏好（HTTP PUT 用户手动编辑，按 ctx 会话目录解析）。
func (s *ReactService) SaveProjectPreferences(ctx context.Context, content string) error {
	if s.projectPrefs == nil {
		return fmt.Errorf("project preferences store not wired")
	}
	return s.projectPrefs.Save(ctx, content)
}

// SetPluginVisibility 注入插件工具角色可见性回调（设计文档 §4.3）。
// 由 bootstrap 注入 plugins.Manager.ToolVisibility；传 nil 关闭插件可见性过滤。
func (s *ReactService) SetPluginVisibility(fn ToolVisibilityFunc) {
	s.pluginVisibility = fn
}

// Profile 返回用户画像全文快照（TODO #28 查看/编辑入口）。未接线返回空画像。
func (s *ReactService) Profile(ctx context.Context) (*userprofile.Profile, error) {
	if s.userProfile == nil {
		return &userprofile.Profile{}, nil
	}
	return s.userProfile.Current(), nil
}

// SaveProfile 全量覆盖用户画像（HTTP PUT 用户手动编辑）。
func (s *ReactService) SaveProfile(ctx context.Context, content string) error {
	if s.userProfile == nil {
		return fmt.Errorf("user profile store not wired")
	}
	return s.userProfile.Save(content)
}

// extractProfilePreferences 会话完成时扫对话提取偏好增量并 Merge 整理入画像
//（TODO #28 双路写入之 b + 2026-09-02 期 1 Merge 升级）。
// 仅提取用户消息（user 角色）；提取失败/空结果零副作用（不阻塞会话收尾）。
// Merge：轻量模型对偏好/技术栈/沟通风格小节去重+冲突归档；合并不可用降级直写反馈记录。
func (s *ReactService) extractProfilePreferences(session *reactInternalSession) {
	if s.userProfile == nil || s.profileExtractor == nil {
		return
	}
	var sb strings.Builder
	for _, m := range session.Messages {
		if m.Role == string(enums.ChatRoleUser) {
			sb.WriteString(m.Content)
			sb.WriteByte('\n')
		}
	}
	text := strings.TrimSpace(sb.String())
	if text == "" {
		return
	}
	ctx, cancel := context.WithTimeout(
		// 挂会话级 logger：画像提取的轻量 LLM 调用由此写 session_logs（BaseBackground 无 ctx 来源）。
		logger.NewContext(context.Background(), s.sessionLogger(session.ID, "UserProfileExtractor")),
		8*time.Second)
	defer cancel()
	prefs, err := s.profileExtractor(ctx, text)
	if err != nil || len(prefs) == 0 {
		log.Printf("[profile] extract preferences failed, skip: err=%v prefs=%d", err, len(prefs))
		return
	}
	cleaned := make([]string, 0, len(prefs))
	for _, p := range prefs {
		if p = strings.TrimSpace(p); p != "" {
			cleaned = append(cleaned, p)
		}
	}
	s.mergeIntoStore(session.ID, "UserProfileExtractor", s.userProfile, userProfileTargets, cleaned)
}

// SetBoard 注入会话任务看板访问器（TODO #22）。
// 由 bootstrap 注入 board.Manager.Get；传 nil 关闭看板功能（TUI 计划面板回退树合成）。
func (s *ReactService) SetBoard(fn func(sessionID string) *board.TaskBoard) {
	s.boardFn = fn
}

// SetTaskLedgerProvider 注入任务台账渲染器（2026-08-28 旧需求重派事故根治）：
// fn(sessionID) 返回【任务台账】块文本，空串=无台账不注入。传 nil 关闭台账注入。
// 由 bootstrap 注入 Dispatcher.TaskLedgerBrief。
func (s *ReactService) SetTaskLedgerProvider(fn func(sessionID string) string) {
	s.ledgerFn = fn
}

// SetPromptEnhance 开启/关闭用户输入自动提示词补全（TODO #36，默认关闭；
// bootstrap 按 config.Agent.PromptEnhance 注入，config.yaml 默认 true）。
// 关闭时 sendMessage 原样接收用户消息，零行为变化。
func (s *ReactService) SetPromptEnhance(enabled bool) {
	s.promptEnhance = enabled
}

// SetPromptEnhanceLLM 配置 L2 轻量模型仲裁（TODO #39）：
// enabled = config prompt_enhance_llm；arbiter = 轻量模型四分类仲裁器（bootstrap 注入，
// nil 时 L2 不可用降级纯规则）；timeout/maxRunes 覆盖 L2 超时与 L0 闸门长度（<=0 用默认）。
func (s *ReactService) SetPromptEnhanceLLM(enabled bool, arbiter IntentArbiter, timeout time.Duration, maxRunes int) {
	s.promptEnhanceLLM = enabled
	s.promptEnhanceArbiter = arbiter
	s.promptEnhanceTimeout = timeout
	s.promptEnhanceMaxRunes = maxRunes
}

// enhanceUserInput 对用户输入做意图分类 + 消歧绑定（TODO #36 Phase 0 规则版 + #39 四层管线）。
// 数据源：会话任务看板（board.Snapshot 失败/未完成任务）+ Agent 树失败/取消节点
// （LoopExit 被杀/心跳杀/用户取消——看板未必回写，树是权威）。无绑定状态时仅给意图标签。
// 开关关闭 / 意图未命中返回原文（零行为变化）。
// 返回 (补全后文本, 判定路径事件备注)：备注供 sendMessage 落可观测事件
// （gate_skip / rule_strong / rule_weak / llm_hit / llm_miss / llm_timeout / llm_error）。
// L2 仲裁在调用方持 store.mu 期间执行（最长 ArbiterTimeout），单用户 TUI 主路径可接受。
func (s *ReactService) enhanceUserInput(ctx context.Context, sessionID, content string) (string, string) {
	if !s.promptEnhance {
		return content, ""
	}
	// 挂会话级 logger：L2 意图仲裁的轻量 LLM 调用（arbiterDecide → CallLightweightWithRetry）
	// 由此写 session_logs；sessionLogger 未注入全局 logger 时返回 nil，NewContext 原样透传。
	ctx = logger.NewContext(ctx, s.sessionLogger(sessionID, "PromptEnhance"))
	st := EnhanceState{}
	if s.boardFn != nil {
		if b := s.boardFn(sessionID); b != nil {
			snap := b.Snapshot()
			st.BoardGoal = snap.Goal
			st.BoardState = string(snap.Status)
			for _, t := range snap.Tasks {
				et := EnhanceTask{Title: t.Title, Domain: t.Domain, Status: string(t.Status), Result: t.Result}
				switch t.Status {
				case board.TaskFailed:
					st.FailedTasks = append(st.FailedTasks, et)
				case board.TaskDone:
					// 终态成功不绑定
				default:
					st.PendingTasks = append(st.PendingTasks, et)
				}
			}
		}
	}
	// 树补充失败/取消节点（看板未覆盖时）：按 domain 去重，避免与看板失败任务重复列。
	if t := s.TreeFor(sessionID); t != nil {
		boardFailed := make(map[string]bool, len(st.FailedTasks))
		for _, ft := range st.FailedTasks {
			boardFailed[ft.Domain] = true
		}
		for _, n := range t.Snapshot() {
			if n.Role != "domain" || (n.Status != orchestrator.StatusFailed && n.Status != orchestrator.StatusCancelled) {
				continue
			}
			if boardFailed[n.Domain] {
				continue
			}
			title := n.Domain
			if title == "" {
				title = n.Task
			}
			reason := n.Err
			if reason == "" {
				reason = n.Summary
			}
			st.FailedTasks = append(st.FailedTasks, EnhanceTask{Title: title, Domain: n.Domain, Status: n.Status.String(), Result: reason})
		}
	}
	opts := EnhanceOptions{MaxInputRunes: s.promptEnhanceMaxRunes}
	if s.promptEnhanceLLM && s.promptEnhanceArbiter != nil {
		opts.Arbiter = s.promptEnhanceArbiter
		opts.ArbiterTimeout = s.promptEnhanceTimeout
	}
	return EnhancePromptWithOptions(ctx, content, st, opts)
}

// Board 返回会话任务看板快照（TODO #22 Phase 2 面板真相源）。
// 未接线或会话无看板（未 write_plan）返回 (nil, nil)，调用方回退旧树合成。
func (s *ReactService) Board(ctx context.Context, sessionID string) (*board.Snapshot, error) {
	if s.boardFn == nil {
		return nil, nil
	}
	b := s.boardFn(sessionID)
	if b == nil {
		return nil, nil
	}
	snap := b.Snapshot()
	return &snap, nil
}

// SetTreeStore 注入 Agent 树持久化层。bootstrap 在创建 ReactService 后调用。
// 传 nil 关闭持久化(纯内存,测试场景)。
func (s *ReactService) SetTreeStore(ts orchestrator.TreeStore) {
	s.treeStore = ts
}

// SetSharedMemoryStore 注入共享记忆 KV,用于话题切换时写入旧话题摘要。
// bootstrap 在创建 sharedKV 后调用。传 nil 关闭摘要写入(测试场景)。
// 若 store 实现 Clear(ctx) error（FileSharedMemoryStore），同时桥接清理闭包到 session store，
// 使新 session 启动时清其有效工作目录 <dir>/.bma/shared 旧 session 残留（spec/file_tree 不跨 session 复用）。
func (s *ReactService) SetSharedMemoryStore(store tool.SharedMemoryStore) {
	s.sharedMemoryStore = store
	if _, ok := store.(interface{ Clear(context.Context) error }); ok {
		s.store.setSharedMemoryReset(func(dir string) {
			if dir == "" {
				return
			}
			// 按会话有效工作目录拼 <dir>/.bma/shared 清理（默认实现内部拼路径）。
			if err := tool.NewFileSharedMemoryStore(dir).Clear(context.Background()); err != nil {
				s.store.logError(context.Background(), "clear shared memory on session start", err)
			}
		})
	}
}

// SetDomainClassifier 注入 LLM 领域分区器，供 EnsureProjectDoc 首生成 PROJECT.md 按职责分区。
// bootstrap 在构造 ModelFactory 后调用；nil（测试）走启发式依赖图兜底。
func (s *ReactService) SetDomainClassifier(cls project.DomainClassifier) {
	s.store.setDomainClassifier(cls)
}

// SetPersonaInjector 注入人格注入器(soul.Loader),使 MetaAgent 系统提示词头部带人格前缀。
// bootstrap 注入 runtime.Soul;传 nil 关闭人格注入(测试场景)。
func (s *ReactService) SetPersonaInjector(p PersonaInjector) {
	s.persona = p
}

// metaPersona 返回 MetaAgent 的注入器组合：人格（soul）+ 用户画像（TODO #28）+ 项目偏好
//（2026-09-02 设计 §5：项目偏好 Meta+Domain 双注入）。
// 用户画像仅注入 MetaAgent；项目偏好经 Dispatcher 前缀同步下发子 Agent（执行层工艺）。
// workDir 为会话工作目录（S2）：项目偏好按会话目录解析，空串回落 store 构造目录。
func (s *ReactService) metaPersona(workDir string) PersonaInjector {
	var userCurrent func() string
	if s.userProfile != nil {
		store := s.userProfile
		userCurrent = func() string { return store.Current().Content }
	}
	var projCurrent func() string
	if s.projectPrefs != nil {
		store := s.projectPrefs
		projCurrent = func() string {
			return store.Current(tool.WithWorkDir(context.Background(), workDir)).Content
		}
	}
	return CombinePersonaInjectors(
		s.persona,
		NewUserProfileInjector(userCurrent, 2000),
		NewSectionInjector("【项目偏好】", projCurrent, 2000),
	)
}

// metaPersonaLite 返回续轮（resumeSession）的轻量注入器：仅人格（soul）。
// 画像/偏好属于首轮一次性注入（TODO 第八项 P0-2）：runSession 首轮带全量
// metaPersona，此后每轮重发 ~4K runes 属纯重复（画像/偏好极少变化，且已在前缀
// 缓存的历史里）——续轮只保留 soul 前缀，省 ~4K runes/呼。
func (s *ReactService) metaPersonaLite() PersonaInjector {
	return s.persona
}

// ReactRuntimeConfig 是 ReAct 引擎的运行时参数快照。
// 由 bootstrap 从 cfg.Agent 派生注入，避免 agent 包反向依赖 config 包；
// 未注入时全部取零值，由 loopConfig 回退到合理默认值。
type ReactRuntimeConfig struct {
	MaxIterations           int // ReAct 最大 LLM 轮数；<0 表示不限制
	LLMTimeoutSec           int // 单次 LLM 调用超时（秒）；<0 表示仅受会话取消控制
	RetryCount              int // LLM 失败重试次数（不含首次）
	RetryBackoffMs          int // 重试初始退避（毫秒）
	HistoryMaxMessages      int // 单次请求最大历史消息数；<0 表示不裁剪
	ToolOutputHistoryMaxRunes int // 写入历史的工具输出最大字符数；<0 表示不截断
	// TokenBudgetPerGoal 退役字段（原累计跨轮 token 预算，已替换为按角色上下文阈值）。
	// 保留字段不破坏旧配置加载，但不再驱动任何闸门。见 roleTokenBudget。
	TokenBudgetPerGoal int
	// TokenBudgetPerRole 按 roleID 设上下文 token 阈值（Assemble 压缩后 messages token
	// >= 阈值即 LimitReached 暂停）。未列出角色默认 150000。nil 时全部走默认。
	// 语义=上下文阈值非累计跨轮；每轮独立估算（各 Agent 独立）。
	TokenBudgetPerRole map[string]int
	// ContextTokenBudget 上下文 token 阈值默认值（未在 TokenBudgetPerRole 列出的角色用此值）。
	// 默认 150000（config applyDefaults 兜底）；<=0 不限制。
	ContextTokenBudget int
	// SessionMaxWallClockMin 会话全局墙钟上限（分钟，TODO #25-4 硬止损）。
	// 从会话创建起超时未终止则级联取消全部节点 + 会话置 error；<=0 关闭（默认）。
	SessionMaxWallClockMin int
	// ToolResultDumpRunes 工具结果统一收口阈值（TODO 第9项②，rune）；<=0 关闭
	//（未注入 ReactRuntimeConfig 的测试场景默认关闭；生产由 config applyDefaults 兜底 8000）。
	ToolResultDumpRunes int
	// ToolResultDigestRunes 收口后头部摘录 rune 数；<=0 按默认 2000。
	ToolResultDigestRunes int
	// StaleToolEvictRounds 陈旧只读工具结果驱逐轮数（TODO 第9项③）；<=0 关闭（生产默认 20）。
	StaleToolEvictRounds int
	// AgentsMDMaxRunes AGENTS.md/CLAUDE.md 项目自述注入上限（TODO 第10项⑦，rune）；<=0 关闭（生产默认 4000）。
	AgentsMDMaxRunes int
	// ToolParallelEnabled 轮内并行工具执行开关（TODO 第9项①）；nil=关闭
	//（未注入 ReactRuntimeConfig 的测试场景零值稳定；生产 config 默认 true）。
	ToolParallelEnabled *bool
	// ToolParallelMaxConcurrency 并行工具并发上限；<=0 按默认 4。
	ToolParallelMaxConcurrency int
}

// SetRuntimeConfig 注入 ReAct 主循环运行时参数（见 ReactRuntimeConfig）。
func (s *ReactService) SetRuntimeConfig(c ReactRuntimeConfig) {
	s.runtimeCfg = c
}

// SetPendingChildrenChecker 注入未决子 Agent 检查器，使后续创建的每个 ReActAgent
// 都开启父会话终结保护。由 bootstrap 在装配 subagent.Dispatcher 后调用。
func (s *ReactService) SetPendingChildrenChecker(p PendingChildrenChecker) {
	s.pendingChecker = p
}

// SetPausedChildChecker 注入 Paused 子 Agent 检查器，使 MetaAgent 在父终结保护
// wait loop 中能检测 Paused 子 DomainAgent 并主动暂停会话。由 bootstrap 注入。
func (s *ReactService) SetPausedChildChecker(p PausedChildChecker) {
	s.pausedChecker = p
}

// SetPausedDomainResumer 注入 Paused DomainAgent 恢复器，使 sendMessage 在
// PausedOnChild 态能优先恢复暂停的 domain。由 bootstrap 注入 subagent.Dispatcher。
func (s *ReactService) SetPausedDomainResumer(r PausedDomainResumer) {
	s.resumeDispatcher = r
}

// SetIdleRosterProvider 注入热驻空闲领域清单提供者（Domain 热驻），
// MetaAgent 每轮注入【空闲领域Agent】段供复用判定。由 bootstrap 注入 subagent.Dispatcher。
func (s *ReactService) SetIdleRosterProvider(p IdleRosterProvider) {
	s.idleRosterProvider = p
}

// SetIdleTTLArmer 注入 Idle TTL 武装器（Domain 热驻），新用户消息到达时武装
// 全部 idle domain 的加权销毁倒计时。由 bootstrap 注入 subagent.Dispatcher。
func (s *ReactService) SetIdleTTLArmer(a IdleTTLArmer) {
	s.idleTTLArmer = a
}

// ActivityEvidenceProvider 查询子 Agent 活动证据（TODO 第10项②展示面）；
// 由 subagent.Dispatcher 实现（ActivityEvidenceOf）。
type ActivityEvidenceProvider interface {
	// ActivityEvidenceOf 返回该 Agent 的最近活动种类与距今时长；ok=false 表示
	// 无活动监控条目（meta/已终结/热驻 Idle）。
	ActivityEvidenceOf(agentID string) (kind string, lastAgo time.Duration, ok bool)
}

// SetActivityEvidenceProvider 注入活动证据查询器，ListAgents 据此填充各节点的
// ActivityKind/LastActivityAgo。由 bootstrap 注入 subagent.Dispatcher。
func (s *ReactService) SetActivityEvidenceProvider(p ActivityEvidenceProvider) {
	s.activityEvidenceProvider = p
}

// SetSessionAgentWaker 注入会话挂起唤醒器（Domain 热驻），sendMessage 恢复路径
// 唤醒全部挂起 Agent（触限暂停波及全树的恢复入口）。由 bootstrap 注入 subagent.Dispatcher。
func (s *ReactService) SetSessionAgentWaker(w SessionAgentWaker) {
	s.sessionAgentWaker = w
}

// rosterFn 构造清单查询闭包；provider 未注入返回 nil（零注入）。
func (s *ReactService) rosterFn(sessionID string) func() []IdleDomainInfo {
	if s.idleRosterProvider == nil {
		return nil
	}
	return func() []IdleDomainInfo { return s.idleRosterProvider.IdleRoster(sessionID) }
}

// ForwardLiveEvent 是子 Agent 实时事件转发入口：Dispatcher 派发子 Agent 时注入的
// WithLiveEvents 回调按 sessionID 路由到这里，使子 Agent token 用量/流式增量/工具事件
// 也走会话级 handleLiveEvent，让 TUI/Web 看到所有 Agent 的累计 token。
// sessionID 来自子 Agent ctx（call_sub_agent 异步路径已 WithSessionID）。
func (s *ReactService) ForwardLiveEvent(sessionID string, ev LiveEvent) {
	if sessionID == "" {
		return
	}
	sess := s.store.snapshotSessionByID(sessionID)
	if sess == nil {
		return
	}
	// 子 Agent 的流式文本与主会话共用单一瞬时展示字段（StreamingText/ThinkingText），
	// 且多路并发会互相覆盖；加来源前缀让 UI 能分辨当前是谁在输出。
	// 主 Agent 的流式事件走 WithLiveEvents 直连 handleLiveEvent，不经过此处，不会被加前缀。
	if (ev.Kind == LiveEventLLMDelta || ev.Kind == LiveEventThinkDelta) && ev.Agent != "" {
		ev.Text = "【" + ev.Agent + "】\n" + ev.Text
	}
	s.handleLiveEvent(sess, ev)
}

// LoopConfig 把服务级配置映射为 ReActAgent 的 LoopConfig：
// 负数（配置语义"不限制"）归一为 0（agent 语义"不启用该限制"），
// 零值（未注入配置）回退到与旧行为一致的默认值。
func (c ReactRuntimeConfig) LoopConfig() LoopConfig {
	lc := LoopConfig{
		MaxIterations:      50,
		LLMTimeout:         300 * time.Second,
		RetryCount:         3,
		RetryBackoff:       100 * time.Millisecond,
		HistoryMaxMessages: 400, // 窗口只兜底防爆；真正约束是 token 阈值（压缩先于窗口），小窗口会抢先裁剪致隐性失忆
		ToolOutputMaxRunes: 2000,
	}
	if c.MaxIterations != 0 {
		lc.MaxIterations = max(c.MaxIterations, 0)
	}
	if c.LLMTimeoutSec != 0 {
		lc.LLMTimeout = time.Duration(max(c.LLMTimeoutSec, 0)) * time.Second
	}
	if c.RetryCount != 0 {
		lc.RetryCount = max(c.RetryCount, 0)
	}
	if c.RetryBackoffMs != 0 {
		lc.RetryBackoff = time.Duration(max(c.RetryBackoffMs, 0)) * time.Millisecond
	}
	if c.HistoryMaxMessages != 0 {
		lc.HistoryMaxMessages = max(c.HistoryMaxMessages, 0)
	}
	if c.ToolOutputHistoryMaxRunes != 0 {
		lc.ToolOutputMaxRunes = max(c.ToolOutputHistoryMaxRunes, 0)
	}
	// 上下文收口三件套 + 项目自述注入（TODO 第9项②③④ + 第10项⑦）：
	// 未注入（零值）保持关闭，测试场景零行为变化；生产由 bootstrap 注入 config 值
	//（applyDefaults 兜底 8000/2000/20/4000）。负数（显式关闭）归一为 0。
	if c.ToolResultDumpRunes != 0 {
		lc.ToolResultDumpRunes = max(c.ToolResultDumpRunes, 0)
	}
	if c.ToolResultDigestRunes != 0 {
		lc.ToolResultDigestRunes = max(c.ToolResultDigestRunes, 0)
	}
	if c.StaleToolEvictRounds != 0 {
		lc.StaleToolEvictRounds = max(c.StaleToolEvictRounds, 0)
	}
	if c.AgentsMDMaxRunes != 0 {
		lc.AgentsMDMaxRunes = max(c.AgentsMDMaxRunes, 0)
	}
	// 轮内并行工具执行（TODO 第9项①）：未注入（nil）保持关闭；生产 config 默认 true。
	if c.ToolParallelEnabled != nil {
		lc.ToolParallelEnabled = c.ToolParallelEnabled
	}
	if c.ToolParallelMaxConcurrency != 0 {
		lc.ToolParallelMaxConcurrency = max(c.ToolParallelMaxConcurrency, 1)
	}
	// TokenBudget 由 LoopConfigByRole 按角色注入（默认 150000，上下文阈值），
	// 不再从累计 TokenBudgetPerGoal 取值（累计预算已退役，见 roleTokenBudget）。
	return lc
}

// LoopConfigByRole 返回按角色定制的 LoopConfig:在 LoopConfig() 基础上按 roleID 覆盖 TokenBudget。
// TokenBudget 语义=上下文 token 阈值（替换原累计跨轮 token 预算）:Assemble 压缩后估算
// messages token >= 阈值即 LimitReached（近 N 单独就超、压不下去），暂停等续跑。
// 默认 150000 全角色（domain/meta/叶子同）；TokenBudgetPerRole 显式配置覆盖。
// 每轮独立估算（非跨轮累计），即每个 Agent 各自独立上下文预算。
func (c ReactRuntimeConfig) LoopConfigByRole(roleID string) LoopConfig {
	lc := c.LoopConfig()
	lc.TokenBudget = c.roleTokenBudget(roleID)
	return lc
}

// roleTokenBudget 返回角色上下文 token 阈值:显式配置优先，否则默认 ContextTokenBudget
//（未配时 150000）。meta 不给 0（无限）:config tool_call_max_rounds=-1 已使 maxIter 无界，
// 若阈值也无界，模型不收敛时会无限循环（实证:TUI 重复思考不前进）。150K 安全网让不收敛时压缩到限暂停等续跑。
func (c ReactRuntimeConfig) roleTokenBudget(roleID string) int {
	if v, ok := c.TokenBudgetPerRole[roleID]; ok {
		return max(v, 0)
	}
	if c.ContextTokenBudget > 0 {
		return c.ContextTokenBudget
	}
	return 150000
}

// SetModelProvider 注入一个 mock 或替代的模型 provider。
// 主要用于需要在无真实 API 密钥情况下运行 ReAct 循环的测试场景。
func (s *ReactService) SetModelProvider(p ModelProvider) {
	s.testProvider = p
}

// workDir 返回会话存储的工作目录，供 ReActAgent 在系统提示词中注入环境信息。
// 优先用 store.workDir；为空时回退到进程 cwd。
func (s *ReactService) workDir() string {
	if wd := s.store.workDir; wd != "" {
		return wd
	}
	return ""
}

// SetWorkDir 注入权威工作目录，覆盖 session store 默认空值。
// bootstrap 从 os.Getwd() 取得后串入，消除 newReactSessionStore 不再自取 cwd 的双源漂移。
// 空值忽略，保留 store 现值（测试场景可能已直设）。
func (s *ReactService) SetWorkDir(wd string) {
	if wd != "" {
		s.store.workDir = wd
	}
}

// SetLogger 注入结构化日志器，使会话存储的错误类日志以 [ERRO] 级别输出。
// 参数 l：已初始化的 Logger 指针；未注入时回退标准库 log。
func (s *ReactService) SetLogger(l *logger.Logger) {
	s.store.setLogger(l)
}

// sessionLogger 返回绑定 sessionID 与 agentName 的日志器，供 ReActAgent 记录 LLM I/O。
// 未注入全局 logger 时返回 nil，ReActAgent 侧跳过 LLM I/O 日志。
func (s *ReactService) sessionLogger(sessionID, agentName string) *logger.Logger {
	base := s.store.logger()
	if base == nil {
		return nil
	}
	return base.WithSession(sessionID).WithAgent(agentName)
}

// NewReactService 创建 ReactService，并注入 ReAct 引擎运行时所需的所有依赖。
//
// 参数说明：
//   - roleRegistry: 角色注册表；
//   - modelFactory: 模型工厂；
//   - toolRegistry: 工具注册表；
//   - mailbox: 消息邮箱；
//   - memory: 记忆管道；
//   - pgStore: PostgreSQL 持久化存储，用于历史会话读写。
func NewReactService(
	roleRegistry *role.Registry,
	modelFactory *model.ModelFactory,
	toolRegistry *tool.Registry,
	mailbox *mailbox.Mailbox,
	memory MemoryPipeline,
	pgStore *store.PostgresStore,
) *ReactService {
	// 初始化 ReactService 实例，并构建新的内存会话存储。
	s := &ReactService{
		store:        newReactSessionStore(),
		roleRegistry: roleRegistry,
		modelFactory: modelFactory,
		toolRegistry: toolRegistry,
		mailbox:      mailbox,
		memory:       memory,
	}
	// 将 PostgreSQL 存储与模型工厂注入会话存储，用于持久化与恢复。
	s.store.setPostgresStore(pgStore)
	s.store.setModelFactory(modelFactory)
	// 如果工具注册表存在，则注册进度回调，
	// 这样工具执行过程中产生的事件可以回流到对应会话。
	if toolRegistry != nil {
		toolRegistry.SetProgressCallback(s.handleToolEvent)
	}
	return s
}

// CreateSession 为指定目标创建一个新的 ReAct 会话，并异步启动 ReAct 主循环。
func (s *ReactService) CreateSession(ctx context.Context, req CreateRequest) (*Session, error) {
	// 防重复提交幂等闸：短间隔内（重载页面/新标签页/双击发送）对同一 goal 的重复
	// 建会话请求直接返回既有运行中会话。2026-09-07 实证：web 端双开导致两个 session
	// 并行执行同一任务 1 小时（其一被用户手动取消），白烧一半算力且互相写盘干扰。
	// 只拦 running 态完全同 goal：awaiting_clarify/paused 会话无法代收新文本，放行新建。
	s.createMu.Lock()
	defer s.createMu.Unlock()
	if dup := s.store.findRunningDuplicateSession(req.Goal); dup != nil {
		return toReactAgentSession(dup), nil
	}
	// 首条消息携带的用户视频（Alt+V 粘贴视频文件）：服务端抽帧（兼容全部
	// provider——所有现有 provider 均无原生视频输入 API），帧并入首轮图片走
	// 现有图片链路，元数据文本并入 goal（持久化）；Videos 本身不落库。
	// 转换同步执行（须在 run 启动前完成才能注入 runCtx），内部带硬超时与降级。
	frames, videoNotes := resolveVideosFn(ctx, req.Videos, s.videoOpts(), len(req.Images))
	goal := req.Goal
	if len(videoNotes) > 0 {
		goal = goal + "\n" + strings.Join(videoNotes, "\n")
	}
	// 在内存中创建会话对象（携带每会话工作目录，空=进程默认）。
	sess := s.store.createSession(goal, req.WorkDir)
	// 首条消息携带的用户图片（Alt+V 粘贴）：runSession 注入 runCtx 后一次性消费。
	sess.firstTurnImages = append(req.Images, frames...)
	s.maybeWatchWallClock(sess)
	// 在独立 goroutine 中运行 ReAct 循环，避免阻塞调用方。
	go s.runSession(sess)
	// 返回转换后的公共 Session DTO。
	return toReactAgentSession(sess), nil
}

// videoOpts 返回带默认值兜底的抽帧参数（config 约定"0=未配置"）。
func (s *ReactService) videoOpts() VideoOptions {
	return s.VideoOpts.withDefaults()
}

// maybeWatchWallClock 启动会话全局墙钟看门狗（TODO #25-4 硬止损）：
// SessionMaxWallClockMin>0 时，会话从创建起超时未终止则级联取消全部节点 +
// 会话置 error"超全局时限"。0=关闭（保持现状语义，不启动 goroutine）。
func (s *ReactService) maybeWatchWallClock(session *reactInternalSession) {
	maxWall := s.runtimeCfg.SessionMaxWallClockMin
	if maxWall <= 0 {
		return
	}
	s.startWallClock(session, time.Duration(maxWall)*time.Minute)
}

// startWallClock 启动墙钟看门狗 goroutine（测试可注入短时长）。
func (s *ReactService) startWallClock(session *reactInternalSession, wall time.Duration) {
	maxWall := int(wall / time.Minute)
	go func() {
		timer := time.NewTimer(wall)
		defer timer.Stop()
		<-timer.C

		s.store.mu.Lock()
		cur := s.store.sessions[session.ID]
		if cur == nil {
			s.store.mu.Unlock()
			return // 已淘汰
		}
		switch cur.Status {
		case enums.SessionStatusRunning, enums.SessionStatusAwaitingClarify, enums.SessionStatusPausedOnChild:
		default:
			s.store.mu.Unlock()
			return // 已终止
		}
		cancelFn := cur.cancelFn
		cur.cancelFn = nil
		cur.Status = enums.SessionStatusError
		cur.Result = fmt.Sprintf("会话超全局时限（%d 分钟），已强制终止", maxWall)
		now := time.Now()
		cur.EndedAt = &now
		s.store.mu.Unlock()

		s.store.addEvent(session, eventkind.System, "System", "会话超全局时限，已强制终止", "", "", "", "", "", true)
		// 级联取消全部在跑节点（与用户取消同语义）。
		s.cascadeCancelTree(session.ID)
		if cancelFn != nil {
			cancelFn()
		}
	}()
}

// cascadeCancelTree 遍历会话权威树，取消所有 Running/Paused/Idle 节点（TODO #25-2/25-4）。
// 供会话取消与全局墙钟到期时级联终止在跑子 Agent（detach ctx 的 pause/resume 好处保留，
// 仅"会话终止"这一刻级联）。Idle（热驻）节点绑定的销毁句柄一并触发——硬取消杀热驻实例。
func (s *ReactService) cascadeCancelTree(sessionID string) {
	t := s.TreeFor(sessionID)
	if t == nil {
		return
	}
	for _, n := range t.Snapshot() {
		if n.Status == orchestrator.StatusRunning || n.Status == orchestrator.StatusPaused || n.Status == orchestrator.StatusIdle {
			t.Cancel(n.ID)
		}
	}
}

// Get 根据会话 ID 获取会话。
// 如果会话不在内存（重启后未随 restoreSessions 批量恢复的老会话），
// 则从 PostgreSQL 单会话懒恢复后返回完整快照（事件 + 完整 History）。
func (s *ReactService) Get(ctx context.Context, sessionID string) (*Session, error) {
	// 优先从内存快照中查找会话。
	if sess := s.store.snapshotSessionByID(sessionID); sess != nil {
		return toReactAgentSession(sess), nil
	}
	// 内存未命中：单会话懒恢复（事件 + 完整 History），恢复成功返回完整快照。
	// 覆盖旧的两条消息降级快照，重启后旧会话的面板/续聊体验与内存会话一致；
	// /board /tree /metrics /logs 等子资源 handler 均先经 Get，旧会话面板自动可用。
	if restored := s.store.restoreOneSession(ctx, sessionID); restored != nil {
		return toReactAgentSession(restored), nil
	}
	// 既不在内存也不在历史记录中，返回未找到错误。
	return nil, ErrSessionNotFound
}

// defaultSessionListLimit List 合并内存与库记录的默认上限，防止旧库膨胀拖垮侧栏。
const defaultSessionListLimit = 200

// sessionFromHistoryRecord 将 session_history 记录转为轻量 Session DTO（不逐条查事件）。
// 状态经 restoredSessionStatus 映射（running → error + 中断提示）；历史会话没有准确的
// 结束时间，使用创建时间占位；Messages 重建为用户目标与结果摘要两条。
func sessionFromHistoryRecord(rec *store.SessionHistoryRecord) *Session {
	status, result, _ := restoredSessionStatus(rec.Status, rec.Summary)
	return &Session{
		ID:        rec.SessionID,
		Goal:      rec.Goal,
		Status:    string(status),
		Result:    result,
		StartedAt: rec.CreatedAt,
		EndedAt:   rec.CreatedAt,
		Events:    make([]Event, 0),
		Messages: []Message{
			{Role: string(enums.ChatRoleUser), Content: rec.Goal, Timestamp: rec.CreatedAt},
			{Role: string(enums.ChatRoleAssistant), Content: result, Timestamp: rec.CreatedAt},
		},
		WorkDir: rec.WorkDir,
	}
}

// mergeSessionLists 合并内存会话与库记录转出的会话列表：内存优先（权威，同 ID 丢弃库行），
// 按 StartedAt 降序排序后截断到 limit（<=0 时取 defaultSessionListLimit）。
// 纯函数，便于无 PG 单测覆盖去重/排序/截断。
func mergeSessionLists(memory, fromDB []*Session, limit int) []*Session {
	if limit <= 0 {
		limit = defaultSessionListLimit
	}
	seen := make(map[string]struct{}, len(memory))
	out := make([]*Session, 0, len(memory)+len(fromDB))
	for _, sess := range memory {
		seen[sess.ID] = struct{}{}
		out = append(out, sess)
	}
	for _, sess := range fromDB {
		if _, dup := seen[sess.ID]; dup {
			continue
		}
		seen[sess.ID] = struct{}{}
		out = append(out, sess)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// List 返回符合过滤条件的会话列表：内存会话优先，PostgreSQL 历史记录补充
// 重启后不在内存的旧会话（查询失败仅记日志，降级为纯内存列表）。
func (s *ReactService) List(ctx context.Context, filter Filter) ([]*Session, error) {
	// 获取所有内存会话（已按开始时间降序）。
	all := s.store.listSessions()
	// 预分配输出切片，容量与总数一致。
	memory := make([]*Session, 0, len(all))
	// 遍历会话并应用状态过滤。
	for _, sess := range all {
		// 如果指定了状态过滤且状态不匹配，则跳过。
		if filter.Status != "" && string(sess.Status) != filter.Status {
			continue
		}
		// 将内部会话转换为公共 DTO 并追加到结果。
		memory = append(memory, toReactAgentSession(sess))
	}
	// 库记录补充：仅当存在 pgStore 时查询；拉取条数取过滤上限与默认上限的较大值。
	var fromDB []*Session
	if s.store.pgStore != nil {
		// 设置 3 秒超时，避免外部存储故障导致长时间阻塞。
		fetchCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		fetchN := filter.Limit
		if fetchN < defaultSessionListLimit {
			fetchN = defaultSessionListLimit
		}
		if recs, err := s.store.pgStore.RecentSessionHistories(fetchCtx, fetchN); err != nil {
			// 查询失败不阻塞列表：记录日志并降级为纯内存列表（启动本就要求 PG 可达，
			// 此处兜底存储瞬时故障）。
			s.store.logError(fetchCtx, "查询会话历史列表失败，降级为内存列表", err)
		} else {
			fromDB = make([]*Session, 0, len(recs))
			for _, rec := range recs {
				// 库行状态映射后同样应用状态过滤。
				status, _, _ := restoredSessionStatus(rec.Status, rec.Summary)
				if filter.Status != "" && string(status) != filter.Status {
					continue
				}
				fromDB = append(fromDB, sessionFromHistoryRecord(rec))
			}
		}
	}
	// 合并去重、排序并截断。
	return mergeSessionLists(memory, fromDB, filter.Limit), nil
}

// Send 向指定会话投递一条用户消息。
func (s *ReactService) Send(ctx context.Context, sessionID string, msg Message) error {
	return s.sendMessageFull(ctx, sessionID, msg.Content, msg.Videos, msg.Images...)
}

// ResumeSession 继续一个之前已结束或暂停的会话。
// 如果请求中携带了 CarryOver 或 UserInput，会将其作为用户输入发送给会话。
func (s *ReactService) ResumeSession(ctx context.Context, sessionID string, req ResumeRequest) (*Session, error) {
	// 只要存在延续内容或用户输入，就尝试发送消息。
	if req.CarryOver != "" || req.UserInput != "" {
		content := req.UserInput
		// 优先使用 UserInput；若为空则退回到 CarryOver。
		if content == "" {
			content = req.CarryOver
		}
		if err := s.sendMessage(ctx, sessionID, content); err != nil {
			return nil, err
		}
	}
	// 再次获取会话快照并返回。
	sess := s.store.snapshotSessionByID(sessionID)
	if sess == nil {
		return nil, ErrSessionNotFound
	}
	return toReactAgentSession(sess), nil
}

// Stream 返回指定会话的实时事件流通道。
func (s *ReactService) Stream(ctx context.Context, sessionID string) (<-chan Event, error) {
	// 如果会话不存在，先尝试懒恢复（重启后未预热的老会话），仍不存在才返回错误。
	if s.store.snapshotSessionByID(sessionID) == nil {
		if s.store.restoreOneSession(ctx, sessionID) == nil {
			return nil, ErrSessionNotFound
		}
	}

	// 创建带缓冲的输出通道，降低发送阻塞。
	out := make(chan Event, 16)
	// 在独立 goroutine 中持续轮询会话事件并推送到通道。
	go func() {
		defer close(out)
		// 轮询间隔 50ms：流式文本/思考文本的增量更新也走该通道推送，
		// 间隔越短，TUI 吐词的跟手度越高。
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		// seen 记录已经推送过的事件数量，避免重复发送。
		seen := 0
		// lastStream/lastThink/lastStatus 记录上次推送时的瞬时状态，
		// 变化时推送一个轻量 live_update 事件，驱动 TUI 立即重绘（不等 tick）。
		var lastStream, lastThink, lastStatus string

		for {
			select {
			case <-ctx.Done():
				// 调用方上下文取消时，结束事件流。
				return
			case <-ticker.C:
				// 每次 ticker 触发时拉取会话快照。
				sess := s.store.snapshotSessionByID(sessionID)
				if sess == nil {
					// 会话已销毁，结束事件流。
					return
				}
				// 推送自 seen 以来的所有新事件。
				for i := seen; i < len(sess.Events); i++ {
					select {
					case out <- *toAgentEvent(&sess.Events[i]):
					case <-ctx.Done():
						return
					}
				}
				// 更新已推送位置。
				seen = len(sess.Events)
				// 流式文本/思考文本/会话状态变化：推送轻量刷新事件。
				if sess.StreamingText != lastStream || sess.ThinkingText != lastThink || string(sess.Status) != lastStatus {
					lastStream = sess.StreamingText
					lastThink = sess.ThinkingText
					lastStatus = string(sess.Status)
					select {
					case out <- *toAgentEvent(&internalEvent{Type: "live_update", Timestamp: time.Now()}):
					case <-ctx.Done():
						return
					}
				}
				// 如果会话已不在运行或等待澄清状态，等待短暂时间后结束，
				// 确保客户端收到最终的收尾事件。
				if sess.Status != enums.SessionStatusRunning && sess.Status != enums.SessionStatusAwaitingClarify {
					select {
					case <-time.After(200 * time.Millisecond):
					case <-ctx.Done():
					}
					return
				}
			}
		}
	}()

	return out, nil
}

// Query 回答关于会话的只读查询。
func (s *ReactService) Query(ctx context.Context, sessionID string, q Query) (Result, error) {
	// 根据查询类型分发处理。
	switch q.Kind {
	case QueryKindSessionCount:
		// 返回内存中的会话数量。
		return Result{Data: s.store.sessionCount()}, nil
	case QueryKindLLMStats:
		// 返回 LLM 调用统计：总调用数、超时数、平均耗时、最大耗时。
		calls, timeouts, avg, max := s.store.llmStats()
		return Result{Data: map[string]any{
			"calls":        calls,
			"timeouts":     timeouts,
			"avg_duration": avg.Round(time.Millisecond).String(),
			"max_duration": max.Round(time.Millisecond).String(),
		}}, nil
	case QueryKindLogs:
		// 返回 session_logs（含完整 LLM I/O prompt/response）。
		// agent/level 可空；limit<=0 时 QuerySessionLogs 默认 100。
		agent, _ := q.Args["agent"].(string)
		level, _ := q.Args["level"].(string)
		limit, _ := q.Args["limit"].(int)
		offset, _ := q.Args["offset"].(int)
		return Result{Data: s.store.queryLogs(ctx, sessionID, agent, level, limit, offset)}, nil
	case QueryKindBoard:
		// 会话任务看板：write_plan 权威快照优先，回退权威树合成（query_react.go）。
		return s.boardQueryResult(ctx, sessionID), nil
	case QueryKindMetrics:
		// 会话级 LLM 指标聚合（调用/超时/耗时/token）。
		return s.metricsQueryResult(ctx, sessionID), nil
	case QueryKindMailbox:
		// 会话权威树全部 Agent 实例的未取走邮箱消息。
		return s.mailboxQueryResult(ctx, sessionID), nil
	case QueryKindWatchdog:
		// watchdog 组件已随 runtime 步骤 6 退役（无决策产生方），
		// 返回空决策列表保持端点兼容。
		return Result{Data: []any{}}, nil
	case QueryKindTokenMetrics:
		// 按 agent|model 聚合的 token 消耗（HTTP 线型）。
		return s.tokenMetricsQueryResult(ctx, sessionID), nil
	case QueryKindEfficiency:
		// 会话效率一等指标（TODO 第9项⑥/第10项③）：五项指标 + 支路成本表。
		return s.efficiencyQueryResult(ctx, sessionID), nil
	case QueryKindAgentEvents:
		// 子 Agent 审计下钻（TODO 第10项③）：按实例 ID 取 agent_events 逐轮事件。
		agentID, _ := q.Args["agent"].(string)
		limit, _ := q.Args["limit"].(int)
		offset, _ := q.Args["offset"].(int)
		return s.agentEventsQueryResult(ctx, sessionID, agentID, limit, offset), nil
	case QueryKindWorktrees:
		// 会话 worktree 副本清单（TODO 第9⑤/#10⑤）：经结构化接口断言访问 Dispatcher。
		// 未接线（nil/未实现）返回空列表，端点可安全轮询。
		if wr, ok := s.stopMarker.(WorktreeReader); ok {
			return Result{Data: wr.ListWorktrees(sessionID)}, nil
		}
		return Result{Data: []WorktreeView{}}, nil
	case QueryKindWorktreeDiff:
		// 指定 worktree 的全量 diff（TODO 第10⑤ review 数据源）。
		agentID, _ := q.Args["agent"].(string)
		if wr, ok := s.stopMarker.(WorktreeReader); ok {
			diff, err := wr.WorktreeDiff(sessionID, agentID)
			if err != nil {
				return Result{}, err
			}
			return Result{Data: map[string]any{"agent_id": agentID, "diff": diff}}, nil
		}
		return Result{}, fmt.Errorf("worktree 能力未接线（dispatcher 未注入）")
	default:
		// 未知查询类型返回空结果。
		return Result{}, nil
	}
}

// Control 向会话发送操作指令。
func (s *ReactService) Control(ctx context.Context, sessionID string, cmd ControlCommand) error {
	// 根据操作类型分发到对应处理函数。
	switch cmd.Op {
	case ControlOpMessage:
		// 普通消息：提取 content 并发送。
		content, _ := cmd.Args["content"].(string)
		return s.sendMessage(ctx, sessionID, content)
	case ControlOpClarify:
		// 澄清答复：提取 answer 并答复。
		answer, _ := cmd.Args["answer"].(string)
		return s.answerClarify(ctx, sessionID, answer)
	case ControlOpInterrupt:
		// 中断：提取 content 并触发中断处理。
		content, _ := cmd.Args["content"].(string)
		return s.interrupt(ctx, sessionID, content)
	case ControlOpEnqueue:
		// 队列注入：提取 content 并注入会话。
		content, _ := cmd.Args["content"].(string)
		return s.enqueue(ctx, sessionID, content)
	case ControlOpCancel:
		// 取消会话。
		return s.cancel(ctx, sessionID)
	case ControlOpStop:
		// 软停止（TODO #37）：停止当前会话全部子任务，可续跑。
		return s.Stop(ctx, sessionID)
	case ControlOpTopic:
		// 话题切换：提取 name 与 goal 并切换话题。
		name, _ := cmd.Args["name"].(string)
		goal, _ := cmd.Args["goal"].(string)
		_, err := s.SwitchTopic(ctx, sessionID, name, goal)
		return err
	case ControlOpTrustMode:
		// 信任模式切换（TODO 第10⑥）：提取 mode 校验枚举后落会话，下一工具调用生效。
		mode, _ := cmd.Args["mode"].(string)
		return s.SetSessionTrustMode(sessionID, mode)
	case ControlOpWorktree:
		// worktree 合并门操作（TODO 第9⑤/#10⑤）：merge 走合并门，reject 驳回回信。
		action, _ := cmd.Args["action"].(string)
		agentID, _ := cmd.Args["agent"].(string)
		comments, _ := cmd.Args["comments"].(string)
		wo, ok := s.stopMarker.(WorktreeOperator)
		if !ok {
			return fmt.Errorf("worktree 能力未接线（dispatcher 未注入）")
		}
		switch action {
		case "merge":
			_, err := wo.MergeWorktree(ctx, sessionID, agentID)
			return err
		case "reject":
			_, err := wo.RejectWorktree(ctx, sessionID, agentID, comments)
			return err
		default:
			return fmt.Errorf("unknown worktree action: %s（可选 merge|reject，review 走 Query worktree-diff）", action)
		}
	default:
		// 未知操作返回错误。
		return fmt.Errorf("unknown control op: %s", cmd.Op)
	}
}

// ApprovalHook 返回破坏性操作的用户确认回调（TODO #17 P1 生产边界确认）。
// 由 bootstrap 注入 tool.Registry.SetApprovalHook；仅对命中边界的调用触发。
//
// 流程：置 PendingClarify + 会话暂停（awaiting_clarify）+ 推 clarify 事件 → 阻塞等用户答复
// （Agent goroutine 存活，不重建会话）→ sendMessage/answerClarify 把答复写入 approval 通道 →
// 返回裁决：true 放行执行，false 拒绝（工具结果带"已被用户拒绝"）。
// 会话取消（ctx 取消）时返回 ctx 错误，ReAct 循环正常退出。
func (s *ReactService) ApprovalHook() tool.ApprovalHookFunc {
	return func(ctx context.Context, toolName string, args map[string]any) (bool, error) {
		sid := tool.SessionIDFromContext(ctx)
		if sid == "" {
			return true, nil // 无法定位会话：放行保持自主。
		}
		s.store.mu.Lock()
		sess := s.store.sessions[sid]
		if sess == nil || sess.approval != nil {
			s.store.mu.Unlock()
			return true, nil // 会话不存在或已有待审批项：放行避免死锁。
		}
		ch := make(chan bool, 1)
		sess.approval = ch
		question := tool.ApprovalMessage(toolName, args)
		sess.pendingClarify = &ClarifyRequest{
			ID:        fmt.Sprintf("approve-%d", time.Now().UnixNano()),
			Question:  question,
			Context:   "破坏性工具调用待用户确认（生产边界/危险命令）",
			AgentID:   tool.AgentIDFromContext(ctx),
			CreatedAt: time.Now(),
			// TODO #53 结构化确认：Kind=confirm + 确认/拒绝两选项，
			// 答复按选项 ID 精确裁决（confirm/reject），自由文本经 parseApproval 兑底。
			Kind: "confirm",
			Options: []ClarifyOption{
				{ID: "confirm", Label: "确认执行", Description: "允许执行该操作"},
				{ID: "reject", Label: "拒绝取消", Description: "拒绝执行，取消本次调用"},
			},
		}
		sess.Status = enums.SessionStatusAwaitingClarify
		s.store.mu.Unlock()

		s.store.addEvent(sess, eventkind.Clarify, "System", question, "", "", "", "", "", true)

		// 等待用户期间保活：阻塞等答复时周期性刷新该 Agent 心跳（沿父链冒泡），
		// 巡检不会把"等用户操作"误判假死——一直等到用户答复或会话取消为止。
		keepalive := s.startUserWaitKeepalive(ctx)
		defer keepalive.Stop()
		select {
		case <-ctx.Done():
			s.store.mu.Lock()
			if sess.approval == ch {
				sess.approval = nil
				sess.pendingClarify = nil
			}
			s.store.mu.Unlock()
			return false, ctx.Err()
		case allow := <-ch:
			s.store.mu.Lock()
			if sess.approval == ch {
				sess.approval = nil
				sess.pendingClarify = nil
			}
			if sess.Status == enums.SessionStatusAwaitingClarify {
				sess.Status = enums.SessionStatusRunning
			}
			s.store.mu.Unlock()
			return allow, nil
		}
	}
}

// SetDefaultTrustMode 设置新建会话的初始信任模式（TODO 第10⑥）：config agent.trust_mode
// 经 bootstrap 注入。非法值返回错误（启动期 fail-fast），空串清空（回退现网语义）。
func (s *ReactService) SetDefaultTrustMode(mode string) error {
	if mode == "" {
		s.store.mu.Lock()
		s.store.defaultTrustMode = ""
		s.store.mu.Unlock()
		return nil
	}
	if !tool.ValidTrustMode(mode) {
		return fmt.Errorf("invalid trust mode %q (want suggest|auto-edit|full-auto)", mode)
	}
	s.store.mu.Lock()
	s.store.defaultTrustMode = mode
	s.store.mu.Unlock()
	return nil
}

// SetSessionTrustMode 切换既有会话的信任模式（TODO 第10⑥，HTTP/TUI 切换通道）：
// atomic 存储即时生效——正在阻塞的 ReAct 循环下一次工具派发即按新模式裁决。
// 非法值返回错误（HTTP 400 / TUI 报错）；会话不存在返回错误。
func (s *ReactService) SetSessionTrustMode(sessionID, mode string) error {
	if !tool.ValidTrustMode(mode) {
		return fmt.Errorf("invalid trust mode %q (want suggest|auto-edit|full-auto)", mode)
	}
	s.store.mu.RLock()
	sess := s.store.sessions[sessionID]
	s.store.mu.RUnlock()
	if sess == nil {
		return fmt.Errorf("session %s not found", sessionID)
	}
	sess.setTrustMode(mode)
	return nil
}

// SessionTrustMode 读取会话当前信任模式（HTTP GET 展示用）；未设置返回空串。
func (s *ReactService) SessionTrustMode(sessionID string) string {
	s.store.mu.RLock()
	sess := s.store.sessions[sessionID]
	s.store.mu.RUnlock()
	if sess == nil {
		return ""
	}
	return sess.currentTrustMode()
}

// parseApproval 把用户对破坏性操作确认的答复解析为裁决：明确同意 → true；
// 其余（拒绝及不明确文本）→ false。fail-closed：破坏性操作宁可拒绝不误执行。
func parseApproval(answer string) bool {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "确认", "同意", "允许", "批准", "yes", "y", "ok", "执行":
		return true
	}
	return false
}

// splitClarifyTokens 按逗号/顿号/分号/空白拆分答复 token，去重保序。
func splitClarifyTokens(answer string) []string {
	fields := strings.FieldsFunc(answer, func(r rune) bool {
		return r == ',' || r == '，' || r == '、' || r == ';' || r == '；' ||
			r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	seen := map[string]bool{}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// parseClarifyAnswer 把用户对澄清请求的答复解析为 (回传文本, 命中选项 ID 列表)
// （TODO #53 结构化选项）。仅当 pc 带选项时启用，否则原样返回（旧自由文本行为）：
//   - 单选/多选按分隔符拆分（逗号/顿号/分号/空白，中英文均可）；
//   - 每个 token 精确匹配选项 ID 或 Label，或数字序号 "N"（1 基）映射第 N 项
//     （TUI/Web 快捷选择都走这里）；
//   - 全部 token 命中 → 回传文本 = 命中选项 Label 以"、"连接，optionIDs 按序；
//   - 任一 token 未命中 → 回退自由文本原文（(answer, nil)），不吞用户输入。
func parseClarifyAnswer(answer string, pc *ClarifyRequest) (string, []string) {
	if pc == nil || len(pc.Options) == 0 {
		return answer, nil
	}
	ids := make([]string, 0, 2)
	labels := make([]string, 0, 2)
	for _, tok := range splitClarifyTokens(answer) {
		idx := -1
		for i, o := range pc.Options {
			if tok == o.ID || tok == o.Label {
				idx = i
				break
			}
		}
		if idx < 0 {
			// 数字序号：第 N 项（1 基）。
			if n, err := strconv.Atoi(tok); err == nil && n >= 1 && n <= len(pc.Options) {
				idx = n - 1
			}
		}
		if idx < 0 {
			return answer, nil // 任一 token 未命中：回退自由文本
		}
		ids = append(ids, pc.Options[idx].ID)
		labels = append(labels, pc.Options[idx].Label)
	}
	return strings.Join(labels, "、"), ids
}

// recordClarifyAnswer 把用户答复记录到 pendingClarify（回传文本 + 命中选项 ID，TODO #53），
// 返回 (回传文本, 命中选项 ID 列表)。回传文本 = 选项命中时 Label 连接 / 否则原文，
// 供 ask_user 工具结果带回 ReAct 循环；选项 ID 随快照透出供前端展示。
func recordClarifyAnswer(pc *ClarifyRequest, answer string) (string, []string) {
	text, ids := parseClarifyAnswer(answer, pc)
	if pc != nil {
		pc.Answer = text
		pc.AnswerOptionIDs = ids
		now := time.Now()
		pc.AnsweredAt = &now
	}
	return text, ids
}

// resolveApproval 把用户对破坏性操作确认的答复解析为裁决（TODO #53 选项化）：
// pendingClarify 带 confirm 选项时，命中选项 ID（confirm/reject）或数字序号直接裁决，
// 优先于关键词匹配；未命中回退 parseApproval 自由文本兑底。fail-closed。
func resolveApproval(answer string, pc *ClarifyRequest) bool {
	if pc != nil && len(pc.Options) > 0 {
		if _, ids := parseClarifyAnswer(answer, pc); len(ids) > 0 {
			switch ids[0] {
			case "confirm":
				return true
			case "reject":
				return false
			}
		}
	}
	return parseApproval(answer)
}

// AskUserHook 返回 ask_user 工具的会话层回调（TODO #24 人在回路，#53 结构化选项）。
// 由 bootstrap 注入 tool.Registry.SetAskUserHook；meta/domain Agent 在任务执行中
// 主动提问时触发。与 ApprovalHook 同通道范式：置 PendingClarify + 会话暂停
// （awaiting_clarify）+ 推 clarify 事件 → 阻塞等用户答复（Agent goroutine 存活）→
// sendMessage/answerClarify 把**原始答复文本**写入 askUser 通道 →
// ask_user 工具结果带回 ReAct 循环。会话取消时返回 ctx 错误。
// opts.Options 非空时（TODO #53）透传为结构化选项（Kind=choice），用户可点选。
func (s *ReactService) AskUserHook() tool.AskUserHookFunc {
	return func(ctx context.Context, question string, opts tool.AskUserOptions) (string, error) {
		sid := tool.SessionIDFromContext(ctx)
		if sid == "" {
			return "", fmt.Errorf("ask_user: missing session context")
		}
		// 等待用户期间保活：同 ApprovalHook，等答复不被心跳巡检误判假死。
		// 提前到排队之前启动：排队等槽位期间本 Agent 同样阻塞无活动，需要保活覆盖。
		keepalive := s.startUserWaitKeepalive(ctx)
		defer keepalive.Stop()
		// 提问槽位只有一个（与审批共用）。已有待答复项时排队等空位（2s 一拍），
		// 不硬错——"路线分叉先问后派"下并行领域 Agent 同时提问是常态，硬错会迫使
		// 后到的 Agent 放弃提问改自行猜测。等待受 ctx 约束（会话取消或 timeout_sec
		// 超时自然退出，工具侧兑底"用户未答复，自行决策"）。
		s.store.mu.Lock()
		for {
			sess := s.store.sessions[sid]
			if sess == nil {
				s.store.mu.Unlock()
				return "", fmt.Errorf("ask_user: 会话不存在")
			}
			if sess.approval == nil && sess.askUser == nil {
				break // 拿到空槽位；锁保持持有，下方直接占位
			}
			s.store.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(2 * time.Second):
			}
			s.store.mu.Lock()
		}
		sess := s.store.sessions[sid]
		ch := make(chan string, 1)
		sess.askUser = ch
		req := &ClarifyRequest{
			ID:          fmt.Sprintf("ask-%d", time.Now().UnixNano()),
			Question:    question,
			Context:     "Agent 向用户提问（人在回路）",
			AgentID:     tool.AgentIDFromContext(ctx),
			CreatedAt:   time.Now(),
			MultiSelect: opts.MultiSelect,
		}
		if len(opts.Options) > 0 {
			// 结构化选项（TODO #53）：Kind=choice；纯自由文本提问保持旧行为（Kind=text）。
			req.Kind = "choice"
			for _, o := range opts.Options {
				req.Options = append(req.Options, ClarifyOption{ID: o.ID, Label: o.Label, Description: o.Description})
			}
			// 单选 choice 自动追加「其他」逃生选项：选项不精确/方向不对时用户点选后
			// 自由填写答案，而不是被迫二选一。多选可勾选组合，不追加。
			if !req.MultiSelect {
				hasOther := false
				for _, o := range req.Options {
					if o.ID == ClarifyOtherOptionID {
						hasOther = true
						break
					}
				}
				if !hasOther {
					req.Options = append(req.Options, ClarifyOption{
						ID:          ClarifyOtherOptionID,
						Label:       "其他（自行输入答案）",
						Description: "以上选项不够精确或方向不对时选这项，然后在输入框填写你的答案",
					})
				}
			}
		} else {
			req.Kind = "text"
		}
		sess.pendingClarify = req
		sess.Status = enums.SessionStatusAwaitingClarify
		s.store.mu.Unlock()

		// detail（如 submit_plan 计划全文）完整落对话区事件流；面板只渲染 question（短）。
		eventText := "Agent 提问: " + question
		if d := strings.TrimSpace(opts.Detail); d != "" {
			eventText = "Agent 提问: " + d + "\n" + question
		}
		s.store.addEvent(sess, eventkind.Clarify, "System", eventText, "", "", "", "", "", true)

		select {
		case <-ctx.Done():
			s.store.mu.Lock()
			if sess.askUser == ch {
				sess.askUser = nil
				sess.pendingClarify = nil
			}
			s.store.mu.Unlock()
			return "", ctx.Err()
		case answer := <-ch:
			s.store.mu.Lock()
			if sess.askUser == ch {
				sess.askUser = nil
				sess.pendingClarify = nil
			}
			if sess.Status == enums.SessionStatusAwaitingClarify {
				sess.Status = enums.SessionStatusRunning
			}
			s.store.mu.Unlock()
			return answer, nil
		}
	}
}

// ListAgents 返回与会话关联的运行时 Agent 实例列表。
// MetaAgent 为固定根节点，子 Agent 实例来自权威 Agent 树（TreeFor），
// 顶层派发节点挂到 MetaAgent 下形成完整层级（Idle 热驻节点由展示层过滤）。
func (s *ReactService) ListAgents(ctx context.Context, sessionID string) ([]AgentInstance, error) {
	// 获取会话快照，确认会话存在。
	sess := s.store.snapshotSessionByID(sessionID)
	if sess == nil {
		return nil, ErrSessionNotFound
	}
	// 默认状态为活跃；若会话已完成或出错，则标记为结束。
	status := string(enums.RoleStatusActive)
	if sess.Status == enums.SessionStatusCompleted || sess.Status == enums.SessionStatusError {
		status = string(enums.RoleStatusDone)
	}
	instances := []AgentInstance{
		{
			Name:     "MetaAgent",
			Role:     "meta",
			RoleType: enums.RoleTypeMeta,
			Status:   status,
			ModuleID: "meta",
		},
	}
	// 权威树快照 → 实例视图：ModuleID=节点 ID，ParentID=父节点 ID（顶层挂 meta）。
	for _, n := range s.TreeFor(sessionID).Snapshot() {
		parent := n.ParentID
		if parent == "" {
			parent = "meta"
		}
		name := n.Domain
		if name == "" {
			name = n.Role
		}
		inst := AgentInstance{
			Name:      name,
			Role:      n.Role,
			ModuleID:  n.ID,
			ParentID:  parent,
			Status:    n.Status.String(),
			Domain:    n.Domain,
			Goal:      n.Task,
			RoleDefID: n.Role,
		}
		// 活动证据展示面（TODO 第10项②）：运行中节点附"最近活动种类 + 距今时长"，
		// TUI/Web 渲染为 "in <tool> · active Xs ago"，让假死可见可判。
		if s.activityEvidenceProvider != nil {
			if kind, ago, ok := s.activityEvidenceProvider.ActivityEvidenceOf(n.ID); ok {
				inst.ActivityKind = kind
				inst.LastActivityAgo = ago.Truncate(time.Second).String()
			}
		}
		instances = append(instances, inst)
	}
	return instances, nil
}

// TreeFor 按 sessionID 取得权威 Agent 树（不存在则 lazy 创建并从持久化层恢复）。
// Dispatcher 在派发子 Agent 时调用此方法注入 Register/Finish/SetCancel。
// treeStore 非 nil 时,新建 Tree 会调 LoadFromStore 恢复历史节点(重启不丢);
// 为 nil 时纯内存,与原行为一致。
func (s *ReactService) TreeFor(sessionID string) *orchestrator.Tree {
	if v, ok := s.trees.Load(sessionID); ok {
		return v.(*orchestrator.Tree)
	}
	t := orchestrator.NewTree(sessionID, s.treeStore)
	if s.treeStore != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = t.LoadFromStore(ctx)
		cancel()
	}
	v, loaded := s.trees.LoadOrStore(sessionID, t)
	if loaded {
		return v.(*orchestrator.Tree)
	}
	return t
}

// Tree 返回会话 Agent 树快照，按启动时间升序。
// 会话不存在返回 ErrSessionNotFound；树为空时返回空切片。
func (s *ReactService) Tree(ctx context.Context, sessionID string) ([]orchestrator.Node, error) {
	if sess := s.store.snapshotSessionByID(sessionID); sess == nil {
		return nil, ErrSessionNotFound
	}
	t := s.TreeFor(sessionID)
	return t.Snapshot(), nil
}

// CancelAgent 取消指定子 Agent 实例。
// instID 对应 call_sub_agent 返回的 sub_agent_id。
// 节点不存在或已 terminal 返回 ErrAgentNotFound。
func (s *ReactService) CancelAgent(ctx context.Context, sessionID, instID string) error {
	if sess := s.store.snapshotSessionByID(sessionID); sess == nil {
		return ErrSessionNotFound
	}
	t := s.TreeFor(sessionID)
	if !t.Cancel(instID) {
		return ErrAgentNotFound
	}
	return nil
}

// PauseAgent 手动暂停指定 domain 支路（TODO 第10项③ 审计面止血动作）。
// 仅 Role=domain 且 Running 的节点可暂停：先 MarkPauseNode 让 dispatcher 收尾分流到
// Pause 分支（SaveMessages + tree.Pause，热驻槽 parked 可 ResumePaused 续），再
// StopRunning 触发该支路 cancel；StopRunning 落空（节点已终态等）则回滚标记返回错误。
// 非热驻叶子请用 CancelAgent；全树软停请用 Stop——两者语义不同勿混。
func (s *ReactService) PauseAgent(ctx context.Context, sessionID, instID string) error {
	s.store.mu.Lock()
	session, ok := s.store.sessions[sessionID]
	s.store.mu.Unlock()
	if !ok {
		return ErrSessionNotFound
	}
	t := s.TreeFor(sessionID)
	node, ok := t.Get(instID)
	if !ok {
		return ErrAgentNotFound
	}
	if node.Role != "domain" {
		return fmt.Errorf("%w: 仅 domain 支路支持手动暂停（叶子用 cancel，全树用 stop）", ErrInvalidSessionState)
	}
	if node.Status != orchestrator.StatusRunning {
		return fmt.Errorf("%w: 支路 %s 状态 %s 不可暂停", ErrInvalidSessionState, instID, node.Status)
	}
	// 先登记暂停意图，再触发 cancel：runSubAgent/runDomainTask 的 Canceled 分支
	// 据此路由到 Pause 收尾而非软停/硬销毁。
	if pm, ok := s.stopMarker.(interface{ MarkPauseNode(string) }); ok {
		pm.MarkPauseNode(instID)
	} else {
		return fmt.Errorf("%w: dispatcher 未接线，无法暂停", ErrInvalidSessionState)
	}
	if !t.StopRunning(instID) {
		// cancel 未触发（节点已终态竞态）：回滚标记防幽灵暂停意图。
		if pm, ok := s.stopMarker.(interface{ ClearPauseNode(string) }); ok {
			pm.ClearPauseNode(instID)
		}
		return fmt.Errorf("%w: 支路 %s 已不在运行", ErrInvalidSessionState, instID)
	}
	s.store.addEvent(session, eventkind.System, "System",
		fmt.Sprintf("手动暂停 domain 支路 %s（%s），恢复请用 resume", instID, node.Domain),
		"", "", "", "", "", true)
	log.Printf("[service] PAUSE-AGENT: session=%s agent=%s domain=%s", sessionID, instID, node.Domain)
	return nil
}

// Shutdown 取消所有正在运行的会话。Domain 热驻模式下同时销毁全部热驻槽
// （agent_messages 保留，进程重启后 idle 树节点标 done）。
func (s *ReactService) Shutdown(ctx context.Context) error {
	if w, ok := s.sessionAgentWaker.(interface{ DestroyAllIdle() }); ok {
		w.DestroyAllIdle()
	}
	// worktree 残留副本清理（TODO 第9⑤）：进程退出前 best-effort 移除全部会话
	// 未合并副本（patch 文件保留），防 git worktree 元数据悬挂。
	if wo, ok := s.stopMarker.(WorktreeOperator); ok {
		for _, sid := range s.store.sessionIDs() {
			wo.CleanupSessionWorktrees(sid)
		}
	}
	s.store.shutdown()
	return nil
}

// SummarizeTaskTitle 为长任务标题生成一个简短的展示标题。
func (s *ReactService) SummarizeTaskTitle(ctx context.Context, title string) string {
	// 如果模型工厂不可用，直接返回原标题。
	if s.store.modelFactory == nil {
		return title
	}
	// 构造压缩提示词，要求模型输出 40 字以内、保留核心动作与对象的任务名。
	prompt := fmt.Sprintf("将以下任务描述压缩成 40 字以内的简短任务名，保留核心动作与对象，不要解释：\n%s", title)
	// 设置 3 秒超时，避免摘要生成拖垮接口。
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	// 调用轻量模型生成摘要。
	brief, err := s.store.modelFactory.CallLightweightWithRetry(callCtx, prompt)
	if err != nil || strings.TrimSpace(brief) == "" {
		return title
	}
	// 去除首尾空白与常见引号、括号等装饰字符。
	brief = strings.TrimSpace(brief)
	brief = strings.Trim(brief, "\"'"+"`「」【】()")
	// 按 rune 截断到 40 字，避免多字节字符被截断。
	if len([]rune(brief)) > 40 {
		brief = string([]rune(brief)[:40]) + "…"
	}
	return brief
}

// LaunchSession 实现 dag.SessionLauncher 接口。
func (s *ReactService) LaunchSession(goal string) string {
	// 创建会话并异步启动 ReAct 循环（dag 启动器无每会话工作目录，回落进程默认）。
	sess := s.store.createSession(goal, "")
	s.maybeWatchWallClock(sess)
	go s.runSession(sess)
	return sess.ID
}

// RestoreSessions 从 PostgreSQL 加载历史会话到内存。
func (s *ReactService) RestoreSessions(ctx context.Context, limit int) int {
	return s.store.restoreSessions(ctx, limit)
}

// ClearSessionChat 清除会话的聊天记录，但保留初始系统/用户消息。
func (s *ReactService) ClearSessionChat(id string) bool {
	return s.store.clearSessionChat(id)
}

// SessionCount 返回当前内存中持有的会话数量。
func (s *ReactService) SessionCount() int {
	return s.store.sessionCount()
}

// LLMStats 返回聚合的 LLM 调用统计信息。
func (s *ReactService) LLMStats() (callCount, timeoutCount int, avgDur, maxDur time.Duration) {
	return s.store.llmStats()
}

// SwitchTopic 切换会话的当前话题(轻量话题隔离)。
//
// 流程:
//  1. 终结旧话题:取消当前 Agent 树所有 Running 节点 -> 压缩旧树快照为摘要 ->
//     写入 sharedKV `topic:{oldTopicID}:summary`(store 非 nil 时) -> 删除 PG 旧节点。
//  2. 起新话题:生成本会话内单调递增的 topicID,更新 session.activeTopicID,
//     新话题从空 Agent 树开始(同 Tree 实例,内存已清空)。
//
// 运行中与已完成会话均支持:已完成会话会重启 running 状态承载新话题。
// 无状态机,纯 KV 摘要 + 树切换。MetaAgent 在新话题召回旧摘要依赖步骤 4 part C(未做)。
//
// name 为新话题显示名;goal 为话题目标,空时回退到 name。
func (s *ReactService) SwitchTopic(ctx context.Context, sessionID, name, goal string) (*Session, error) {
	if name == "" {
		return nil, fmt.Errorf("name cannot be empty")
	}
	if goal == "" {
		goal = name
	}

	// 查找会话并记录切换前状态。
	s.store.mu.Lock()
	session, ok := s.store.sessions[sessionID]
	if !ok {
		s.store.mu.Unlock()
		return nil, ErrSessionNotFound
	}
	oldTopicID := session.activeTopicID
	workDir := session.workDir // 话题摘要 KV 按会话目录解析(S2):锁内快照,供下方注入
	wasRunning := session.Status == enums.SessionStatusRunning
	s.store.mu.Unlock()

	// 终结旧话题的 Agent 树:取消 Running 节点 + 压缩摘要 + 清内存 + 删 PG。
	tree := s.TreeFor(sessionID)
	snapshot := tree.EndCurrentTopic()
	// 旧话题有节点时压缩摘要写入 sharedKV,供新话题召回。
	// key 格式 `topic:{sessionID}:{topicID}:summary`:sessionID 前缀隔离,防跨 session 污染
	// (与块记忆 recall 同原则)。召回侧 recallTopicSummaries 按 session 前缀读取。
	if oldTopicID != "" && s.sharedMemoryStore != nil && len(snapshot) > 0 {
		summary := summarizeTopicSnapshot(oldTopicID, snapshot)
		// 注入会话工作目录:HTTP 请求 ctx 不含会话 workDir,KV 须按会话目录解析 .bma 根。
		kvCtx, cancel := context.WithTimeout(tool.WithWorkDir(ctx, workDir), 3*time.Second)
		if err := s.sharedMemoryStore.Set(kvCtx, "topic:"+sessionID+":"+oldTopicID+":summary", summary); err != nil {
			s.store.logError(ctx, "switch topic: write old topic summary to KV failed", err)
		}
		cancel()
	}

	// 生成新话题 ID:本会话内单调递增,首话题用 "1",后续 +1。
	newTopicID := nextTopicID(oldTopicID)

	s.store.mu.Lock()
	session.activeTopicID = newTopicID
	// 已完成会话切换话题时重启为 running,承载新任务。
	if !wasRunning {
		session.Status = enums.SessionStatusRunning
		session.Result = ""
		endedAt := time.Time{}
		session.EndedAt = &endedAt
	}
	s.store.mu.Unlock()

	// 记录话题切换事件(含旧/新 topic ID,便于审计)。
	s.store.addEvent(session, eventkind.Progress, "User",
		fmt.Sprintf("切换话题: [%s] -> [%s] (topic %s -> %s)", session.Goal, name, oldTopicID, newTopicID),
		eventkind.TopicSwitch, "", "", "", "", true)
	session.Goal = goal

	return toReactAgentSession(s.store.snapshotSessionByID(sessionID)), nil
}

// nextTopicID 根据旧 topic ID 生成本会话内下一个单调递增 ID。
// 旧 ID 空返回 "1";旧 ID 为纯数字返回 N+1;解析失败回退带 "-2" 后缀保证唯一。
func nextTopicID(old string) string {
	if old == "" {
		return "1"
	}
	if n, err := strconv.Atoi(old); err == nil {
		return strconv.Itoa(n + 1)
	}
	return old + "-2"
}

// summarizeTopicSnapshot 把旧话题 Agent 树快照压缩为 KV 摘要文本。
// 格式:话题 ID 头 + 每节点一行(角色/状态/摘要/错误)。供新话题召回参考。
func summarizeTopicSnapshot(topicID string, nodes []orchestrator.Node) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "【话题 %s 摘要】\n", topicID)
	for _, n := range nodes {
		fmt.Fprintf(&sb, "- [%s/%s] %s", n.Role, n.Status.String(), n.Task)
		if n.Summary != "" {
			fmt.Fprintf(&sb, " => %s", n.Summary)
		}
		if n.Err != "" {
			fmt.Fprintf(&sb, " (err: %s)", n.Err)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// recallTopicSummaries 召回当前 session 历史话题摘要,供新话题 MetaAgent 续接上下文。
// key 格式 `topic:{sessionID}:{topicID}:summary`:按 session 前缀过滤防跨 session 污染,
// 跳过 currentTopicID(当前话题摘要未写,只在切换时落 KV)。无配置/无历史返回空串。
func (s *ReactService) recallTopicSummaries(ctx context.Context, sessionID, currentTopicID string) string {
	if s.sharedMemoryStore == nil {
		return ""
	}
	keys := s.sharedMemoryStore.Keys(ctx)
	prefix := "topic:" + sessionID + ":"
	var summaries []string
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) || !strings.HasSuffix(k, ":summary") {
			continue
		}
		topicID := strings.TrimSuffix(strings.TrimPrefix(k, prefix), ":summary")
		if topicID == "" || topicID == currentTopicID {
			continue
		}
		v, err := s.sharedMemoryStore.Get(ctx, k)
		if err != nil || strings.TrimSpace(v) == "" {
			continue
		}
		summaries = append(summaries, v)
	}
	return strings.Join(summaries, "\n\n")
}

// injectTopicRecall 按 activeTopicID 召回旧话题摘要并拼到 input 前。
// 用 recalledTopicID 去重:同一话题只注入一次,避免每次 resume 重复烧 token。
// 无历史摘要时也标记 recalledTopicID,避免反复扫 KV。
// 返回注入后的 input(无摘要时原样),以及是否已处理(recalledTopicID 已更新)。
func (s *ReactService) injectTopicRecall(ctx context.Context, session *reactInternalSession, input string) string {
	s.store.mu.Lock()
	active := session.activeTopicID
	done := session.recalledTopicID
	s.store.mu.Unlock()
	if done == active {
		return input
	}
	prefix := s.recallTopicSummaries(ctx, session.ID, active)
	s.store.mu.Lock()
	session.recalledTopicID = active
	s.store.mu.Unlock()
	if prefix == "" {
		return input
	}
	return prefix + "\n\n【当前话题目标】\n" + input
}

// handleToolEvent 接收工具注册表产生的进度事件，并将其注入到对应运行中会话的事件流。
func (s *ReactService) handleToolEvent(ctx context.Context, ev tool.ProgressEvent) {
	// 从 ctx 提取调用方 Agent ID（ReActAgent.Run 经 WithAgentID 注入）。
	// ProgressEvent.Agent 字段未被 emitTool/emitResult 填充，始终为空；
	// 不在此回填会导致日志 agentName 永远空串，sub-agent 工具事件无法归属。
	// MetaAgent 的 agent ID = session-ID（"session-1"），sub-agent = "session-1/code_assistant-5"。
	agentID := AgentIDFromContext(ctx)
	// 展示名优先取 role.Name（"MetaAgent"/"代码助手"/"领域Agent:xxx"），
	// 使日志与对话页可读；缺失时回退到 agentID，保证不空串。
	agentName := AgentDisplayNameFromContext(ctx)
	if agentName == "" {
		agentName = agentID
	}
	// 加读锁判断会话是否存在且处于运行状态。
	s.store.mu.RLock()
	session, ok := s.store.sessions[ev.SessionID]
	isRunning := ok && session != nil && session.Status == enums.SessionStatusRunning
	s.store.mu.RUnlock()
	// 会话不存在或未运行则直接丢弃事件。
	if !isRunning {
		return
	}
	// call_sub_agent 的事件由 LiveEvent 通道以更丰富的形式记录（sub_agent_dispatch
	// 含角色与任务摘要、tool_exec 含子 Agent ID），跳过注册表侧的固定文案事件，避免重复。
	if ev.Tool == "call_sub_agent" {
		return
	}

	// 只要不是 Error 类型事件，就视为成功。
	success := ev.Kind != eventkind.Error
	// 实例归属写入 DetailJSON，供 TUI 领域进度面板按 agent_id 匹配事件到 Agent 实例。
	detailJSON := agentIDJSON(agentID)
	// 工具调用事件：直接记录工具执行事件。
	if ev.Kind == "tool_call" || ev.Kind == eventkind.ToolCall {
		s.store.addEventDetail(session, eventkind.ToolExec, agentName, ev.Message, ev.Kind, ev.Tool, toolArgsLabel(ev.Detail), "", "", success, detailJSON)
		return
	}
	// 工具结果事件：尝试解析 Detail 中的 output、error 与 path 字段。
	if ev.Kind == "tool_result" || ev.Kind == eventkind.ToolResult {
		var output, toolErr, toolPath string
		if ev.Detail != "" {
			var detail map[string]any
			// 解析 JSON 详情，忽略解析失败的情况。
			if err := json.Unmarshal([]byte(ev.Detail), &detail); err == nil {
				if v, ok := detail["output"].(string); ok {
					output = v
				}
				if v, ok := detail["error"].(string); ok {
					toolErr = v
				}
				// path 由工具执行器填充（如 ReadFile 的文件路径、HTTPGet 的 URL），
				// 供 TUI 在工具行显示操作对象。
				if v, ok := detail["path"].(string); ok {
					toolPath = v
				}
			}
		}
		// 失败时把 path+error 拼进 message，让 logInfo 输出可见失败原因；
		// 否则日志只显示"工具结果 WriteFile"，错误细节只存在 DB 事件里，排查困难。
		msg := ev.Message
		if toolErr != "" {
			extra := ""
			if toolPath != "" {
				extra = " path=" + toolPath
			}
			msg = fmt.Sprintf("%s [FAIL]%s err=%s", ev.Message, extra, toolErr)
		}
		s.store.addEventDetail(session, eventkind.ToolExec, agentName, msg, ev.Kind, ev.Tool, toolPath, output, toolErr, success, detailJSON)
		return
	}
	// 其他类型事件作为进度事件记录。
	s.store.addEventDetail(session, eventkind.Progress, agentName, ev.Message, ev.Kind, ev.Tool, "", "", "", success, detailJSON)
}

// toolArgsLabel 从工具调用参数 JSON 中提取一个简短的展示标签（路径/命令/URL 等），
// 供 TUI 在工具调用行中显示操作对象；无法解析时返回空串。
func toolArgsLabel(argsJSON string) string {
	if argsJSON == "" {
		return ""
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ""
	}
	for _, k := range []string{"path", "command", "url", "pattern", "dir", "query", "file"} {
		if v, ok := args[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// subAgentDispatchInfo 从 call_sub_agent 的入参 JSON 中提取角色 ID 与任务全文，
// 供子 Agent 派发事件记录使用；显示端（TUI）自行截断标题，完整记录看全文。
// 解析失败时返回空角色与原始输入的截断。
func subAgentDispatchInfo(argsJSON string) (roleID, taskBrief string) {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", textutil.TruncateRunes(argsJSON, 2000, "…")
	}
	if v, ok := args["role_id"].(string); ok {
		roleID = v
	}
	if v, ok := args["task"].(string); ok {
		taskBrief = strings.ReplaceAll(strings.TrimSpace(v), "\n", " ")
	}
	return roleID, taskBrief
}

// runSession 为新创建的会话执行 ReAct 主循环。
func (s *ReactService) runSession(session *reactInternalSession) {	// 获取会话上下文；若不存在则使用 Background。
	ctx := sessionContext(session)
	// 会话结束后清理临时目录并淘汰已完成会话。
	defer s.finalizeSession(session)

	// 记录会话启动事件。
	s.store.addEvent(session, eventkind.System, "System", "会话启动", "", "", "", "", "", true)

	// 轮开始落库 running 状态行（009 status 列；History 为空时 persistFullHistory 自动跳过）：
	// 进程在本轮中途崩溃/断电时，重启恢复逻辑依据库中 running 状态把会话标记为"因服务重启中断"，
	// 优雅停机路径标记不到硬崩溃场景。
	s.store.persistHistory(session)
	s.store.persistEvents(session)

	// 获取 meta 角色配置；若缺失则标记会话错误并退出。
	metaRole := s.roleRegistry.Get("meta")
	if metaRole == nil {
		s.setSessionError(session, "meta role not found")
		return
	}

	// 确定模型 provider：优先使用测试注入的 provider，否则从模型工厂获取。
	var provider ModelProvider
	if s.testProvider != nil {
		provider = s.testProvider
	} else {
		p, err := s.modelFactory.GetBladesProvider(ctx, "meta")
		if err != nil {
			s.setSessionError(session, fmt.Sprintf("failed to get blades provider: %v", err))
			return
		}
		provider = p
	}

	// 构造 ReActAgent，并注入邮箱、记忆管道与主循环运行时配置。
	// MetaAgent 暴露 call_sub_agent + 只读/信息类工具（ReadFile/ListDir/SearchInFiles/HTTPGet），
	// 不暴露 WriteFile/RunCommand，防止越位直接改文件或跑命令（metaRole.Tools 白名单限定）。
	// 任务看板注入（TODO #35 Phase 0）：每轮上下文末尾追加【任务看板】段——编排状态
	// 机器可读且压缩不可达，"重新执行"类短指令的消歧锚点。仅 meta 注入，子 Agent 不注入。
	// 热驻空闲领域清单注入（Domain 热驻）：供 MetaAgent 自主判定强相关复用 vs 弱相关新建。
	// 系统提示词工作目录按会话解析(终审修复):会话 workDir 优先,空串回落进程默认目录,
	// 使 buildEnvBlock/LoadProjectDoc 与 createSession 的 EnsureProjectDoc 落在同一目录。
	wd := session.workDir
	if wd == "" {
		wd = s.workDir()
	}
	metaMemory := wrapMetaMemory(s.memory, s.boardFn, session.ID)
	metaMemory = wrapMetaMemoryWithRoster(metaMemory, s.rosterFn(session.ID))
	metaMemory = wrapMetaMemoryWithLedger(metaMemory, s.ledgerFn, session.ID)
	agent := NewReActAgent(session.ID, *metaRole, provider, NewToolRegistryAdapterForRole(s.toolRegistry, session.ID, metaRole.Tools, "meta", s.pluginVisibility)).
		WithMailbox(s.mailbox).
		WithMemory(metaMemory).
		WithLoopConfig(s.runtimeCfg.LoopConfigByRole("meta")).
		WithLiveEvents(func(ev LiveEvent) { s.handleLiveEvent(session, ev) }).
		WithLogger(s.sessionLogger(session.ID, metaRole.Name)).
		WithWorkDir(wd).
		WithSkillBlock(s.metaSkillBlock()).
		WithPersonaInjector(s.metaPersona(session.workDir))
	// 注入未决子 Agent 检查器，开启父会话终结保护。
	if s.pendingChecker != nil {
		agent = agent.WithPendingChildrenChecker(s.pendingChecker)
	}
	// 注入 Paused 子 Agent 检查器，使 MetaAgent 在 wait loop 检测 Paused 子 domain 并主动暂停。
	if s.pausedChecker != nil {
		agent = agent.WithPausedChildChecker(s.pausedChecker)
	}

	// 将会话 ID 注入工具上下文，便于工具内部识别当前会话。
	runCtx := tool.WithSessionID(ctx, session.ID)
	// 注入每会话工作目录（空则原样返回，回落 Executor 默认目录）。
	runCtx = tool.WithWorkDir(runCtx, session.workDir)
	// 注入会话级 stopCtx（TODO 第10④）：子派发以此取消基底，stop 窗口期新派发即刻终止。
	runCtx = tool.WithStopContext(runCtx, session.stopCtx)
	// 注入信任模式读取器（TODO 第10⑥）：闭包实时读会话 atomic 值，HTTP/TUI 中途切换
	// 下一工具调用即生效；未设置返回空串 → Registry 回退现网语义。
	runCtx = tool.WithTrustModeFunc(runCtx, session.currentTrustMode)

	// 首条消息用户图片（Alt+V 粘贴）注入 runCtx 后一次性消费置 nil。
	if len(session.firstTurnImages) > 0 {
		runCtx = WithUserImages(runCtx, session.firstTurnImages)
		session.firstTurnImages = nil
	}

	// 召回旧话题摘要拼到目标前(切换话题后续接上下文);同一话题只注入一次。
	// 用注入会话 workDir 后的 runCtx:话题摘要 KV 按会话目录解析 .bma 根。
	goal := s.injectTopicRecall(runCtx, session, session.Goal)
	// 经验技能预筛（设计 §6.5）：goal 向量 top-3 命中只注一行提示，不注全文。
	goal = s.injectSkillRecall(ctx, goal)

	// 运行 ReAct 主循环，传入会话目标。
	result, err := agent.Run(runCtx, goal)
	if err != nil {
		// 热驻模式软停止分流：用户 Stop 取消了 session ctx（打断流式 LLM），
		// domain 已转 Idle 热驻——落可续跑暂停态而非 error，partial history 保留。
		if s.isSoftStopCancel(session, err) {
			s.pauseSession(session, result.History, s.softStopPauseKind(session))
			return
		}
		// 运行出错时标记会话错误并退出。
		s.setSessionError(session, err.Error())
		return
	}

	// 达到轮数上限：不视为失败——暂停会话、保留全部进度，等待用户消息续跑。
	if result.LimitReached {
		kind := PauseIterationLimit
		if result.PausedOnChild {
			kind = PauseOnChild
		}
		s.pauseSession(session, result.History, kind)
		return
	}

	// 运行成功：更新会话状态为已完成，并记录结果与历史。
	now := time.Now()
	s.store.mu.Lock()
	session.Status = enums.SessionStatusCompleted
	session.Result = result.Text
	session.EndedAt = &now
	session.History = result.History
	s.store.mu.Unlock()

	// 添加 Agent 完成事件。
	s.store.addEvent(session, eventkind.AgentDone, "MetaAgent", result.Text, "", "", "", "", "", true)

	// 会话进化（2026-09-02 设计 §6.1）：一次轻量模型调用产出三类沉淀
	//（用户偏好/项目经验/技能包），失败降级纯画像提取，零副作用。
	s.evolveSession(session, "success")

	// 持久化历史与事件到 Postgres。
	s.store.persistHistory(session)
	s.store.persistEvents(session)
}

// resumeSession 使用新的用户输入继续会话。
func (s *ReactService) resumeSession(session *reactInternalSession) {
	// 获取会话上下文。
	ctx := sessionContext(session)
	// 结束后清理资源。
	defer s.finalizeSession(session)

	// 获取 meta 角色；缺失则报错。
	metaRole := s.roleRegistry.Get("meta")
	if metaRole == nil {
		s.setSessionError(session, "meta role not found")
		return
	}

	// 确定模型 provider，逻辑同 runSession。
	var provider ModelProvider
	if s.testProvider != nil {
		provider = s.testProvider
	} else {
		p, err := s.modelFactory.GetBladesProvider(ctx, "meta")
		if err != nil {
			s.setSessionError(session, fmt.Sprintf("failed to get blades provider: %v", err))
			return
		}
		provider = p
	}

	// 构造并配置 ReActAgent。
	// MetaAgent 暴露 call_sub_agent + 只读/信息类工具（ReadFile/ListDir/SearchInFiles/HTTPGet），
	// 不暴露 WriteFile/RunCommand，防止越位直接改文件或跑命令（metaRole.Tools 白名单限定）。
	// 任务看板注入（TODO #35 Phase 0）：同 runSession，每轮末尾追加【任务看板】段。
	// 热驻空闲领域清单注入（Domain 热驻）：供复用判定。
	// 系统提示词工作目录按会话解析(终审修复,同 runSession):会话 workDir 优先,空串回落进程默认目录。
	wd := session.workDir
	if wd == "" {
		wd = s.workDir()
	}
	metaMemory := wrapMetaMemory(s.memory, s.boardFn, session.ID)
	metaMemory = wrapMetaMemoryWithRoster(metaMemory, s.rosterFn(session.ID))
	metaMemory = wrapMetaMemoryWithLedger(metaMemory, s.ledgerFn, session.ID)
	agent := NewReActAgent(session.ID, *metaRole, provider, NewToolRegistryAdapterForRole(s.toolRegistry, session.ID, metaRole.Tools, "meta", s.pluginVisibility)).
		WithMailbox(s.mailbox).
		WithMemory(metaMemory).
		WithLoopConfig(s.runtimeCfg.LoopConfigByRole("meta")).
		WithLiveEvents(func(ev LiveEvent) { s.handleLiveEvent(session, ev) }).
		WithLogger(s.sessionLogger(session.ID, metaRole.Name)).
		WithWorkDir(wd).
		WithSkillBlock(s.metaSkillBlock()).
		WithPersonaInjector(s.metaPersonaLite())
	// 注入未决子 Agent 检查器，开启父会话终结保护。
	if s.pendingChecker != nil {
		agent = agent.WithPendingChildrenChecker(s.pendingChecker)
	}
	// 注入 Paused 子 Agent 检查器，使 MetaAgent 在 wait loop 检测 Paused 子 domain 并主动暂停。
	if s.pausedChecker != nil {
		agent = agent.WithPausedChildChecker(s.pausedChecker)
	}

	// 注入会话 ID 到工具上下文。
	runCtx := tool.WithSessionID(ctx, session.ID)
	// 注入每会话工作目录（空则原样返回，回落 Executor 默认目录）。
	runCtx = tool.WithWorkDir(runCtx, session.workDir)
	// 注入会话级 stopCtx（TODO 第10④）：续跑路径同 runSession，子派发以此取消基底。
	runCtx = tool.WithStopContext(runCtx, session.stopCtx)
	// 注入信任模式读取器（TODO 第10⑥）：同 runSession，中途切换下一工具调用生效。
	runCtx = tool.WithTrustModeFunc(runCtx, session.currentTrustMode)

	// 使用最新用户消息作为本轮输入，并以之前的历史作为种子。
	// 倒序取最后一条 user 消息（中途可能追加了澄清/审批答复等非 user 项），
	// 同步取出该轮用户图片注入 runCtx：带外穿透给 RunWithHistory 的首条
	// user 消息与 call_sub_agent 子 Agent（每轮新 runCtx，图片按轮作用域）。
	var input string
	var turnImages []tool.ResultImage
	s.store.mu.RLock()
	for i := len(session.Messages) - 1; i >= 0; i-- {
		if session.Messages[i].Role == string(enums.ChatRoleUser) {
			input = session.Messages[i].Content
			turnImages = session.Messages[i].Images
			break
		}
	}
	// 拷贝历史记录，避免在加锁期间被外部修改。
	history := make([]ReactMessage, len(session.History))
	copy(history, session.History)
	s.store.mu.RUnlock()
	if len(turnImages) > 0 {
		runCtx = WithUserImages(runCtx, turnImages)
	}

	// 召回旧话题摘要拼到本轮输入前(切换话题后续接上下文);同一话题只注入一次。
	// 用注入会话 workDir 后的 runCtx(同 runSession):话题摘要 KV 按会话目录解析。
	input = s.injectTopicRecall(runCtx, session, input)
	// 经验技能预筛（设计 §6.5）：续跑新输入同样做向量预筛提示。
	input = s.injectSkillRecall(ctx, input)

	// 调用带历史的 ReAct 运行接口。
	result, err := agent.RunWithHistory(runCtx, input, history)
	if err != nil {
		// 软停止分流（同 runSession）。
		if s.isSoftStopCancel(session, err) {
			s.pauseSession(session, result.History, s.softStopPauseKind(session))
			return
		}
		s.setSessionError(session, err.Error())
		return
	}

	// 达到轮数上限：不视为失败——暂停会话、保留全部进度，等待用户消息续跑。
	if result.LimitReached {
		kind := PauseIterationLimit
		if result.PausedOnChild {
			kind = PauseOnChild
		}
		s.pauseSession(session, result.History, kind)
		return
	}

	// 更新会话状态为已完成。
	now := time.Now()
	s.store.mu.Lock()
	session.Status = enums.SessionStatusCompleted
	session.Result = result.Text
	session.EndedAt = &now
	session.History = result.History
	s.store.mu.Unlock()

	// 添加完成事件并持久化。
	s.store.addEvent(session, eventkind.AgentDone, "MetaAgent", result.Text, "", "", "", "", "", true)

	// 会话进化（2026-09-02 设计 §6.1）：一次轻量模型调用产出三类沉淀
	//（用户偏好/项目经验/技能包），失败降级纯画像提取，零副作用。
	s.evolveSession(session, "success")

	s.store.persistHistory(session)
	s.store.persistEvents(session)
}

// finalizeSession 在会话结束时执行清理工作。
func (s *ReactService) finalizeSession(session *reactInternalSession) {
	// 会话结束（完成/出错/暂停）时清空流式输出与思考过程状态，UI 停止渲染瞬时内容。
	s.store.setStreamingText(session, "")
	s.store.setThinkingText(session, "")
	// 暂停待续（awaiting_clarify / paused_on_child）的会话保留临时目录，用户续跑时仍需其中的中间产物。
	if session.Status != enums.SessionStatusAwaitingClarify && session.Status != enums.SessionStatusPausedOnChild {
		// 清理会话临时目录。
		s.store.cleanupSessionTempDir(session.ID, session.TempDir)
	}
	// 淘汰已完成的会话，避免内存无限增长。
	s.store.evictCompletedSessions()
}

// finalizeThinking 把累积的思考过程（session.ThinkingText）落为会话 think 事件并清空。
// 调用时机：答复文本开始输出（LiveEventLLMDelta）或工具调用开始（LiveEventToolCall），
// 即一次 LLM 轮次的思考阶段结束。思考不再只是瞬时展示，而是保留在事件流中，
// 让 TUI/Web 能回看每个 Agent 每轮的推理（>500 事件时被 trimDebugEvents 头部裁剪，量有界）。
func (s *ReactService) finalizeThinking(session *reactInternalSession, ev LiveEvent) {
	if session == nil || strings.TrimSpace(session.ThinkingText) == "" {
		return
	}
	text := session.ThinkingText
	// 子 Agent 的思考文本经 ForwardLiveEvent 加过【展示名】前缀，落事件时剥掉，
	// 事件本身已带 Agent 归属，避免前缀污染 Web 思考链与 TUI 展示。
	if ev.Agent != "" {
		text = strings.TrimPrefix(text, "【"+ev.Agent+"】\n")
	}
	// 思考文本不在事件层截断：Web 需全量回看，TUI 展示侧自行折叠。
	text = strings.TrimSpace(text)
	if text == "" {
		s.store.setThinkingText(session, "")
		return
	}
	// 去重：思考流与答复流交错时每个 LLMDelta 触发一次 finalize，累积式思考文本
	// 会产出数十条内容完全相同的 think 事件（日志重复刷屏，实证同一条等待叙事 40+ 行）。
	if text == session.lastThinkEventText {
		s.store.setThinkingText(session, "")
		return
	}
	session.lastThinkEventText = text
	s.store.addEventDetail(session, eventkind.Think, ev.Agent, text, eventkind.Think, "", "", "", "", true, agentIDJSON(ev.AgentID))
	s.store.setThinkingText(session, "")
}

// agentIDJSON 把 Agent 实例 ID 编码为事件的 DetailJSON（{"agent_id":"..."}），
// 供 TUI 领域进度面板按实例匹配事件；空 ID 返回空串。
func agentIDJSON(agentID string) string {
	if agentID == "" {
		return ""
	}
	b, err := json.Marshal(map[string]string{"agent_id": agentID})
	if err != nil {
		return ""
	}
	return string(b)
}

// handleLiveEvent 把 ReAct 运行中的实时进度事件写入会话：
// LLM/思考流式增量原位更新对应文本字段（不产生事件记录，避免事件流/数据库被 token 级事件淹没）；
// 工具调用/执行完成与子 Agent 完成追加为会话事件（少量且有审计价值，随 persistEvents 持久化）。
func (s *ReactService) handleLiveEvent(session *reactInternalSession, ev LiveEvent) {
	switch ev.Kind {
	case LiveEventLLMDelta:
		// 答复文本开始输出时，思考阶段结束：先落 think 事件，再清空瞬时思考展示。
		s.finalizeThinking(session, ev)
		s.store.setStreamingText(session, ev.Text)
	case LiveEventThinkDelta:
		s.store.setThinkingText(session, ev.Text)
	case LiveEventToolCall:
		// 工具调用开始同样意味着思考阶段结束（思考型模型常见 think→tool 而非 think→text）。
		s.finalizeThinking(session, ev)
		// call_sub_agent 是子 Agent 派发：记录专用派发事件（角色 ID 与任务摘要），
		// 供 TUI 对话区展示阶段标记、编排面板派生子 Agent 节点。
		if ev.Tool == "call_sub_agent" {
			roleID, taskBrief := subAgentDispatchInfo(ev.Input)
			s.store.addEvent(session, eventkind.Message, ev.Agent, taskBrief, "sub_agent_dispatch", roleID, "", "", "", true)
		}
		// 其他工具的调用事件已由工具注册表的进度回调记录（handleToolEvent），此处不重复。
	case LiveEventToolExec:
		// 其他工具的执行事件已由工具注册表的进度回调记录（handleToolEvent），
		// 这里只补 call_sub_agent：其 Output 是子 Agent ID，记入 ToolPath 供 UI 统计"等待中的子 Agent"。
		if ev.Tool != "call_sub_agent" {
			return
		}
		s.store.addEvent(session, eventkind.ToolExec, ev.Agent, "", "", ev.Tool, strings.TrimSpace(ev.Output), ev.Output, ev.Error, ev.Success)
	case LiveEventSubAgentDone:
		s.store.addEvent(session, eventkind.Message, "SubAgent", ev.Tool, "sub_agent_done", "", "", "", "", true)
		// 子 Agent 结果摘要（react_agent 侧已截断 200 runes）落 llm_result 事件，
		// 让 TUI/Web 直接看到领域 Agent 的关键产出；Tool 承载子 Agent ID 供 TUI 映射展示名。
		if strings.TrimSpace(ev.Text) != "" {
			s.store.addEventDetail(session, eventkind.Progress, "SubAgent", ev.Text, eventkind.LLMResult, ev.Tool, "", "", "", true, agentIDJSON(ev.Tool))
		}
	case LiveEventTokenUsage:
		// 单次 LLM 调用 token 用量：记为 token_usage 调试事件，供前端实时累加展示。
		// 消息格式 in=<n> out=<n> cache_hit=<n> cache_miss=<n> 与 textutil.ParseTokenUsage
		// 兼容（前缀 in=/out= 不变），便于后端日志解析复用。
		msg := fmt.Sprintf("in=%d out=%d cache_hit=%d cache_miss=%d", ev.InputTokens, ev.OutputTokens, ev.CacheHitTokens, ev.CacheMissTokens)
		s.store.addEventDebug(session, eventkind.Stats, ev.Agent, msg, eventkind.TokenUsage, "", "", "", "", true, "", int(ev.InputTokens), int(ev.OutputTokens), int(ev.CacheHitTokens), int(ev.CacheMissTokens), "")
	}
}

// PauseKind 区分会话暂停的原因，供 pauseSession 选择目标状态与文案。
type PauseKind int

const (
	// PauseIterationLimit MetaAgent 达到最大轮数上限（budget=0 不触 token，仅轮数）。
	PauseIterationLimit PauseKind = iota
	// PauseTokenBudget 达到 token 预算上限（非 meta 角色路径，预留）。
	PauseTokenBudget
	// PauseOnChild 子 DomainAgent 触达 token 上限暂停，MetaAgent 检测后主动暂停会话。
	PauseOnChild
	// PauseUserStop 用户软停止（Domain 热驻模式）：MetaAgent API 调用被取消，
	// partial history 保留，领域 Agent 已转 Idle 热驻；发消息可续跑（复用 idle 域）。
	PauseUserStop
)

// pauseMessage 按 PauseKind 返回面向用户的暂停提示文案。
func (s *ReactService) pauseMessage(kind PauseKind) string {
	switch kind {
	case PauseOnChild:
		return "子领域 Agent 触达 token 上限已暂停（完整历史已持久化，可恢复）。发送任意消息（如\"继续\"）将优先恢复暂停的领域 Agent 继续执行。"
	case PauseTokenBudget:
		return "已达 token 预算上限，会话已暂停。发送任意消息（如\"继续\"）将从当前进度继续执行。"
	case PauseUserStop:
		if s.hotResident {
			return "任务已停止（领域 Agent 已热驻保留）。发送消息可继续任务或派发新任务（空闲领域 Agent 可复用）。"
		}
		return "任务已停止。发送消息可继续任务（销毁倒计时内未续跑将销毁）。"
	default:
		// PauseIterationLimit：maxIter<=0（不限制）时不显示具体轮数，避免 "0 轮" 误报。
		maxIter := s.runtimeCfg.LoopConfig().MaxIterations
		if maxIter <= 0 {
			return "已达轮数上限，会话已暂停。发送任意消息（如\"继续\"）将从当前进度继续执行。"
		}
		return fmt.Sprintf("已达最大轮数上限（%d 轮），会话已暂停。发送任意消息（如\"继续\"）将从当前进度继续执行。", maxIter)
	}
}

// isSoftStopCancel 判定 MetaAgent 的 ctx 取消错误是否来自用户软停止。
// 判据：软停止标记命中 + 错误链含 context.Canceled。硬取消（cancel 路径）
// 先置 Error 终态再 cancel，会话已非 Running，此处为 false——setSessionError 的首错
// 胜出保护兜底，不会覆盖"cancelled by user"。
func (s *ReactService) isSoftStopCancel(session *reactInternalSession, err error) bool {
	if s.stopMarker == nil {
		return false
	}
	if !errors.Is(err, context.Canceled) {
		return false
	}
	type softStopQuerier interface {
		IsSoftStop(sessionID string) bool
	}
	if q, ok := s.stopMarker.(softStopQuerier); ok {
		return q.IsSoftStop(session.ID)
	}
	return false
}

// softStopPauseKind 软停止取消后的暂停分类：旧模式（非热驻）下树中存在
// Running/Paused domain 节点（MetaAgent 原本阻塞在等子 domain）时维持
// PauseOnChild 原恢复路由（resumePausedDomain 续跑暂停域）；否则（如在跑
// LLM 被打断、无域在跑）落 PauseUserStop（awaiting_clarify，发消息走
// MetaAgent 续跑）。热驻模式恒为 PauseUserStop（域转 Idle 由
// ResumeSessionAgents 唤醒，不依赖 PausedOnChild 路由）。
func (s *ReactService) softStopPauseKind(session *reactInternalSession) PauseKind {
	if s.hotResident {
		return PauseUserStop
	}
	if t := s.TreeFor(session.ID); t != nil {
		for _, n := range t.Snapshot() {
			if n.Role == "domain" && (n.Status == orchestrator.StatusRunning || n.Status == orchestrator.StatusPaused) {
				return PauseOnChild
			}
		}
	}
	return PauseUserStop
}

// pauseSession 按暂停原因将会话置为对应暂停态而非错误：
// PauseOnChild -> paused_on_child（优先恢复暂停的 domain）；其他 -> awaiting_clarify（普通续跑）。
// History 完整保留，sendMessage -> resumeSession/resumePausedDomain 从当前进度续跑。
func (s *ReactService) pauseSession(session *reactInternalSession, history []ReactMessage, kind PauseKind) {
	s.store.mu.Lock()
	session.History = history
	switch kind {
	case PauseOnChild:
		session.Status = enums.SessionStatusPausedOnChild
		session.Result = "子领域 Agent 触达 token 上限暂停，发\"继续\"恢复该领域"
	case PauseUserStop:
		session.Status = enums.SessionStatusAwaitingClarify
		if s.hotResident {
			session.Result = "任务已停止，领域 Agent 热驻保留，发送消息续跑"
		} else {
			session.Result = "任务已停止，发送消息可续跑（销毁倒计时内未续跑将销毁）"
		}
	default:
		session.Status = enums.SessionStatusAwaitingClarify
		session.Result = "已达上限，会话暂停，等待用户消息续跑"
	}
	s.store.mu.Unlock()

	s.store.addEvent(session, eventkind.System, "System", s.pauseMessage(kind), "", "", "", "", "", true)

	s.store.persistHistory(session)
	s.store.persistEvents(session)
}

// setSessionError 将会话标记为错误状态，并记录相关事件与持久化。
func (s *ReactService) setSessionError(session *reactInternalSession, msg string) {
	now := time.Now()
	// 更新会话状态、结果与结束时间。
	s.store.mu.Lock()
	// 首次错误胜出：会话已处于 Error 终态且已有结果时不覆盖（TODO #25-4——
	// 墙钟到期先置"超全局时限"，随后 runSession 因 ctx 取消的 setSessionError 不应覆盖）。
	if session.Status == enums.SessionStatusError && session.Result != "" {
		s.store.mu.Unlock()
		return
	}
	session.Status = enums.SessionStatusError
	session.Result = msg
	session.EndedAt = &now
	s.store.mu.Unlock()

	// 添加错误事件。
	s.store.addEvent(session, eventkind.Error, "System", msg, "", "", "", "", "", false)

	// 会话进化（设计 §6.1 失败会话参与进化，踩坑经验价值更高）：
	// 用户主动取消（context canceled）不进化。
	if !strings.Contains(msg, "context canceled") {
		s.evolveSession(session, "failed")
	}

	// 持久化历史与事件。
	s.store.persistHistory(session)
	s.store.persistEvents(session)
}

// sessionContext 返回会话的上下文；若未设置则返回 Background。
func sessionContext(session *reactInternalSession) context.Context {
	if session.ctx != nil {
		return session.ctx
	}
	return context.Background()
}

// restartSessionContext 为被恢复的会话创建一个全新的可取消上下文，
// 确保之前被取消的会话能够再次运行。stopCtx（TODO 第10④）同步重建：
// 上次 Stop/destroy 已把旧 stopCtx 取消，续跑派发的子 Agent 必须换绑新基底，
// 否则恢复后新派发全部继承已取消 context 秒死。
func restartSessionContext(session *reactInternalSession) {
	ctx, cancel := context.WithCancel(context.Background())
	session.ctx = ctx
	session.cancelFn = cancel
	stopCtx, stopCancel := context.WithCancel(context.Background())
	session.stopCtx = stopCtx
	session.stopCancel = stopCancel
}

// sendMessage 向会话发送一条消息，并在必要时恢复会话运行。
// images 为用户随消息粘贴的图片（Alt+V），内存透传：挂到该条 Message 上，
// 由 resumeSession 取出注入 runCtx（带外穿透到 call_sub_agent 子 Agent）。
func (s *ReactService) sendMessage(ctx context.Context, sessionID, content string, images ...tool.ResultImage) error {
	return s.sendMessageFull(ctx, sessionID, content, nil, images...)
}

// sendMessageFull 是 sendMessage 的完整版，额外接收视频附件 vids：
// 服务端抽帧（兼容全部 provider）并入 images、元数据文本并入 content，
// 转换后 vids 即弃（与 Images 同为内存透传不持久化）。
func (s *ReactService) sendMessageFull(ctx context.Context, sessionID, content string, vids []WireVideo, images ...tool.ResultImage) error {
	// 空内容直接拒绝。
	if content == "" {
		return fmt.Errorf("content cannot be empty")
	}

	// 用户视频附件（Alt+V 粘贴视频文件）：抽帧并入 images、元数据文本并入
	// content。转换必须在取 store.mu 之前完成（ffmpeg 可达数秒，不能持锁），
	// 内部带硬超时与降级，不返回 error。
	frames, videoNotes := resolveVideosFn(ctx, vids, s.videoOpts(), len(images))
	if len(frames) > 0 {
		images = append(images, frames...)
	}
	if len(videoNotes) > 0 {
		content = content + "\n" + strings.Join(videoNotes, "\n")
	}

	// 加锁查找会话。
	s.store.mu.Lock()
	session, ok := s.store.sessions[sessionID]
	if !ok {
		s.store.mu.Unlock()
		// 懒恢复：老会话不在内存（重启后未在 restoreSessions 批量恢复范围内）时，
		// 从 PG 恢复该会话后继续本次发送，实现"任意旧会话可续聊"。
		// 恢复会话 approval/askUser 通道为 nil，下方澄清分支自然跳过；非 Running
		// 状态走置 Running + resumeSession 路径（History 已自 agent_messages 重建）。
		restored := s.store.restoreOneSession(ctx, sessionID)
		if restored == nil {
			return ErrSessionNotFound
		}
		s.store.mu.Lock()
		session = restored
	}

	// 待答复的 Agent 提问（TODO #24 ask_user；#53 选项解析）：答复写 askUser 通道。
	// 带选项时按选项 ID/Label/数字序号解析回传（Label 连接文本），自由文本原样透传。
	if session.askUser != nil {
		text, _ := recordClarifyAnswer(session.pendingClarify, content)
		session.askUser <- text
		session.Messages = append(session.Messages, Message{
			Role:      string(enums.ChatRoleUser),
			Content:   "[澄清答复] " + content,
			Timestamp: time.Now(),
		})
		s.store.mu.Unlock()
		s.store.addEvent(session, eventkind.Clarify, "User", "提问答复: "+content, "", "", "", "", "", true)
		return nil
	}

	// 待审批的破坏性操作（TODO #17 P1；#53 选项化）：答复路由进审批通道。
	// Agent goroutine 存活，不重建会话不 resume（避免双跑）；答复不进入 LLM 对话历史，
	// 由工具结果带回 ReAct 循环。confirm 选项命中（confirm/reject/数字序号）直接裁决，
	// 否则 parseApproval 关键词兑底（fail-closed：不明确即拒绝）。
	if session.approval != nil {
		_, _ = recordClarifyAnswer(session.pendingClarify, content)
		session.approval <- resolveApproval(content, session.pendingClarify)
		session.Messages = append(session.Messages, Message{
			Role:      string(enums.ChatRoleUser),
			Content:   "[澄清答复] " + content,
			Timestamp: time.Now(),
		})
		s.store.mu.Unlock()
		s.store.addEvent(session, eventkind.Clarify, "User", "审批答复: "+content, "", "", "", "", "", true)
		return nil
	}

	// 用户输入自动提示词补全（TODO #36 Phase 0 规则版 + #39 四层管线）：命中续跑/控制/
	// 诊断意图时附加【系统补全】段（意图标签 + 最近失败/未完成任务绑定），只增不改用户原文。
	// askUser/approval 澄清答复分支在上面已提前返回，不经过补全（答复非新任务）。
	// 判定路径备注暂存，待 store.mu 释放后落 Prompt 事件（addEvent 自身要加锁，
	// 持锁调用会死锁——approval 分支同款模式），便于复盘误判率与 L2 调用率（TODO #39 可观测性）。
	content, enhanceNote := s.enhanceUserInput(ctx, sessionID, content)

	// 将用户消息追加到会话消息列表。
	session.Messages = append(session.Messages, Message{
		Role:      string(enums.ChatRoleUser),
		Content:   content,
		Timestamp: time.Now(),
		Images:    images,
	})

	// 新用户消息 = 新任务起点：清空该 session 的 ReadFile 已读记录。
	// 设计意图：原始事故是单任务内反复读同一文件；任务完成后用户提新需求（如修 bug）
	// 需重读已改文件，不应被历史记录卡死。重复读限制为单任务级而非整个 session 级。
	if s.toolRegistry != nil {
		s.toolRegistry.ResetReadHistory(sessionID)
	}

	// 同理重置派发计数：全局派发限额按 session 累计，上一任务的消耗不应卡死下一任务。
	if r, ok := s.pendingChecker.(DispatchCountResetter); ok {
		r.ResetDispatchCounts(sessionID)
	}

	// 热驻 Idle TTL 武装（Domain 热驻）：完成后一直热存，用户下一条消息到达才启动
	// 加权销毁倒计时（未武装的 idle 槽不受影响；已武装的保持——复用路径会重置满额）。
	if s.idleTTLArmer != nil {
		s.idleTTLArmer.ArmIdleTTLs(sessionID)
	}

	// 记录会话原先状态：Running 在跑；非 Running 需恢复（paused_on_child 优先恢复暂停的 domain）。
	priorStatus := session.Status
	wasRunning := priorStatus == enums.SessionStatusRunning
	// 如果不在运行，则重新置为运行状态，清除结束时间，并重建上下文。
	if !wasRunning {
		session.Status = enums.SessionStatusRunning
		session.EndedAt = nil
		restartSessionContext(session)
	}
	s.store.mu.Unlock()

	// 判定路径备注落 Prompt 事件（store.mu 已释放；addEvent 自身加锁，持锁调用死锁）。
	if enhanceNote != "" {
		s.store.addEvent(session, eventkind.Prompt, "System", "输入补全: "+enhanceNote, "", "", "", "", "", true)
	}

	// 软停止窗口内任何用户消息都视作续跑意图：清倒计时定时器 + 清标记（幂等）。
	// 不能只挂在 !wasRunning 分支--Stop 后会话可能尚 Running（MetaAgent 的 LLM
	// 调用正在中断收尾途中），消息只入队不清倒计时，销毁倒计时到期会整体销毁。
	s.cancelSoftStopState(sessionID)

	// 添加用户消息事件。
	s.store.addEvent(session, eventkind.UserMessage, "User", content, "", "", "", "", "", true)

	// 如果会话原先未运行，则在 goroutine 中恢复执行。
	if !wasRunning {
		// 轮开始落库 running 状态（历史/事件为上一轮快照，store.mu 已解锁——
		// persistFullHistory 需 RLock，持写锁调用会死锁）：进程若在本轮中途崩溃，
		// 重启恢复逻辑依据库中 running 状态把会话标记为"因服务重启中断"。
		s.store.persistHistory(session)
		s.store.persistEvents(session)
		// 热驻模式（Domain 热驻）：唤醒全部挂起 Agent--wake 广播（叶子+domain 的
		// SuspendGate 解除阻塞）+ Paused 域置回 Running（supervisor 收 opResume 续跑
		// 当前任务）+ 恢复冻结的 idle TTL。旧路径（resumePausedDomain 逐个恢复）仅作
		// 跨重启兜底（内存槽丢失时）。
		if s.sessionAgentWaker != nil {
			s.sessionAgentWaker.ResumeSessionAgents(sessionID)
		}
		// PausedOnChild: 优先恢复 earliest paused domain（任意消息，含"继续"与新任务，D1）。
		// 各 Agent 独立上下文：domain 从 agent_messages 加载 history 续跑，fresh budget。
		// 热驻模式下域已被唤醒（ResumeSessionAgents），此处仅在唤醒器未接线或槽丢失时兜底。
		// 无 paused domain 或未注入恢复器时回退普通 resumeSession（MetaAgent 续跑）。
		if priorStatus == enums.SessionStatusPausedOnChild && s.resumeDispatcher != nil && s.sessionAgentWaker == nil {
			if pausedID := s.findEarliestPausedDomain(session.ID); pausedID != "" {
				go s.resumePausedDomain(session, pausedID)
				return nil
			}
		}
		go s.resumeSession(session)
	}
	return nil
}

// resumePausedDomain 恢复一个因触达 token 上限而 Paused 的 DomainAgent。
// 委托 dispatcher.ResumePaused：从 agent_messages 加载历史，用 fresh budget 重建 domain Agent 续跑
// （各 Agent 独立上下文，不强制压缩，靠 Assemble 步频自动压缩）。
//   - 完成：dispatcher 已 tree.Finish + notify 父 + trackChildDone（父 MetaAgent 解除阻塞）；
//     此处 go resumeSession 让 MetaAgent drain mailbox 整合结果续跑。
//   - 再触限：dispatcher 已覆盖存 history + tree.Pause；此处 pauseSession(PauseOnChild) 等下次"继续"。
//   - 出错：回退 pauseSession(PauseOnChild)，不丢已持久化上下文（防跑飞）。
func (s *ReactService) resumePausedDomain(session *reactInternalSession, pausedNodeID string) {
	ctx := sessionContext(session)
	// stopCtx 注入（TODO 第10④）：恢复路径派生的子 ctx 同样以会话 stopCtx 为取消基底。
	res, err := s.resumeDispatcher.ResumePaused(
		tool.WithStopContext(tool.WithWorkDir(tool.WithSessionID(ctx, session.ID), session.workDir), session.stopCtx), pausedNodeID)
	if err != nil {
		s.store.addEvent(session, eventkind.Error, "System", fmt.Sprintf("恢复暂停领域 Agent 失败，已回退暂停态: %v", err), "", "", "", "", "", false)
		s.pauseSession(session, session.History, PauseOnChild)
		s.finalizeSession(session)
		return
	}
	if res.LimitReached {
		// 再触限：dispatcher 已 re-pause（覆盖存 history + tree.Pause）。会话置 PausedOnChild 等下次"继续"。
		s.pauseSession(session, session.History, PauseOnChild)
		s.finalizeSession(session)
		return
	}
	// 完成：MetaAgent 解除阻塞，go resumeSession 让其 drain mailbox 整合 domain 结果续跑。
	s.store.addEvent(session, eventkind.System, "System", "暂停的领域 Agent 已恢复完成，主 Agent 继续整合。", "", "", "", "", "", true)
	go s.resumeSession(session)
}

// findEarliestPausedDomain 扫描会话 Agent 树，返回最早 Started 的 Paused domain 节点 ID。
// 无则返回空串。供 sendMessage 在 PausedOnChild 态决定恢复目标（一次恢复一个，D1）。
func (s *ReactService) findEarliestPausedDomain(sessionID string) string {
	t := s.TreeFor(sessionID)
	if t == nil {
		return ""
	}
	var earliest string
	var earliestTime time.Time
	for _, n := range t.Snapshot() {
		if n.Role != "domain" || n.Status != orchestrator.StatusPaused {
			continue
		}
		if earliest == "" || n.Started.Before(earliestTime) {
			earliest = n.ID
			earliestTime = n.Started
		}
	}
	return earliest
}

// answerClarify 处理用户对澄清问题的答复。
func (s *ReactService) answerClarify(ctx context.Context, sessionID, answer string) error {
	// 空答复拒绝处理。
	if answer == "" {
		return fmt.Errorf("answer cannot be empty")
	}
	// 加锁查找会话。
	s.store.mu.Lock()
	session, ok := s.store.sessions[sessionID]
	if !ok {
		s.store.mu.Unlock()
		return ErrSessionNotFound
	}
	// 只有处于等待澄清状态的会话才能接收澄清答复（paused_on_child 走 sendMessage 恢复 domain）。
	if session.Status != enums.SessionStatusAwaitingClarify {
		s.store.mu.Unlock()
		if session.Status == enums.SessionStatusPausedOnChild {
			return fmt.Errorf("%w: session is paused on child, send a message to resume the paused domain", ErrInvalidSessionState)
		}
		return fmt.Errorf("%w: session is not awaiting clarification", ErrInvalidSessionState)
	}
	// 待答复的 Agent 提问（TODO #24 ask_user；#53 选项解析）：答复写 askUser 通道。
	// 带选项时按选项 ID/Label/数字序号解析回传，自由文本原样透传。
	if session.askUser != nil {
		text, _ := recordClarifyAnswer(session.pendingClarify, answer)
		session.askUser <- text
		session.Messages = append(session.Messages, Message{
			Role:      string(enums.ChatRoleUser),
			Content:   "[澄清答复] " + answer,
			Timestamp: time.Now(),
		})
		s.store.mu.Unlock()
		s.store.addEvent(session, eventkind.Clarify, "User", "提问答复: "+answer, "", "", "", "", "", true)
		return nil
	}
	// 待审批的破坏性操作确认（TODO #17 P1；#53 选项化）：答复路由进审批通道，不重建会话（Agent goroutine 存活）。
	if session.approval != nil {
		_, _ = recordClarifyAnswer(session.pendingClarify, answer)
		session.approval <- resolveApproval(answer, session.pendingClarify)
		session.Messages = append(session.Messages, Message{
			Role:      string(enums.ChatRoleUser),
			Content:   "[澄清答复] " + answer,
			Timestamp: time.Now(),
		})
		s.store.mu.Unlock()
		s.store.addEvent(session, eventkind.Clarify, "User", "审批答复: "+answer, "", "", "", "", "", true)
		return nil
	}
	// 将澄清答复作为用户消息追加。
	session.Messages = append(session.Messages, Message{
		Role:      string(enums.ChatRoleUser),
		Content:   "[澄清答复] " + answer,
		Timestamp: time.Now(),
	})
	// 恢复为运行状态并重建上下文。
	session.Status = enums.SessionStatusRunning
	restartSessionContext(session)
	s.store.mu.Unlock()

	// 记录澄清答复事件。
	s.store.addEvent(session, eventkind.Clarify, "User", "用户答复: "+answer, "", "", "", "", "", true)

	// 异步恢复会话执行。
	go s.resumeSession(session)
	return nil
}

// interrupt 向运行中或已暂停的会话发送抢占中断消息。
func (s *ReactService) interrupt(ctx context.Context, sessionID, content string) error {
	if content == "" {
		return fmt.Errorf("content cannot be empty")
	}
	// 加锁查找会话。
	s.store.mu.Lock()
	session, ok := s.store.sessions[sessionID]
	if !ok {
		s.store.mu.Unlock()
		return ErrSessionNotFound
	}
	// 若会话未运行，则重新激活。
	wasRunning := session.Status == enums.SessionStatusRunning
	if !wasRunning {
		session.Status = enums.SessionStatusRunning
		session.EndedAt = nil
		restartSessionContext(session)
	}
	s.store.mu.Unlock()

	// 记录中断事件。
	s.store.addEvent(session, eventkind.Interrupt, "User", "抢占中断: "+content, "", "", "", "", "", true)

	// 若原先未运行，则异步恢复执行以处理中断。
	if !wasRunning {
		go s.resumeSession(session)
	}
	return nil
}

// enqueue 向会话注入一条队列消息，通常用于后台任务继续推进。
func (s *ReactService) enqueue(ctx context.Context, sessionID, content string) error {
	if content == "" {
		return fmt.Errorf("content cannot be empty")
	}
	// 加锁查找会话。
	s.store.mu.Lock()
	session, ok := s.store.sessions[sessionID]
	if !ok {
		s.store.mu.Unlock()
		return ErrSessionNotFound
	}
	// 待答复的提问/审批挂起时（ask_user / 破坏性确认）：入队内容按澄清答复路由到
	// askUser/approval 通道。Agent goroutine 仍存活且阻塞在 hook 上，若走下方
	// "置 Running + resumeSession" 会双开 ReAct 循环、换绑 session.ctx（阻塞中 hook
	// 持有的旧 ctx 取消句柄丢失，旧 goroutine 永久滞留）且本内容不进 Messages 彻底丢失
	//（2026-09-08 web 端 ask_user 答复误入 enqueue 通道事故修复）。
	if session.askUser != nil || session.approval != nil {
		s.store.mu.Unlock()
		return s.answerClarify(ctx, sessionID, content)
	}
	// 若会话未运行，则重新激活。
	wasRunning := session.Status == enums.SessionStatusRunning
	if !wasRunning {
		session.Status = enums.SessionStatusRunning
		session.EndedAt = nil
		restartSessionContext(session)
	}
	s.store.mu.Unlock()

	// 记录队列注入事件。
	s.store.addEvent(session, eventkind.Enqueue, "User", "队列注入: "+content, "", "", "", "", "", true)

	// 若原先未运行，则异步恢复执行。
	if !wasRunning {
		go s.resumeSession(session)
	}
	return nil
}

// cancel 取消指定会话的执行。
func (s *ReactService) cancel(ctx context.Context, sessionID string) error {
	// 加锁查找会话。
	s.store.mu.Lock()
	session, ok := s.store.sessions[sessionID]
	if !ok {
		s.store.mu.Unlock()
		return ErrSessionNotFound
	}
	// 运行中或暂停待续（awaiting_clarify / paused_on_child）的会话都可取消：
	// 暂停态没有运行中的 goroutine，但必须允许用户退出暂停死锁/死等场景
	// （实证：paused_on_child 态拒绝取消，会话无任何逃生通道，永久卡死）。
	switch session.Status {
	case enums.SessionStatusRunning, enums.SessionStatusAwaitingClarify, enums.SessionStatusPausedOnChild:
	default:
		s.store.mu.Unlock()
		return fmt.Errorf("%w: session is not running", ErrInvalidSessionState)
	}
	// 取出取消函数并在解锁后调用，避免在持有锁时执行取消回调。
	cancelFn := session.cancelFn
	session.cancelFn = nil
	// 中断传播（TODO 第10④）：硬取消同步取消 stopCtx。
	stopCancel := session.stopCancel
	session.stopCancel = nil
	// 将会话标记为错误状态并记录结束时间。
	session.Status = enums.SessionStatusError
	session.Result = "cancelled by user"
	now := time.Now()
	session.EndedAt = &now
	s.store.mu.Unlock()

	// 记录取消事件。
	s.store.addEvent(session, eventkind.System, "System", "会话已被用户取消", "", "", "", "", "", true)

	// 调用取消函数通知 ReAct 循环退出。
	if cancelFn != nil {
		cancelFn()
	}
	// 中断传播（TODO 第10④）：stopCtx 取消在飞子 Agent 基底（窗口期新派发/孙代即刻终止）。
	if stopCancel != nil {
		stopCancel()
	}
	// 级联取消在跑/暂停子 Agent（TODO #25-2）：会话终止这一刻，树中节点逐个 Cancel，
	// 子 goroutine 经 context.Canceled 路径退出（runSubAgent 对该路径不重复通知）。
	s.cascadeCancelTree(sessionID)
	// worktree 残留副本清理（TODO 第9⑤）：硬取消即会话终态，未合并副本 best-effort
	// 强制移除（patch 文件保留供事后审计）。
	if wo, ok := s.stopMarker.(WorktreeOperator); ok {
		wo.CleanupSessionWorktrees(sessionID)
	}
	return nil
}

// SetSoftStopMarker 注入会话软停止标记器（TODO #37，subagent.Dispatcher 实现）。
// 传 nil 时 Stop 退化为级联取消（无暂停语义）。bootstrap 注入。
func (s *ReactService) SetSoftStopMarker(m SoftStopMarker) {
	s.stopMarker = m
}

// SetStopCountdown 配置软停止销毁倒计时（TODO #37，默认 300s；<=0 关闭）。
// bootstrap 按 config.Agent.StopDestroyCountdownSec 注入。
func (s *ReactService) SetStopCountdown(d time.Duration) {
	s.stopCountdown = d
}

// SetHotResident 标记 Domain 热驻模式开启（bootstrap 按 config 注入）。
// 开启后 Stop 语义变化：杀叶子 + domain 转 Idle 热驻 + 取消 MetaAgent API 调用 +
// 会话落可续跑暂停态；session 级 300s 销毁倒计时停用（idle 域由自身加权 TTL 治理）。
func (s *ReactService) SetHotResident(enabled bool) {
	s.hotResident = enabled
}

// Stop 软停止会话（TODO #37）：停止当前会话全部在跑子 Agent，可续跑。
//
// 与 cancel（硬销毁）的区别：不置 error、不标 Cancelled——先置软停止标记，
// 再触发 Running 节点 cancel 回调（StopRunning 不改状态），dispatcher 收尾分支
// 据此把 domain 落 Paused（存 history 可续跑）、叶子部分回灌；全部落定后
// PendingChildren>0 触发父终结保护，会话自然落入 PausedOnChild，恢复路由零改动生效。
// 之后启动销毁倒计时：到期未续跑则硬销毁（cascadeCancelTree + 会话 error）。
//
// 热驻模式（hotResident，Domain 热驻）：叶子软停部分回灌（domain 收到取消提示），
// domain supervisor 的 taskStopped 分支收尾（SaveMessages + tree.Idle + trackChildDone
// + enterIdle）；session 级倒计时停用（idle 域由加权 TTL 治理，用户下次消息才武装）。
//
// 边界：仅 Running 会话可停止；停止中发消息照常入 Messages，待会话落入
// PausedOnChild 后按既有恢复路由处理（不注入正在收尾的 MetaAgent）。
func (s *ReactService) Stop(ctx context.Context, sessionID string) error {
	s.store.mu.Lock()
	session, ok := s.store.sessions[sessionID]
	if !ok {
		s.store.mu.Unlock()
		return ErrSessionNotFound
	}
	if session.Status != enums.SessionStatusRunning {
		s.store.mu.Unlock()
		return fmt.Errorf("%w: session is not running", ErrInvalidSessionState)
	}
	// 1. 置软停止标记（dispatcher 收尾分流依据；domain supervisor 的软停分支与叶子部分回灌共用）。
	if s.stopMarker != nil {
		s.stopMarker.SetSoftStop(sessionID)
	}
	// 2. 触发全部 Running 节点 cancel 回调（不改节点状态，Pause/Idle 由 dispatcher 收尾做）。
	//    热驻模式与旧模式一致：叶子 ctx 取消走软停分支（部分回灌+取消提示邮件），
	//    domain 任务 ctx 取消走 supervisor 软停分支（SaveMessages + tree.Idle + enterIdle）。
	if t := s.TreeFor(sessionID); t != nil {
		for _, n := range t.Snapshot() {
			if n.Status == orchestrator.StatusRunning {
				t.StopRunning(n.ID)
			}
		}
	}
	// 3. 取消 MetaAgent 当前 API 调用：session ctx 取消打断流式 LLM（provider 直透），
	//    runSession 的 context.Canceled 分支落暂停态（软停止标记区分于硬取消）。
	cancelFn := session.cancelFn
	// 中断传播（TODO 第10④）：stopCtx 同步取消——stop 窗口期新派发的子 Agent 与深层
	// 孙代即刻随会话终止，不再依赖树快照逐节点 StopRunning。
	stopCancel := session.stopCancel
	// 4. 销毁倒计时：旧模式启用（到期未续跑硬销毁含 Paused 节点）；
	//    热驻模式停用——Idle 域由自身加权 TTL 治理（用户下次消息武装）。
	if s.stopCountdown > 0 && !s.hotResident {
		deadline := time.Now().Add(s.stopCountdown)
		session.destroyAt = &deadline
		session.stopTimer = time.AfterFunc(s.stopCountdown, func() {
			s.destroyAfterSoftStop(sessionID)
		})
	}
	s.store.mu.Unlock()

	// 取消 MetaAgent 当前 API 调用：session ctx 取消打断流式 LLM（provider 直透），
	// runSession/resumeSession 的 context.Canceled 分支落暂停态（软停止标记区分于
	// 硬取消）。两种模式都执行--旧模式否则 MetaAgent 卡在跑中的 LLM 调用上，
	// 会话长期 Running，倒计时到期即整体销毁。
	if cancelFn != nil {
		cancelFn()
	}
	// 中断传播（TODO 第10④）：全树在飞子 Agent（含 stop 窗口期新派发/孙代）≤瞬时取消；
	// domain 走既有软停收尾转 Idle 热驻，续跑前 restartSessionContext 重建 stopCtx。
	if stopCancel != nil {
		stopCancel()
	}

	if s.hotResident {
		s.store.addEvent(session, eventkind.System, "System",
			"软停止: 叶子任务已停止，领域 Agent 转入热驻（下次消息后进入存活倒计时），发送消息可续跑", "", "", "", "", "", true)
	} else {
		s.store.addEvent(session, eventkind.System, "System",
			fmt.Sprintf("软停止: 已停止全部子任务（%s 后未续跑将销毁）", s.stopCountdown), "", "", "", "", "", true)
	}
	log.Printf("[service] SOFT-STOP: session=%s countdown=%s hotResident=%t", sessionID, s.stopCountdown, s.hotResident)
	return nil
}

// destroyAfterSoftStop 软停止倒计时到期：硬销毁会话（cascadeCancelTree 含 Paused 节点），
// 清停止标记与倒计时状态。幂等：会话已恢复/已终止时 no-op。
// 触发条件：仍在软停止窗口内（destroyAt 非 nil）且会话处于 PausedOnChild（正常落定）
// 或 Running（级联仍在进行）——两种状态再加上 AwaitingClarify（软停取消落 PauseUserStop）都属于"停止后未续跑"，到期即销毁。
func (s *ReactService) destroyAfterSoftStop(sessionID string) {
	s.store.mu.Lock()
	session, ok := s.store.sessions[sessionID]
	if !ok || session.destroyAt == nil {
		s.store.mu.Unlock()
		return
	}
	switch session.Status {
	case enums.SessionStatusPausedOnChild, enums.SessionStatusRunning, enums.SessionStatusAwaitingClarify:
	default:
		s.store.mu.Unlock()
		return
	}
	if session.stopTimer != nil {
		session.stopTimer.Stop()
		session.stopTimer = nil
	}
	session.destroyAt = nil
	cancelFn := session.cancelFn
	session.cancelFn = nil
	stopCancel := session.stopCancel
	session.stopCancel = nil
	s.store.mu.Unlock()

	if s.stopMarker != nil {
		s.stopMarker.ClearSoftStop(sessionID)
	}
	if cancelFn != nil {
		cancelFn()
	}
	// 中断传播（TODO 第10④）：硬销毁同步取消 stopCtx（与 cancel 同语义）。
	if stopCancel != nil {
		stopCancel()
	}
	s.cascadeCancelTree(sessionID)
	// worktree 残留副本清理（TODO 第9⑤）：软停超时硬销毁等同会话终态。
	if wo, ok := s.stopMarker.(WorktreeOperator); ok {
		wo.CleanupSessionWorktrees(sessionID)
	}

	s.store.mu.Lock()
	if session.Status == enums.SessionStatusPausedOnChild || session.Status == enums.SessionStatusRunning || session.Status == enums.SessionStatusAwaitingClarify {
		session.Status = enums.SessionStatusError
		session.Result = "软停止超时未续跑，任务已销毁"
		now := time.Now()
		session.EndedAt = &now
	}
	s.store.mu.Unlock()
	s.store.addEvent(session, eventkind.System, "System", "软停止倒计时到期，任务已销毁（如需续跑请重开会话）", "", "", "", "", "", true)
}

// cancelSoftStopState 续跑触发时清除软停止状态：停倒计时定时器 + 清标记。
// sendMessage 恢复路径（PausedOnChild / 普通 resume）调用，幂等。
func (s *ReactService) cancelSoftStopState(sessionID string) {
	s.store.mu.Lock()
	session, ok := s.store.sessions[sessionID]
	if ok && session.stopTimer != nil {
		session.stopTimer.Stop()
		session.stopTimer = nil
		session.destroyAt = nil
	}
	s.store.mu.Unlock()
	if s.stopMarker != nil {
		s.stopMarker.ClearSoftStop(sessionID)
	}
}

// toReactAgentSession 将内部 reactInternalSession 转换为公共 Session DTO。
func toReactAgentSession(s *reactInternalSession) *Session {
	// 空指针安全处理。
	if s == nil {
		return nil
	}

	// 处理可能为空的结束时间，DTO 使用值类型。
	var endedAt time.Time
	if s.EndedAt != nil {
		endedAt = *s.EndedAt
	}

	// 转换内部事件列表为公共事件列表。
	events := make([]Event, 0, len(s.Events))
	for i := range s.Events {
		events = append(events, *toAgentEvent(&s.Events[i]))
	}

	// 拷贝消息列表，避免外部修改内部状态。
	messages := make([]Message, len(s.Messages))
	copy(messages, s.Messages)

	// 组装并返回公共 Session。
	return &Session{
		ID:             s.ID,
		Goal:           s.Goal,
		Status:         string(s.Status),
		Result:         s.Result,
		State:          "active",
		StartedAt:      s.StartedAt,
		EndedAt:        endedAt,
		Events:         events,
		Messages:       messages,
		TempDir:        s.TempDir,
		WorkDir:        s.workDir,
		StreamingText:  s.StreamingText,
		ThinkingText:   s.ThinkingText,
		ActiveBlocks:   []ActiveBlock{},
		PendingClarify: s.pendingClarify,
		DestroyAt:      s.destroyAt,
		ActiveTopicID:  s.activeTopicID,
		TrustMode:      s.currentTrustMode(),
	}
}

// newReactServiceForTest 构造一个用于包级测试的 ReactService，使用 mock provider。
func newReactServiceForTest(provider ModelProvider, workDir string) *ReactService {
	// 使用空的角色配置创建角色注册表。
	cfg := &pkgconfig.RoleConfigFile{}
	roleRegistry := role.NewRegistry(cfg)
	// 创建仅含内建工具的注册表，不带外部回调。
	toolRegistry := tool.NewBuiltinRegistry(workDir, nil, nil)
	// 创建 ReactService，模型工厂与邮箱等依赖为空。
	s := NewReactService(roleRegistry, nil, toolRegistry, nil, NopMemoryPipeline{}, nil)
	// 注入 mock provider。
	s.testProvider = provider
	// 如果提供了工作目录，则覆盖默认存储工作目录。
	if workDir != "" {
		s.store.workDir = workDir
	}
	return s
}
