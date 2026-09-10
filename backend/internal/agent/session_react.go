// Package agent 维护 ReAct 会话的内存存储、持久化与恢复逻辑，
// 是 ReactService 的后台会话仓库实现。
package agent

// 标准库导入：上下文控制、格式化、日志、文件系统、路径处理、排序、字符串解析、并发与同步、原子操作、时间
import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	// 内部包：模型工厂、事件类型、持久化存储、枚举、文本工具
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/logger"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/project"
	"github.com/blockmemory/agent/backend/internal/server/eventkind"
	"github.com/blockmemory/agent/backend/internal/store"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/textutil"
)

// reactInternalSession 是 ReactService 在内存中维护的会话运行时对象。
// 为了复用事件与持久化逻辑，它有意与 internalSession 保持相似结构。
type reactInternalSession struct {
	// ID 是会话唯一标识，格式为 "session-{自增序号}"。
	ID string
	// Goal 记录用户最初给出的任务目标。
	Goal string
	// Status 表示会话当前状态（运行中/已完成等）。
	Status enums.SessionStatus
	// Result 是任务结束后生成的总结结果。
	Result string
	// StartedAt 记录会话创建时间。
	StartedAt time.Time
	// EndedAt 指向会话结束时间；未完成时为 nil。
	EndedAt *time.Time
	// Events 按时间顺序保存会话生命周期内的内部事件。
	Events []internalEvent
	// Messages 保存用于前端/LLM 对话上下文的聊天消息。
	Messages []Message
	// firstTurnImages 是首条消息（创建会话时）携带的用户图片（Alt+V 粘贴）：
	// 纯内存，runSession 注入 runCtx 后一次性消费置 nil；重启丢失，
	// 历史只剩 [image:N] 占位文本。
	firstTurnImages []tool.ResultImage
	// TempDir 是会话专用的临时工作目录路径。
	TempDir string
	// workDir 是每会话工作目录（绝对路径，空=进程默认 st.workDir）：S2 每会话工作目录，
	// runSession/resumeSession 经 tool.WithWorkDir 注入 runCtx，工具执行按会话解析。
	workDir string
	// History 保存 React 对话历史，用于后续推理与恢复。
	History []ReactMessage
	// StreamingText 保存当前正在流式生成的助手文本（累积值，运行中才有意义），
	// 供 TUI/Web 实时渲染"正在输出"的内容；会话结束时清空。
	StreamingText string
	// ThinkingText 保存当前 LLM 调用思考阶段的过程文本（累积值，瞬时不持久化），
	// 答复文本开始输出或会话结束时清空。
	ThinkingText string
	// lastThinkEventText 是已落事件流的最后一条 think 文本：finalizeThinking 去重依据。
	// 思考流与答复流交错时每个 LLMDelta 都会触发 finalize，累积式思考文本每轮可触发
	// 数十次完全相同的事件（实证：一条"等待回传"叙事在日志重复 40+ 行），
	// 相同文本只落一次。
	lastThinkEventText string
	// ctx 是会话的运行上下文，用于控制生命周期与取消。
	ctx context.Context
	// cancelFn 用于取消 ctx，通常在会话结束或关闭时调用。
	cancelFn context.CancelFunc
	// stopCtx 是会话级中断传播基底（TODO 第10④）：独立于 ctx 从 Background 建，
	// 仅 Stop（软停）/cancel（硬取消）/destroyAfterSoftStop/shutdown 取消；
	// pauseSession（token/轮数触顶）与父正常结束不触碰——暂停可恢复、热驻 Idle 保留复用。
	// 经 tool.WithStopContext 注入 runCtx，dispatchOne/ResumePaused/runDomainTask 派生
	// 子 Agent ctx 以它为基底（缺省回退 Background），stop 窗口期新派发与深层孙代即刻随会话终止。
	stopCtx context.Context
	// stopCancel 取消 stopCtx；与 stopCtx 同建同重建（restartSessionContext）。
	stopCancel context.CancelFunc
	// trustMode 是会话当前信任模式（TODO 第10⑥，suggest|auto-edit|full-auto）：
	// createSession 取 config 默认（st.defaultTrustMode），会话内可经 HTTP/TUI 随时切换。
	// atomic.Value 保证跨 goroutine 读取（ReAct 循环工具派发侧实时读取——切换下一工具调用生效）；
	// 未设置返回空串 → Registry 回退现网生产边界 + 危险命令语义（测试/旧路径兼容）。
	trustMode atomic.Value
	// activeTopicID 当前活跃话题 ID。切换话题时旧 Agent 树终结 + 新树起,
	// 旧话题摘要写入 sharedKV `topic:{sessionID}:{topicID}:summary`。空表示单话题(未切换过)。
	// 话题 ID 也在切换时用于emetries 标签(若需)。
	activeTopicID string
	// recalledTopicID 已注入旧话题摘要的话题 ID。与 activeTopicID 不等时,
	// 下次 MetaAgent 运行前 recallTopicSummaries 注入旧话题摘要并更新此字段,
	// 避免同一话题多次 resume 重复注入(省 token)。
	recalledTopicID string
	// approval 是待审批的破坏性操作确认通道（TODO #17 P1）：非 nil 表示有工具调用
	// 正在等用户确认，sendMessage/answerClarify 把答复写入通道；Agent goroutine 仍存活，
	// 收到答复后工具调用返回裁决继续 ReAct 循环。会话取消时由 ctx 取消解除阻塞。
	approval chan bool
	// askUser 是 ask_user 工具（TODO #24 人在回路）的答复通道：非 nil 表示有 Agent 提问
	// 正在等用户答复；sendMessage/answerClarify 把**原始答复文本**写入通道（不 parseApproval），
	// ask_user 工具结果带回 ReAct 循环。与 approval 互斥（同时只能有一个待答复项）。
	askUser chan string
	// askUserBatch 是 ask_user 批量模式（任务 140）的答复通道：全部题目同屏挂出、
	// 用户统一提交后写入逐题答复文本（下标与题目对齐）。与 approval/askUser 互斥
	//（槽不变式：三者至多一个非 nil）。
	askUserBatch chan []string
	// pendingClarify 是当前待用户答复的确认/澄清请求（含审批问题），随会话快照透出给前端。
	pendingClarify *ClarifyRequest
	// stopTimer 是软停止销毁倒计时定时器（TODO #37）：Stop 后启动，到期硬销毁；
	// 续跑触发时取消。nil=无进行中的倒计时。
	stopTimer *time.Timer
	// destroyAt 是软停止销毁截止时间（倒计时期间非 nil，供 TUI 显示剩余时间）。
	destroyAt *time.Time
}

