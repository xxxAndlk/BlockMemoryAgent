// connector.go 实现 Widget「连接器」（万物皆可外接）的清单段、校验与运行时：
//   - ConnectorSpec 是 widget.json 的 connector 段（url/api/mcp/webhook 四型可选段）；
//   - ValidateConnector 保存期校验 + 就地规范化（钳制/限长/拒外部 token）；
//   - Manager.SaveConnector 原子改写已有部件的 connector 段（热载缓存一致）；
//   - HookRateLimiter    webhook 回调限流（单部件 60s/60 次滑窗，超限 429）；
//   - ConnectorScheduler api/mcp 定时执行（15s tick + 连续失败退避），结果经注入的
//     广播回调进各会话 widget_data 瞬时通道（与 POST /widgets/push 同款出口）；
//   - RunAPIConnector / RunMCPConnector / Manager.ExecuteConnector 单次执行
//     （POST /api/widgets/:id/poll 与调度器共用）。
//
// 定位：部件此前只会渲染 widget_save 写的静态文件；连接器让部件接上外部世界——
// 外链页面（url）、后端定时拉 HTTP 接口（api）、后端定时调 MCP 插件工具（mcp）、
// 外部系统反向推数（webhook）。webhook token 是访问凭据：服务端生成、清单落盘、
// 文件服务拒发 widget.json（见 server HandleWidgetFile 的钉死测试）。
//
// 密钥纪律：api headers 的值支持 ${VAR}/${VAR:default} 环境变量插值——复用
// config.ResolveEnvWithDefault（与 plugins.yaml 完全同款语义），清单落盘的是
// 原文，执行请求那一刻才展开，密钥不落 widget.json。
package widgets

import (
	"context"       // 调度与单次执行的取消/超时
	"crypto/rand"   // webhook token 生成
	"encoding/hex"  // token hex 编码
	"encoding/json" // 清单原子改写序列化
	"fmt"           // 错误信息
	"io"            // 响应体限长读取
	"net/http"      // api 连接器 HTTP 拉取
	"net/url"       // api/url 类型 target 校验
	"os"            // 清单原子改写落盘
	"strings"       // 形态规整
	"sync"          // 限流器/调度器状态锁
	"time"          // 滑窗/调度周期/超时

	"github.com/blockmemory/agent/backend/internal/config" // ${VAR} 插值（plugins.yaml 同款纪律）
)

// 连接器四型：url=iframe 直连外部页面；api=后端定时拉 HTTP 接口；mcp=后端定时调
// MCP 插件工具；webhook=外部系统反向 POST 数据进来推给部件。
const (
	ConnectorTypeURL     = "url"
	ConnectorTypeAPI     = "api"
	ConnectorTypeMCP     = "mcp"
	ConnectorTypeWebhook = "webhook"
)

// 连接器数值边界与缺省（钳制口径见 ValidateConnector）。
const (
	connectorDefaultIntervalSec = 30
	connectorMinIntervalSec     = 10
	connectorMaxIntervalSec     = 3600
	connectorDefaultTimeoutSec  = 10
	connectorMinTimeoutSec      = 1
	connectorMaxTimeoutSec      = 60
	connectorMaxHeaderKeyLen    = 64
	connectorMaxHeaderValueLen  = 256
	connectorTokenBytes         = 16 // 16 字节随机数 → 32 位 hex
)

// ConnectorPayloadMaxBytes 是连接器数据出口的单次上限（webhook 请求体 / api 响应体）：
// 超出截断（widget_data 瞬时通道本身另有 64KB 截断，见 agent.widgetPayloadMaxBytes）。
const ConnectorPayloadMaxBytes = 256 << 10

// ConnectorSpec 是 widget.json 清单的 connector 段（可选项；字段经 Info 原样透出）。
type ConnectorSpec struct {
	Type        string            `json:"type"`                   // url|api|mcp|webhook（必填）
	Target      string            `json:"target,omitempty"`       // url/api=http(s) URL；mcp=工具名；webhook 留空
	Method      string            `json:"method,omitempty"`       // api 的 HTTP 方法（GET/POST，缺省 GET）
	Headers     map[string]string `json:"headers,omitempty"`      // api 请求头（值支持 ${VAR}/${VAR:default} 执行期插值）
	Body        string            `json:"body,omitempty"`         // api POST 请求体（可选，原文透传）
	Args        map[string]any    `json:"args,omitempty"`         // mcp 工具调用参数
	IntervalSec int               `json:"interval_sec,omitempty"` // api/mcp 轮询间隔秒（钳制 [10,3600]，缺省 30）
	TimeoutSec  int               `json:"timeout_sec,omitempty"`  // 单次请求/调用超时秒（钳制 [1,60]，缺省 10）
	Token       string            `json:"token,omitempty"`        // webhook 明文 token（服务端生成；外部传入一律拒绝）
}

