// Package orchestrator 提供 Agent 树编排元数据层。
//
// Dispatcher 在派发子 Agent 时调用 Register/SetCancel/Finish 维护权威树，
// HTTP API 与 TUI 通过 Snapshot 读取，Cancel 端点通过存储的 cancel func 取消子 Agent。
// 不参与 ReAct 循环控制流，仅作可观测/可取消的元数据层。
//
// 持久化：Tree 可选注入 TreeStore,Register/Finish/Cancel 后 best-effort 写入 PG。
// 服务重启后 TreeFor 调 LoadNodes 恢复节点元数据（cancel func 无法恢复,已运行中的
// 子 Agent 重启后视为失联,需外部清理）。store 为 nil 时纯内存,与原行为一致。
package orchestrator

import (
	"context"
	"log"
	"sync"
	"time"
)

// TreeStore 抽象 Agent 树节点的持久化能力。
// 实现者负责把节点保存到磁盘或数据库,并按 sessionID 加载全部节点。
// SaveNode 应为 upsert 语义(同 sessionID+nodeID 覆盖)。
// LoadNodes 返回空切片表示 session 无持久化节点(新会话或首次访问)。
// DeleteNodesBySession 清空 session 的全部节点,用于话题切换时终结旧树。
type TreeStore interface {
	SaveNode(ctx context.Context, sessionID string, node Node) error
	LoadNodes(ctx context.Context, sessionID string) ([]Node, error)
	DeleteNodesBySession(ctx context.Context, sessionID string) error
}

// Status 表示 Agent 节点在树中的生命周期状态。
type Status int

const (
	StatusRunning Status = iota
	StatusDone
	StatusFailed
	StatusCancelled
	// StatusPaused 标记 DomainAgent 触达 token 上限暂停(可恢复)。
	// 仅 Running 节点可 Pause;resume 完成后 Finish 转 Done。
	// 与 Failed/Cancelled 区分:Paused 保留可恢复语义,history 持久化在 agent_messages。
	StatusPaused
	// StatusIdle 标记 DomainAgent 任务完结转入热驻留(可复用)态。
	// 仅 Running 节点可 Idle;复用派发时 Wake 置回 Running。
	// 与 Paused 区分:Paused=任务中断待续(等"继续"),Idle=任务完结待复用。
	// Idle 非终态:可 Cancel(硬取消/话题切换)、可 Finish(TTL 到期销毁)、可 Wake(复用)。
	StatusIdle
	// StatusUnverified 标记"已交付但未验证"（三态化）：
	// 产出已回传（正文含结果），但缺少机器可执行的验证证据（L0 证据缺/校验不可用），
	// 非失败语义——看板标黄不标红，由父 Agent 决定补验证或收口。
	// 与 Failed 区分:Failed=真失败(冒烟不过/超时/守卫终止),Unverified=产出可用但没证据。
	StatusUnverified
)

// String 返回状态的可读名称，用于日志与 JSON 序列化。
func (s Status) String() string {
	switch s {
	case StatusRunning:
		return "running"
	case StatusDone:
		return "done"
	case StatusFailed:
		return "failed"
	case StatusCancelled:
		return "cancelled"
	case StatusPaused:
		return "paused"
	case StatusIdle:
		return "idle"
	case StatusUnverified:
		return "delivered-unverified"
	}
	return "unknown"
}

// MarshalJSON 实现 json.Marshaler，使 Status 序列化为字符串而非整数。
func (s Status) MarshalJSON() ([]byte, error) {
	return []byte(`"` + s.String() + `"`), nil
}

// Node 是 Agent 树中的一个节点快照（值类型，外部不可变）。
type Node struct {
	ID       string    `json:"id"`
	ParentID string    `json:"parent_id,omitempty"`
	Role     string    `json:"role"`
	Domain   string    `json:"domain,omitempty"`
	Task     string    `json:"task,omitempty"`
	Status   Status    `json:"status"`
	Started  time.Time `json:"started"`
	Finished  time.Time `json:"finished,omitzero"`
	Summary  string    `json:"summary,omitempty"`
	Err      string    `json:"err,omitempty"`
}

// Tree 维护单个会话的 Agent 树权威状态。
// nodes 存储节点元数据，cancels 存储对应 subAgentID 的 cancel func。
// 两个 map 同步生命周期：Register/Finish/Cancel 都先持锁再操作。
//
// store 与 sessionID 用于持久化:store 非 nil 时,Register/Finish/Cancel 后
// best-effort 调 SaveNode 写入 PG;TreeFor lazy init 时调 LoadNodes 恢复节点。
// store 为 nil 时纯内存,与原行为一致(测试场景)。
type Tree struct {
	mu       sync.RWMutex
	nodes    map[string]*Node
	cancels  map[string]context.CancelFunc
	store    TreeStore
	sessionID string
}

