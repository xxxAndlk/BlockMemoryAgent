// Package agent 提供基于 ReAct 引擎的 Agent 服务实现，
// 负责会话生命周期管理与外部接口适配。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/blockmemory/agent/backend/internal/board" // board 提供任务看板快照（TODO #22）
	"github.com/blockmemory/agent/backend/internal/domain/decision"
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/domain/role"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/logger"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/project"
	"github.com/blockmemory/agent/backend/internal/server/eventkind"
	"github.com/blockmemory/agent/backend/internal/skill"
	"github.com/blockmemory/agent/backend/internal/store"
	"github.com/blockmemory/agent/backend/internal/userprofile"
	pkgconfig "github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/textutil"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// AcceptanceRunner 交付验收闭环接口（测试助手大改，2026-09-12）：
// 由 subagent.AcceptanceManager 实现，bootstrap 装配注入。
// RunWrap 在终答提交前执行验收循环（内含三态开关判定），返回最终交付文本
// （原文 / 原文+验收通过附报告路径 / 原文+【未验证项】段+报告路径 / 原文+未验收警告）。
// agent 包不能 import subagent（subagent 已 import agent），故接口定义在本侧。
type AcceptanceRunner interface {
	RunWrap(ctx context.Context, sessionID, workDir, goal, finalAnswer string) string
}

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

	// notifier 会话终态 Webhook 通知器（TODO #18-5 T32）：finalizeSession 收尾时
	// best-effort 通知外部渠道。为 nil 时零开销跳过；bootstrap 按 config.notify 构造注入。
	notifier *Notifier

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
	// 消息到达时调用 ArmIdleTTLs（倒计时自任务完成 enterIdle 即已武装，此调用幂等兼容）。
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
	// boardRemoveFn 移除会话任务看板（DeleteSession 硬删除路径）；nil 表示未接线。
	// 由 bootstrap 注入 board.Manager.Remove。
	boardRemoveFn func(sessionID string)

	// ledgerFn 按 sessionID 渲染【任务台账】块文本（2026-08-28 旧需求重派事故根治）；
	// 空串=无台账不注入，nil 表示未接线。由 bootstrap 注入 Dispatcher.TaskLedgerBrief。
	ledgerFn func(sessionID string) string

	// memoryIndexFn 渲染【沉淀索引】块（TODO #20③+#22③ 记忆索引槽）：会话启动注入，
	// 行数/runes 双配额（超限自带重写指令），nil=未接线不注入。
	memoryIndexFn func() string

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
	// decisionLayer 决策层（TODO #23）：①任务级意图分诊 + ⑥档位建议（只读影子）两个
	// 消费切入点。nil=关闭（默认，测试场景零行为变化）；影子先行，对拍达标才逐点 enforce。
	decisionLayer *decision.Layer
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
	// topLevelTools 顶层必备插件工具提供者（bootstrap 注入 plugins.Manager.TopLevelTools）：
	// 快速/日常档会话启动时把返回的工具名预挂到顶层 Agent scope（=sessionID），
	// 使顶层对接用户即具备联网搜索等能力；子 Agent scope 不预挂。nil 时零行为变化。
	topLevelTools func() []string
	// activityPinger 等待用户答复期间的心跳保活回调（bootstrap 注入
	// subagent.Dispatcher.PingActivity）：审批/提问阻塞时周期性刷新子 Agent 活动时间，
	// 防止心跳巡检把"等用户操作"误判假死 kill（实证 2026-08-18：三次误杀均卡在
	// Remove-Item 确认框无人答复，每次白耗 ~12 分钟 + 重派）。nil 时不保活。
	activityPinger func(agentID string)
	// skillPool 全局技能池（技能渐进披露）：nil 时不注入 MetaAgent 技能目录块。
	// bootstrap 经 SetSkillCatalog 注入与 Dispatcher 同一个池。
	skillPool *skill.Pool
	// skillCatalog 经验技能（learned）目录收敛回调：返回 enabled 技能按价值排序的
	// top-N 元数据与启用总数，供 meta 系统提示只列常用技能（其余经 list_skills 检索）。
	// 由 bootstrap 接 learned_skills 的 TopEnabled 查询；nil 时 meta 提示不列经验技能。
	skillCatalog func() ([]SkillRecallHint, int)

	// agentMsgCache/msgLogger 编排页对话视图：Redis 热层读缓存 + meta 消息热写器；
	// 均由 SetAgentMsgCache 一并装配，nil 时读侧回退 PG、写侧跳过。
	agentMsgCache *store.AgentMsgRedisStore
	msgLogger     MessageLogger
	// messenger 用户直连写通道（编排页对话面板），由 subagent.Dispatcher 实现。
	// nil（未接线/测试）时 MessageAgent 一律拒绝。
	messenger AgentMessenger

	// acceptance 交付验收闭环（测试助手大改）：runSession/resumeSession 终答提交前
	// 经 RunWrap 触发 test_assistant 验收循环，返回最终交付文本。
	// 由 bootstrap 注入 subagent.AcceptanceManager；nil 时原样交付（零行为变化）。
	acceptance AcceptanceRunner
}

// SetSkillCatalog 注入全局技能池：MetaAgent 会话系统提示追加【可用技能】目录块
// （Meta 持全集、可派发任意技能给下级）；nil 关闭（测试/未配置场景）。
func (s *ReactService) SetSkillCatalog(p *skill.Pool) {
	s.skillPool = p
}

// SetSkillCatalogSource 注入经验技能目录收敛回调（A 目录治理）：fn 返回
// enabled 经验技能按 use_count 排序的 top-N 与启用总数；nil 关闭（meta 提示
// 不列经验技能）。bootstrap 接 learned_skills.TopEnabled。
func (s *ReactService) SetSkillCatalogSource(fn func() ([]SkillRecallHint, int)) {
	s.skillCatalog = fn
}

// SetAgentMsgCache 注入 Agent 消息 Redis 热层（编排页对话视图）：读侧缓存 + meta 热写器。
// nil 安全：读回退 PG、写关闭。
func (s *ReactService) SetAgentMsgCache(c *store.AgentMsgRedisStore) {
	s.agentMsgCache = c
	s.msgLogger = NewMessageLogger(c)
}

// metaSkillBlock 渲染 MetaAgent 的技能目录块：静态技能（yaml/builtin/目录/插件包）
// 全列，经验技能（learned）只列常用 top-N + 检索提示——自进化技能库随会话沉淀
// 无限增长（每次会话最多 +2 且无淘汰），全量入提示会持续膨胀并干扰模型选择
//（治理三件套：A 目录收敛 / B 写入门 / C 库存整理）。
// skillPool 未注入或池为空时返回空串（零注入）。
func (s *ReactService) metaSkillBlock() string {
	if s.skillPool == nil {
		return ""
	}
	base := skill.MetadataBlock(s.skillPool, s.skillPool.NamesExceptSource("learned"))
	learned := s.learnedSkillCatalogBlock()
	switch {
	case learned == "":
		return base
	case base == "":
		return learned + "\n用 load_skill(名称) 获取技能全文；派发子 Agent 时可用 skills 参数下放其中技能。"
	default:
		return learned + "\n" + base
	}
}

