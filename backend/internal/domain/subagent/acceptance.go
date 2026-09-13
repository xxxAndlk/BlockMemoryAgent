package subagent

// 交付验收闭环（测试助手大改，2026-09-12）：Meta 终答提交前由 ReactService 钩子触发，
// 派发 test_assistant（验收测试员）模拟真人复核交付物；FAIL 时把可归属错误经
// ReviveWithMessage 派回责任 Agent 修复、mailbox 抄送 Meta（只读知悉）、userNotifyFn
// 通知用户，修复完成后复验，直至 PASS 或 max_rounds 硬顶按【未验证项】交付。
// PASS 后进入录播演示阶段（demo.go，2026-09-13）：用户确认 → 录制演示视频 → 评审查验，
// 打回意见归因后复用同款返工链路（与验收共享 max_rounds 硬顶）。
//
// 三态开关为工作目录级：<workDir>/.bma/tester.yaml，默认 off（零行为变化）。
// 历史背景：旧 verifyloop 自动闭环 A/B 实证负收益，本次按用户明确要求实现，以
// 「默认关 + max_rounds 硬顶 + tester 崩溃降级交付」控制风险。

import (
	"context"       // context 控制验收循环与轻量判定调用的取消/超时
	"fmt"           // fmt 格式化验收任务文本与通知消息
	"log"           // log 记录验收降级与派发失败（不影响主流程）
	"os"            // os 读写 .bma/tester.yaml 与检查验收报告落盘
	"path/filepath" // filepath 拼接 .bma 目录路径
	"regexp"        // regexp 解析机读契约块（【验收结论】/【错误清单】）
	"sort"          // sort 稳定 Agent 名单顺序（按启动时间）
	"strings"       // strings 文本裁剪与判定
	"time"          // time 等待轮询节拍与轻量判定超时

	"github.com/blockmemory/agent/backend/internal/agent"               // agent.WithAgentID 构造内部派发 ctx
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator" // orchestrator.Node/Tree 权威树读取
	"github.com/blockmemory/agent/backend/internal/mailbox"             // mailbox 抄送 Meta
	"gopkg.in/yaml.v3"                                                  // yaml.v3 读写 tester.yaml（项目既有 yaml 库）
)

// 验收三态开关取值（.bma/tester.yaml mode 字段）。
const (
	// TesterModeOff 关：任何情况不执行验收（默认）。
	TesterModeOff = "off"
	// TesterModeAuto 智能：按 auto_prompt 描述经轻量模型判定命中才执行；判定失败/描述为空不执行（降级安全方向）。
	TesterModeAuto = "auto"
	// TesterModeOn 开：总是执行验收。
	TesterModeOn = "on"
)

const (
	// defaultTesterMaxRounds 验收轮次硬顶默认值（防无限纠偏循环）。
	defaultTesterMaxRounds = 2
	// acceptanceDomain 验收测试员节点的领域名（树/看板展示用，兼作名单过滤标记）。
	acceptanceDomain = "acceptance"
	// acceptanceTesterWallClock 验收测试员派发级墙钟：页面连续操作类验收需预算，
	// 又不能拿满全局 sub_agent_timeout（dispatchOne 取 min）。
	acceptanceTesterWallClock = 15 * time.Minute
	// acceptanceJudgeTimeout auto 模式轻量判定单次调用预算。
	acceptanceJudgeTimeout = 45 * time.Second
	// acceptanceWaitTick 等待 tester/修复节点终态的轮询节拍（仿 waitForChildren 30s tick 模式）。
	acceptanceWaitTick = 30 * time.Second
)

// TesterConfig 工作目录级测试助手开关（<workDir>/.bma/tester.yaml）。
type TesterConfig struct {
	// Mode 三态：off（不执行）/ auto（轻量模型按 AutoPrompt 判定命中才执行）/ on（总是执行）。
	Mode string `json:"mode" yaml:"mode"`
	// AutoPrompt 智能模式的命中场景描述（如"涉及页面/UI 的交付"），仅 auto 生效；空=不执行。
	AutoPrompt string `json:"auto_prompt" yaml:"auto_prompt"`
	// MaxRounds 验收-返工轮次硬顶；<=0 按默认值 2。
	MaxRounds int `json:"max_rounds" yaml:"max_rounds"`
}