// maxReactInMemorySessions 限制 ReactService 同时保留在内存中的最大会话数，
// 防止内存无限增长；超出时将淘汰最早完成的会话。
const maxReactInMemorySessions = 20

// interruptedByRestartMsg 运行中会话因进程退出而中断时的统一提示。
// 优雅停机落库与崩溃后恢复两条路径共用同一文案，Web/TUI 发送消息即可续跑。
const interruptedByRestartMsg = "因服务重启中断，可发送消息继续"

// restoredSessionStatus 将 session_history.status 列映射为内存会话状态、结果文本与是否中断。
//   - "running"：进程死亡遗留的运行态 → error + 中断提示（优雅停机会先把 running 标为
//     error 再落库，库里还能读到 running 说明是硬崩溃/断电）；
//   - ""（009 迁移前的旧数据）或无法识别的值：无从判断终态，按 completed + summary 兜底；
//   - awaiting_clarify/paused_on_child/completed/error：状态原样保留，结果取 summary。
//     恢复后 approval/askUser 通道为 nil，sendMessage 对 nil 通道自然回落普通续跑路径。
func restoredSessionStatus(stored, summary string) (status enums.SessionStatus, result string, interrupted bool) {
	switch enums.SessionStatus(stored) {
	case enums.SessionStatusRunning:
		return enums.SessionStatusError, interruptedByRestartMsg, true
	case enums.SessionStatusAwaitingClarify, enums.SessionStatusPausedOnChild,
		enums.SessionStatusCompleted, enums.SessionStatusError:
		return enums.SessionStatus(stored), summary, false
	default:
		return enums.SessionStatusCompleted, summary, false
	}
}

// reactSessionStore 是 ReactService 的内存会话仓库，
// 持有会话映射、并发锁、自增序号以及持久化与指标依赖。
type reactSessionStore struct {
	// mu 保护 sessions 映射，保证并发安全。
	mu sync.RWMutex
	// sessions 是会话 ID 到 reactInternalSession 的内存映射。
	sessions map[string]*reactInternalSession
	// seq 用于生成进程内单调递增的会话序号，使用原子操作避免锁竞争。
	seq atomic.Int64
	// bootEpoch 进程启动 Unix 纳秒，注入 sessionID 前缀保证跨重启唯一。
	// 实证问题：旧实现 sessionID = "session-N"，N 由进程内 seq 自增；重启后回到 1。
	// block-memory 按 meta->>'session_id' 过滤召回旧 session 数据时，新 session-1 命中
	// 旧 session-1 写入的"重写全部 JS"记录，污染 HTML+CSS 任务上下文。
	// bootEpoch + bootRand 双保险：Windows 时钟精度低，连续两次创建 ReactService
	// 可能拿到相同纳秒，追加 4 字节随机确保全局唯一。
	bootEpoch int64
	bootRand  string
	// pgStore 是 PostgreSQL 持久化存储，用于保存历史与事件。
	pgStore *store.PostgresStore
	// modelFactory 提供 LLM 模型实例，供推理时调用。
	modelFactory *model.ModelFactory
	// workDir 是当前工作目录，作为临时目录的根路径。
	workDir string
	// metrics 收集 LLM 调用指标（调用次数、延迟等）。
	metrics *metricsCollector
	// log 是结构化日志器，由 setLogger 注入；nil 时回退标准库 log，保持旧行为。
	log *logger.Logger
	// domainClassifier 是 LLM 领域分区器，供 EnsureProjectDoc 首生成 PROJECT.md 时按职责分区。
	// nil（测试）走启发式依赖图兜底。由 bootstrap 经 ReactService.SetDomainClassifier 注入。
	domainClassifier project.DomainClassifier
	// sharedMemoryReset 在新 session 启动时调用，清理有效工作目录 <dir>/.bma/shared 下
	// 旧 session 残留文件（spec/file_tree 不跨 session 复用），入参为会话有效工作目录。
	// 由 bootstrap 经 ReactService.SetSharedMemoryStore 桥接注入。
	sharedMemoryReset func(dir string)
	// defaultTrustMode 是新建会话的初始信任模式（TODO 第10⑥）：config agent.trust_mode
	// 经 ReactService.SetDefaultTrustMode 注入；空 = 未配置，会话 trustMode 不设置，
	// Registry 回退现网语义。
	defaultTrustMode string
}

// newReactSessionStore 创建一个新的 reactSessionStore 实例。
// workDir 不在此自取：由 bootstrap 经 ReactService.SetWorkDir 注入权威值，
// 消除与 bootstrap.go 各自 os.Getwd 的双源漂移；未注入时为空，createSession 退回相对路径。
func newReactSessionStore() *reactSessionStore {
	// 生成 4 字节随机 hex 作为实例唯一后缀，防止 Windows 低精度时钟导致 bootEpoch 相同。
	bootRand := genBootRand()
	return &reactSessionStore{
		sessions:  make(map[string]*reactInternalSession),
		metrics:   newMetricsCollector(),
		bootEpoch: time.Now().UnixNano(),
		bootRand:  bootRand,
	}
}

// genBootRand 生成 8 字符 hex 随机串作为 sessionID 实例后缀。
// crypto/rand 失败时回退时间戳，保证测试可重复。
func genBootRand() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// setPostgresStore 注入 PostgreSQL 持久化存储。
// pg: 已初始化的 PostgresStore 指针，允许会话历史落库。
func (st *reactSessionStore) setPostgresStore(pg *store.PostgresStore) {
	// 直接赋值，运行时通常在服务启动阶段调用一次。
	st.pgStore = pg
}

// setModelFactory 注入模型工厂，用于后续创建 LLM 客户端。
// mf: 已初始化的 ModelFactory 指针。
func (st *reactSessionStore) setModelFactory(mf *model.ModelFactory) {
	// 保存依赖，供推理时按需获取模型实例。
	st.modelFactory = mf
}