// ValidateConnector 校验并就地规范化 connector 段（保存期唯一入口，HTTP 端点与
// widget_save 工具共用）：type 四选一必填；url/api 的 target 必填 http(s) URL；
// mcp 的 target 必填非空串；webhook 忽略 target；method 仅 GET/POST；headers 键值
// 限长（值是纯文本或 ${...} 引用，不做 URL 校验）；interval/timeout 按契约钳制；
// token 由服务端生成，不接受外部传入（webhook 的生成见 saveLocked/SaveConnector）。
func ValidateConnector(c *ConnectorSpec) error {
	if c == nil {
		return fmt.Errorf("connector 必填")
	}
	c.Type = strings.ToLower(strings.TrimSpace(c.Type))
	switch c.Type {
	case ConnectorTypeURL, ConnectorTypeAPI:
		target := strings.TrimSpace(c.Target)
		u, err := url.Parse(target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("connector.target 非法 %q（%s 类型须为 http(s) URL）", c.Target, c.Type)
		}
		c.Target = target
	case ConnectorTypeMCP:
		if strings.TrimSpace(c.Target) == "" {
			return fmt.Errorf("connector.target 必填（mcp 类型为工具名）")
		}
		c.Target = strings.TrimSpace(c.Target)
	case ConnectorTypeWebhook:
		// webhook 无 target：回调地址由服务端按 id+token 拼（hook_url）。
	default:
		return fmt.Errorf("connector.type 非法 %q（仅支持 url/api/mcp/webhook）", c.Type)
	}
	c.Method = strings.ToUpper(strings.TrimSpace(c.Method))
	if c.Method != "" && c.Method != http.MethodGet && c.Method != http.MethodPost {
		return fmt.Errorf("connector.method 非法 %q（仅支持 GET/POST）", c.Method)
	}
	for k, v := range c.Headers {
		if len(k) > connectorMaxHeaderKeyLen {
			return fmt.Errorf("connector.headers 键超长（%d>%d）: %q", len(k), connectorMaxHeaderKeyLen, k)
		}
		if len(v) > connectorMaxHeaderValueLen {
			return fmt.Errorf("connector.headers 值超长（%d>%d）: %q", len(v), connectorMaxHeaderValueLen, k)
		}
	}
	if c.IntervalSec == 0 {
		c.IntervalSec = connectorDefaultIntervalSec
	}
	c.IntervalSec = clampInt(c.IntervalSec, connectorMinIntervalSec, connectorMaxIntervalSec)
	if c.TimeoutSec == 0 {
		c.TimeoutSec = connectorDefaultTimeoutSec
	}
	c.TimeoutSec = clampInt(c.TimeoutSec, connectorMinTimeoutSec, connectorMaxTimeoutSec)
	if c.Token != "" {
		return fmt.Errorf("connector.token 由服务端生成，不接受外部传入")
	}
	return nil
}

// clampInt 把 v 钳制到 [lo, hi]。
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// connectorInterval 返回执行期轮询间隔（防御手改清单写出越界/缺省值）。
func connectorInterval(conn *ConnectorSpec) time.Duration {
	if conn.IntervalSec == 0 {
		return connectorDefaultIntervalSec * time.Second
	}
	return time.Duration(clampInt(conn.IntervalSec, connectorMinIntervalSec, connectorMaxIntervalSec)) * time.Second
}

// connectorTimeout 返回执行期单次超时（口径同 connectorInterval）。
func connectorTimeout(conn *ConnectorSpec) time.Duration {
	if conn.TimeoutSec == 0 {
		return connectorDefaultTimeoutSec * time.Second
	}
	return time.Duration(clampInt(conn.TimeoutSec, connectorMinTimeoutSec, connectorMaxTimeoutSec)) * time.Second
}