// normalizeTesterConfig 归一化配置：非法 mode 回落 off，max_rounds<=0 回落默认值。
func normalizeTesterConfig(cfg TesterConfig) TesterConfig {
	switch cfg.Mode {
	case TesterModeOff, TesterModeAuto, TesterModeOn:
	default:
		cfg.Mode = TesterModeOff
	}
	if cfg.MaxRounds <= 0 {
		cfg.MaxRounds = defaultTesterMaxRounds
	}
	return cfg
}

// testerConfigPath 返回工作目录下 tester.yaml 路径。
func testerConfigPath(workDir string) string {
	return filepath.Join(workDir, ".bma", "tester.yaml")
}

// LoadTesterConfig 读取 <workDir>/.bma/tester.yaml；文件缺失/解析失败/workDir 为空
// 一律回落默认 {off, "", 2}（降级安全方向：不执行验收）。
func LoadTesterConfig(workDir string) TesterConfig {
	cfg := TesterConfig{Mode: TesterModeOff, MaxRounds: defaultTesterMaxRounds}
	if strings.TrimSpace(workDir) == "" {
		return cfg
	}
	data, err := os.ReadFile(testerConfigPath(workDir))
	if err != nil {
		return cfg
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Printf("[acceptance] parse tester.yaml failed, fallback to off: %v", err)
		return TesterConfig{Mode: TesterModeOff, MaxRounds: defaultTesterMaxRounds}
	}
	return normalizeTesterConfig(cfg)
}