// setLogger 注入结构化日志器，让错误类日志以 [ERRO] 级别输出。
// l: 已初始化的 Logger 指针；未注入时回退标准库 log（级别固定 INFO）。
func (st *reactSessionStore) setLogger(l *logger.Logger) {
	st.log = l
}

// setDomainClassifier 注入 LLM 领域分区器，供 EnsureProjectDoc 首生成按职责分区。
func (st *reactSessionStore) setDomainClassifier(cls project.DomainClassifier) {
	st.domainClassifier = cls
}

// setSharedMemoryReset 注入 session 启动时的 shared 清理闭包（清 <dir>/.bma/shared 旧 session 残留）。
func (st *reactSessionStore) setSharedMemoryReset(fn func(dir string)) {
	st.sharedMemoryReset = fn
}

// logger 返回注入的结构化日志器；未注入时返回 nil。
func (st *reactSessionStore) logger() *logger.Logger {
	return st.log
}

// logInfo 记录信息类日志；未注入 logger 时回退标准库 log。
func (st *reactSessionStore) logInfo(msg string) {
	if st.log != nil {
		st.log.Info(context.Background(), msg)
		return
	}
	log.Print(msg)
}

// logError 记录错误类日志；未注入 logger 时回退标准库 log。
// ctx 供日志器内部异步落库做超时/取消控制；无上下文可用处传 context.Background()。
func (st *reactSessionStore) logError(ctx context.Context, msg string, err error) {
	if st.log != nil {
		st.log.Error(ctx, msg, err)
		return
	}
	log.Printf("%s: %v", msg, err)
}