// NewConnectorToken 生成 webhook 回调 token：crypto/rand 16 字节 → 32 位 hex。
func NewConnectorToken() (string, error) {
	buf := make([]byte, connectorTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate connector token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// SaveConnector 原子改写已有部件清单的 connector 段（PUT /api/widgets/:id/connector
// 的写侧，与 SetEnabled 同款临时文件 + rename 通道，热载缓存一致）。
// regenToken=true 且 type=webhook 时重新生成 token（旧回调地址即刻失效）；
// 非 webhook 类型 token 恒清空；name/version/description/entry/enabled/owner 原样保留。
func (m *Manager) SaveConnector(id string, c *ConnectorSpec, regenToken bool) (*Info, error) {
	if err := ValidateID(id); err != nil {
		return nil, err
	}
	if err := ValidateConnector(c); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.refreshLocked(id)
	if e.info == nil {
		return nil, fmt.Errorf("部件不存在: %s", id)
	}
	if c.Type == ConnectorTypeWebhook {
		old := ""
		if e.info.Connector != nil && e.info.Connector.Type == ConnectorTypeWebhook {
			old = e.info.Connector.Token
		}
		switch {
		case regenToken || old == "":
			tok, err := NewConnectorToken()
			if err != nil {
				return nil, err
			}
			c.Token = tok
		default:
			c.Token = old
		}
	}
	mf := Manifest{
		Name:        e.info.Name,
		Version:     e.info.Version,
		Description: e.info.Description,
		Entry:       e.info.Entry,
		Enabled:     e.info.Enabled,
		Owner:       e.info.Owner,
		Connector:   c,
	}
	if mf.Name == id {
		mf.Name = "" // 缺省回落 id 的不落盘，保持清单干净（与 SetEnabled 同口径）
	}
	return m.rewriteManifestLocked(id, &mf)
}

// rewriteManifestLocked 把 mf 原子写回 widget.json（临时文件 + rename；Windows rename
// 不覆盖，先删旧文件），随后清缓存重建基线（调用方持锁）。SetEnabled/SaveConnector
// 共用的清单改写通道。
func (m *Manager) rewriteManifestLocked(id string, mf *Manifest) (*Info, error) {
	body, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal manifest %s: %w", id, err)
	}
	tmp, err := os.CreateTemp(m.dirOf(id), ".manifest-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create temp manifest: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return nil, fmt.Errorf("write temp manifest: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return nil, fmt.Errorf("close temp manifest: %w", err)
	}
	path := m.manifestPathOf(id)
	if err := os.Remove(path); err != nil {
		os.Remove(tmpName)
		return nil, fmt.Errorf("remove old manifest: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return nil, fmt.Errorf("rename manifest: %w", err)
	}
	delete(m.entries, id)
	e := m.refreshLocked(id)
	return e.info, nil
}

// inheritConnectorToken 覆写清单时继承 webhook token（调用方持锁）：新 connector 是
// webhook 且未带 token 时，旧 connector 同为 webhook 则续用其 token（改代码/改间隔
// 不使回调地址失效），否则服务端新生成。token 生成失败即保存失败（不留空 token
// 的"开放回调"半成品——空 token 会被 hook 端点当成匹配放过）。
func (m *Manager) inheritConnectorTokenLocked(mf *Manifest, old *ConnectorSpec) error {
	if mf.Connector == nil || mf.Connector.Type != ConnectorTypeWebhook || mf.Connector.Token != "" {
		return nil
	}
	if old != nil && old.Type == ConnectorTypeWebhook && old.Token != "" {
		mf.Connector.Token = old.Token
		return nil
	}
	tok, err := NewConnectorToken()
	if err != nil {
		return err
	}
	mf.Connector.Token = tok
	return nil
}

// MCPToolCaller 是 mcp 连接器的工具调用能力（窄接口，避免 widgets 反向依赖
// domain/tool；bootstrap 用 tool.Registry.Dispatch 适配实现，ctx 由调用方带
// 超时，工作目录由适配层注入）。
type MCPToolCaller interface {
	// CallTool 调用名 name 的工具（args 按清单原样透传），成功返回输出文本。
	CallTool(ctx context.Context, name string, args map[string]any) (string, error)
}

// RunAPIConnector 执行一次 api 连接器：按 method/headers/body 发起 HTTP 请求——
// headers 的值在执行期做 ${VAR}/${VAR:default} 插值（config.ResolveEnvWithDefault，
// 与 plugins.yaml 同款纪律，密钥不落 widget.json）。响应体限 ConnectorPayloadMaxBytes
// 截断；非 2xx 视为执行失败（记日志跳过的依据）。
func RunAPIConnector(ctx context.Context, conn *ConnectorSpec) (string, error) {
	method := strings.ToUpper(strings.TrimSpace(conn.Method))
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(conn.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, conn.Target, body)
	if err != nil {
		return "", fmt.Errorf("构造请求失败: %w", err)
	}
	for k, v := range conn.Headers {
		req.Header.Set(k, config.ResolveEnvWithDefault(v))
	}
	client := &http.Client{Timeout: connectorTimeout(conn)}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, ConnectorPayloadMaxBytes+1))
	if err != nil {
		return "", fmt.Errorf("读取响应失败: %w", err)
	}
	if len(data) > ConnectorPayloadMaxBytes {
		data = data[:ConnectorPayloadMaxBytes]
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		preview := strings.TrimSpace(string(data))
		if len(preview) > 200 {
			preview = preview[:200] + "..."
		}
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, preview)
	}
	return string(data), nil
}