// NewTree 构造空树。store 可为 nil(纯内存,测试场景)。
// sessionID 用于持久化时标识会话归属;store 为 nil 时 sessionID 可为空。
func NewTree(sessionID string, store TreeStore) *Tree {
	return &Tree{
		nodes:    make(map[string]*Node),
		cancels:  make(map[string]context.CancelFunc),
		store:    store,
		sessionID: sessionID,
	}
}

// persistNode best-effort 写入持久化层。失败仅记日志,不影响主流程。
// 2 秒超时防止 DB 阻塞 ReAct 循环。store 为 nil 时 no-op。
func (t *Tree) persistNode(node Node) {
	if t.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := t.store.SaveNode(ctx, t.sessionID, node); err != nil {
		log.Printf("[orchestrator] persist node failed: session=%s node=%s err=%v", t.sessionID, node.ID, err)
	}
}

// Register 写入新节点。若同 ID 已存在则覆盖（派发竞态保护）。
// 不设置 cancels 条目；SetCancel 单独调用以解耦 Register 与 ctx 创建时机。
// 持久化:store 非 nil 时 best-effort 写入,失败仅记日志。
func (t *Tree) Register(n Node) {
	t.mu.Lock()
	if n.Started.IsZero() {
		n.Started = time.Now()
	}
	if n.Status == 0 {
		n.Status = StatusRunning
	}
	node := n
	t.nodes[node.ID] = &node
	t.mu.Unlock()
	t.persistNode(node)
}

// SetCancel 绑定 subAgentID 对应的 cancel func。
// 在 goroutine 启动前调用，保证 Cancel 端点不会因时序漏掉 cancel 句柄。
// 若节点不存在则忽略（Register 未调用的边缘场景）。
func (t *Tree) SetCancel(id string, cancel context.CancelFunc) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.nodes[id]; !ok {
		return
	}
	t.cancels[id] = cancel
}

// Finish 标记节点结束。err 非 nil 则 StatusFailed，否则 StatusDone。
// summary 为最终文本或部分结果。删除 cancels[id] 防止 map 无界增长。
// 幂等：已 terminal 的节点重复调用 no-op。
// 持久化:store 非 nil 时 best-effort 更新节点状态。
func (t *Tree) Finish(id, summary string, err error) {
	if err != nil {
		t.finishWithStatus(id, summary, StatusFailed, err.Error())
		return
	}
	t.finishWithStatus(id, summary, StatusDone, "")
}

// FinishUnverified 标记节点"已交付但未验证"（三态化）：
// errVerifyMissing/errUnverified 类缺证据产出走此通道——节点不标红（非失败语义），
// summary 附产出，reason 记缺证据原因。其余语义与 Finish 相同。
func (t *Tree) FinishUnverified(id, summary, reason string) {
	t.finishWithStatus(id, summary, StatusUnverified, reason)
}

// finishWithStatus 是 Finish/FinishUnverified 的共享实现。
// 幂等：已 terminal（Done/Failed/Cancelled/Unverified）的节点重复调用 no-op；
// Paused/Idle 节点允许收尾（resume 完成路径 / TTL 到期销毁路径）。
func (t *Tree) finishWithStatus(id, summary string, status Status, errText string) {
	t.mu.Lock()
	node, ok := t.nodes[id]
	if !ok {
		t.mu.Unlock()
		return
	}
	if node.Status == StatusDone || node.Status == StatusFailed || node.Status == StatusCancelled || node.Status == StatusUnverified {
		t.mu.Unlock()
		return
	}
	node.Finished = time.Now()
	node.Summary = summary
	node.Status = status
	if errText != "" {
		node.Err = errText
	}
	delete(t.cancels, id)
	snapshot := *node
	t.mu.Unlock()
	t.persistNode(snapshot)
}

// Pause 标记 DomainAgent 触达 token 上限暂停。仅 Running 节点可 Pause;
// 已 terminal(Done/Failed/Cancelled)或已 Paused 的节点 no-op。
// 删除 cancels[id](暂停即停止执行,resume 时重建 Agent)。持久化:best-effort SaveNode。
// summary 记暂停原因(如 "token budget exhausted")。
func (t *Tree) Pause(id, summary string) {
	t.mu.Lock()
	node, ok := t.nodes[id]
	if !ok || node.Status != StatusRunning {
		t.mu.Unlock()
		return
	}
	node.Status = StatusPaused
	node.Summary = summary
	node.Finished = time.Now()
	delete(t.cancels, id)
	snapshot := *node
	t.mu.Unlock()
	t.persistNode(snapshot)
}