// findRunningDuplicateSession 返回 goal 完全一致（trim 后）且仍在 running 的既有
// 会话（取最新）。仅匹配 running：awaiting_clarify/paused 会话无法代收新文本，
// 放行新建。供 CreateSession 防重复提交幂等闸使用。
func (st *reactSessionStore) findRunningDuplicateSession(goal string) *reactInternalSession {
	target := strings.TrimSpace(goal)
	if target == "" {
		return nil
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	var newest *reactInternalSession
	for _, sess := range st.sessions {
		if sess.Status != enums.SessionStatusRunning || strings.TrimSpace(sess.Goal) != target {
			continue
		}
		if newest == nil || sess.StartedAt.After(newest.StartedAt) {
			newest = sess
		}
	}
	return newest
}

// createSession 创建一个运行中的 React 会话。
// goal: 用户输入的任务目标字符串。
// workDir: 每会话工作目录（绝对路径），空串回落 st.workDir（进程默认）。
// 返回: 已注册到内存中的新会话指针。
func (st *reactSessionStore) createSession(goal, workDir string) *reactInternalSession {
	// sessionID = "session-<bootEpoch>-<bootRand>-<seq>"：bootEpoch+bootRand 跨重启唯一；
	// seq 进程内单调递增。旧实现 "session-N" 重启后回 1，block-memory 按 session_id
	// 召回旧 session 数据污染新 session。
	sessionID := fmt.Sprintf("session-%d-%s-%d", st.bootEpoch, st.bootRand, st.seq.Add(1))

	// eff 是会话有效工作目录：每会话 workDir 优先，空则回落进程默认 st.workDir。
	eff := workDir
	if eff == "" {
		eff = st.workDir
	}

	// session 初始化基础字段：设置 ID、目标、运行状态、开始时间、
	// 空事件列表以及基于有效工作目录构建的临时目录。
	session := &reactInternalSession{
		ID:        sessionID,
		Goal:      goal,
		Status:    enums.SessionStatusRunning,
		StartedAt: time.Now(),
		Events:    make([]internalEvent, 0),
		TempDir:   filepath.Join(eff, ".bma", "tmp", sessionID),
		workDir:   workDir,
		// Messages 初始化系统提示与用户目标，为后续 LLM 对话提供上下文。
		Messages: []Message{
			{Role: string(enums.ChatRoleSystem), Content: "Goal: " + goal, Timestamp: time.Now()},
			{Role: string(enums.ChatRoleUser), Content: goal, Timestamp: time.Now()},
		},
	}

	// runCtx 与 cancelFn 构成会话生命周期上下文，可外部取消。
	runCtx, cancelFn := context.WithCancel(context.Background())
	session.ctx = runCtx
	session.cancelFn = cancelFn
	// stopCtx 独立于 runCtx 建（TODO 第10④）：runCtx 取消≠中断传播（pause 触顶不传播），
	// 仅显式 stop/cancel/destroy 路径取消 stopCtx。
	stopCtx, stopCancel := context.WithCancel(context.Background())
	session.stopCtx = stopCtx
	session.stopCancel = stopCancel
	// 初始信任模式（TODO 第10⑥）：取 store 默认（config agent.trust_mode）；未配置不设置，
	// Registry 回退现网语义。
	if st.defaultTrustMode != "" {
		session.trustMode.Store(st.defaultTrustMode)
	}

	// 加写锁后将新会话放入内存映射。
	st.mu.Lock()
	st.sessions[sessionID] = session
	st.mu.Unlock()

	// 新 session 启动时清理有效工作目录 .bma/shared 下旧 session 残留文件（spec/file_tree 不跨 session 复用）。
	// 失败静默：仅影响共享记忆初态，旧文件留存由 Layer 3 mtime 校验兜底，不阻断会话。
	if st.sharedMemoryReset != nil {
		st.sharedMemoryReset(eff)
	}

	// 首个 session 启动时确保有效工作目录下存在 .bma/PROJECT.md（缺失则按职责分区生成）。
	// 失败仅记录日志，不阻断会话：PROJECT.md 是辅助上下文，缺失时系统提示词略去项目概览段。
	if err := project.EnsureProjectDoc(context.Background(), eff, st.domainClassifier); err != nil {
		st.logError(context.Background(), "ensure project doc", err)
	}

	// 返回刚创建的会话指针，调用方可立即使用。
	return session
}

// currentTrustMode 返回会话当前信任模式；未设置（测试/未配置）返回空串，
// Registry 据此回退现网生产边界 + 危险命令语义。
// 供 runCtx 注入的读取器闭包在每次工具派发时实时调用——中途切换下一工具调用生效。
func (s *reactInternalSession) currentTrustMode() string {
	if v := s.trustMode.Load(); v != nil {
		if m, ok := v.(string); ok {
			return m
		}
	}
	return ""
}

// setTrustMode 切换会话信任模式（atomic 存储无需额外加锁）。
func (s *reactInternalSession) setTrustMode(mode string) {
	s.trustMode.Store(mode)
}

// getSession 根据会话 ID 获取会话指针。
// id: 会话标识字符串。
// 返回: 若存在返回指针，否则返回 nil。
func (st *reactSessionStore) getSession(id string) *reactInternalSession {
	// 加读锁，允许并发读取。
	st.mu.RLock()
	// 函数返回前释放读锁，避免死锁与资源泄漏。
	defer st.mu.RUnlock()
	// 从映射中取值；若不存在返回 nil。
	return st.sessions[id]
}

// snapshotSession 对给定会话做浅拷贝快照。
// session: 待快照的会话指针。
// 返回: 拷贝后的新指针；若入参为 nil 返回 nil。
func (st *reactSessionStore) snapshotSession(session *reactInternalSession) *reactInternalSession {
	// nil 检查：避免对空指针解引用。
	if session == nil {
		return nil
	}
	// 加读锁，防止快照过程中事件/消息被并发修改。
	st.mu.RLock()
	// 返回前释放读锁。
	defer st.mu.RUnlock()
	// cp 是结构体的浅拷贝；切片字段需要单独深拷贝。
	cp := *session
	// 若 Events 非空则复制切片内容，避免快照受后续追加影响。
	if session.Events != nil {
		cp.Events = make([]internalEvent, len(session.Events))
		copy(cp.Events, session.Events)
	}
	// 若 Messages 非空则复制切片内容。
	if session.Messages != nil {
		cp.Messages = make([]Message, len(session.Messages))
		copy(cp.Messages, session.Messages)
	}
	// 若 History 非空则复制切片内容。
	if session.History != nil {
		cp.History = make([]ReactMessage, len(session.History))
		copy(cp.History, session.History)
	}
	// 返回拷贝后的指针，原始会话仍由仓库持有。
	return &cp
}

// snapshotSessionByID 根据 ID 对会话做快照。
// id: 会话标识字符串。
// 返回: 快照指针；不存在或失败返回 nil。
func (st *reactSessionStore) snapshotSessionByID(id string) *reactInternalSession {
	// 先加读锁检查 ID 是否存在。
	st.mu.RLock()
	session, ok := st.sessions[id]
	// 检查完即释放读锁，避免在 snapshotSession 中重复加锁造成死锁。
	st.mu.RUnlock()
	// 若不存在直接返回 nil。
	if !ok {
		return nil
	}
	// 调用 snapshotSession 执行拷贝，该函数内部会自行加锁。
	return st.snapshotSession(session)
}

// listSessions 返回内存中所有会话的切片，按开始时间降序排列。
// 返回: 会话指针切片，最近开始的排在最前。
func (st *reactSessionStore) listSessions() []*reactInternalSession {
	// 加读锁遍历映射。
	st.mu.RLock()
	// 返回前释放读锁。
	defer st.mu.RUnlock()
	// 预分配容量，避免多次扩容。
	result := make([]*reactInternalSession, 0, len(st.sessions))
	// 遍历所有会话并追加到结果切片。
	for _, s := range st.sessions {
		result = append(result, s)
	}
	// 按 StartedAt 降序排序，让最新会话在前。
	sort.Slice(result, func(i, j int) bool {
		return result[i].StartedAt.After(result[j].StartedAt)
	})
	// 返回排序后的切片。
	return result
}

// sessionCount 返回当前内存中的会话总数。
// 返回: 会话数量。
func (st *reactSessionStore) sessionCount() int {
	// 加读锁保证并发安全。
	st.mu.RLock()
	// 返回前释放读锁。
	defer st.mu.RUnlock()
	// 返回映射长度。
	return len(st.sessions)
}

// sessionIDs 返回当前内存中全部会话 ID（shutdown 遍历清理等场景用）。
func (st *reactSessionStore) sessionIDs() []string {
	st.mu.RLock()
	defer st.mu.RUnlock()
	ids := make([]string, 0, len(st.sessions))
	for id := range st.sessions {
		ids = append(ids, id)
	}
	return ids
}

// shutdown 优雅停机：先把 Running 会话标记为"因服务重启中断"（error + 统一提示），
// 再取消全部会话上下文，最后在锁外对被中断的会话落库（历史/事件 + 中断事件）。
// 标记先行：setSessionError 的"首次错误胜出"守卫（Status==Error 且 Result!="" 即跳过）
// 使随后 ctx 取消触发的 "context canceled" 错误不会覆盖中断提示。
// addEvent/persistHistory/persistEvents 自身要加锁，不得持写锁调用（RWMutex 不可重入）。
func (st *reactSessionStore) shutdown() {
	// 加写锁：取消上下文并就地标记中断状态。
	st.mu.Lock()
	now := time.Now()
	var interrupted []*reactInternalSession
	for _, s := range st.sessions {
		// 先取消运行上下文，通知依赖 ctx 的 goroutine 尽快退出；置空避免二次取消。
		if s.cancelFn != nil {
			s.cancelFn()
			s.cancelFn = nil
		}
		// 中断传播（TODO 第10④）：stopCtx 同步取消（同硬取消语义，进程退出全树终止）。
		if s.stopCancel != nil {
			s.stopCancel()
			s.stopCancel = nil
		}
		// Running 会话标记为中断：重启后列表显示"因服务重启中断"而非静止的 running。
		if s.Status == enums.SessionStatusRunning {
			s.Status = enums.SessionStatusError
			s.Result = interruptedByRestartMsg
			s.EndedAt = &now
			interrupted = append(interrupted, s)
		}
	}
	st.mu.Unlock()
	// 锁外落库：History 为上一轮完成时的快照，进行中轮次尽力而为（真实终态若在
	// 停机瞬间完成并落库，会覆盖中断标记，属更准确的结果）。
	for _, s := range interrupted {
		st.addEvent(s, eventkind.System, "System", interruptedByRestartMsg, "", "", "", "", "", false)
		st.persistHistory(s)
		st.persistEvents(s)
	}
}

// addEvent 是会话事件记录的便捷入口，内部转发到 addEventDebug 的简化版本。
// session: 目标会话；eventType/agentName/message/kind/tool/toolPath/toolOutput/toolError/success: 事件各字段。
func (st *reactSessionStore) addEvent(session *reactInternalSession, eventType, agentName, message, kind, tool, toolPath, toolOutput, toolError string, success bool) {
	// 直接调用完整版，prompt、token、detailJSON 使用零值。
	st.addEventDebug(session, eventType, agentName, message, kind, tool, toolPath, toolOutput, toolError, success, "", 0, 0, 0, 0, "")
}

// addEventDetail 与 addEvent 相同但携带 detailJSON（如 {"agent_id":"..."} 实例归属），
// 供 TUI 领域进度面板按 Agent 实例匹配事件。
func (st *reactSessionStore) addEventDetail(session *reactInternalSession, eventType, agentName, message, kind, tool, toolPath, toolOutput, toolError string, success bool, detailJSON string) {
	st.addEventDebug(session, eventType, agentName, message, kind, tool, toolPath, toolOutput, toolError, success, "", 0, 0, 0, 0, detailJSON)
}

// setStreamingText 更新会话当前正在流式生成的累积文本；传空串表示流式结束。
func (st *reactSessionStore) setStreamingText(session *reactInternalSession, text string) {
	st.mu.Lock()
	session.StreamingText = text
	st.mu.Unlock()
}

// setThinkingText 更新会话当前思考阶段的累积文本；传空串表示思考阶段结束。
func (st *reactSessionStore) setThinkingText(session *reactInternalSession, text string) {
	st.mu.Lock()
	session.ThinkingText = text
	st.mu.Unlock()
}

// addEventDebug 向会话追加一条带调试信息的事件，并同步打印日志。
// session: 目标会话；eventType/agentName/message/kind/tool/toolPath/toolOutput/toolError/success: 事件字段；
// prompt: 原始提示词；inputTokens/outputTokens: token 用量；cacheHit/cacheMiss: 缓存命中/未命中
// token（TODO #40 可观测）；detailJSON: 额外调试 JSON。
func (st *reactSessionStore) addEventDebug(session *reactInternalSession, eventType, agentName, message, kind, tool, toolPath, toolOutput, toolError string, success bool, prompt string, inputTokens, outputTokens, cacheHit, cacheMiss int, detailJSON string) {
	// 工具输出不在事件层截断（上限由工具执行层 ReadFileMaxChars/RunCommandMaxOutput 兜底），
	// Web 详情需展示全量，TUI 在展示侧自行压缩（helpers.truncateToolOutput）。
	// 组装内部事件结构体，填充所有字段。
	ev := internalEvent{
		Type:            eventType,
		Agent:           agentName,
		Message:         message,
		Kind:            kind,
		Tool:            tool,
		ToolPath:        toolPath,
		ToolOutput:      toolOutput,
		ToolError:       toolError,
		Success:         success,
		Timestamp:       time.Now(),
		Prompt:          prompt,
		InputTokens:     inputTokens,
		OutputTokens:    outputTokens,
		CacheHitTokens:  cacheHit,
		CacheMissTokens: cacheMiss,
		DetailJSON:      detailJSON,
	}
	// 加写锁后追加事件，保证并发安全。
	st.mu.Lock()
	session.Events = append(session.Events, ev)
	// 若事件数超过 500，则裁剪到 200，避免内存无限增长。
	if len(session.Events) > 500 {
		session.Events = trimDebugEvents(session.Events, 200)
	}
	st.mu.Unlock()

	// 在标准日志中打印事件摘要，便于实时排查问题。
	st.logInfo(fmt.Sprintf("[%s] %s: %s", session.ID, agentName, message))
}

// cleanupSessionTempDir 清理指定会话的临时目录。
// sessionID: 会话标识，用于日志；tempDir: 待清理目录路径。
func (st *reactSessionStore) cleanupSessionTempDir(sessionID, tempDir string) {
	// 空路径无需处理，直接返回。
	if tempDir == "" {
		return
	}
	// 若目录已不存在，无需清理，直接返回。
	if _, err := os.Stat(tempDir); os.IsNotExist(err) {
		return
	}
	// 递归删除临时目录；出错时记录错误日志。
	if err := os.RemoveAll(tempDir); err != nil {
		st.logError(context.Background(), fmt.Sprintf("[%s] 清理临时目录失败", sessionID), err)
	} else {
		// 删除成功记录信息日志。
		st.logInfo(fmt.Sprintf("[%s] 已清理临时目录: %s", sessionID, tempDir))
	}
}

// evictCompletedSessions 淘汰最早完成的会话，使内存会话数不超过上限。
// 只有状态非运行中且已设置 EndedAt 的会话才会被淘汰。
func (st *reactSessionStore) evictCompletedSessions() {
	// 加写锁，因为可能删除映射中的会话。
	st.mu.Lock()
	// 返回前释放写锁。
	defer st.mu.Unlock()
	// 若当前数量未超过上限，无需淘汰，直接返回。
	if len(st.sessions) <= maxReactInMemorySessions {
		return
	}
	// kv 用于临时保存已完成会话的 ID 与结束时间。
	type kv struct {
		id    string    // id 会话唯一标识
		ended time.Time // ended 会话结束时间，用于排序决定淘汰顺序
	}
	// completed 收集所有可被驱逐的会话。
	var completed []kv
	for id, s := range st.sessions {
		// 跳过仍在运行或未设置结束时间的会话。
		if s.Status == enums.SessionStatusRunning || s.EndedAt == nil {
			continue
		}
		// 记录该会话的结束时间，用于后续排序。
		completed = append(completed, kv{id, *s.EndedAt})
	}
	// 按结束时间升序排序，最早的在前，优先被淘汰。
	sort.Slice(completed, func(i, j int) bool {
		return completed[i].ended.Before(completed[j].ended)
	})
	// excess 计算需要淘汰的数量。
	excess := len(st.sessions) - maxReactInMemorySessions
	// dropped 统计实际淘汰数量。
	dropped := 0
	// 从头开始删除已完成会话，直到满足上限或没有可淘汰对象。
	for i := 0; i < len(completed) && dropped < excess; i++ {
		delete(st.sessions, completed[i].id)
		dropped++
	}
	// 若有淘汰，打印日志说明保留数量。
	if dropped > 0 {
		st.logInfo(fmt.Sprintf("从内存淘汰了 %d 个已完成会话 (保留 %d)", dropped, len(st.sessions)))
	}
}

// snapshotSessionEvents 在读锁内拷贝会话事件切片。
// persistHistory/persistEvents 与并发的 addEvent（子 Agent 事件来自其他 goroutine）
// 及优雅停机落库（turn goroutine 收尾中）并存，必须经锁同步读取，避免数据竞争。
func (st *reactSessionStore) snapshotSessionEvents(session *reactInternalSession) []internalEvent {
	st.mu.RLock()
	events := make([]internalEvent, len(session.Events))
	copy(events, session.Events)
	st.mu.RUnlock()
	return events
}

// persistHistory 将对话历史持久化到 PostgreSQL。
// session: 待保存的会话。
func (st *reactSessionStore) persistHistory(session *reactInternalSession) {
	// 若未配置 pgStore，直接返回，避免空指针。
	if st.pgStore == nil {
		return
	}
	// 锁内快照事件，避免与并发 addEvent / 停机落库竞争。
	events := st.snapshotSessionEvents(session)
	// toolResults 汇总所有工具执行事件的结果，用于历史记录。
	toolResults := make([]map[string]any, 0, len(events))
	// 遍历事件，仅保留工具执行类型的事件。
	for _, ev := range events {
		// 跳过非工具执行事件。
		if ev.Type != eventkind.ToolExec {
			continue
		}
		// 将工具名、路径、输出、错误、成功标志加入结果集，并对输出截断。
		// 文本字段统一过 sanitizeUTF8：Postgres jsonb 同样拒绝 NUL（），
		// 工具输出夹带 0x00 会导致整条历史写入失败。
		toolResults = append(toolResults, map[string]any{
			"tool":   ev.Tool,
			"path":   sanitizeUTF8(ev.ToolPath),
			"output": sanitizeUTF8(textutil.TruncateRunes(ev.ToolOutput, 500, "...(truncated)")),
			"error":  sanitizeUTF8(ev.ToolError),
			"ok":     ev.Success,
		})
	}
	// rec 组装为数据库存储所需的历史记录结构。
	rec := &store.SessionHistoryRecord{
		SessionID:   session.ID,
		Goal:        sanitizeUTF8(session.Goal),
		Summary:     sanitizeUTF8(session.Result),
		ToolResults: toolResults,
		MetaMemory:  []map[string]any{},
		CreatedAt:   time.Now(),
		WorkDir:     session.workDir,
		// 009: 记录落库时的会话状态。轮开始时为 running（崩溃后被恢复逻辑标记中断），
		// 轮结束时为 completed/error/paused 等终态。
		Status: string(session.Status),
	}
	// 使用 3 秒超时上下文，避免数据库挂起导致无限等待。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	// 函数返回前取消上下文，释放资源。
	defer cancel()
	// 调用持久化接口；失败仅记录日志，不中断业务流程。
	if err := st.pgStore.SaveSessionHistory(ctx, rec); err != nil {
		st.logError(ctx, fmt.Sprintf("[%s] 持久化会话历史失败", session.ID), err)
	}
	// 同步持久化主对话完整消息历史（重启恢复上下文用，见 persistFullHistory）。
	st.persistFullHistory(session)
}

// persistFullHistory 把主对话（MetaAgent，agentID==sessionID）的完整 ReAct 消息历史
// 落 agent_messages 表：进程重启后 restoreSessions 经 LoadMessages 重建 session.History，
// 修复"重启即失忆"（此前只存 goal + 最终结果各截 500 字符，Messages 只重建 2 条）。
// 复用 Paused DomainAgent 的持久化通道（SaveMessages 为 delete-then-insert 全量覆盖语义，
// 支持反复重写）；子 Agent 的持久化由 dispatcher 在 pause 时单独负责，这里只覆盖主对话。
// 每轮结束调用一次（完成/暂停/出错路径均经 persistHistory 至此）。
func (st *reactSessionStore) persistFullHistory(session *reactInternalSession) {
	// 若未配置 pgStore，直接返回，避免空指针。
	if st.pgStore == nil {
		return
	}
	// 拷贝历史，避免持久化期间被并发修改。
	st.mu.RLock()
	history := make([]ReactMessage, len(session.History))
	copy(history, session.History)
	st.mu.RUnlock()
	if len(history) == 0 {
		return
	}
	// 历史较长时 delete-then-insert 略慢，给 10 秒超时；失败仅记录日志。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := NewPostgresMessagesStore(st.pgStore.DB()).SaveMessages(ctx, session.ID, session.ID, history); err != nil {
		st.logError(ctx, fmt.Sprintf("[%s] 持久化完整对话历史失败", session.ID), err)
	}
}