// RunMCPConnector 执行一次 mcp 连接器：调用 target 指定的工具（args 原样透传），
// 返回输出文本。caller 未接线或调用失败都视为本次执行失败（调度器记日志后跳过，
// 下一个间隔再试）。
func RunMCPConnector(ctx context.Context, conn *ConnectorSpec, caller MCPToolCaller) (string, error) {
	if caller == nil {
		return "", fmt.Errorf("mcp 工具调用未接线（bootstrap 未注入 toolRegistry）")
	}
	return caller.CallTool(ctx, conn.Target, conn.Args)
}

// ExecuteConnector 立即执行一次连接器并同步返回结果文本（api/mcp；POST
// /api/widgets/:id/poll 与调度器共用执行侧）。广播分发由调用方决定（poll 端点
// 照常经数据总线分发）。非 api/mcp 类型返回错误（poll 端点据此回 400）。
func (m *Manager) ExecuteConnector(ctx context.Context, id string, caller MCPToolCaller) (string, error) {
	info, ok := m.Get(id)
	if !ok {
		return "", fmt.Errorf("部件不存在: %s", id)
	}
	conn := info.Connector
	if conn == nil {
		return "", fmt.Errorf("部件 %s 无连接器", id)
	}
	ctx, cancel := context.WithTimeout(ctx, connectorTimeout(conn))
	defer cancel()
	switch conn.Type {
	case ConnectorTypeAPI:
		return RunAPIConnector(ctx, conn)
	case ConnectorTypeMCP:
		return RunMCPConnector(ctx, conn, caller)
	default:
		return "", fmt.Errorf("connector 类型 %q 不支持立即执行（仅 api/mcp）", conn.Type)
	}
}

// 连接器调度器常量：tick 周期、连续失败退避阈值与时长（按 tick 计）。
const (
	ConnectorSchedulerTick = 15 * time.Second // 调度扫描周期
	connectorMaxFailures   = 5                // 连续失败达阈值即退避
	connectorBackoffTicks  = 10               // 退避时长（10 个 tick）
)

// ConnectorScheduler 定时执行 api/mcp 连接器：每 tick 扫一遍 Manager.List，到期且
// 未退避的连接器执行一次，结果经 dispatch 广播到打开部件的会话（widget_data 瞬时
// 通道，与 POST /widgets/push 同款出口；不是 push 语义变更，只是新调用方）。
// 失败只记 widgets 独立日志（Manager 同款 logWarnf 风格），不写 evolution_log。
type ConnectorScheduler struct {
	mgr      *Manager                       // 部件清单源（热载缓存自动跟进手改）
	dispatch func(widgetID, payload string) // 广播回调（bootstrap 接 ReactService.BroadcastWidgetData）
	caller   MCPToolCaller                  // mcp 工具调用（nil 时 mcp 连接器执行即失败）
	tick     time.Duration                  // 扫描周期（测试可改小）

	mu           sync.Mutex           // 保护下面三张状态表
	lastRun      map[string]time.Time // widgetID -> 上次执行时刻（间隔判定）
	failCount    map[string]int       // widgetID -> 连续失败计数
	backoffUntil map[string]time.Time // widgetID -> 退避截止（连失达阈值后跳过 10 个 tick）
}

// NewConnectorScheduler 创建调度器；mgr/dispatch 为 nil 时运行即空转（精简装配安全）。
func NewConnectorScheduler(mgr *Manager, dispatch func(string, string)) *ConnectorScheduler {
	return &ConnectorScheduler{
		mgr:          mgr,
		dispatch:     dispatch,
		tick:         ConnectorSchedulerTick,
		lastRun:      make(map[string]time.Time),
		failCount:    make(map[string]int),
		backoffUntil: make(map[string]time.Time),
	}
}

// SetMCPToolCaller 注入 mcp 工具调用能力（bootstrap 装配后调用）。
func (s *ConnectorScheduler) SetMCPToolCaller(c MCPToolCaller) { s.caller = c }