// SaveTesterConfig 全量覆盖写 <workDir>/.bma/tester.yaml（HTTP PUT 用户手动编辑）。
func SaveTesterConfig(workDir string, cfg TesterConfig) error {
	if strings.TrimSpace(workDir) == "" {
		return fmt.Errorf("workDir 为空，无法定位 .bma/tester.yaml")
	}
	cfg = normalizeTesterConfig(cfg)
	if err := os.MkdirAll(filepath.Dir(testerConfigPath(workDir)), 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(testerConfigPath(workDir), data, 0o644)
}

// acceptanceError 是机读块【错误清单】中一条可解析的错误条目。
type acceptanceError struct {
	AgentID string // 责任 Agent 实例 ID（[agent:<id>] 照抄；无法归属时 tester 写 meta）
	Desc    string // 问题描述
}

var (
	// acceptanceVerdictRe 匹配【验收结论】PASS/FAIL（允许标记与结论间有空白）。
	acceptanceVerdictRe = regexp.MustCompile(`【验收结论】\s*(PASS|FAIL)`)
	// acceptanceAgentErrRe 从错误清单行提取 [agent:id] 与描述。
	acceptanceAgentErrRe = regexp.MustCompile(`\[agent:([^\[\]\s]+)\]\s*(.*)`)
)

// parseAcceptanceReport 解析测试助手最终答复末尾的机读契约块。
// 返回 (hasVerdict, passed, items)：缺【验收结论】段时 hasVerdict=false（无效验收）；
// FAIL 但无【错误清单】或无 [agent:id] 条目时 items 为空（全部记未验证项，不复活）。
func parseAcceptanceReport(text string) (hasVerdict bool, passed bool, items []acceptanceError) {
	loc := acceptanceVerdictRe.FindStringSubmatchIndex(text)
	if loc == nil {
		return false, false, nil
	}
	passed = text[loc[2]:loc[3]] == "PASS"
	if passed {
		return true, true, nil
	}
	// FAIL：仅在【验收结论】之后的【错误清单】段内提取条目，防正文误命中。
	rest := text[loc[1]:]
	idx := strings.Index(rest, "【错误清单】")
	if idx < 0 {
		return true, false, nil
	}
	for _, line := range strings.Split(rest[idx+len("【错误清单】"):], "\n") {
		m := acceptanceAgentErrRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		items = append(items, acceptanceError{AgentID: m[1], Desc: strings.TrimSpace(m[2])})
	}
	return true, false, items
}

// AcceptanceManager 交付验收闭环管理器，挂 Dispatcher 复用其派发/复活/邮箱/树机制。
// 由 bootstrap 装配并注入 ReactService（agent.AcceptanceRunner 接口），
// runSession/resumeSession 终答提交前经 RunWrap 触发。
type AcceptanceManager struct {
	d *Dispatcher // d 子 Agent 调度器（派发 tester、复活责任节点、抄送邮箱）

	// lightCall 轻量模型一次性调用（auto 模式命中判定），bootstrap 注入
	// modelFactory.CallLightweightWithRetry；nil 时 auto 一律不执行（降级安全方向）。
	lightCall func(ctx context.Context, prompt string) (string, error)

	// waitTick 等待节点终态的轮询节拍；<=0 用 acceptanceWaitTick。测试注入短节拍。
	waitTick time.Duration
}

// NewAcceptanceManager 构造验收闭环管理器。lightCall 可为 nil（auto 模式退化为不执行）。
func NewAcceptanceManager(d *Dispatcher, lightCall func(ctx context.Context, prompt string) (string, error)) *AcceptanceManager {
	return &AcceptanceManager{d: d, lightCall: lightCall}
}

// ShouldRun 三态判断本次终答是否触发验收。
// off→false；on→true；auto→AutoPrompt/goal 为空或 lightCall 未接线或判定调用失败
// 一律 false（降级安全方向），命中（回答含 YES）才 true。
func (m *AcceptanceManager) ShouldRun(ctx context.Context, cfg TesterConfig, goal string) bool {
	switch cfg.Mode {
	case TesterModeOn:
		return true
	case TesterModeAuto:
		desc := strings.TrimSpace(cfg.AutoPrompt)
		if desc == "" || strings.TrimSpace(goal) == "" || m.lightCall == nil {
			return false
		}
		prompt := fmt.Sprintf("判定以下任务是否命中需要交付验收测试的场景。\n【命中场景描述】\n%s\n【任务目标】\n%s\n只回答 YES 或 NO。", desc, truncateRunes(goal, 500))
		jctx, cancel := context.WithTimeout(ctx, acceptanceJudgeTimeout)
		defer cancel()
		resp, err := m.lightCall(jctx, prompt)
		if err != nil {
			log.Printf("[acceptance] auto judge failed, skip acceptance: %v", err)
			return false
		}
		return strings.Contains(strings.ToUpper(resp), "YES")
	default: // off 及非法值（LoadTesterConfig 已归一化，这里是防御）
		return false
	}
}

// RunWrap 是 ReactService 终答钩子入口：内含 ShouldRun 判定，返回最终交付文本
// （原文 / 原文+验收通过附报告路径 / 原文+【未验证项】段+报告路径 / 原文+未验收警告）。
// 实现 agent.AcceptanceRunner 接口；m 或 dispatcher 为 nil 时原样返回。
func (m *AcceptanceManager) RunWrap(ctx context.Context, sessionID, workDir, goal, finalAnswer string) string {
	if m == nil || m.d == nil {
		return finalAnswer
	}
	cfg := LoadTesterConfig(workDir)
	if !m.ShouldRun(ctx, cfg, goal) {
		return finalAnswer
	}
	return m.run(ctx, cfg, sessionID, workDir, goal, finalAnswer)
}

// run 执行验收-返工循环（max_rounds 硬顶），返回最终交付文本。
// 树/邮箱未接线（测试场景）时降级原样交付。
func (m *AcceptanceManager) run(ctx context.Context, cfg TesterConfig, sessionID, workDir, goal, finalAnswer string) string {
	if m.d.treeFn == nil || m.d.mailbox == nil {
		return finalAnswer
	}
	t := m.d.treeFn(sessionID)
	if t == nil {
		return finalAnswer
	}
	var lastItems []acceptanceError // 最后一轮 FAIL 的错误清单（熔断后作【未验证项】交付）
	var lastReport string           // 最近一份实际落盘的验收报告（相对路径）
	for round := 1; round <= cfg.MaxRounds; round++ {
		m.notifyUser(sessionID, fmt.Sprintf("测试助手交付验收中（第 %d/%d 轮）……", round, cfg.MaxRounds))
		roster, machineSummary := collectAcceptanceRoster(t)
		task := buildAcceptanceTask(sessionID, round, goal, roster, machineSummary)
		// 内部派发：以 Meta（agentID=sessionID）为父走 dispatchOne 同链路；
		// test_assistant spec_exempt 豁免 WriteSpec 门禁（门禁在 Execute 层，dispatchOne 本身不含）。
		dctx := agent.WithAgentID(ctx, sessionID)
		testerID, errRes := m.d.dispatchOne(dctx, "test_assistant", acceptanceDomain, task, "", "", "",
			nil, nil, acceptanceTesterWallClock, "", "")
		if errRes != nil {
			log.Printf("[acceptance] dispatch tester failed: session=%s round=%d err=%s", sessionID, round, errRes.Error)
			return finalAnswer + "\n\n（交付验收未完成：测试助手派发失败——" + errRes.Error + "，本次交付未经验收）"
		}
		node, ok := m.waitNodeTerminal(ctx, t, testerID)
		if !ok {
			return finalAnswer + "\n\n（交付验收被中断，本次交付未经验收）"
		}
		if ref := acceptanceReportRef(workDir, sessionID, round); ref != "" {
			lastReport = ref
		}
		// tester 自身失败（超时/被杀/崩溃）：带警告交付，不重试。
		if node.Status == orchestrator.StatusFailed || node.Status == orchestrator.StatusCancelled {
			reason := strings.TrimSpace(node.Err)
			if reason == "" {
				reason = "测试助手异常终止"
			}
			return finalAnswer + "\n\n（交付验收未完成：" + reason + "，本次交付未经验收）"
		}
		hasVerdict, passed, items := parseAcceptanceReport(node.Summary)
		if !hasVerdict {
			// 缺机读块=无效验收（tester 未遵守契约），不重试，带警告交付。
			return finalAnswer + "\n\n（交付验收未完成：测试报告缺【验收结论】段，本次交付未经验收）"
		}
		if passed {
			m.notifyUser(sessionID, fmt.Sprintf("测试助手验收通过（第 %d/%d 轮）。", round, cfg.MaxRounds))
			// 演示阶段（demo.go）：用户确认后录播演示并评审。跳过/降级/评审通过
			// approved=true 直接交付；打回返回 false + 意见原文，走下方归因返工。
			approved, feedback, demoArtifact := m.demoStage(ctx, sessionID, t, round, lastReport)
			if approved {
				out := finalAnswer + "\n\n—— 测试助手验收通过"
				var extras []string
				if lastReport != "" {
					extras = append(extras, "验收报告："+lastReport)
				}
				if demoArtifact != "" {
					extras = append(extras, "演示视频："+demoArtifact)
				}
				if len(extras) > 0 {
					out += "（" + strings.Join(extras, "；") + "）"
				}
				return out
			}
			// 演示打回：与验收 FAIL 共享 max_rounds 硬顶（消耗本轮 round），
			// 意见归因后走与 FAIL 相同的 reworkFailures 返工路径，下一轮重新验收+确认演示。
			m.notifyUser(sessionID, fmt.Sprintf("演示被用户打回（第 %d/%d 轮），正在归因派回修复……", round, cfg.MaxRounds))
			items = m.attributeDemoRejection(ctx, sessionID, t, feedback, lastReport)
			if len(items) == 0 {
				// 归因不出条目且名单为空：意见记未验证项，按现状交付（不阻塞）。
				m.notifyUser(sessionID, "演示打回意见无法归因到任何执行 Agent，按现状交付（未验证项见终答）。")
				lastItems = []acceptanceError{{AgentID: "demo", Desc: "演示打回意见（无法归因）：" + feedback}}
				break
			}
			lastItems = items
			if round == cfg.MaxRounds {
				// 轮次硬顶仍打回：打回意见一并记入【未验证项】交付。
				lastItems = append(lastItems, acceptanceError{AgentID: "demo", Desc: "演示打回意见：" + feedback})
				break
			}
			revived, unrevived := m.reworkFailures(ctx, sessionID, t, items, round)
			lastItems = append(lastItems, unrevived...)
			if len(revived) > 0 {
				m.waitNodesTerminal(ctx, t, revived) // 等修复子 Agent 全完成后进入下一轮复验
			}
			continue
		}
		lastItems = items
		if round == cfg.MaxRounds {
			break // 轮次硬顶：跳出按【未验证项】交付
		}
		// FAIL：可归属且非 meta 的错误派回责任 Agent 修复，其余留作未验证项。
		revived, unrevived := m.reworkFailures(ctx, sessionID, t, items, round)
		lastItems = append(lastItems, unrevived...)
		if len(revived) > 0 {
			m.waitNodesTerminal(ctx, t, revived) // 等修复子 Agent 全完成后进入下一轮复验
		}
	}
	// 超过 max_rounds 仍 FAIL：停止循环，终答附【未验证项】+ 报告路径交付。
	m.notifyUser(sessionID, fmt.Sprintf("验收 %d 轮仍未全部通过，按现状交付（未验证项见终答）。", cfg.MaxRounds))
	var b strings.Builder
	b.WriteString(finalAnswer)
	b.WriteString("\n\n【未验证项】\n")
	if len(lastItems) == 0 {
		b.WriteString("- 验收未通过但错误清单为空（测试报告未给出可定位条目）\n")
	}
	for _, it := range lastItems {
		b.WriteString("- [" + it.AgentID + "] " + it.Desc + "\n")
	}
	if lastReport != "" {
		b.WriteString("\n验收报告：" + lastReport)
	}
	return b.String()
}

// reworkFailures 处理一轮 FAIL：按责任 Agent 归组错误，可复活节点经 ReviveWithMessage
// 派回修复（同 Agent 多错误合并一条指令），mailbox 抄送 Meta（MsgInfo 只读知悉），
// userNotifyFn 通知用户。返回（成功复活的节点 ID 列表，无法派回的遗留错误）。
// 责任方为 meta 或节点不可识别/非终态时不复活（Meta 无法自修复），记入遗留。
func (m *AcceptanceManager) reworkFailures(ctx context.Context, sessionID string, t *orchestrator.Tree, items []acceptanceError, round int) (revived []string, left []acceptanceError) {
	// 按责任 Agent 归组（保持出现顺序），同一 Agent 多条错误合并一次复活。
	order := []string{}
	byAgent := map[string][]string{}
	for _, it := range items {
		id := it.AgentID
		if _, ok := byAgent[id]; !ok {
			order = append(order, id)
		}
		byAgent[id] = append(byAgent[id], it.Desc)
	}
	var names []string
	for _, id := range order {
		if id == "" || id == "meta" || id == sessionID {
			// Meta 无法自修复：不复活，记未验证项。
			for _, desc := range byAgent[id] {
				left = append(left, acceptanceError{AgentID: "meta", Desc: desc})
			}
			continue
		}
		node, ok := t.Get(id)
		if !ok || !acceptanceNodeTerminal(node.Status) {
			for _, desc := range byAgent[id] {
				left = append(left, acceptanceError{AgentID: id, Desc: desc})
			}
			continue
		}
		var fixMsg strings.Builder
		fmt.Fprintf(&fixMsg, "【验收返工 第 %d 轮】验收测试员发现以下问题，请逐一修复并自检后回传：", round)
		for i, desc := range byAgent[id] {
			fmt.Fprintf(&fixMsg, "\n%d. %s", i+1, desc)
		}
		if err := m.d.ReviveWithMessage(ctx, node, fixMsg.String()); err != nil {
			log.Printf("[acceptance] revive %s failed: %v", id, err)
			for _, desc := range byAgent[id] {
				left = append(left, acceptanceError{AgentID: id, Desc: desc})
			}
			continue
		}
		revived = append(revived, id)
		names = append(names, acceptanceDisplayName(node))
	}
	if len(revived) == 0 {
		return revived, left
	}
	// mailbox 抄送 Meta（MsgInfo：无需动作；下一轮会话 drain 时可见）。
	_, _ = m.d.mailbox.Send(&mailbox.Message{
		From: "test_assistant", To: sessionID, Type: mailbox.MsgInfo,
		Subject: "验收返工已派回（你无需动作）",
		Body: fmt.Sprintf("第 %d 轮验收发现 %d 处问题，已派回 %s 修复，完成后将自动复验。此为抄送知悉，无需任何动作，勿重复派发。",
			round, len(items), strings.Join(names, "、")),
	})
	// 用户对话页系统消息：「某某地方有错误，某某领域正在修复（第 N 轮）」。
	brief := truncateRunes(strings.Join(firstDescs(items, 3), "；"), 120)
	m.notifyUser(sessionID, fmt.Sprintf("验收发现 %d 处问题：%s。%s 正在修复（第 %d 轮）。",
		len(items), brief, strings.Join(names, "、"), round))
	return revived, left
}

// notifyUser 经 dispatcher 的 userNotifyFn 回调向用户对话页发系统消息（nil 安全）。
func (m *AcceptanceManager) notifyUser(sessionID, msg string) {
	if m.d.userNotifyFn != nil {
		m.d.userNotifyFn(sessionID, msg)
	}
}

// waitNodeTerminal 轮询树等指定节点进入终态；ctx 取消（会话停止）返回 false。
func (m *AcceptanceManager) waitNodeTerminal(ctx context.Context, t *orchestrator.Tree, id string) (orchestrator.Node, bool) {
	for {
		if n, ok := t.Get(id); ok && acceptanceNodeTerminal(n.Status) {
			return n, true
		}
		select {
		case <-ctx.Done():
			return orchestrator.Node{}, false
		case <-time.After(m.tick()):
		}
	}
}

// waitNodesTerminal 等全部修复节点进入终态（ctx 取消即返回，不等齐）。
func (m *AcceptanceManager) waitNodesTerminal(ctx context.Context, t *orchestrator.Tree, ids []string) {
	for {
		all := true
		for _, id := range ids {
			if n, ok := t.Get(id); !ok || !acceptanceNodeTerminal(n.Status) {
				all = false
				break
			}
		}
		if all {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(m.tick()):
		}
	}
}

// tick 返回等待轮询节拍（默认 30s，测试可注入短节拍）。
func (m *AcceptanceManager) tick() time.Duration {
	if m.waitTick > 0 {
		return m.waitTick
	}
	return acceptanceWaitTick
}

// acceptanceNodeTerminal 判定节点是否终态（复活/完成的等待出口）。
func acceptanceNodeTerminal(s orchestrator.Status) bool {
	switch s {
	case orchestrator.StatusDone, orchestrator.StatusFailed, orchestrator.StatusCancelled, orchestrator.StatusUnverified:
		return true
	}
	return false
}

// acceptanceRosterEntry 是 Agent 名单中的一行（谁负责什么、结果如何）。
type acceptanceRosterEntry struct {
	ID      string
	Role    string
	Domain  string
	Task    string
	Status  string
	Summary string
}

// collectAcceptanceRoster 从权威树收集执行 Agent 名单 + 机器校验摘要。
// 名单排除全部 test_assistant 节点（验收/演示/归因均不承担实现责任）；
// 机器校验摘要汇总非 Done 节点（Failed/Cancelled/Unverified）及其原因。
func collectAcceptanceRoster(t *orchestrator.Tree) (roster []acceptanceRosterEntry, machineSummary string) {
	nodes := t.Snapshot()
	sort.SliceStable(nodes, func(i, j int) bool {
		if nodes[i].Started.Equal(nodes[j].Started) {
			return nodes[i].ID < nodes[j].ID
		}
		return nodes[i].Started.Before(nodes[j].Started)
	})
	var problems []string
	for _, n := range nodes {
		if n.Role == "test_assistant" {
			continue
		}
		roster = append(roster, acceptanceRosterEntry{
			ID: n.ID, Role: n.Role, Domain: n.Domain,
			Task: truncateRunes(n.Task, 120), Status: n.Status.String(),
			Summary: truncateRunes(strings.ReplaceAll(strings.TrimSpace(n.Summary), "\n", " "), 200),
		})
		switch n.Status {
		case orchestrator.StatusFailed, orchestrator.StatusCancelled:
			problems = append(problems, fmt.Sprintf("- %s（%s）%s：%s", n.ID, n.Status, truncateRunes(n.Task, 60), truncateRunes(n.Err, 120)))
		case orchestrator.StatusUnverified:
			problems = append(problems, fmt.Sprintf("- %s 已交付但未验证：%s", n.ID, truncateRunes(n.Err, 120)))
		}
	}
	if len(problems) == 0 {
		machineSummary = "全部节点 Done，无失败/未验证项。"
	} else {
		machineSummary = "以下节点机器侧非 Done，须亲自复核：\n" + strings.Join(problems, "\n")
	}
	return roster, machineSummary
}

// buildAcceptanceTask 构造验收测试员的派发任务文本（任务目标 + Agent 名单 +
// 机器校验摘要 + 报告路径 + 机读契约硬约束）。
// 【接力理由】标记：dispatcher 接力熔断（relay.go）对同 (parent, domain) 第 4 代起
// 拒派无声明任务，验收复验属合法代际，恒带声明防 max_rounds>=4 时误拒。
func buildAcceptanceTask(sessionID string, round int, goal string, roster []acceptanceRosterEntry, machineSummary string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "【验收任务】你是交付验收测试员，这是第 %d 轮验收。\n", round)
	b.WriteString("【接力理由】每轮验收为独立复验，确认上轮修复是否生效并检查回归。\n\n")
	b.WriteString("【任务目标】\n" + truncateRunes(goal, 800) + "\n\n")
	b.WriteString("【执行 Agent 名单】（错误归因用，[agent:id] 照抄下列 ID）\n")
	if len(roster) == 0 {
		b.WriteString("- （无子 Agent 记录，交付物由 Meta 自执行产出，错误归属写 [agent:meta]）\n")
	}
	for _, e := range roster {
		label := e.ID
		if e.Domain != "" {
			label += "（领域：" + e.Domain + "）"
		}
		fmt.Fprintf(&b, "- %s 角色 %s，状态 %s\n  任务：%s\n  结果摘要：%s\n", label, e.Role, e.Status, e.Task, e.Summary)
	}
	b.WriteString("\n【机器校验摘要】\n" + machineSummary + "\n\n")
	fmt.Fprintf(&b, "【报告落盘】把详细中文测试报告写入 .bma/acceptance/%s-r%d.md（相对工作目录）。\n\n", sessionID, round)
	b.WriteString("【机读契约（硬约束）】最终答复末尾必须逐字输出：\n【验收结论】PASS\n或\n【验收结论】FAIL\n【错误清单】\n1. [agent:<agentID>] <问题描述>\n")
	return b.String()
}

// acceptanceReportRef 返回第 round 轮验收报告的相对路径（文件实际落盘才返回，否则空串）。
func acceptanceReportRef(workDir, sessionID string, round int) string {
	if strings.TrimSpace(workDir) == "" {
		return ""
	}
	rel := filepath.Join(".bma", "acceptance", fmt.Sprintf("%s-r%d.md", sessionID, round))
	if _, err := os.Stat(filepath.Join(workDir, rel)); err != nil {
		return ""
	}
	return rel
}

// acceptanceDisplayName 取节点对用户可见的展示名（领域名优先，回退 ID）。
func acceptanceDisplayName(n orchestrator.Node) string {
	if strings.TrimSpace(n.Domain) != "" {
		return n.Domain
	}
	return n.ID
}

// firstDescs 取错误清单前 n 条描述（用户通知摘要用）。
func firstDescs(items []acceptanceError, n int) []string {
	out := make([]string, 0, n)
	for i, it := range items {
		if i >= n {
			break
		}
		out = append(out, it.Desc)
	}
	return out
}