// persistEvents 将当前会话的所有事件批量持久化到 PostgreSQL。
// session: 待保存的会话。
func (st *reactSessionStore) persistEvents(session *reactInternalSession) {
	// 若未配置 pgStore，直接返回。
	if st.pgStore == nil {
		return
	}
	// 锁内快照事件，避免与并发 addEvent / 停机落库竞争。
	events := st.snapshotSessionEvents(session)
	// records 预分配容量，避免多次扩容。
	records := make([]store.SessionEventRecord, 0, len(events))
	// 遍历事件并转换为数据库记录格式，同时对文本字段做 UTF-8 清理与截断。
	for _, ev := range events {
		records = append(records, store.SessionEventRecord{
			SessionID:    session.ID,
			Type:         ev.Type,
			Agent:        ev.Agent,
			Message:      sanitizeUTF8(ev.Message),
			Kind:         ev.Kind,
			Tool:         ev.Tool,
			ToolPath:     sanitizeUTF8(ev.ToolPath),
			ToolOutput:   sanitizeUTF8(ev.ToolOutput),
			ToolError:    sanitizeUTF8(ev.ToolError),
			Success:      ev.Success,
			Timestamp:    ev.Timestamp,
			Prompt:       sanitizeUTF8(textutil.TruncateRunes(ev.Prompt, 2048, "...(truncated)")),
			InputTokens:  ev.InputTokens,
			OutputTokens: ev.OutputTokens,
			DetailJSON:   sanitizeUTF8(ev.DetailJSON),
		})
	}
	// 使用 5 秒超时上下文保存事件。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	// 返回前取消上下文。
	defer cancel()
	// 批量保存；失败仅记录日志。
	if err := st.pgStore.SaveSessionEvents(ctx, session.ID, records); err != nil {
		st.logError(ctx, fmt.Sprintf("[%s] 持久化会话事件失败", session.ID), err)
	}
}