// RunConnectors 调度主循环：启动即扫一轮（仿 startDataMaintenance"启动即跑一遍"），
// 之后每 tick 扫描；ctx 取消即退出（bootstrap 在 App cleanup 里 cancel，逆序停）。
func (s *ConnectorScheduler) RunConnectors(ctx context.Context) {
	s.runOnce(ctx, time.Now())
	t := time.NewTicker(s.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.runOnce(ctx, now)
		}
	}
}

// runOnce 执行一轮到期连接器（now 注入便于测试精确控时；Manager.List 自带锁，
// 内部状态自锁，可与 HTTP 端点并发）。
func (s *ConnectorScheduler) runOnce(ctx context.Context, now time.Time) {
	if s.mgr == nil || s.dispatch == nil {
		return
	}
	for _, info := range s.mgr.List() {
		conn := info.Connector
		if conn == nil || !info.Enabled ||
			(conn.Type != ConnectorTypeAPI && conn.Type != ConnectorTypeMCP) {
			continue
		}
		id := info.ID
		s.mu.Lock()
		if until, ok := s.backoffUntil[id]; ok && now.Before(until) {
			s.mu.Unlock()
			continue // 退避期内：跳过 10 个 tick
		}
		if last, ok := s.lastRun[id]; ok && now.Sub(last) < connectorInterval(conn) {
			s.mu.Unlock()
			continue // 距上次执行未到 interval_sec
		}
		s.lastRun[id] = now
		s.mu.Unlock()

		runCtx, cancel := context.WithTimeout(ctx, connectorTimeout(conn))
		payload, err := s.exec(runCtx, conn)
		cancel()
		if err != nil {
			s.recordFailure(id, now, err)
			continue
		}
		s.mu.Lock()
		delete(s.failCount, id)
		s.mu.Unlock()
		s.dispatch(id, payload)
	}
}

// exec 按类型执行单连接器（api HTTP 拉取 / mcp 工具调用）。
func (s *ConnectorScheduler) exec(ctx context.Context, conn *ConnectorSpec) (string, error) {
	switch conn.Type {
	case ConnectorTypeAPI:
		return RunAPIConnector(ctx, conn)
	case ConnectorTypeMCP:
		return RunMCPConnector(ctx, conn, s.caller)
	}
	return "", fmt.Errorf("connector 类型 %q 不支持定时执行", conn.Type)
}

// recordFailure 记一次执行失败：连续 connectorMaxFailures 次进入退避（跳过
// connectorBackoffTicks 个 tick）并告警；其余失败按序号记 warn 明细。
func (s *ConnectorScheduler) recordFailure(id string, now time.Time, err error) {
	s.mu.Lock()
	s.failCount[id]++
	count := s.failCount[id]
	enterBackoff := count >= connectorMaxFailures
	if enterBackoff {
		s.backoffUntil[id] = now.Add(connectorBackoffTicks * s.tick)
		s.failCount[id] = 0
	}
	s.mu.Unlock()
	if enterBackoff {
		s.mgr.logWarnf("[widgets] connector %s 连续失败 %d 次，退避 %d tick: %v",
			id, connectorMaxFailures, connectorBackoffTicks, err)
		return
	}
	s.mgr.logWarnf("[widgets] connector %s 执行失败(%d/%d): %v", id, count, connectorMaxFailures, err)
}

// webhook 回调限流口径：单部件 60 秒 60 次（滑窗）；超限 429。防外部系统刷爆
// 瞬时数据通道（SSE 出口与 64KB 快照序列化都是共享资源）。
const (
	hookRateWindow = time.Minute
	hookRateMax    = 60
)

// HookRateLimiter 单部件滑窗限流器（进程内；APIHandler 持一份，生命周期同 router）。
type HookRateLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time // widgetID -> 窗口内命中时刻
}

// NewHookRateLimiter 创建限流器。
func NewHookRateLimiter() *HookRateLimiter {
	return &HookRateLimiter{hits: make(map[string][]time.Time)}
}

// Allow 判定该部件本次回调是否放行（按调用时刻登记滑窗命中）。
func (l *HookRateLimiter) Allow(id string) bool { return l.AllowAt(id, time.Now()) }

// AllowAt 是 Allow 的可注入时钟版（测试用 now 精确断言滑窗裁剪）。
func (l *HookRateLimiter) AllowAt(id string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-hookRateWindow)
	var win []time.Time
	for _, t := range l.hits[id] {
		if t.After(cutoff) {
			win = append(win, t)
		}
	}
	if len(win) >= hookRateMax {
		l.hits[id] = win
		return false
	}
	l.hits[id] = append(win, now)
	return true
}