// learnedSkillCatalogBlock 收敛后的经验技能目录（A）：列出价值 top-N，超出部分
// 只提示总数与检索方式（list_skills 的 query 参数），不再全量进系统提示。
// 回调未接线或无非禁用技能时返回空串。
func (s *ReactService) learnedSkillCatalogBlock() string {
	if s.skillCatalog == nil {
		return ""
	}
	top, total := s.skillCatalog()
	if total <= 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("【经验技能】\n")
	for _, h := range top {
		b.WriteString("- ")
		b.WriteString(h.Name)
		b.WriteString(": ")
		b.WriteString(h.Title)
		b.WriteByte('\n')
	}
	if total > len(top) {
		fmt.Fprintf(&b, "经验技能共 %d 个，此处按使用频次列前 %d 个；检索其余用 list_skills(query=关键词)。\n", total, len(top))
	}
	return strings.TrimRight(b.String(), "\n")
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

// SetTopLevelToolsProvider 注入顶层必备插件工具提供者（bootstrap 注入
// plugins.Manager.TopLevelTools）；传 nil 关闭预挂载。
func (s *ReactService) SetTopLevelToolsProvider(fn func() []string) {
	s.topLevelTools = fn
}

// mountTopLevelEssentials 把配置声明的顶层必备插件工具（web_search 等）预挂到
// 会话顶层 scope：三档全挂（2026-09-18 修复：原实现排除 cluster 档，前提是
// "cluster 档 MetaAgent 已有角色授权，走 tool_catalog+tool_mount"——T13 收窄把这两个
// 工具从 meta 白名单删了，前提失效，实测集群档 meta 无 web_search 可用、联网调研
// 任务结构性卡死。统一预挂后三档顶层"问一句搜一下"同权）。子 Agent 的独立 scope
// 不受影响。幂等；插件未运行/未接线时零行为变化。
func (s *ReactService) mountTopLevelEssentials(sessionID string, gear string) {
	if s.toolRegistry == nil || s.topLevelTools == nil {
		return
	}
	names := s.topLevelTools()
	if len(names) == 0 {
		return
	}
	if accepted := s.toolRegistry.MountPreApprovedForScope(sessionID, gear, names); len(accepted) > 0 {
		log.Printf("[agent] top-level essentials mounted: session=%s gear=%s tools=%v", sessionID, gear, accepted)
	}
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
// （TODO #28 双路写入之 b + 2026-09-02 期 1 Merge 升级）。
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

// SetMemoryIndexProvider 注入【沉淀索引】渲染器（TODO #20③+#22③）：
// fn() 返回已按配额渲染的索引块（renderMemoryIndex 产物，超限自带重写指令），
// 空串=无沉淀不注入。传 nil 关闭索引槽。由 bootstrap 接 KnowledgeStore 组装。
func (s *ReactService) SetMemoryIndexProvider(fn func() string) {
	s.memoryIndexFn = fn
}

// memoryIndexBlock 取当前索引块（未接线返回空串）。
func (s *ReactService) memoryIndexBlock() string {
	if s.memoryIndexFn == nil {
		return ""
	}
	return s.memoryIndexFn()
}

// SetBoardRemover 注入会话任务看板移除器（DeleteSession 硬删除路径）。
// 由 bootstrap 注入 board.Manager.Remove；传 nil 时删除会话不清看板（测试场景）。
func (s *ReactService) SetBoardRemover(fn func(sessionID string)) {
	s.boardRemoveFn = fn
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

// SetDecisionLayer 注入决策层（TODO #23，消费侧照 WithFactExtractor 模式）。
// 两个切入点：① sendMessageFull 任务级意图分诊（只做建议与澄清触发，不自动改档）、
// ⑥ gear 档位建议（只读影子，永不做自动选档）。传 nil 关闭（默认）。
func (s *ReactService) SetDecisionLayer(l *decision.Layer) {
	s.decisionLayer = l
}

// decideTaskTriage ①任务级意图分诊（TODO #23 切入点1）：Choice(任务性质)+
// Choice(所需工具面)+Noul(需澄清?)。低置信→建议先澄清而非硬猜；只做建议与
// 澄清触发（返回建议前缀文本），不自动改档。影子期返回空串（GoObserve 异步落
// 对拍行，零延迟税）；enforce 点同步取建议。失败 fail-open 返回空串。
func (s *ReactService) decideTaskTriage(ctx context.Context, sessionID, content string) string {
	if s.decisionLayer == nil {
		return ""
	}
	qs := decision.TaskTriageQuestions(content)
	req := decision.Request{State: "任务级意图分诊", Questions: qs}
	anyEnforce := s.decisionLayer.EnforceMode(decision.PointIntentKind) ||
		s.decisionLayer.EnforceMode(decision.PointToolFace) ||
		s.decisionLayer.EnforceMode(decision.PointNeedClarify)
	if !anyEnforce {
		// 影子期异步观测：MetaAgent 实际路由（速答/单/多域）离线经 sub_agent_dispatch
		// 事件关联补对拍（actual 留空 → pending）。
		s.decisionLayer.GoObserve(ctx, sessionID, req, nil)
		return ""
	}
	resp, err := s.decisionLayer.Decide(ctx, req)
	if err != nil {
		return ""
	}
	for _, ans := range resp.Answers {
		s.decisionLayer.Observe(ctx, sessionID, "", ans)
	}
	nature, ok1 := resp.AnswerOf("task_nature")
	face, ok2 := resp.AnswerOf("tool_face")
	clar, ok3 := resp.AnswerOf("need_clarify")
	// 低置信→触发澄清而非硬猜：need_clarify 判 yes 或整批置信低于门槛时，
	// 建议先 ask_user 澄清；其余情况给分诊建议。永不改档。
	if ok3 && s.decisionLayer.ShouldEnforce(decision.PointNeedClarify, clar.Confidence) && clar.Value == "yes" {
		return fmt.Sprintf("【决策层分诊】任务意图存在关键缺口（置信 %.2f），建议先调用 ask_user 澄清目标/范围/验收标准再开工，勿硬猜", clar.Confidence)
	}
	if ok1 && s.decisionLayer.ShouldEnforce(decision.PointIntentKind, nature.Confidence) {
		advice := "【决策层分诊】任务性质=" + nature.Value
		if ok2 && s.decisionLayer.ShouldEnforce(decision.PointToolFace, face.Confidence) {
			advice += "，所需工具面=" + face.Value
		}
		return fmt.Sprintf("%s（置信 %.2f）", advice, nature.Confidence)
	}
	return ""
}

// gearHintShadow ⑥档位建议（只读影子，TODO #23 切入点6）：与 T18 隐性信号并行给
// 档位建议落影子对拍（suggestion vs 当前实际档位）。**永不做自动选档**——auto 档
// 2026-09-16 已退役，本切入点不破该决策；escalate_gear 用户确认卡流程不动。
func (s *ReactService) gearHintShadow(ctx context.Context, sessionID, content, currentGear string) {
	if s.decisionLayer == nil {
		return
	}
	req := decision.Request{State: "档位建议（只读）", Questions: []decision.Question{decision.GearHintQuestion(content)}}
	// 对拍值=会话当前实际档位（同步已知，match/mismatch 即时可算）。
	s.decisionLayer.GoObserve(ctx, sessionID, req, func(decision.Answer) string { return currentGear })
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
// （2026-09-02 设计 §5：项目偏好 Meta+Domain 双注入）。
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
	MaxIterations             int // ReAct 最大 LLM 轮数；<0 表示不限制
	LLMTimeoutSec             int // 单次 LLM 调用超时（秒）；<0 表示仅受会话取消控制
	RetryCount                int // LLM 失败重试次数（不含首次）
	RetryBackoffMs            int // 重试初始退避（毫秒）
	HistoryMaxMessages        int // 单次请求最大历史消息数；<0 表示不裁剪
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

// AgentMessenger 是用户直连子 Agent 的写通道（编排页对话面板发送框），
// 由 subagent.Dispatcher 实现（见 Task 6 内核）：
//   - InjectUserMessage：向等子返回中的 Agent 邮箱投 From=user 消息并唤醒其 wait loop；
//   - ReviveWithMessage：终态节点 Reopen 后同 ID 重跑（种子=原任务+上轮结果+用户消息）；
//   - WakeIdleWithMessage：热驻 idle 节点唤醒续聊（任务=用户消息，完成回 idle 后
//     按新权重重新武装 TTL——对话刷新墙钟）。
type AgentMessenger interface {
	InjectUserMessage(agentID, content string) error
	ReviveWithMessage(ctx context.Context, node orchestrator.Node, userMsg string) error
	WakeIdleWithMessage(agentID, content string) error
}

// SetAgentMessenger 注入用户直连写通道（编排页）。nil 时 MessageAgent 一律拒绝
// （未接线场景：端点存在但不可用，不静默丢消息）。
func (s *ReactService) SetAgentMessenger(m AgentMessenger) {
	s.messenger = m
}

// SetAcceptanceRunner 注入交付验收闭环（测试助手大改，subagent.AcceptanceManager 实现）。
// nil（默认）时终答原样交付，零行为变化。
func (s *ReactService) SetAcceptanceRunner(r AcceptanceRunner) {
	s.acceptance = r
}

// SetNotifier 注入会话终态 Webhook 通知器（TODO #18-5 T32）。
// nil（默认，config 未配 webhook_url）时零开销跳过。
func (s *ReactService) SetNotifier(n *Notifier) {
	s.notifier = n
}

// NotifyUserSystemMessage 按会话 ID 向用户对话页发一条系统消息
// （验收闭环进度通告：bootstrap 经 Dispatcher.WithUserNotify 接线到这里）。
// 会话不在内存（已淘汰/未恢复）时静默跳过。
func (s *ReactService) NotifyUserSystemMessage(sessionID, msg string) {
	sess := s.store.getSession(sessionID)
	if sess == nil {
		return
	}
	s.store.addEvent(sess, eventkind.System, "System", msg, "", "", "", "", "", true)
}

// injectUserMessageToRunningSession 把用户新指令投给**运行中**会话的 MetaAgent 邮箱。
//
// 为什么需要（2026-09-12 用户实证）：任务全部派发出去、正在等子 Agent 时用户重新输入，
// 此前只写进 session.Messages——而运行中的 ReAct 主循环根本不读该字段（它只在
// resumeSession 建新轮时用它作输入），消息等于静默丢失，用户看着"发送成功"但 Agent
// 毫无反应。改投邮箱后，主循环在本轮内的下一个检查点读到它：waitForChildren 每周期
// drain（等子返回时立即生效）、主循环顶部 drain（其他阶段在下一步生效），经
// mailboxMessageToReact 转成 user 消息进入历史，MetaAgent 即按新指令重新规划。
// 邮箱未接线（测试/精简装配）时返回错误，调用方降级为"仅记录"，不影响主流程。
func (s *ReactService) injectUserMessageToRunningSession(session *reactInternalSession, content string) error {
	if s.messenger == nil {
		return fmt.Errorf("用户注入通道未接线")
	}
	// MetaAgent 的邮箱名 = 会话 ID（runSession 里 NewReActAgent(session.ID, ...)），
	// InjectUserMessage 内部 Send + poke：不 poke 要等满一次等待周期（30s）才可见。
	return s.messenger.InjectUserMessage(session.ID, content)
}

// MessageAgent 用户直连子 Agent（编排页对话面板发送框）。状态机路由：
// 等子返回（running + activity_kind=child_wait）→ 邮箱注入+唤醒；
// 执行中（running 其他）→ 邮箱排队（P0-2 steering，TODO #14 T8：不再 409 拒绝，
// 下一个检查点即达；ErrAgentBusy 保留给注入失败路径）；
// 终态（done/failed/cancelled/delivered-unverified）→ 复活重跑；
// Idle（热驻待复用）→ 唤醒续聊，完成回 idle 后 TTL 重新起计；
// Paused/meta → 拒绝（Paused 走监控页恢复；meta 走主对话页）。
// 返回 queued=true 表示消息已入邮箱排队（running 态）；false = 即时投递/复活。
// 成功写一条 System 会话事件留痕，主对话流可见"用户直连了某 Agent"。
func (s *ReactService) MessageAgent(ctx context.Context, sessionID, instID, content string) (bool, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return false, fmt.Errorf("%w: 消息内容不能为空", ErrInvalidSessionState)
	}
	// 主 Agent 走主对话通道，不经此端点（设计 §4 明确 meta 不开放直连）。
	if instID == "" || instID == "meta" {
		return false, fmt.Errorf("%w: 主 Agent 请用主对话页", ErrAgentNotDirectable)
	}
	if s.messenger == nil {
		return false, fmt.Errorf("%w: 用户直连通道未接线", ErrAgentNotDirectable)
	}
	s.store.mu.RLock()
	sess := s.store.sessions[sessionID]
	s.store.mu.RUnlock()
	if sess == nil {
		return false, ErrSessionNotFound
	}
	node, ok := s.TreeFor(sessionID).Get(instID)
	if !ok {
		return false, ErrAgentNotFound
	}
	queued := false
	switch node.Status {
	case orchestrator.StatusRunning:
		// P0-2 steering 排队（TODO #14 T8）：running 非 child_wait 不再 409 拒绝——
		// 与 child_wait 同通道投邮箱排队，主循环在下一个检查点读到（generate 前补一次
		// drain 压缩插入延迟上界）。邮箱注入失败（未接线等）才回 ErrAgentBusy（409）。
		if err := s.messenger.InjectUserMessage(instID, content); err != nil {
			return false, fmt.Errorf("%w: 邮箱投递失败（%v）", ErrAgentBusy, err)
		}
		queued = true
		// 竞态兜底：投递后复查节点状态——窗口内恰好落终态时邮箱消息可能永不被消费，
		// 改走复活重跑通道（种子=原任务+上轮结果+用户消息）。
		if n, ok2 := s.TreeFor(sessionID).Get(instID); ok2 && isTerminalNodeStatus(n.Status) {
			if err := s.messenger.ReviveWithMessage(tool.WithSessionID(ctx, sessionID), n, content); err != nil {
				return false, err
			}
		}
	case orchestrator.StatusDone, orchestrator.StatusFailed, orchestrator.StatusCancelled, orchestrator.StatusUnverified:
		// 直连 ctx 需补会话标识：HTTP 请求 ctx 不携带 sessionID，而复活路径靠它
		// treeFn(sessionID) 取权威树、runSubAgent 靠它落终态/台账——缺了会取到空树，
		// 每次复活都以"节点非终态，不可复活"失败（并把空树缓存进 s.trees）。
		if err := s.messenger.ReviveWithMessage(tool.WithSessionID(ctx, sessionID), node, content); err != nil {
			return false, err
		}
	case orchestrator.StatusIdle:
		// 热驻待复用：唤醒续聊（任务=用户消息），完成回 idle 后 TTL 按新权重重新起计。
		if err := s.messenger.WakeIdleWithMessage(instID, content); err != nil {
			return false, err
		}
	default: // Paused / 未知
		return false, fmt.Errorf("%w（Paused 请到监控页恢复）", ErrAgentNotDirectable)
	}
	s.store.addEvent(sess, eventkind.System, "System", "用户直连 "+instID+"："+truncateRunes(content, 200), "", "", "", "", "", true)
	return queued, nil
}

// isTerminalNodeStatus 树节点终态判定（MessageAgent 复活路由同口径）。
func isTerminalNodeStatus(st orchestrator.Status) bool {
	switch st {
	case orchestrator.StatusDone, orchestrator.StatusFailed, orchestrator.StatusCancelled, orchestrator.StatusUnverified:
		return true
	}
	return false
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
// （未配时 150000）。meta 不给 0（无限）:config tool_call_max_rounds=-1 已使 maxIter 无界，
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
	// 显式初始档位（TODO #14 新会话页选档）：合法枚举覆盖 store 默认档，其余回落默认。
	if tool.ValidGear(req.Gear) {
		sess.setGear(req.Gear)
	}
	// 会话级思考强度（2026-09-16 新会话页选择）：合法枚举入会话，空=跟随角色默认。
	if t := tool.NormalizeThinking(req.Thinking); tool.ValidThinking(t) {
		sess.setThinking(t)
	}
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
		case enums.SessionStatusRunning, enums.SessionStatusAwaitingClarify, enums.SessionStatusPausedOnChild, enums.SessionStatusAwaitingChild:
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
	case QueryKindAgentMessages:
		// 编排页 Agent 对话视图：完整消息历史 + mailbox 留痕。
		agentID, _ := q.Args["agent"].(string)
		beforeSeq, _ := q.Args["before_seq"].(int)
		afterSeq, _ := q.Args["after_seq"].(int)
		limit, _ := q.Args["limit"].(int)
		return s.agentMessagesQueryResult(ctx, sessionID, agentID, beforeSeq, afterSeq, limit), nil
	case QueryKindMailboxTrace:
		// 会话级邮件留痕：主对话栏展示"上级 ↔ 下级"全部往来（父侧视角此前看不到邮件）。
		limit, _ := q.Args["limit"].(int)
		return s.mailboxTraceQueryResult(ctx, sessionID, limit), nil
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
		// 澄清答复：提取 answer（单题）与 answers（批量逐题，任务 140）并答复。
		// answers 兼容两种形态：HTTP 层 gin 解码后直接传入 []string；经 JSON
		// 序列化的通道（cmdqueue 等）还原为 []any。只断言 []any 会把 HTTP 批量
		// 答复静默丢成空切片，answerClarify 误报 "answer cannot be empty"（500）。
		answer, _ := cmd.Args["answer"].(string)
		var answers []string
		switch raw := cmd.Args["answers"].(type) {
		case []string:
			answers = raw
		case []any:
			for _, a := range raw {
				if s, ok := a.(string); ok {
					answers = append(answers, s)
				}
			}
		}
		return s.answerClarify(ctx, sessionID, answer, answers)
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
	case ControlOpGear:
		// 执行档位切换（TODO #14 三档全手动）：提取 gear 校验枚举后落会话，下一轮生效。
		gear, _ := cmd.Args["gear"].(string)
		return s.SetSessionGear(sessionID, gear)
	case ControlOpThinking:
		// 会话级思考强度切换（2026-09-16）：提取 thinking（空串合法=跟随角色默认）落会话，
		// providerForRole 每次 LLM 调用实时读取，下一次调用即生效。
		thinking, _ := cmd.Args["thinking"].(string)
		return s.SetSessionThinking(sessionID, thinking)
	case ControlOpWorkDir:
		// 每会话工作目录修改：落库即时保存，下一回合生效（目录在每回合开始时读取）。
		// 参数断言必须 fail-closed：空串是**合法**的"清除为默认目录"载荷，若把
		// 缺参数/类型不符也降级成空串，一次序列化误差就会静默清掉用户的目录。
		dir, ok := cmd.Args["work_dir"].(string)
		if !ok {
			return fmt.Errorf("%w: work_dir 参数缺失或类型不符", ErrInvalidSessionState)
		}
		return s.SetSessionWorkDir(ctx, sessionID, dir)
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
		report := clarifyReportJSON(strings.TrimSpace(sess.StreamingText))
		s.store.mu.Unlock()

		s.store.addEventDetail(sess, eventkind.Clarify, "System", question, "", "", "", "", "", true, report)

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
			// 同 AskUserHook：恢复即清上一轮流式缓冲，防 live 帧把旧答复文本
			// 当新轮实时流重复渲染。直写字段（store.mu 不可重入）。
			sess.StreamingText = ""
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

// SetDefaultGear 设置新建会话的初始执行档位（TODO #14 会话三档控制）：config
// agent.default_gear 经 bootstrap 注入。非法值返回错误（启动期 fail-fast），
// 空串清空（会话 gear 不设置，runSession 按集群档兜底）。
func (s *ReactService) SetDefaultGear(gear string) error {
	if gear == "" {
		s.store.mu.Lock()
		s.store.defaultGear = ""
		s.store.mu.Unlock()
		return nil
	}
	if !tool.ValidGear(gear) {
		return fmt.Errorf("invalid gear %q (want fast|daily|cluster)", gear)
	}
	s.store.mu.Lock()
	s.store.defaultGear = gear
	s.store.mu.Unlock()
	return nil
}

// SetSessionGear 切换既有会话的执行档位（TODO #14，HTTP/TUI 切换通道）：
// atomic 即时生效——正在运行的 ReAct 循环下一轮按新档裁决（在飞子 Agent 不强杀）。
// 手动切换允许任意向（三档是实体控制，自动升档只升不降的约束只在 escalate 发起侧）。
// 非法值返回错误（HTTP 400）；会话不存在返回错误。
func (s *ReactService) SetSessionGear(sessionID, gear string) error {
	if !tool.ValidGear(gear) {
		return fmt.Errorf("invalid gear %q (want fast|daily|cluster)", gear)
	}
	s.store.mu.RLock()
	sess := s.store.sessions[sessionID]
	s.store.mu.RUnlock()
	if sess == nil {
		return fmt.Errorf("session %s not found", sessionID)
	}
	// D-2 留痕（TODO #14 T7）：档位切换全量落事件（含方向与来源），会话事件流可追溯。
	from := sess.currentGear()
	sess.setGear(gear)
	s.store.addEvent(sess, eventkind.System, "System",
		fmt.Sprintf("档位切换: %s → %s（手动）", gearDisplayName(from), gear),
		"", "", "", "", "", true)
	return nil
}

// SessionGear 读取会话当前执行档位（HTTP GET 展示用）；未设置返回空串。
func (s *ReactService) SessionGear(sessionID string) string {
	s.store.mu.RLock()
	sess := s.store.sessions[sessionID]
	s.store.mu.RUnlock()
	if sess == nil {
		return ""
	}
	return sess.currentGear()
}

// SetSessionThinking 切换会话级思考强度（2026-09-16，HTTP 切换通道）：
// atomic 即时生效——providerForRole 闭包每次 LLM 调用实时读取，下一次调用即用新档
//（免重启）；只覆盖本会话顶层 Agent，在飞子 Agent 各按角色解析不受影响。
// 空串 = 跟随角色默认（清除会话覆盖）。非法值返回错误（HTTP 400）；会话不存在返回错误。
func (s *ReactService) SetSessionThinking(sessionID, thinking string) error {
	t := tool.NormalizeThinking(thinking)
	if !tool.ValidThinking(t) {
		return fmt.Errorf("invalid thinking %q (want off|low|medium|high or empty)", thinking)
	}
	s.store.mu.RLock()
	sess := s.store.sessions[sessionID]
	s.store.mu.RUnlock()
	if sess == nil {
		return fmt.Errorf("session %s not found", sessionID)
	}
	from := sess.currentThinking()
	sess.setThinking(t)
	s.store.addEvent(sess, eventkind.System, "System",
		fmt.Sprintf("思考强度切换: %s → %s（手动）", thinkingDisplayName(from), thinkingDisplayName(t)),
		"", "", "", "", "", true)
	return nil
}

// SessionThinking 读取会话当前思考强度（HTTP GET 展示用）；未设置（跟随角色默认）返回空串。
func (s *ReactService) SessionThinking(sessionID string) string {
	s.store.mu.RLock()
	sess := s.store.sessions[sessionID]
	s.store.mu.RUnlock()
	if sess == nil {
		return ""
	}
	return sess.currentThinking()
}

// thinkingDisplayName 思考强度展示名（事件文案用）：空串显示为"跟随角色默认"。
func thinkingDisplayName(t string) string {
	if t == "" {
		return "跟随角色默认"
	}
	return t
}

// SetSessionWorkDir 修改会话的每会话工作目录（会话页"本会话目录"入口，HTTP/TUI 共用）。
//
// 语义：
//   - **落库即时保存**：驻留会话写内存（atomic）+ 更新 session_history.work_dir；非驻留会话
//     （懒恢复、只在 PG 里）直接更新库——下次恢复经 rec.WorkDir 读到新值；
//   - **空串 = 回落进程默认目录**（与创建路径同语义，即"清除本会话目录"）；
//   - **下一回合生效**：workDir 在每回合开始时读取（runSession/resumeSession），正在执行的
//     工具调用已按旧目录解析，已产出的文件不迁移；
//   - 会话不存在 → ErrSessionNotFound（HTTP 404）。
func (s *ReactService) SetSessionWorkDir(ctx context.Context, sessionID, dir string) error {
	s.store.mu.RLock()
	sess := s.store.sessions[sessionID]
	s.store.mu.RUnlock()
	if sess == nil {
		// 非驻留会话：内存无副本可改，直接落库（恢复路径会读回新值）。
		if s.store.pgStore == nil {
			return ErrSessionNotFound
		}
		ok, err := s.store.pgStore.UpdateSessionWorkDir(ctx, sessionID, dir)
		if err != nil {
			return err
		}
		if !ok {
			return ErrSessionNotFound
		}
		return nil
	}
	sess.setWorkDir(dir)
	if s.store.pgStore != nil {
		ok, err := s.store.pgStore.UpdateSessionWorkDir(ctx, sessionID, dir)
		if err != nil {
			return err
		}
		if !ok {
			// 该会话尚未落过库（新建后首回合前）：走全量 upsert 补插入行，
			// 否则本次改动会在重启后丢失（恢复只认 session_history）。
			s.store.persistHistory(sess)
		}
	}
	// 审计留痕：主对话流留一条系统事件，事后可回溯"这个会话何时换了目录"
	//（产物路径变了却查不到原因是常见困惑）。
	note := "工作目录已改为 " + dir
	if dir == "" {
		note = "工作目录已重置为进程默认目录"
	}
	s.store.addEvent(sess, eventkind.System, "System", note, "", "", "", "", "", true)
	return nil
}

// SessionWorkDir 返回会话**有效**工作目录（每会话目录为空时回落进程默认目录）。
// 与 runSession/resumeSession 注入 runCtx 的口径一致（会话目录优先、空串回落默认），
// 供工作区文件服务这类只读路径解析使用；会话不存在返回空串。
func (s *ReactService) SessionWorkDir(sessionID string) string {
	s.store.mu.RLock()
	sess := s.store.sessions[sessionID]
	s.store.mu.RUnlock()
	if sess == nil {
		return ""
	}
	if wd := sess.currentWorkDir(); wd != "" {
		return wd
	}
	return s.workDir()
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

// recordClarifyBatchAnswer 把批量答复逐题记录到 pendingClarify.Questions（任务 140）：
// 每题按各自选项解析（命中选项回传 Label 连接文本，否则原文），回填 item 的
// Answer/AnswerOptionIDs/AnsweredAt；顶层 Answer/AnswerOptionIDs/AnsweredAt 镜像
// 第一题。返回逐题回传文本（下标与题目对齐）。
func recordClarifyBatchAnswer(pc *ClarifyRequest, answers []string) []string {
	if pc == nil {
		return answers
	}
	texts := make([]string, len(answers))
	for i, a := range answers {
		texts[i] = a
		if i >= len(pc.Questions) {
			continue
		}
		item := &pc.Questions[i]
		text, ids := parseClarifyAnswer(a, &ClarifyRequest{Options: item.Options})
		item.Answer = text
		item.AnswerOptionIDs = ids
		now := time.Now()
		item.AnsweredAt = &now
		texts[i] = text
	}
	if len(pc.Questions) > 0 {
		pc.Answer = pc.Questions[0].Answer
		pc.AnswerOptionIDs = pc.Questions[0].AnswerOptionIDs
		pc.AnsweredAt = pc.Questions[0].AnsweredAt
	}
	return texts
}

// numberedAnswers 把逐题答复编成 "1. X\n2. Y" 汇总文本（任务 140 批量答复
// 合并为一条聊天消息/事件——web 每条 user_message 事件开新 turn，逐题拆开
// 会把答复区打成 N 段）。
func numberedAnswers(answers []string) string {
	parts := make([]string, len(answers))
	for i, a := range answers {
		parts[i] = fmt.Sprintf("%d. %s", i+1, a)
	}
	return strings.Join(parts, "\n")
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
			ID:        fmt.Sprintf("ask-%d", time.Now().UnixNano()),
			Question:  question,
			Context:   "Agent 向用户提问（人在回路）",
			AgentID:   tool.AgentIDFromContext(ctx),
			CreatedAt: time.Now(),
			Detail:    strings.TrimSpace(opts.Detail),
		}
		// 结构化选项（TODO #53）：Kind=choice；纯自由文本提问保持旧行为（Kind=text）。
		// 单选自动追加「其他」逃生选项（buildClarifyOptions）。
		req.Options = buildClarifyOptions(opts.Options, opts.MultiSelect)
		req.MultiSelect = opts.MultiSelect
		// 随附产物（演示评审卡内嵌演示视频/回放页）：透传 agent 层 DTO。
		for _, a := range opts.Artifacts {
			req.Artifacts = append(req.Artifacts, ClarifyArtifact{
				Kind: a.Kind, Path: a.Path, Title: a.Title, Caption: a.Caption, MIME: a.MIME,
			})
		}
		if len(req.Options) > 0 {
			req.Kind = "choice"
		} else {
			req.Kind = "text"
		}
		if dl, ok := ctx.Deadline(); ok {
			req.Deadline = &dl
		}
		sess.pendingClarify = req
		sess.Status = enums.SessionStatusAwaitingClarify
		report := clarifyReportJSON(strings.TrimSpace(sess.StreamingText))
		s.store.mu.Unlock()

		// detail（如 submit_plan 计划全文）先于问题独立成事件（kind=clarify_detail，
		// 任务 140）——长上下文在前、短问题在后；独立 kind 避开前端 clarify 事件
		// last-wins 覆盖，面板/卡片按结构化字段渲染。
		if req.Detail != "" {
			s.store.addEvent(sess, eventkind.Clarify, "System", req.Detail, eventkind.ClarifyDetail, "", "", "", "", true)
		}
		s.store.addEventDetail(sess, eventkind.Clarify, "System", "Agent 提问: "+question, "", "", "", "", "", true, report)

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
			// 恢复即清上一轮流式缓冲：ask_user 阻塞期间 StreamingText 保留的是提问前
			// 那轮的答复文本，不清零会随 live 帧继续推送，新一轮思考阶段前端把旧文本
			// 当实时流重复渲染（2026-09-09 事故）。store.mu 不可重入，setStreamingText
			// 内部再加锁会死锁，持锁区必须直写字段。
			sess.StreamingText = ""
			s.store.mu.Unlock()
			return answer, nil
		}
	}
}

// EscalateGearHook 返回 escalate_gear 工具的会话层回调（TODO #14 T7 升档 + D-2 留痕）。
// fast（doc_assistant）/daily（domain）档顶层接到超范围工程任务时请求升级集群档：
// 经 askUser 通道推确认卡（槽位不变式与 ask_user/审批一致，占用排队等空位），用户确认后
// 切档 + 档位事件留痕 + 合成种子消息走"终态会话收消息→新 run"既有通道以集群档
// （meta 全装）重启；拒绝则原样返回继续当前档对话。cluster 档调用为误用（防御兜底）。
// 软停倒计时已武装（用户此前 Stop 过）时放弃自动续跑，只留档位（T7④）。
func (s *ReactService) EscalateGearHook() tool.EscalateGearHookFunc {
	return func(ctx context.Context, req tool.EscalateGearRequest) (string, error) {
		sid := tool.SessionIDFromContext(ctx)
		if sid == "" {
			return "", fmt.Errorf("escalate_gear: missing session context")
		}
		// 顶层守卫：escalate_gear 挂载于 doc_assistant（fast 顶层）与 domain（daily 顶层，
		// 同时是全部 domain 子 Agent 共享的 schema）——升档是会话级动作，仅会话顶层 Agent
		// 可调用（顶层 Agent name == 会话 ID，见 react_agent RunWithHistory 的 WithAgentID）。
		// 子 Agent 调用为误用，返回提示不占确认槽。
		if tool.AgentIDFromContext(ctx) != sid {
			return "escalate_gear 仅会话顶层可用：你是子 Agent，请把超出范围的需求写进结果摘要交由上层处理。", nil
		}
		// 等待用户期间保活 + 排队等槽位：与 AskUserHook 同范式（提问槽与审批共用一个）。
		keepalive := s.startUserWaitKeepalive(ctx)
		defer keepalive.Stop()
		s.store.mu.Lock()
		var sess *reactInternalSession
		for {
			sess = s.store.sessions[sid]
			if sess == nil {
				s.store.mu.Unlock()
				return "", fmt.Errorf("escalate_gear: 会话不存在")
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
		// 占槽推确认卡：Kind=confirm（确认/拒绝两选项，答复经 resolveApproval 裁决）。
		ch := make(chan string, 1)
		sess.askUser = ch
		question := "是否升级到集群档继续这个任务？原因：" + req.Reason
		curGearLabel := gearLabelCN(sess.currentGear())
		pc := &ClarifyRequest{
			ID:        fmt.Sprintf("gear-%d", time.Now().UnixNano()),
			Question:  question,
			Context:   fmt.Sprintf("%s→集群档升级确认（确认后当前对话将以集群档工程模式自动重启）", curGearLabel),
			AgentID:   tool.AgentIDFromContext(ctx),
			CreatedAt: time.Now(),
			Kind:      "confirm",
			Options: []ClarifyOption{
				{ID: "confirm", Label: "升级集群档", Description: "以全量工程能力重启任务（当前进展作为种子带上）"},
				{ID: "reject", Label: "继续当前档位", Description: "维持当前档位，不升级"},
			},
		}
		sess.pendingClarify = pc
		sess.Status = enums.SessionStatusAwaitingClarify
		report := clarifyReportJSON(strings.TrimSpace(sess.StreamingText))
		s.store.mu.Unlock()
		s.store.addEventDetail(sess, eventkind.Clarify, "System", question, "", "", "", "", "", true, report)

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
			// 收答复：清槽 + 恢复 Running + 捕获当前档位进展（切档收尾要用，AskUserHook 同款清流缓冲）。
			s.store.mu.Lock()
			if sess.askUser == ch {
				sess.askUser = nil
				sess.pendingClarify = nil
			}
			if sess.Status == enums.SessionStatusAwaitingClarify {
				sess.Status = enums.SessionStatusRunning
			}
			progress := strings.TrimSpace(sess.StreamingText)
			sess.StreamingText = ""
			currentGear := sess.currentGear()
			goal := sess.Goal
			s.store.mu.Unlock()

			if !resolveApproval(answer, pc) {
				s.store.addEvent(sess, eventkind.System, "System",
					fmt.Sprintf("用户选择继续%s，未升级", curGearLabel), "", "", "", "", "", true)
				return "用户选择保持当前档位：请以当前档位继续处理，不要再重复请求升级。", nil
			}
			// 升级只升不降：cluster 档调用本工具属模型误用（本工具只在 fast/daily 档
			// 顶层角色的 schema 里，防御性兜底），工具结果级提示不切档。
			if currentGear == tool.GearCluster {
				return "当前已是集群档，无需升级：请直接以对话方式处理任务。", nil
			}
			// 切档 + D-2 留痕（escalate 来源区分手动切换）。
			s.store.mu.Lock()
			sess.setGear(tool.GearCluster)
			s.store.mu.Unlock()
			s.store.addEvent(sess, eventkind.System, "System",
				fmt.Sprintf("档位升级: %s → cluster（escalate_gear，用户已确认）", gearDisplayName(currentGear)),
				"", "", "", "", "", true)

			// 软停倒计时已武装（用户此前 Stop 过）：放弃自动续跑只留档位（T7④）——
			// 倒计时到期硬销毁是用户明示意图，自动重启会违背它；用户下次发消息即按集群档续跑。
			s.store.mu.RLock()
			stopping := sess.destroyAt != nil || sess.stopTimer != nil
			s.store.mu.RUnlock()
			if stopping {
				return "档位已切换为集群档；会话处于停止倒计时，不自动重启。用户发送消息时将按集群档续跑。", nil
			}

			// 软停当前 run（落 awaiting_clarify 暂停态）→ 种子消息经 sendMessage
			// → resumeSession 按新档位（cluster）选 meta 角色全装重启。
			seed := gearEscalationSeed(currentGear, goal, progress, req)
			if err := s.Stop(ctx, sid); err != nil {
				s.store.logError(ctx, "[agent] escalate 升级软停当前 run 失败，已保留集群档", err)
				return "用户已确认升级集群档，但自动重启失败；请以对话方式告知用户稍后发送消息即可按集群档续跑。", nil
			}
			go s.restartWithGearSeed(sid, seed)
			return "用户已确认升级集群档。当前对话即将收尾，集群档任务已自动接手（含你的进展摘要与文件清单），无需你再输出。", nil
		}
	}
}

// gearDisplayName 档位展示名（事件箭头文案用，英文枚举 id）：空档按 daily（默认档语义）。
func gearDisplayName(gear string) string {
	switch gear {
	case tool.GearFast:
		return "fast"
	case tool.GearCluster:
		return "cluster"
	default:
		return "daily"
	}
}

// gearLabelCN 档位中文名（用户可见文案用）：空档按默认 daily。
func gearLabelCN(gear string) string {
	switch gear {
	case tool.GearFast:
		return "快速档"
	case tool.GearCluster:
		return "集群档"
	default:
		return "日常档"
	}
}

// gearEscalationSeed 合成升档种子消息（T7③）：原目标 + 原档位进展摘要 + 升级原因 +
// 任务简报 + 相关文件。集群档 meta 看不到原档位对话历史，这段种子是其唯一接手上下文。
func gearEscalationSeed(gear, goal, progress string, req tool.EscalateGearRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "【档位升级续跑】用户已确认把本会话从%s升级为集群档，请以全量工程能力接手完成以下任务。\n", gearLabelCN(gear))
	fmt.Fprintf(&b, "原目标: %s\n", strings.TrimSpace(goal))
	if p := truncateRunes(progress, 600); p != "" {
		fmt.Fprintf(&b, "%s阶段进展/结论: %s\n", gearLabelCN(gear), p)
	}
	fmt.Fprintf(&b, "升级原因: %s\n", req.Reason)
	fmt.Fprintf(&b, "任务简报: %s\n", req.TaskBrief)
	if len(req.Files) > 0 {
		fmt.Fprintf(&b, "相关文件: %s\n", strings.Join(req.Files, ", "))
	}
	return b.String()
}

// restartWithGearSeed 升档续跑：等旧 run 落定（Stop 软停后 runSession 的
// Canceled 分支落 awaiting_clarify）再投递种子消息——sendMessage 对非 running 会话
// 走 resumeSession 新 run，resumeSession 按档位选 meta 角色全装。旧 run 若 30s 未落定
//（流式重试等）放弃并留痕，用户手动发消息仍可续跑（档位已切换，不会丢）。
func (s *ReactService) restartWithGearSeed(sessionID, seed string) {
	bg := context.Background()
	deadline := time.Now().Add(30 * time.Second)
	for {
		s.store.mu.RLock()
		sess := s.store.sessions[sessionID]
		status := ""
		if sess != nil {
			status = string(sess.Status)
		}
		s.store.mu.RUnlock()
		if sess == nil {
			return // 会话已销毁（倒计时/硬删）：档位已随 MetaMemory 落库，无续跑可言
		}
		if status != string(enums.SessionStatusRunning) {
			if err := s.sendMessage(bg, sessionID, seed); err != nil {
				s.store.logError(bg, "[agent] escalate 升级续跑投递种子失败", err)
			}
			return
		}
		if time.Now().After(deadline) {
			s.store.logError(bg, "[agent] escalate 升级续跑超时：旧 run 未在预期内落定，等待用户手动续跑",
				fmt.Errorf("session %s still running", sessionID))
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// buildClarifyOptions 把 ask_user 工具的结构化选项转为 ClarifyOption 列表；
// 单选 choice 自动追加「其他」逃生选项：选项不精确/方向不对时用户点选后自由填写
// 答案，而不是被迫二选一。多选可勾选组合，不追加。单题/批量两个 hook 共用防漂移。
func buildClarifyOptions(opts []tool.AskUserOption, multiSelect bool) []ClarifyOption {
	if len(opts) == 0 {
		return nil
	}
	out := make([]ClarifyOption, 0, len(opts)+1)
	for _, o := range opts {
		out = append(out, ClarifyOption{ID: o.ID, Label: o.Label, Description: o.Description})
	}
	if !multiSelect {
		hasOther := false
		for _, o := range out {
			if o.ID == ClarifyOtherOptionID {
				hasOther = true
				break
			}
		}
		if !hasOther {
			out = append(out, ClarifyOption{
				ID:          ClarifyOtherOptionID,
				Label:       "其他（自行输入答案）",
				Description: "以上选项不够精确或方向不对时选这项，然后在输入框填写你的答案",
			})
		}
	}
	return out
}

// AskUserBatchHook 返回 ask_user 批量模式回调（任务 140）：全部题目一次挂出
// （同屏分页、可回退改选、必须逐题作答后统一提交）。与单题 AskUserHook 同通道
// 范式：置 pendingClarify（Questions 全量 + 顶层镜像第一题）+ awaiting_clarify +
// 事件流（detail 非空先落 clarify_detail，再逐题 question-only clarify）→ 阻塞等
// answerClarify 批量分支把逐题答复写入 askUserBatch 通道 → 按题序返回。
// 会话取消时返回 ctx 错误；槽位与 approval/askUser 互斥（2s 一拍排队等空位）。
func (s *ReactService) AskUserBatchHook() tool.AskUserBatchHookFunc {
	return func(ctx context.Context, questions []tool.AskUserQuestion, detail string) ([]string, error) {
		sid := tool.SessionIDFromContext(ctx)
		if sid == "" {
			return nil, fmt.Errorf("ask_user: missing session context")
		}
		// 等待用户期间保活：同 AskUserHook，排队与阻塞阶段都覆盖。
		keepalive := s.startUserWaitKeepalive(ctx)
		defer keepalive.Stop()
		s.store.mu.Lock()
		for {
			sess := s.store.sessions[sid]
			if sess == nil {
				s.store.mu.Unlock()
				return nil, fmt.Errorf("ask_user: 会话不存在")
			}
			if sess.approval == nil && sess.askUser == nil && sess.askUserBatch == nil {
				break // 拿到空槽位；锁保持持有，下方直接占位
			}
			s.store.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(2 * time.Second):
			}
			s.store.mu.Lock()
		}
		sess := s.store.sessions[sid]
		ch := make(chan []string, 1)
		sess.askUserBatch = ch
		req := &ClarifyRequest{
			ID:        fmt.Sprintf("ask-%d", time.Now().UnixNano()),
			Context:   "Agent 向用户提问（人在回路）",
			AgentID:   tool.AgentIDFromContext(ctx),
			CreatedAt: time.Now(),
			Detail:    strings.TrimSpace(detail),
		}
		for _, q := range questions {
			opts := buildClarifyOptions(q.Options, q.MultiSelect)
			kind := "text"
			if len(opts) > 0 {
				kind = "choice"
			}
			req.Questions = append(req.Questions, ClarifyQuestionItem{
				Question:    q.Question,
				Kind:        kind,
				MultiSelect: q.MultiSelect,
				Options:     opts,
			})
		}
		// 顶层镜像第一题：旧客户端只读顶层字段仍能渲染 Q1（答复会被批量分支的
		// 数量校验拒绝并提示走面板统一提交，不会静默错位）。
		if len(req.Questions) > 0 {
			first := req.Questions[0]
			req.Question = first.Question
			req.Kind = first.Kind
			req.MultiSelect = first.MultiSelect
			req.Options = first.Options
		}
		if dl, ok := ctx.Deadline(); ok {
			req.Deadline = &dl
		}
		sess.pendingClarify = req
		sess.Status = enums.SessionStatusAwaitingClarify
		report := clarifyReportJSON(strings.TrimSpace(sess.StreamingText))
		s.store.mu.Unlock()

		// 事件流：detail 先于问题（kind=clarify_detail），逐题 question-only
		//（与单题 AskUserHook 同形态；每题独立事件便于历史按问答对回看）。
		if req.Detail != "" {
			s.store.addEvent(sess, eventkind.Clarify, "System", req.Detail, eventkind.ClarifyDetail, "", "", "", "", true)
		}
		for i, q := range req.Questions {
			// 正文只挂第一条：前端按"任一 clarify 事件带正文即渲染"取值，无需题题复制。
			detail := ""
			if i == 0 {
				detail = report
			}
			s.store.addEventDetail(sess, eventkind.Clarify, "System", "Agent 提问: "+q.Question, "", "", "", "", "", true, detail)
		}

		select {
		case <-ctx.Done():
			s.store.mu.Lock()
			if sess.askUserBatch == ch {
				sess.askUserBatch = nil
				sess.pendingClarify = nil
			}
			s.store.mu.Unlock()
			return nil, ctx.Err()
		case answers := <-ch:
			s.store.mu.Lock()
			if sess.askUserBatch == ch {
				sess.askUserBatch = nil
				sess.pendingClarify = nil
			}
			if sess.Status == enums.SessionStatusAwaitingClarify {
				sess.Status = enums.SessionStatusRunning
			}
			// 恢复即清上一轮流式缓冲（同 AskUserHook；直写字段防死锁）。
			sess.StreamingText = ""
			s.store.mu.Unlock()
			return answers, nil
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
	// 树节点的 ParentID 是派发时的父 agentID（顶层子 Agent 记的是 meta 的 agentID==sessionID），
	// 而上方 meta 实例的 ModuleID 固定为 "meta"；不归一化会让前端按 parent_id 组树时
	// 找不到父节点、整棵子树被丢弃（2026-09-11 实证：编排面板只剩 MetaAgent）。
	for _, n := range s.TreeFor(sessionID).Snapshot() {
		parent := n.ParentID
		if parent == "" || parent == sessionID {
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
	// 取消即终结：回收实例级模型覆盖（工厂未接线时静默跳过）。
	if s.modelFactory != nil {
		s.modelFactory.ClearAgentModel(instID)
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
	workDir := session.currentWorkDir() // 话题摘要 KV 按会话目录解析(S2):锁内快照,供下方注入
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
				// 可视成果（ShowArtifact 的 Result.Artifacts）：并入 detail_json 落库，
				// 对话栏据此渲染媒体卡片（只带工作区相对路径，零二进制）。
				// detail_json 是既有持久列，无需迁移；历史回放靠它。
				if v, ok := detail["artifacts"]; ok {
					detailJSON = mergeArtifactsJSON(detailJSON, v)
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

// mergeArtifactsJSON 把工具产出的可视成果并入事件 detail_json（现为 {"agent_id":…}）。
// 失败时原样返回原值——留痕/展示数据不值得为此中断事件记录。
func mergeArtifactsJSON(detailJSON string, artifacts any) string {
	if artifacts == nil {
		return detailJSON
	}
	m := map[string]any{}
	if detailJSON != "" {
		_ = json.Unmarshal([]byte(detailJSON), &m)
	}
	m["artifacts"] = artifacts
	b, err := json.Marshal(m)
	if err != nil {
		return detailJSON
	}
	return string(b)
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

// subAgentDispatch 是一条派发事件要落的信息：对话栏子 Agent 列表、编排页节点、
// TUI 阶段标记都从这里取（领域名对用户可见，必须是中文展示名）。
type subAgentDispatch struct {
	RoleID string // 角色 ID（domain 或固定助手）
	Domain string // 中文领域展示名（仅 role_id=domain 时非空）
	Task   string // 任务摘要（单行，超长截断）
}

// parseSubAgentDispatch 从 call_sub_agent / call_sub_agents 的入参 JSON 解析派发清单：
// 单派发返回 1 项；批量工具按 tasks 数组**逐项**返回——对话栏要为每个子 Agent 列一行，
// 只报一条会把整批最多 6 个领域压成一行原始 JSON（用户看到的是乱码般的参数串）。
// 解析失败回退"原文摘要一项"，宁可不精确也不丢展示。
func parseSubAgentDispatch(argsJSON string) []subAgentDispatch {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return []subAgentDispatch{{Task: textutil.TruncateRunes(argsJSON, 2000, "…")}}
	}
	one := func(m map[string]any) subAgentDispatch {
		d := subAgentDispatch{}
		d.RoleID, _ = m["role_id"].(string)
		d.Domain, _ = m["domain"].(string)
		d.Domain = strings.TrimSpace(d.Domain)
		if v, ok := m["task"].(string); ok {
			d.Task = strings.ReplaceAll(strings.TrimSpace(v), "\n", " ")
		}
		return d
	}
	// 批量形态：tasks=[{role_id,task,domain,...}, ...]
	if raw, ok := args["tasks"].([]any); ok {
		out := make([]subAgentDispatch, 0, len(raw))
		for _, r := range raw {
			if m, ok := r.(map[string]any); ok {
				out = append(out, one(m))
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []subAgentDispatch{one(args)}
}

// dispatchDomainJSON 把派发事件的中文领域名编码为 DetailJSON（{"domain":"..."}），
// 供对话栏子 Agent 列表取展示名（事件 Tool 字段只放得下角色 ID，domain 角色恒为 "domain"，
// 九个领域会同名无法区分）。空领域名返回空串。
func dispatchDomainJSON(domain string) string {
	if domain == "" {
		return ""
	}
	b, err := json.Marshal(map[string]string{"domain": domain})
	if err != nil {
		return ""
	}
	return string(b)
}

// providerForRole 返回按角色解析 provider 的 WithProviderFunc 闭包（TODO #14 D-1 模型随档）：
// fast 档 doc_assistant 解析其模型绑定（roles.yaml model_ref），daily/cluster 解析 domain/meta 绑定。
// T17：sessionID 非空时走带备胎链的解析（models.json role_bindings[].fallback），
// 降级切换写会话事件流 + 日志。
// 会话级思考强度（2026-09-16）：sessionID 非空时实时读取会话 thinking 覆盖角色档，
// 运行中切换下一次 LLM 调用即生效（热）。
// 测试注入优先；模型工厂未装配时报错（generateOnce 侧保留旧 provider 不打断任务）。
func (s *ReactService) providerForRole(roleID, sessionID string) func(context.Context) (ModelProvider, error) {
	return func(ctx context.Context) (ModelProvider, error) {
		if s.testProvider != nil {
			return s.testProvider, nil
		}
		if s.modelFactory == nil {
			return nil, errors.New("model factory not available")
		}
		// 会话级思考强度实时读取（2026-09-16）：运行中切换，下一次 LLM 调用即生效
		//（providerForRole 闭包每次调用重解析）；会话不存在（罕见竞态）按空串=跟随角色默认。
		var thinking string
		if sessionID != "" {
			s.store.mu.RLock()
			sess := s.store.sessions[sessionID]
			s.store.mu.RUnlock()
			if sess != nil {
				thinking = sess.currentThinking()
			}
			return s.modelFactory.GetBladesProviderWithThinking(ctx, roleID, thinking, s.fallbackObserver(sessionID))
		}
		if thinking != "" {
			return s.modelFactory.GetBladesProviderWithThinking(ctx, roleID, thinking, nil)
		}
		return s.modelFactory.GetBladesProvider(ctx, roleID)
	}
}

// fallbackObserver 返回会话作用域的模型降级观察者（TODO 第15项 T17）：每次 fallback
// 切换向会话事件流落一条 System 事件（前端事件流可见"模型降级"），日志留档。
// 会话已不存在（恢复/删除竞态）时只留日志。
func (s *ReactService) fallbackObserver(sessionID string) model.FallbackObserver {
	return func(evt model.FallbackEvent) {
		log.Printf("[react] model fallback: session=%s role=%s %s → %s (attempt=%d, err=%s)",
			sessionID, evt.RoleID, evt.From, evt.To, evt.Attempt, evt.Err)
		s.store.mu.RLock()
		sess := s.store.sessions[sessionID]
		s.store.mu.RUnlock()
		if sess == nil {
			return
		}
		s.store.addEvent(sess, eventkind.System, "System",
			fmt.Sprintf("模型降级: %s → %s（主模型出错，已自动切换备胎）", evt.From, evt.To),
			"", "", "", "", "", true)
	}
}

// resolveGearMetaRole 按会话档位解析本轮顶层角色（TODO #14 三档全手动，2026-09-16）：
//   - fast → "doc_assistant"（文档助手顶层直达，挂 escalate_gear 升档）；
//   - daily → "domain"（DomainAgent 顶层直接执行，可自执行亦可自行下拆叶子）；
//   - cluster/未设置/未知 → "meta" 全装编排。
//
// 角色未登记时回退 "meta"（配置残缺退化现行为而非报错）。返回角色定义与实际角色 ID
// （D-1 模型随档按其解析）。
func (s *ReactService) resolveGearMetaRole(session *reactInternalSession) (*types.RoleDefinition, string) {
	roleID := "meta"
	switch session.currentGear() {
	case tool.GearFast:
		roleID = "doc_assistant"
	case tool.GearDaily:
		roleID = "domain"
	}
	if r := s.roleRegistry.Get(roleID); r != nil {
		return r, roleID
	}
	if roleID != "meta" {
		if r := s.roleRegistry.Get("meta"); r != nil {
			return r, "meta"
		}
	}
	return nil, roleID
}

// roleSkillBlock 渲染指定角色的固定技能元数据块（【可用技能】第一层）。
// daily 档（domain 顶层）复用 domain 角色 Skills（如【派发与规格】），与 dispatcher
// 给子 Agent 的技能块同源（口径同 subagent.fixedSkillNames——agent 包不能反向
// import subagent，此处复刻）。技能池未注入或角色无技能时返回空串（零注入）。
func (s *ReactService) roleSkillBlock(roleDef *types.RoleDefinition) string {
	if s.skillPool == nil || roleDef == nil || len(roleDef.Skills) == 0 {
		return ""
	}
	var names []string
	for _, key := range roleDef.Skills {
		sk := s.skillPool.FindByNameOrID(strings.TrimSpace(key))
		if sk == nil {
			log.Printf("[agent] skill: role %s 固定技能 %q 不在池中，跳过", roleDef.ID, key)
			continue
		}
		name := sk.Name
		if name == "" {
			name = sk.SkillID
		}
		names = append(names, name)
	}
	return skill.MetadataBlock(s.skillPool, names)
}

// runSession 为新创建的会话执行 ReAct 主循环。
func (s *ReactService) runSession(session *reactInternalSession) { // 获取会话上下文；若不存在则使用 Background。
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

	// 按档位解析本轮顶层角色（fast=doc_assistant / daily=domain / cluster=meta）；
	// 缺失则标记会话错误并退出。
	metaRole, gearRoleID := s.resolveGearMetaRole(session)
	if metaRole == nil {
		s.setSessionError(session, fmt.Sprintf("role %q not found", gearRoleID))
		return
	}

	// 顶层必备插件工具预挂载（fast/daily：联网搜索等直接可用，不必 tool_catalog+tool_mount；
	// cluster 的 meta 已有角色授权不重复挂；子 Agent scope 不受影响）。
	s.mountTopLevelEssentials(session.ID, tool.NormalizeGear(session.currentGear()))

	// PROJECT.md 就绪有界等待（TODO 第14项 T9）：cluster/daily 等 ≤2s 让【项目概览】段
	// 赶上首轮系统提示词前缀缓存；fast（doc_assistant）不等（秒回优先，缺失本就略段）。
	if gearRoleID != "doc_assistant" {
		waitProjectDoc(session, 2*time.Second)
	}

	// 确定模型 provider：优先使用测试注入的 provider，否则从模型工厂获取。
	// D-1 模型随档（TODO #14）：按本轮角色 ID 解析（fast=doc_assistant，daily=domain，cluster=meta）。
	var provider ModelProvider
	if s.testProvider != nil {
		provider = s.testProvider
	} else {
		// 会话级思考强度随首次解析生效（后续轮经 providerForRole 实时读取，见其注释）。
		p, err := s.modelFactory.GetBladesProviderWithThinking(ctx, gearRoleID, session.currentThinking(), s.fallbackObserver(session.ID))
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
	// 看板/空闲清单/台账三段包装 + meta 技能块仅集群档（meta）注入；daily（domain 顶层）
	// 与 fast（doc_assistant）跳过 meta 专属编排观测（TODO #14 P0-1 语义沿用）。
	// 系统提示词工作目录按会话解析(终审修复):会话 workDir 优先,空串回落进程默认目录,
	// 使 buildEnvBlock/LoadProjectDoc 与 createSession 的 EnsureProjectDoc 落在同一目录。
	wd := session.currentWorkDir()
	if wd == "" {
		wd = s.workDir()
	}
	metaMemory := s.memory
	skillBlock := ""
	switch gearRoleID {
	case "meta":
		metaMemory = wrapMetaMemory(s.memory, s.boardFn, session.ID)
		metaMemory = wrapMetaMemoryWithRoster(metaMemory, s.rosterFn(session.ID))
		metaMemory = wrapMetaMemoryWithLedger(metaMemory, s.ledgerFn, session.ID)
		skillBlock = s.metaSkillBlock()
	case "domain":
		skillBlock = s.roleSkillBlock(metaRole)
	}
	agent := NewReActAgent(session.ID, *metaRole, provider, NewToolRegistryAdapterForRole(s.toolRegistry, session.ID, metaRole.Tools, gearRoleID, s.pluginVisibility)).
		WithMailbox(s.mailbox).
		WithMemory(metaMemory).
		WithLoopConfig(s.runtimeCfg.LoopConfigByRole(gearRoleID)).
		WithLiveEvents(func(ev LiveEvent) { s.handleLiveEvent(session, ev) }).
		WithLogger(s.sessionLogger(session.ID, metaRole.Name)).
		WithWorkDir(wd).
		WithSkillBlock(skillBlock).
		WithMemoryIndex(s.memoryIndexBlock()).
		WithPersonaInjector(s.metaPersona(session.currentWorkDir())).
		WithMessageLogger(s.msgLogger).
		// 顶层 Agent（meta/domain）挂起等子语义：终答轮仍有未决子 Agent 时置
		// awaiting_child 而非原地阻塞（原按 role.ID=="meta" 硬判，daily 档放开为显式开关）。
		WithSuspendOnChildWait(true).
		// meta 长会话运行期间模型被切换时，下一次 LLM 调用即用新模型。
		WithProviderFunc(s.providerForRole(gearRoleID, session.ID))
	// 注入未决子 Agent 检查器，开启父会话终结保护（fast=doc_assistant 无派发，跳过；
	// daily=domain 可自行下拆叶子，需保留）。
	if gearRoleID != "doc_assistant" {
		if s.pendingChecker != nil {
			agent = agent.WithPendingChildrenChecker(s.pendingChecker)
		}
		// 注入 Paused 子 Agent 检查器，使顶层 Agent 在 wait loop 检测 Paused 子 domain 并主动暂停。
		if s.pausedChecker != nil {
			agent = agent.WithPausedChildChecker(s.pausedChecker)
		}
	}

	// 将会话 ID 注入工具上下文，便于工具内部识别当前会话。
	runCtx := tool.WithSessionID(ctx, session.ID)
	// 注入每会话工作目录（空则原样返回，回落 Executor 默认目录）。
	runCtx = tool.WithWorkDir(runCtx, session.currentWorkDir())
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
		// 错误分支同样落历史：ReAct 循环内部累积的 history 原本只在成功/暂停分支
		// 提交到 session.History，出错即丢弃——用户终止/出错后发新消息续跑时
		// History 为空，前文意图全失（2026-09-20 实证：计划被驳回后用户终止、
		// 补发"剔除范围"消息，模型只看当条消息做事，范围完全做反）。
		s.commitHistory(session, result.History)
		// 运行出错时标记会话错误并退出。
		s.setSessionError(session, err.Error())
		return
	}

	// 挂起等子（awaiting_child）：任务已全部派发、终答轮仍有未决子 Agent——
	// 中继文本落库 + 置挂起态，等子完成回调（WakeOnChildDone）或用户消息唤醒。
	// 先于 LimitReached 判断（两者互斥），不进验收闭环（非真终答，验收只在 pending==0 触发）。
	if result.SuspendOnChildWait {
		s.suspendOnChildWait(session, result)
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

	// 交付验收闭环（测试助手大改）：终答先经验收循环处理再交付——
	// 未接线或 .bma/tester.yaml 为 off 时 RunWrap 原样返回，零行为变化。
	finalText := result.Text
	if s.acceptance != nil {
		awd := session.currentWorkDir()
		if awd == "" {
			awd = s.workDir()
		}
		finalText = s.acceptance.RunWrap(runCtx, session.ID, awd, session.Goal, result.Text)
	}

	// 运行成功：先落完成事件再翻状态——SSE 流见终态即推 done 帧关流（stream_http.go），
	// 事件必须先入列才能被最后一个 tick 带出；先翻状态的话 tick 落在窗口内就只推
	// session_status+done，最终答复永远到不了前端（2026-09-09 事故缺口 B）。
	s.flushPendingTopText(session)
	s.store.addEvent(session, eventkind.AgentDone, "MetaAgent", finalText, "", "", "", "", "", true)

	// 更新会话状态为已完成，并记录结果与历史。
	now := time.Now()
	s.store.mu.Lock()
	session.Status = enums.SessionStatusCompleted
	session.Result = finalText
	session.EndedAt = &now
	session.History = result.History
	s.store.mu.Unlock()

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

	// 使用最新用户消息作为本轮输入。挂起等子唤醒轮（WakeOnChildDone 写入
	// wakeInput）优先取 wakeInput 作输入，即取即清（一次性）；否则倒序取最后一条
	// user 消息（中途可能追加了澄清/审批答复等非 user 项），同步取出该轮用户图片
	// 供 runCtx 注入：带外穿透给 RunWithHistory 的首条 user 消息与 call_sub_agent
	// 子 Agent（每轮新 runCtx，图片按轮作用域）。
	var input string
	var turnImages []tool.ResultImage
	s.store.mu.Lock()
	wakeTurn := session.wakeInput != ""
	if wakeTurn {
		input = session.wakeInput
		session.wakeInput = ""
	} else {
		for i := len(session.Messages) - 1; i >= 0; i-- {
			if session.Messages[i].Role == string(enums.ChatRoleUser) {
				input = session.Messages[i].Content
				turnImages = session.Messages[i].Images
				break
			}
		}
	}
	// 拷贝历史记录，避免在加锁期间被外部修改。
	history := make([]ReactMessage, len(session.History))
	copy(history, session.History)
	s.store.mu.Unlock()

	// 按档位解析本轮顶层角色（fast=doc_assistant / daily=domain / cluster=meta）；缺失则报错。
	metaRole, gearRoleID := s.resolveGearMetaRole(session)
	if metaRole == nil {
		s.setSessionError(session, fmt.Sprintf("role %q not found", gearRoleID))
		return
	}

	// 顶层必备插件工具预挂载（同 runSession：fast/daily 顶层直接可用，子 Agent 不受影响）。
	s.mountTopLevelEssentials(session.ID, tool.NormalizeGear(session.currentGear()))

	// 确定模型 provider，逻辑同 runSession（D-1 模型随档：按本轮角色 ID 解析）。
	var provider ModelProvider
	if s.testProvider != nil {
		provider = s.testProvider
	} else {
		// 会话级思考强度随首次解析生效（后续轮经 providerForRole 实时读取，见其注释）。
		p, err := s.modelFactory.GetBladesProviderWithThinking(ctx, gearRoleID, session.currentThinking(), s.fallbackObserver(session.ID))
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
	// 看板/空闲清单/台账三段包装 + meta 技能块仅集群档注入（同 runSession，TODO #14 P0-1）。
	// 系统提示词工作目录按会话解析(终审修复,同 runSession):会话 workDir 优先,空串回落进程默认目录。
	wd := session.currentWorkDir()
	if wd == "" {
		wd = s.workDir()
	}
	metaMemory := s.memory
	skillBlock := ""
	switch gearRoleID {
	case "meta":
		metaMemory = wrapMetaMemory(s.memory, s.boardFn, session.ID)
		metaMemory = wrapMetaMemoryWithRoster(metaMemory, s.rosterFn(session.ID))
		metaMemory = wrapMetaMemoryWithLedger(metaMemory, s.ledgerFn, session.ID)
		skillBlock = s.metaSkillBlock()
	case "domain":
		skillBlock = s.roleSkillBlock(metaRole)
	}
	agent := NewReActAgent(session.ID, *metaRole, provider, NewToolRegistryAdapterForRole(s.toolRegistry, session.ID, metaRole.Tools, gearRoleID, s.pluginVisibility)).
		WithMailbox(s.mailbox).
		WithMemory(metaMemory).
		WithLoopConfig(s.runtimeCfg.LoopConfigByRole(gearRoleID)).
		WithLiveEvents(func(ev LiveEvent) { s.handleLiveEvent(session, ev) }).
		WithLogger(s.sessionLogger(session.ID, metaRole.Name)).
		WithWorkDir(wd).
		WithSkillBlock(skillBlock).
		WithMemoryIndex(s.memoryIndexBlock()).
		WithPersonaInjector(s.metaPersonaLite()).
		WithMessageLogger(s.msgLogger).
		// 顶层 Agent（meta/domain）挂起等子语义（同 runSession）。
		WithSuspendOnChildWait(true).
		// 同 runSession：运行期模型切换在下一次 LLM 调用生效。
		WithProviderFunc(s.providerForRole(gearRoleID, session.ID))
	// 注入未决子 Agent 检查器，开启父会话终结保护（fast=doc_assistant 无派发，跳过；
	// daily=domain 可自行下拆叶子，需保留）。
	if gearRoleID != "doc_assistant" {
		if s.pendingChecker != nil {
			agent = agent.WithPendingChildrenChecker(s.pendingChecker)
		}
		// 注入 Paused 子 Agent 检查器，使顶层 Agent 在 wait loop 检测 Paused 子 domain 并主动暂停。
		if s.pausedChecker != nil {
			agent = agent.WithPausedChildChecker(s.pausedChecker)
		}
	}

	// 注入会话 ID 到工具上下文。
	runCtx := tool.WithSessionID(ctx, session.ID)
	// 注入每会话工作目录（空则原样返回，回落 Executor 默认目录）。
	runCtx = tool.WithWorkDir(runCtx, session.currentWorkDir())
	// 注入会话级 stopCtx（TODO 第10④）：续跑路径同 runSession，子派发以此取消基底。
	runCtx = tool.WithStopContext(runCtx, session.stopCtx)
	// 注入信任模式读取器（TODO 第10⑥）：同 runSession，中途切换下一工具调用生效。
	runCtx = tool.WithTrustModeFunc(runCtx, session.currentTrustMode)

	if len(turnImages) > 0 {
		runCtx = WithUserImages(runCtx, turnImages)
	}

	// 召回旧话题摘要拼到本轮输入前(切换话题后续接上下文);同一话题只注入一次。
	// 用注入会话 workDir 后的 runCtx(同 runSession):话题摘要 KV 按会话目录解析。
	// acceptGoal 保留注入前的原始用户输入，供终答验收闭环作任务目标（不掺召回/技能前缀）。
	// 挂起等子唤醒轮的输入是系统唤醒提示而非用户目标，acceptGoal 回落会话 Goal。
	acceptGoal := input
	if wakeTurn {
		acceptGoal = session.Goal
	}
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
		// 错误分支同样落历史（同 runSession：终止/出错后续跑不丢前文意图）。
		s.commitHistory(session, result.History)
		s.setSessionError(session, err.Error())
		return
	}

	// 挂起等子（同 runSession）：中继文本落库 + 置 awaiting_child，不进验收闭环。
	if result.SuspendOnChildWait {
		s.suspendOnChildWait(session, result)
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

	// 交付验收闭环（同 runSession）：终答先经验收循环处理再交付。
	finalText := result.Text
	if s.acceptance != nil {
		awd := session.currentWorkDir()
		if awd == "" {
			awd = s.workDir()
		}
		if strings.TrimSpace(acceptGoal) == "" {
			acceptGoal = session.Goal
		}
		finalText = s.acceptance.RunWrap(runCtx, session.ID, awd, acceptGoal, result.Text)
	}

	// 先落完成事件再翻状态（同 runSession：done 帧关流前事件必须已在列，
	// 2026-09-09 事故缺口 B）。
	s.flushPendingTopText(session)
	s.store.addEvent(session, eventkind.AgentDone, "MetaAgent", finalText, "", "", "", "", "", true)

	// 更新会话状态为已完成。
	now := time.Now()
	s.store.mu.Lock()
	session.Status = enums.SessionStatusCompleted
	session.Result = finalText
	session.EndedAt = &now
	session.History = result.History
	s.store.mu.Unlock()

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
	// 会话终态 Webhook 通知（TODO #18-5 T32）：best-effort 异步，未接线/不关心该状态时零开销。
	s.notifier.NotifySessionFinal(session.ID, string(session.Status), session.Goal)
	// 暂停待续（awaiting_clarify / paused_on_child / awaiting_child）的会话保留临时目录，用户续跑时仍需其中的中间产物。
	if session.Status != enums.SessionStatusAwaitingClarify && session.Status != enums.SessionStatusPausedOnChild && session.Status != enums.SessionStatusAwaitingChild {
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

// isClusterTopEvent 判定事件是否来自集群档的顶层 Meta（而非子 Agent 转发）：
// 顶层 Meta 的实例 ID 就是会话 ID（emitLive：AgentID 填实例 ID，MetaAgent=session ID；
// 子 Agent 为 "session-1/code_assistant-5"）。该档顶层 Meta 的中间轮口播必须与
// 用户流隔离——LiveEventLLMDelta 只进轮缓冲、LiveEventToolCall 边界丢弃
//（2026-09-18 编排内心独白泄露进对话栏实证；初版误用 ev.Agent=="" 判定，实测顶层
// 事件带展示名 role.Name="MetaAgent"，条件永不命中）。
func (s *ReactService) isClusterTopEvent(session *reactInternalSession, ev LiveEvent) bool {
	return ev.AgentID == session.ID && session.currentGear() == tool.GearCluster
}

// hasActiveSubAgents 判定会话编排树上是否存在运行中的子 Agent 节点（不含顶层会话节点）。
// 集群档顶层 Meta 的流式分流依据：子 Agent 在跑时的顶层文本是编排口播（缓冲丢弃，防
// 内心独白泄露进用户流，2026-09-18 治理）；子 Agent 全部结束后顶层文本即终答/直接汇报，
// 实时直推 StreamingText——否则终答生成期（可达数分钟）前端只有一个转圈占位，
// 用户长时间看不到任何内容（2026-09-20 用户实证）。树注册表自带锁，可与 store.mu 嵌套。
func (s *ReactService) hasActiveSubAgents(sessionID string) bool {
	t := s.TreeFor(sessionID)
	if t == nil {
		return false
	}
	for _, n := range t.Snapshot() {
		if n.ID != sessionID && n.Status == orchestrator.StatusRunning {
			return true
		}
	}
	return false
}

// flushPendingTopText 把集群档顶层 Meta 的轮缓冲冲刷进 StreamingText：
// 中间轮口播在工具调用边界已丢弃，能活到 run 完成的只有终答（或 ask_user 正文，
// 那条在 ToolCall 分支已自行冲刷）——冲刷让 SSE 末帧快照带上最终答复文本，
// 与 agent_done 事件同 tick 推送，前端直播行与完成气泡都有内容。
func (s *ReactService) flushPendingTopText(session *reactInternalSession) {
	if session.pendingTopText == "" {
		return
	}
	s.store.setStreamingText(session, session.pendingTopText)
	session.pendingTopText = ""
}

// persistInterimText 把"调用工具前模型输出的正文"落为会话 assistant_text 事件。
//
// 为什么需要：模型常见「先说一句（刚做了什么/接下来干什么）、再调工具」。这段正文只存在于
// 瞬时字段 StreamingText（流式增量按设计不落事件）——下一个 LLM 轮次的 delta 直接覆盖它，
// 前端 live 行也在工具调用事件到达时清掉，于是用户看到「话刚出现、一调工具就整段消失」，
// 刷新/回放后更是从来没有过（2026-09-13 用户实证）。在工具调用边界落一条事件即持久化：
// 一次 LLM 轮次至多一条，量有界（不落 token 级事件）。
//
// 刻意不清 StreamingText：ask_user / 审批 hook 还要用它做提问正文快照（clarifyReportJSON），
// 提前清空会让问答卡上方的正文丢失。ask_user 自身跳过——它的正文由提问事件的
// detail_json.report_text 承载，这里再落一条会在对话栏重复展示同一段话。
func (s *ReactService) persistInterimText(session *reactInternalSession, ev LiveEvent) {
	if session == nil || ev.Tool == "ask_user" {
		return
	}
	text := strings.TrimSpace(session.StreamingText)
	// 子 Agent 的正文经 ForwardLiveEvent 加过【展示名】前缀，落事件时剥掉，
	// 事件本身已带 Agent 归属，避免前缀污染展示（同 finalizeThinking）。
	if ev.Agent != "" {
		text = strings.TrimSpace(strings.TrimPrefix(text, "【"+ev.Agent+"】\n"))
	}
	// 同轮并行多个工具调用会连发多条 ToolCall 事件而流式文本未变——同一段正文只落一条。
	if text == "" || text == session.lastInterimText {
		return
	}
	session.lastInterimText = text
	s.store.addEventDetail(session, eventkind.Message, ev.Agent, text, eventkind.AssistantText, "", "", "", "", true, agentIDJSON(ev.AgentID))
}

// clarifyReportJSON 把"提问前的答复正文"编码为事件的 DetailJSON（{"report_text":"..."}）。
//
// 为什么需要：模型常见「先输出正文、再调 ask_user」——正文只存在于瞬时字段
// StreamingText（流式增量按设计不落事件），提问卡片一出现，前端就切到待澄清态，
// 上一段正文被整段吞掉，用户只剩思考链，还得靠脑补才能回答问题。
// 在提问事件上挂一份快照（ask_user 阻塞期间 StreamingText 仍保留该正文，恢复才清），
// 刷新/回放/重启后仍在，前端在问答卡上方原样渲染。空正文返回空串。
func clarifyReportJSON(report string) string {
	if report == "" {
		return ""
	}
	b, err := json.Marshal(map[string]string{"report_text": report})
	if err != nil {
		return ""
	}
	return string(b)
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
		if s.isClusterTopEvent(session, ev) {
			// 集群档顶层 Meta 分流：子 Agent 在跑 → 正文只进轮缓冲不推 StreamingText
			//（中间轮的编排口播绝对不能进用户流，2026-09-18 用户实证）；子 Agent 全部
			// 结束 → 顶层文本即终答/直接汇报，实时直推——终答生成期不再整段转圈
			//（2026-09-20 用户实证）。ask_user 恢复路径会清 StreamingText，无残留问题。
			if s.hasActiveSubAgents(session.ID) {
				session.pendingTopText = ev.Text
			} else {
				session.pendingTopText = ""
				s.store.setStreamingText(session, ev.Text)
			}
			break
		}
		s.store.setStreamingText(session, ev.Text)
	case LiveEventThinkDelta:
		if s.isClusterTopEvent(session, ev) {
			// 集群档顶层 Meta：思考链与中间轮口播同治理（2026-09-19 定案修复）——
			// 编排推理既不进 ThinkingText（live 思考盒）也不落 think 事件（回放不再
			// 以同等级推理重现）。该档用户只看最终交付；日常档与子 Agent 的思考
			// 展示不受影响。
			break
		}
		s.store.setThinkingText(session, ev.Text)
	case LiveEventToolCall:
		// 工具调用开始同样意味着思考阶段结束（思考型模型常见 think→tool 而非 think→text）。
		s.finalizeThinking(session, ev)
		if s.isClusterTopEvent(session, ev) {
			// 集群档顶层 Meta：本轮正文是中间轮口播，连同缓冲一起丢弃——不落 assistant_text
			// 事件、不进 StreamingText（用户流里绝不出现编排内心独白，2026-09-18 用户实证）。
			// ask_user 例外：提问正文快照（clarifyReportJSON）读 StreamingText，冲刷保留，
			// 提问卡上方要展示这段正文。
			if ev.Tool == "ask_user" && session.pendingTopText != "" {
				// 直推模式下缓冲恒空、StreamingText 已是实时正文，不得冲刷覆盖为空。
				s.store.setStreamingText(session, session.pendingTopText)
			}
			session.pendingTopText = ""
		} else {
			// 同一边界也是"这一轮正文说完了"：模型常见「口播一句（做了什么/接下来干什么）→调工具」，
			// 那段正文只活在瞬时 StreamingText 里，下一个轮次的 delta 直接覆盖、前端 live 行也在
			// 工具调用事件到达时清掉——用户看到的是"话刚出现就凭空消失"（2026-09-13 用户实证）。
			s.persistInterimText(session, ev)
		}
		// call_sub_agent 是子 Agent 派发：记录专用派发事件（角色 ID 与任务摘要），
		// 供 TUI 对话区展示阶段标记、编排面板派生子 Agent 节点。
		if ev.Tool == "call_sub_agent" || ev.Tool == "call_sub_agents" {
			// 批量工具逐项落事件（对话栏子 Agent 列表一人一行）；中文领域名随 detail_json
			// 带给前端——事件 Tool 字段只放得下角色 ID，domain 角色恒为 "domain"，
			// 九个领域会同名无法区分谁是谁。
			for _, dis := range parseSubAgentDispatch(ev.Input) {
				s.store.addEventDetail(session, eventkind.Message, ev.Agent, dis.Task, "sub_agent_dispatch", dis.RoleID, "", "", "", true, dispatchDomainJSON(dis.Domain))
			}
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
	case LiveEventPeerAsk:
		// 收到其他 Agent 的提问（cross-Agent 问答，send_message request/escalate 投递）：
		// 落 Message 事件供用户看到问答在发生；正文为提问方+摘要。
		s.store.addEvent(session, eventkind.Message, "PeerAsk", "收到 "+ev.Tool+" 的询问", "peer_ask", ev.Tool, "", "", "", true)
		if strings.TrimSpace(ev.Text) != "" {
			s.store.addEventDetail(session, eventkind.Progress, "PeerAsk", ev.Text, eventkind.LLMResult, ev.Tool, "", "", "", true, agentIDJSON(ev.Tool))
		}
	case LiveEventMilestone:
		// 子 Agent 关键节点里程碑（C-2：subject 前缀「里程碑:」的 info 播报）：
		// 落 Message 事件让用户看到长任务中途进展；kind=milestone 与 sub_agent_done
		// 区分（是中途播报不是完成回传），前端按发言档展示。
		if strings.TrimSpace(ev.Text) != "" {
			s.store.addEvent(session, eventkind.Message, "SubAgent", ev.Text, "milestone", ev.Tool, "", "", "", true)
		}
	case LiveEventNotify:
		// 系统级通知（如"模型不支持图片输入，已剥图降级"）：落 System 事件，
		// 让用户在对白流里看到 agent 为何改变了做法（而非静默换路）。
		if strings.TrimSpace(ev.Text) != "" {
			s.store.addEvent(session, eventkind.System, ev.Agent, ev.Text, "", "", "", "", "", true)
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
	// PauseChildWait 挂起等子（awaiting_child）：Meta 任务已全部派发、终答轮仍有未决
	// 子 Agent，挂起等子完成回调（WakeOnChildDone）或用户新消息唤醒续跑。
	PauseChildWait
)

// pauseMessage 按 PauseKind 返回面向用户的暂停提示文案。
func (s *ReactService) pauseMessage(kind PauseKind) string {
	switch kind {
	case PauseChildWait:
		return "任务已全部派发，挂起等待子 Agent 回传。子 Agent 完成会自动继续；你也可以直接发新消息，我会结合当前任务状态处理。"
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
// PauseOnChild -> paused_on_child（优先恢复暂停的 domain）；
// PauseChildWait -> awaiting_child（挂起等子回传，子完成回调或用户消息唤醒）；
// 其他 -> awaiting_clarify（普通续跑）。
// History 完整保留，sendMessage -> resumeSession/resumePausedDomain 从当前进度续跑。
// 不动 EndedAt：暂停态会话非终态，evictCompletedSessions 不会将其淘汰。
// commitHistory 把 ReAct 循环返回的历史提交到会话（错误/取消路径专用）。
// history 为本次运行累积的完整历史（RunWithHistory 返回时必然 ⊇ 续跑前历史），
// 仅在非空时覆盖，避免异常路径把会话历史清空。
func (s *ReactService) commitHistory(session *reactInternalSession, history []ReactMessage) {
	if len(history) == 0 {
		return
	}
	s.store.mu.Lock()
	session.History = history
	s.store.mu.Unlock()
}

func (s *ReactService) pauseSession(session *reactInternalSession, history []ReactMessage, kind PauseKind) {
	s.store.mu.Lock()
	session.History = history
	switch kind {
	case PauseChildWait:
		session.Status = enums.SessionStatusAwaitingChild
		session.Result = "任务已派发，挂起等待子 Agent，发消息或子 Agent 完成自动继续"
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

// suspendOnChildWait 落定「挂起等子」：Meta 中继文本落普通消息事件（前端按正常发言展示），
// 会话置 awaiting_child（保留 History 与临时目录）。
// 竞态收口：Run 判定 pending>0 到挂起落定之间有时间窗——若窗口内最后一个子 Agent 已完成
// （trackChildDone 的唤醒回调彼时见 Running 态空转），此处补检 pending==0 立即自唤醒；
// 同理窗口内子 Agent 发来 send_message(request)/submit_plan 审批等任意邮箱消息，
// 投递时的唤醒因状态仍 Running 被过滤空转、消息滞留邮箱（发信方阻塞等答复，双方死锁），
// 补检邮箱未读同样立即自唤醒（P0-2a）。否则再无事件触发唤醒，会话死等。
func (s *ReactService) suspendOnChildWait(session *reactInternalSession, result ReactResult) {
	if text := strings.TrimSpace(result.Text); text != "" {
		s.store.addEvent(session, eventkind.Message, "MetaAgent", text, "", "", "", "", "", true)
	}
	s.pauseSession(session, result.History, PauseChildWait)
	pending := 0
	if s.pendingChecker != nil {
		pending = s.pendingChecker.PendingChildren(session.ID)
	}
	if pending == 0 || (s.mailbox != nil && s.mailbox.Count(session.ID) > 0) {
		s.WakeOnChildDone(session.ID)
	}
}

// WakeOnChildDone 子 Agent 完成回传时唤醒「挂起等子」会话（awaiting_child → running）。
// 由 bootstrap 经 Dispatcher.WithChildDoneNotify 接线到 trackChildDone 回调；
// trackChildDone 对每次子完成都触发（含父非 meta 的场景），此处按会话态过滤幂等。
//
// 智能唤醒（C-3b 集群档提速）：仍有未决子（pending>0）且邮箱无未读时**不**翻态——
// 中间完成往往只有"收到，继续等"可说，保持挂起省一轮空转；最后一个完成
// （trackChildDone 先减计数再触发回调，末次必见 pending==0）或邮箱有未读
// （回传摘要/审批请求/直问/纪要）时必醒。漏醒无路径：邮箱到达自带独立唤醒
// （wakeSuspendedParent），pending 归零必经本回调。pendingChecker/mailbox 任一
// 未注入时保守退回旧行为（恒唤醒）。
func (s *ReactService) WakeOnChildDone(parentID string) {
	if s.pendingChecker != nil && s.mailbox != nil &&
		s.pendingChecker.PendingChildren(parentID) > 0 && s.mailbox.Count(parentID) == 0 {
		return
	}
	s.WakeSuspended(parentID, "【系统】有子 Agent 完成回传，请查收邮箱摘要、整合进度后继续（用户暂无新指令）。")
}

// WakeSuspended 唤醒「挂起等子」会话（awaiting_child → running），wakeInput 作本轮
// 输入（即取即清，替代用户新指令）。除子完成（WakeOnChildDone）外，还服务邮箱
// MsgRequest 到达路径——submit_plan 审批请求与 send_message request/escalate 直问：
// 挂起中的会话不会 drain 邮箱，不先翻态续跑消息会滞留到超时（死信）。
// 仅 awaiting_child 翻转并起 resumeSession；其余状态（Running/Completed/会话不在
// 内存/父是 domain 等）幂等空转返回 false。
func (s *ReactService) WakeSuspended(parentID, wakeInput string) bool {
	session := s.store.getSession(parentID)
	if session == nil {
		return false
	}
	s.store.mu.Lock()
	if session.Status != enums.SessionStatusAwaitingChild {
		s.store.mu.Unlock()
		return false
	}
	session.Status = enums.SessionStatusRunning
	session.EndedAt = nil
	session.wakeInput = wakeInput
	restartSessionContext(session)
	s.store.mu.Unlock()

	// 轮开始落库 running（同 sendMessage 恢复路径）：进程在本轮中途崩溃时，
	// 重启恢复逻辑依据库中 running 状态把会话标记为"因服务重启中断"。
	s.store.persistHistory(session)
	s.store.persistEvents(session)
	go s.resumeSession(session)
	return true
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
	session.runStartedAt = time.Now()
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

	// 批量 ask_user 待答复（任务 140）：单条文本无法对齐逐题答复，明确拒绝引导
	// 走问答面板统一提交（走下方续跑路径会双开 ReAct 循环）。
	if session.askUserBatch != nil {
		s.store.mu.Unlock()
		return fmt.Errorf("%w: 批量提问待答复，请在问答面板逐题作答后一次性提交", ErrInvalidSessionState)
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
	// 决策层①任务级意图分诊（TODO #23 切入点1）：四层管线只覆盖短指令 IntentKind，
	// 任务级意图无前置——决策层补 Choice(任务性质)+Choice(工具面)+Noul(需澄清?)。
	// 只做建议与澄清触发（建议前缀并入 content），不自动改档；影子期异步零延迟。
	triageAdvice := s.decideTaskTriage(ctx, sessionID, content)
	if triageAdvice != "" {
		content = content + "\n\n" + triageAdvice
	}
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
	// T18 选档误判信号①（只落事件不改行为）：快速档 + 上一 run 终态 ≤5min + 行动动词
	// ——疑似"工作活进了快速档"。数据锁内取（EndedAt 在下方被清），事件锁外落。
	gearSignalDetail := gearSignalOnSend(session.currentGear(), session.EndedAt, content)
	// 决策层⑥档位建议（只读影子，TODO #23 切入点6）：与 T18 信号并行落对拍行，
	// 永不做自动选档（auto 退役决策不破）。数据锁内取当前档位，异步观测。
	s.gearHintShadow(ctx, sessionID, content, session.currentGear())
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
	// T18 信号①落事件（type=system，「档位信号:」前缀 + detail_json 结构化字段）。
	if gearSignalDetail != "" {
		s.store.addEventDetail(session, eventkind.System, "System",
			"档位信号: 快速档会话收到任务型指令（距上次完成 ≤5min），疑似应升集群档",
			"", "", "", "", "", true, gearSignalDetail)
	}

	// 软停止窗口内任何用户消息都视作续跑意图：清倒计时定时器 + 清标记（幂等）。
	// 不能只挂在 !wasRunning 分支--Stop 后会话可能尚 Running（MetaAgent 的 LLM
	// 调用正在中断收尾途中），消息只入队不清倒计时，销毁倒计时到期会整体销毁。
	s.cancelSoftStopState(sessionID)

	// 添加用户消息事件。
	s.store.addEvent(session, eventkind.UserMessage, "User", content, "", "", "", "", "", true)

	// 运行中的会话：把新指令**投进 MetaAgent 邮箱**，本轮内的下一个检查点即读到它
	//（等子 Agent 时 waitForChildren 每周期 drain，立即生效；其他阶段主循环顶部 drain，
	// 下一步生效）。不投递的话这条消息只躺在 session.Messages 里，而运行中的循环从不读
	// 该字段——用户看着"已发送"，Agent 毫无反应（2026-09-12 实证）。
	if wasRunning {
		if err := s.injectUserMessageToRunningSession(session, content); err != nil {
			// 邮箱未接线（测试/精简装配）：降级为"随下一轮生效"，但要留痕，不静默。
			s.store.logError(ctx, "[agent] 运行中指令注入失败，本条将在下一轮生效", err)
		}
	}

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
		tool.WithStopContext(tool.WithWorkDir(tool.WithSessionID(ctx, session.ID), session.currentWorkDir()), session.stopCtx), pausedNodeID)
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
// answers 为批量模式（任务 140）的逐题答复（下标与题目对齐）；单题路径只用 answer。
func (s *ReactService) answerClarify(ctx context.Context, sessionID, answer string, answers []string) error {
	// 空答复拒绝处理（批量路径由 answers 承载）。
	if answer == "" && len(answers) == 0 {
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
	// 批量 ask_user 待答复（任务 140）：answers 必须逐题对齐且全部非空（同屏分页
	// 必须全部作答才允许提交），回填后写入 askUserBatch 通道；答复合并为一条
	// [澄清答复] 消息 + 一条「提问答复」事件（web 每条 user_message 事件开新 turn，
	// 逐题拆事件会把答复区打成 N 段）。
	if session.askUserBatch != nil {
		pc := session.pendingClarify
		if pc == nil || len(answers) != len(pc.Questions) {
			n := 0
			if pc != nil {
				n = len(pc.Questions)
			}
			s.store.mu.Unlock()
			return fmt.Errorf("%w: 答复数量与问题数不一致（%d 题），请在问答面板逐题作答后一次性提交", ErrInvalidSessionState, n)
		}
		for i, a := range answers {
			if strings.TrimSpace(a) == "" {
				s.store.mu.Unlock()
				return fmt.Errorf("%w: 第 %d 题答复为空，请逐题作答后再提交", ErrInvalidSessionState, i+1)
			}
		}
		texts := recordClarifyBatchAnswer(pc, answers)
		session.askUserBatch <- texts
		numbered := numberedAnswers(texts)
		session.Messages = append(session.Messages, Message{
			Role:      string(enums.ChatRoleUser),
			Content:   "[澄清答复] " + numbered,
			Timestamp: time.Now(),
		})
		s.store.mu.Unlock()
		s.store.addEvent(session, eventkind.Clarify, "User", "提问答复: "+numbered, "", "", "", "", "", true)
		return nil
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
		return s.answerClarify(ctx, sessionID, content, nil)
	}
	// 批量 ask_user 待答复（任务 140）：单条文本无法对齐逐题答复，明确拒绝引导
	// 走问答面板统一提交（不能路由——会经 answerClarify 的数量校验失败，也不能
	// 走续跑路径——双开 ReAct 循环）。
	if session.askUserBatch != nil {
		s.store.mu.Unlock()
		return fmt.Errorf("%w: 批量提问待答复，请在问答面板逐题作答后一次性提交", ErrInvalidSessionState)
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
	// 运行中：投进 MetaAgent 邮箱即时生效（同 sendMessage）。此前该分支只发事件不投递，
	// 内容根本没进对话历史/循环——"队列注入"名不副实（2026-09-12 实证）。
	if wasRunning {
		if err := s.injectUserMessageToRunningSession(session, content); err != nil {
			s.store.logError(ctx, "[agent] enqueue 注入失败，本条将在下一轮生效", err)
		}
		return nil
	}

	// 未运行：把内容补进对话消息（resumeSession 取最后一条 user 消息作本轮输入，
	// 不补就同样丢失），再异步恢复执行。
	s.store.mu.Lock()
	session.Messages = append(session.Messages, Message{
		Role:      string(enums.ChatRoleUser),
		Content:   content,
		Timestamp: time.Now(),
	})
	s.store.mu.Unlock()
	go s.resumeSession(session)
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
	// 运行中或暂停待续（awaiting_clarify / paused_on_child / awaiting_child）的会话都可取消：
	// 暂停/挂起态没有运行中的 goroutine，但必须允许用户退出暂停死锁/死等场景
	// （实证：paused_on_child 态拒绝取消，会话无任何逃生通道，永久卡死）。
	switch session.Status {
	case enums.SessionStatusRunning, enums.SessionStatusAwaitingClarify, enums.SessionStatusPausedOnChild, enums.SessionStatusAwaitingChild:
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

// SessionResourcePurge 是会话硬删除的运行时资源清扫能力（subagent.Dispatcher 实现）：
// 清软停标记/手动暂停标记/热驻槽（级联销毁+模型覆盖回收）/任务台账/分发计数，
// 并对 nodeIDs（会话内全部节点）做邮箱与活动证据兜底清理。
type SessionResourcePurge interface {
	PurgeSession(sessionID string, nodeIDs []string)
}

// DeleteSession 硬删除会话：终止运行、清扫运行时资源、物理删除全部持久化数据。
//
// 不软删——session_history / session_events / session_logs / agent_events /
// agent_messages / agent_compress_states / agent_tree_nodes 逐表物理删除；
// 块记忆（global_knowledge）是跨会话外脑沉淀，不随会话删除。
// 运行中的会话先取消（cancelFn + stopCtx + 级联取消树节点）再摘除；删除后迟到的
// 收尾落库由 reactSessionStore.stillLive 守卫拦截，不会"复活"。
// 仅存于 PG 的历史会话（重启后未物化）无需运行时清理，直接删库。
// 会话在内存与库中都不存在时返回 ErrSessionNotFound。
func (s *ReactService) DeleteSession(ctx context.Context, sessionID string) error {
	// 1. 内存摘除 + 取消运行（锁内摘除后锁外取消，避免持锁执行回调）。
	s.store.mu.Lock()
	session := s.store.sessions[sessionID]
	var cancelFn, stopCancel context.CancelFunc
	var tempDir, workDir string
	if session != nil {
		cancelFn = session.cancelFn
		session.cancelFn = nil
		stopCancel = session.stopCancel
		session.stopCancel = nil
		if session.stopTimer != nil {
			session.stopTimer.Stop()
			session.stopTimer = nil
		}
		// 先摘除再取消：取消会异步触发收尾 goroutine，摘除先行保证 stillLive 守卫生效。
		delete(s.store.sessions, sessionID)
		tempDir = session.TempDir
		workDir = session.currentWorkDir()
	}
	s.store.mu.Unlock()
	if cancelFn != nil {
		cancelFn()
	}
	if stopCancel != nil {
		stopCancel()
	}

	// 2. 树快照 + 终结：取消全部 Running/Paused/Idle 节点、清内存、删 PG 节点。
	// 树实例保留在 s.trees（不删条目）：迟到的子 Agent 收尾经 TreeFor 拿到空树而非
	// 触发 lazy init 重建，避免竞态下重复加载。
	var nodeIDs []string
	if v, ok := s.trees.Load(sessionID); ok {
		if t, okTree := v.(*orchestrator.Tree); okTree {
			for _, n := range t.Snapshot() {
				nodeIDs = append(nodeIDs, n.ID)
			}
			t.EndCurrentTopic()
		}
	}

	// 3. 运行时清扫：热驻槽销毁（含模型覆盖回收）/软停与暂停标记/台账/分发计数/邮箱。
	if p, ok := s.stopMarker.(SessionResourcePurge); ok {
		p.PurgeSession(sessionID, nodeIDs)
	}
	// 4. 邮箱兜底：meta 自身（agentID==sessionID）与全部节点（幂等）。
	if s.mailbox != nil {
		s.mailbox.Purge(sessionID)
		for _, id := range nodeIDs {
			s.mailbox.Purge(id)
		}
	}
	// 5. 看板移除（写计划会话的执行计划面板真相源）。
	if s.boardRemoveFn != nil {
		s.boardRemoveFn(sessionID)
	}
	// 6. worktree 残留副本清理（未合并副本 best-effort 移除，patch 文件保留审计）。
	if wo, ok := s.stopMarker.(WorktreeOperator); ok {
		wo.CleanupSessionWorktrees(sessionID)
	}

	// 7. 仅存于 PG 的历史会话：取库记录供工作目录解析与存在性判定（内存命中则跳过）。
	// 内存与库（或未配置库）都没有 → 会话不存在。
	var rec *store.SessionHistoryRecord
	if session == nil {
		if s.store.pgStore == nil {
			return ErrSessionNotFound
		}
		rec, _ = s.store.pgStore.GetSessionHistoryByID(ctx, sessionID)
		if rec == nil {
			return ErrSessionNotFound
		}
		workDir = rec.WorkDir
		eff := workDir
		if eff == "" {
			eff = s.workDir()
		}
		tempDir = filepath.Join(eff, ".bma", "tmp", sessionID)
	}

	// 8. 临时目录清理。
	if tempDir != "" {
		s.store.cleanupSessionTempDir(sessionID, tempDir)
	}
	// 9. 话题摘要 KV 清理（key 格式 `topic:{sessionID}:{topicID}:summary`，按会话前缀清扫）。
	if s.sharedMemoryStore != nil {
		kvCtx, cancelKV := context.WithTimeout(tool.WithWorkDir(ctx, workDir), 3*time.Second)
		for _, k := range s.sharedMemoryStore.Keys(kvCtx) {
			if strings.HasPrefix(k, "topic:"+sessionID+":") {
				if err := s.sharedMemoryStore.Delete(kvCtx, k); err != nil {
					s.store.logError(ctx, "delete session: remove topic summary failed", err)
				}
			}
		}
		cancelKV()
	}
	// 10. PG 硬删（单事务，全部会话从属表）。
	if s.store.pgStore != nil {
		if err := s.store.pgStore.DeleteSessionData(ctx, sessionID); err != nil {
			return fmt.Errorf("删除会话数据失败: %w", err)
		}
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
	// T18 选档误判信号②（只落事件不改行为）：集群档 + 本次 run 开始 ≤30s 即被软停
	// ——疑似"集群档太重（等不及）"。数据锁内取，事件锁外落（下方软停止事件旁）。
	gearStopDetail := gearSignalOnStop(session.currentGear(), session.runStartedAt)
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
	// T18 信号②落事件（type=system，「档位信号:」前缀 + detail_json 结构化字段）。
	if gearStopDetail != "" {
		s.store.addEventDetail(session, eventkind.System, "System",
			"档位信号: 集群档会话刚开始（≤30s）即被停止，疑似档位过重",
			"", "", "", "", "", true, gearStopDetail)
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
		WorkDir:        s.currentWorkDir(),
		StreamingText:  s.StreamingText,
		ThinkingText:   s.ThinkingText,
		ActiveBlocks:   []ActiveBlock{},
		PendingClarify: s.pendingClarify,
		DestroyAt:      s.destroyAt,
		ActiveTopicID:  s.activeTopicID,
		TrustMode:      s.currentTrustMode(),
		Gear:           s.currentGear(),
		Thinking:       s.currentThinking(),
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