// loadSessionEvents 从 PostgreSQL 加载指定会话的事件。
// ctx: 请求上下文；sessionID: 会话标识。
// 返回: 按数据库顺序加载的内部事件切片。
func (st *reactSessionStore) loadSessionEvents(ctx context.Context, sessionID string) []internalEvent {
	// 若未配置 pgStore，返回空切片。
	if st.pgStore == nil {
		return make([]internalEvent, 0)
	}
	// 从数据库读取事件记录；忽略错误，避免恢复流程中断。
	records, _ := st.pgStore.GetSessionEvents(ctx, sessionID)
	// events 预分配容量。
	events := make([]internalEvent, 0, len(records))
	// 将数据库记录转换回内部事件结构。
	for _, r := range records {
		events = append(events, internalEvent{
			Type:         r.Type,
			Agent:        r.Agent,
			Message:      r.Message,
			Kind:         r.Kind,
			Tool:         r.Tool,
			ToolPath:     r.ToolPath,
			ToolOutput:   r.ToolOutput,
			ToolError:    r.ToolError,
			Success:      r.Success,
			Timestamp:    r.Timestamp,
			Prompt:       r.Prompt,
			InputTokens:  r.InputTokens,
			OutputTokens: r.OutputTokens,
			DetailJSON:   r.DetailJSON,
		})
	}
	// 返回加载的事件切片。
	return events
}