// Resume 把 Paused 节点置回 Running 并绑定新 cancel func（resume 重建 Agent 后调用）。
// 仅 Paused 节点可 Resume；非 Paused no-op，返回 false。清空 Finished/Summary 恢复运行态。
// 持久化：best-effort SaveNode。
func (t *Tree) Resume(id string, cancel context.CancelFunc) bool {
	t.mu.Lock()
	node, ok := t.nodes[id]
	if !ok || node.Status != StatusPaused {
		t.mu.Unlock()
		return false
	}
	node.Status = StatusRunning
	node.Finished = time.Time{}
	node.Summary = ""
	if cancel != nil {
		t.cancels[id] = cancel
	}
	snapshot := *node
	t.mu.Unlock()
	t.persistNode(snapshot)
	return true
}

// Idle 标记 DomainAgent 任务完结转入热驻留态。仅 Running 节点可 Idle;
// 已 terminal 或 Paused/Idle 的节点 no-op。
// cancel 参数绑定销毁句柄(硬取消/话题切换/TTL 到期统一走 Tree.Cancel 或
// 直接调用),保持 Idle 节点仍是树公民可被外部取消。
// 持久化:best-effort SaveNode。summary 记任务结果摘要。
func (t *Tree) Idle(id, summary string, cancel context.CancelFunc) bool {
	t.mu.Lock()
	node, ok := t.nodes[id]
	if !ok || node.Status != StatusRunning {
		t.mu.Unlock()
		return false
	}
	node.Status = StatusIdle
	node.Summary = summary
	node.Finished = time.Now()
	if cancel != nil {
		t.cancels[id] = cancel
	}
	snapshot := *node
	t.mu.Unlock()
	t.persistNode(snapshot)
	return true
}

// Wake 把 Idle 节点置回 Running 并绑定新 cancel func（复用派发后调用）。
// 仅 Idle 节点可 Wake；非 Idle no-op，返回 false。清空 Finished/Summary 恢复运行态。
// 持久化：best-effort SaveNode。
func (t *Tree) Wake(id string, cancel context.CancelFunc) bool {
	t.mu.Lock()
	node, ok := t.nodes[id]
	if !ok || node.Status != StatusIdle {
		t.mu.Unlock()
		return false
	}
	node.Status = StatusRunning
	node.Finished = time.Time{}
	node.Summary = ""
	if cancel != nil {
		t.cancels[id] = cancel
	}
	snapshot := *node
	t.mu.Unlock()
	t.persistNode(snapshot)
	return true
}

// Reopen 复活终态节点（编排页用户直连"复活重跑"）：仅 Done/Failed/Cancelled/
// Unverified 可复活回 Running；其他状态 no-op 返回 false。清 Finished 恢复运行态，
// 保留 Summary/Err 作为上一轮留痕（复活种子上下文由调用方读取后注入新一轮，
// 下一轮 Finish 时自然覆盖）。
func (t *Tree) Reopen(id string) bool {
	t.mu.Lock()
	node, ok := t.nodes[id]
	if !ok {
		t.mu.Unlock()
		return false
	}
	switch node.Status {
	case StatusDone, StatusFailed, StatusCancelled, StatusUnverified:
	default:
		t.mu.Unlock()
		return false
	}
	node.Status = StatusRunning
	node.Finished = time.Time{}
	snapshot := *node
	t.mu.Unlock()
	t.persistNode(snapshot)
	return true
}

// Cancel 调用已绑定的 cancel func 并将状态置为 StatusCancelled。
// 返回是否找到对应节点且处于可取消状态（Running）。
// context.CancelFunc 幂等（Go doc），与 goroutine defer cancel 重复调用安全。
// 已 terminal 的节点 no-op。
// 持久化:store 非 nil 时 best-effort 更新状态为 cancelled。
func (t *Tree) Cancel(id string) bool {
	t.mu.Lock()
	node, ok := t.nodes[id]
	if !ok {
		t.mu.Unlock()
		return false
	}
	// Running/Paused/Idle 均可 Cancel(Running 取消执行;Paused 丢弃暂停态;Idle 销毁热驻实例)。
	// 已 terminal(Done/Failed/Cancelled)no-op。
	if node.Status != StatusRunning && node.Status != StatusPaused && node.Status != StatusIdle {
		t.mu.Unlock()
		return false
	}
	cancel, hasCancel := t.cancels[id]
	node.Status = StatusCancelled
	node.Finished = time.Now()
	delete(t.cancels, id)
	snapshot := *node
	t.mu.Unlock()
	if hasCancel && cancel != nil {
		cancel()
	}
	t.persistNode(snapshot)
	return true
}