// buildRestoredSession 从一条 session_history 记录重建内存会话（不触碰 store 锁与映射）。
// events 自 session_events 全量加载；History 自 agent_messages 重建（persistFullHistory
// 每轮落库），续跑时 resumeSession 以其为种子，MetaAgent 上下文不再清零；更早的上下文
// 由 memory.Pipeline 懒加载压缩金字塔（agent_compress_states）接续。失败/无数据时
// History 保持 nil，退回旧行为（仅 goal + 总结两条可读消息）。
// 不设置 ctx/cancelFn：会话非 Running，sendMessageFull/resume 路径会 restartSessionContext。
// ctx: 加载事件的请求上下文；rec: 历史记录。
func (st *reactSessionStore) buildRestoredSession(ctx context.Context, rec *store.SessionHistoryRecord) *reactInternalSession {
	// 加载该会话关联的详细事件。
	restoredEvents := st.loadSessionEvents(ctx, rec.SessionID)
	// 状态映射：running（进程死亡遗留）→ error + 中断提示，并补一条合成系统事件说明原因，
	// 使时间线在 Web/TUI 上可见中断原因。
	status, result, interrupted := restoredSessionStatus(rec.Status, rec.Summary)
	if interrupted {
		restoredEvents = append(restoredEvents, internalEvent{
			Type:      eventkind.System,
			Agent:     "System",
			Message:   interruptedByRestartMsg,
			Success:   false,
			Timestamp: rec.CreatedAt,
		})
	}
	// endedAt 使用历史记录的创建时间作为会话结束时间。
	endedAt := rec.CreatedAt
	// eff 是会话有效工作目录：每会话 workDir 优先，空则回落进程默认 st.workDir（同 createSession）。
	eff := rec.WorkDir
	if eff == "" {
		eff = st.workDir
	}
	sess := &reactInternalSession{
		ID:        rec.SessionID,
		Goal:      rec.Goal,
		Status:    status,
		Result:    result,
		StartedAt: rec.CreatedAt,
		EndedAt:   &endedAt,
		Events:    restoredEvents,
		// S2: 恢复每会话工作目录，重启后续跑仍在原目录；空串回落进程默认。
		workDir: rec.WorkDir,
		// TempDir 与 createSession 同款规则计算（createSession 也从不预建目录，
		// cleanupSessionTempDir 对不存在路径幂等）。
		TempDir: filepath.Join(eff, ".bma", "tmp", rec.SessionID),
		// Messages 重建为用户目标与助手总结，保持对话上下文可读。
		Messages: []Message{
			{Role: string(enums.ChatRoleUser), Content: rec.Goal, Timestamp: rec.CreatedAt},
			{Role: string(enums.ChatRoleAssistant), Content: rec.Summary, Timestamp: rec.CreatedAt},
		},
	}
	// 重建完整对话历史（若重启前经 persistFullHistory 持久化过）。
	if msgs, err := NewPostgresMessagesStore(st.pgStore.DB()).LoadMessages(ctx, rec.SessionID); err == nil && len(msgs) > 0 {
		sess.History = msgs
	}
	return sess
}

// restoreOneSession 按需从 PG 恢复单个会话到内存（懒恢复）。
// 用于重启后未随 restoreSessions 批量恢复（超出 limit / restore_sessions 关闭）的旧会话：
// Get/Stream/Send 命中未知 ID 时物化该会话，实现"任意旧会话可查看可续聊"。
// 已在内存直接返回；PG 无此会话或未配置 pgStore 返回 nil（调用方回落 ErrSessionNotFound）。
// 插入幂等（并发双恢复先到者胜，后到者丢弃已构建副本）；插入后调用
// evictCompletedSessions 防止浏览大量旧会话撑爆内存上限。
// ctx: 加载事件的请求上下文；id: 会话 ID。
func (st *reactSessionStore) restoreOneSession(ctx context.Context, id string) *reactInternalSession {
	// 快路径：读锁查内存，命中直接返回。
	st.mu.RLock()
	sess, ok := st.sessions[id]
	st.mu.RUnlock()
	if ok {
		return sess
	}
	// 未配置 pgStore 无法恢复。
	if st.pgStore == nil {
		return nil
	}
	// DB 查询与构建在锁外完成，3 秒超时防止存储故障拖住调用方。
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	rec, err := st.pgStore.GetSessionHistoryByID(ctx, id)
	if err != nil || rec == nil {
		return nil
	}
	built := st.buildRestoredSession(ctx, rec)
	// 写锁内二次判重后插入，避免与批量恢复/并发懒恢复互相覆盖。
	st.mu.Lock()
	if existing, ok := st.sessions[id]; ok {
		st.mu.Unlock()
		return existing
	}
	st.sessions[id] = built
	st.mu.Unlock()
	// 物化的会话均带 EndedAt（可淘汰），立即触发一次淘汰防止内存膨胀。
	st.evictCompletedSessions()
	return built
}

// restoreSessions 从 PostgreSQL 恢复近期会话到内存（启动时批量预热）。
// ctx: 请求上下文；limit: 最大恢复数量，<=0 时默认 50。
// 返回: 实际恢复的会话数量。
func (st *reactSessionStore) restoreSessions(ctx context.Context, limit int) int {
	// 若未配置 pgStore，无法恢复，返回 0。
	if st.pgStore == nil {
		return 0
	}
	// 规范化 limit，避免无效值。
	if limit <= 0 {
		limit = 50
	}
	// 拉取最近的历史记录。
	recs, err := st.pgStore.RecentSessionHistories(ctx, limit)
	// 若查询失败，记录日志并返回 0。
	if err != nil {
		st.logError(ctx, "恢复会话失败", err)
		return 0
	}

	// 批内去重 + 过滤已存在会话。迁移前旧库 session_history 无唯一约束，
	// RecentHistories（created_at DESC）可能返回同 ID 重复行，首行即最新写入，保留首行。
	seen := make(map[string]struct{}, len(recs))
	pending := make([]*store.SessionHistoryRecord, 0, len(recs))
	st.mu.RLock()
	for _, rec := range recs {
		if _, dup := seen[rec.SessionID]; dup {
			continue
		}
		seen[rec.SessionID] = struct{}{}
		// 已在内存的会话跳过，避免覆盖当前运行状态。
		if _, exists := st.sessions[rec.SessionID]; exists {
			continue
		}
		pending = append(pending, rec)
	}
	st.mu.RUnlock()

	// 逐条重建（DB I/O 在锁外执行：加载事件 + 重建 History 可达数十毫秒/条）。
	built := make([]*reactInternalSession, 0, len(pending))
	for _, rec := range pending {
		built = append(built, st.buildRestoredSession(ctx, rec))
	}

	// 加写锁，向内存映射写入恢复的会话。
	restored := 0
	st.mu.Lock()
	for _, sess := range built {
		// 双保险：构建期间同 ID 被占则丢弃副本（理论不可达，ID 含 bootRand）。
		if _, exists := st.sessions[sess.ID]; exists {
			continue
		}
		st.sessions[sess.ID] = sess
		restored++
	}
	st.mu.Unlock()

	// maxSeq 用于恢复后同步自增序号，避免新会话 ID 与历史 ID 冲突。
	// 新格式 sessionID = "session-<bootEpoch>-<bootRand>-<seq>"，解析取最后段 seq；
	// 旧格式 "session-N" 解析整段为 N。取两者最大值确保新 seq 更大。
	var maxSeq int64
	// 遍历历史记录，解析 ID 并找出最大序号。
	for _, rec := range recs {
		id := rec.SessionID
		if !strings.HasPrefix(id, "session-") {
			continue
		}
		rest := strings.TrimPrefix(id, "session-")
		// 新格式含多段 "-"：取最后一段作为 seq。
		if idx := strings.LastIndex(rest, "-"); idx >= 0 {
			rest = rest[idx+1:]
		}
		if n, err := strconv.ParseInt(rest, 10, 64); err == nil && n > maxSeq {
			maxSeq = n
		}
	}
	// 若解析到有效序号，将其写入原子变量，确保后续 createSession 生成的 ID 更大。
	if maxSeq > 0 {
		st.seq.Store(maxSeq)
	}

	// 若有恢复，打印日志说明数量。
	if restored > 0 {
		st.logInfo(fmt.Sprintf("从历史恢复了 %d 个会话", restored))
	}
	// 恢复数量可能超过内存上限（默认 50 > 20），按结束时间淘汰最早的完成会话。
	st.evictCompletedSessions()
	// 返回实际恢复数量。
	return restored
}

// clearSessionChat 清空指定会话的聊天上下文与事件，但保留系统提示和用户初始目标。
// id: 会话标识。
// 返回: 若会话存在并清理成功返回 true，否则 false。
func (st *reactSessionStore) clearSessionChat(id string) bool {
	// 加写锁，因为会修改会话的消息与事件。
	st.mu.Lock()
	// 返回前释放写锁。
	defer st.mu.Unlock()
	// 查找会话；不存在返回 false。
	session, ok := st.sessions[id]
	if !ok {
		return false
	}
	// keep 用于保留系统提示和用户初始目标，容量最多 2 条。
	keep := make([]Message, 0, 2)
	// 遍历现有消息，按角色筛选保留。
	for _, msg := range session.Messages {
		// 仅保留系统消息与用户消息。
		if msg.Role == string(enums.ChatRoleSystem) || msg.Role == string(enums.ChatRoleUser) {
			keep = append(keep, msg)
			// 收集到 2 条后提前结束，避免保留多余消息。
			if len(keep) >= 2 {
				break
			}
		}
	}
	// 用保留的消息替换原聊天上下文。
	session.Messages = keep
	// 清空事件列表，释放历史事件占用的内存。
	session.Events = make([]internalEvent, 0)
	// 清空 React 历史记录。
	session.History = nil
	// 返回 true 表示清理完成。
	return true
}

// llmStats 返回 LLM 调用统计指标。
// 返回: callCount 调用次数、timeoutCount 超时次数、avgDur 平均耗时、maxDur 最大耗时。
func (st *reactSessionStore) llmStats() (callCount, timeoutCount int, avgDur, maxDur time.Duration) {
	// 获取指标收集器的快照，避免直接读取内部并发状态。
	snap := st.metrics.snapshot()
	// 返回快照中的各项指标，并将毫秒转换为 time.Duration。
	return snap.CallCount,
		snap.TimeoutCount,
		time.Duration(snap.AvgDurationMs) * time.Millisecond,
		time.Duration(snap.MaxDurationMs) * time.Millisecond
}

// queryLogs 查询会话的 session_logs（含完整 LLM I/O）。
// 直接代理到 pgStore.QuerySessionLogs；pgStore 缺失时返回空切片。
func (st *reactSessionStore) queryLogs(ctx context.Context, sessionID, agent, level string, limit, offset int) []*store.SessionLogRecord {
	if st.pgStore == nil {
		return nil
	}
	recs, err := st.pgStore.QuerySessionLogs(ctx, sessionID, agent, level, limit, offset)
	if err != nil {
		st.logError(ctx, fmt.Sprintf("[%s] 查询 session_logs 失败", sessionID), err)
		return nil
	}
	return recs
}