// StopRunning 触发运行中节点的取消回调但**不改状态**（TODO #37 软停止专用）：
// 状态保持 Running，让 dispatcher 的 context.Canceled 收尾分支按会话软停止标记
// 把 domain 落 Paused（存 history 可续跑）、叶子部分回灌——与硬取消（Cancel 直接标
// Cancelled）分流。删除 cancels 绑定（停止后不再可外部 cancel，resume 时重新绑定）。
// 已 terminal / Paused 节点 no-op，返回 false。
// 状态不变故不触发持久化。
func (t *Tree) StopRunning(id string) bool {
	t.mu.Lock()
	node, ok := t.nodes[id]
	if !ok || node.Status != StatusRunning {
		t.mu.Unlock()
		return false
	}
	cancel, hasCancel := t.cancels[id]
	delete(t.cancels, id)
	t.mu.Unlock()
	if hasCancel && cancel != nil {
		cancel()
	}
	return true
}

// LoadFromStore 从持久化层加载节点到内存。store 为 nil 时 no-op。
// 用于 TreeFor lazy init 时恢复历史节点。cancel func 无法恢复,
// 已 Running 的节点重启后状态保留但不可外部 cancel(需等其自然 Finish 或超时)。
func (t *Tree) LoadFromStore(ctx context.Context) error {
	if t.store == nil {
		return nil
	}
	nodes, err := t.store.LoadNodes(ctx, t.sessionID)
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range nodes {
		n := nodes[i]
		// 重启恢复时 Idle 节点不可恢复(热驻 goroutine 已随进程消亡),降级为 Done。
		if n.Status == StatusIdle {
			n.Status = StatusDone
			if n.Summary != "" {
				n.Summary += "; 进程重启,热驻 Agent 已释放"
			} else {
				n.Summary = "进程重启,热驻 Agent 已释放"
			}
			n.Finished = time.Now()
		}
		// 已存在的内存节点优先(Race 场景:本会话已 Register 过的不覆盖)。
		if _, exists := t.nodes[n.ID]; !exists {
			t.nodes[n.ID] = &n
		}
	}
	return nil
}

// EndCurrentTopic 终结当前话题:取消所有 Running 节点,返回快照供调用方压缩为 KV 摘要,
// 清空内存节点并删除持久化层该 session 的全部节点。store 为 nil 时仅清内存。
//
// 用于话题切换:旧 Agent 树终结 + 新 Agent 树起(同 Tree 实例,内存清空)。
// 旧话题的摘要由调用方(ReactService.SwitchTopic)写入 sharedKV `topic:{id}:summary`。
// PG 节点删除后,新话题的节点从空开始,LoadNodes 不会混入旧话题节点。
//
// 返回的快照按 Started 升序,调用方可据此构造摘要文本。
func (t *Tree) EndCurrentTopic() []Node {
	t.mu.Lock()
	for id, node := range t.nodes {
		if node.Status == StatusRunning || node.Status == StatusPaused || node.Status == StatusIdle {
			node.Status = StatusCancelled
			node.Finished = time.Now()
			if cancel, ok := t.cancels[id]; ok && cancel != nil {
				cancel()
			}
			delete(t.cancels, id)
		}
	}
	snapshot := make([]Node, 0, len(t.nodes))
	for _, node := range t.nodes {
		snapshot = append(snapshot, *node)
	}
	// 清空内存节点,新话题从空树开始。
	t.nodes = make(map[string]*Node)
	t.mu.Unlock()

	// 排序快照(按 Started 升序),与 Snapshot 行为一致。
	for i := 1; i < len(snapshot); i++ {
		for j := i; j > 0 && snapshot[j].Started.Before(snapshot[j-1].Started); j-- {
			snapshot[j], snapshot[j-1] = snapshot[j-1], snapshot[j]
		}
	}

	// best-effort 删除 PG 节点。失败仅记日志,不影响新话题启动。
	if t.store != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := t.store.DeleteNodesBySession(ctx, t.sessionID); err != nil {
			log.Printf("[orchestrator] delete nodes for topic end failed: session=%s err=%v", t.sessionID, err)
		}
	}
	return snapshot
}

// Get 返回指定 ID 的节点拷贝。不存在返回 false。
func (t *Tree) Get(id string) (Node, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	node, ok := t.nodes[id]
	if !ok {
		return Node{}, false
	}
	return *node, true
}

// Snapshot 返回所有节点的值拷贝切片，按 Started 升序。
// 外部修改不影响内部状态。
func (t *Tree) Snapshot() []Node {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Node, 0, len(t.nodes))
	for _, node := range t.nodes {
		out = append(out, *node)
	}
	// 简单插入排序：节点数通常 <100，无需 sort.Slice 引入依赖。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Started.Before(out[j-1].Started); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
