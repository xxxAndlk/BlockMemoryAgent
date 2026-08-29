package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/dag"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// pasteEnterThreshold 用于区分终端粘贴产生的连续 Enter 与手动回车。
// 连续两次按键间隔小于该阈值时，Enter 被当作多行粘贴的一部分，插入换行而非提交。
const pasteEnterThreshold = 80 * time.Millisecond

// handleInputKey 在输入栏获得焦点时处理键盘事件，返回更新后的 Model 与 bubbletea 命令。
func (m *Model) handleInputKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	now := time.Now()
	// 记录按键时间，用于区分终端粘贴产生的快速连续 Enter 与手动回车。
	defer func() { m.inputBar.lastKeyTime = now }()

	// Alt+V：异步读取剪贴板图片（须在 KeyRunes case 之前拦截，否则 'v'
	// 会被当普通字符插入输入栏）。
	if msg.Alt && msg.String() == "alt+v" {
		return m, m.pasteImageCmd()
	}

	switch msg.Type {
	case tea.KeyEsc:
		// 软停止（TODO #37）：当前会话 Running 时第一次 ESC 进入 2s 武装窗（不清空输入栏），
		// flashMsg 提示"再按一次 ESC 停止所有任务"；窗内第二次 ESC → POST /stop。
		// 武装窗超时或非 Running 会话：行为与现状完全一致（清空输入并离开输入栏）。
		if s := m.selectedSession(); s != nil && s.Status == enums.SessionStatusRunning && !m.stopArmedUntil.IsZero() && time.Now().Before(m.stopArmedUntil) {
			m.stopArmedUntil = time.Time{}
			m.postJSON(fmt.Sprintf("/api/sessions/%s/stop", s.ID), map[string]any{})
			m.flashMsg("软停止已发出：子任务暂停中，可发消息续跑（有销毁倒计时）")
			m.inputBar.runes = nil
			m.inputBar.cursor = 0
			m.inputBar.mode = inputNormal
			m.inputBar.histIdx = -1
			m.inputBar.clearPendingImages()
			return m, nil
		}
		if s := m.selectedSession(); s != nil && s.Status == enums.SessionStatusRunning && m.stopArmedUntil.IsZero() {
			m.stopArmedUntil = time.Now().Add(2 * time.Second)
			m.flashMsg("再按一次 ESC 停止当前会话全部子任务（可续跑，倒计时内任意消息恢复；到期未续跑销毁）")
			return m, nil
		}
		m.stopArmedUntil = time.Time{}
		// Esc 离开输入栏，清空输入并回到对话面板。
		m.focus = panelChat
		m.inputBar.runes = nil
		m.inputBar.cursor = 0
		m.inputBar.mode = inputNormal
		m.inputBar.histIdx = -1
		m.inputBar.clearPendingImages()
		return m, nil

	case tea.KeyEnter:
		// Alt+Enter 或粘贴产生的 Enter 都作为换行插入，普通 Enter 才提交。
		// 很多终端（尤其是 Windows）粘贴多行时不会给每个 KeyEnter 打 Paste 标记，
		// 因此用时间间隔做兜底：连续快速到达的 Enter 视为粘贴的一部分。
		isPasteEnter := msg.Alt || msg.Paste
		if !isPasteEnter && !m.inputBar.lastKeyTime.IsZero() && now.Sub(m.inputBar.lastKeyTime) < pasteEnterThreshold {
			isPasteEnter = true
		}
		if isPasteEnter {
			// 在光标位置插入换行符。
			m.inputBar.runes = append(m.inputBar.runes[:m.inputBar.cursor], append([]rune{'\n'}, m.inputBar.runes[m.inputBar.cursor:]...)...)
			m.inputBar.cursor++
			return m, nil
		}
		// TODO #53：澄清选项导航模式且输入为空时，Enter 提交当前选项
		// （单选=高亮项 ID；多选=勾选集 join ","，后端按 ID 解析）。
		// 输入栏已有文字时走普通提交（自由文本答复优先）。
		if m.clarifyNavActive() && len(m.inputBar.runes) == 0 {
			s := m.selectedSession()
			pc := s.State.PendingClarify
			answer, ok := m.clarifySubmitAnswer(pc)
			if !ok {
				m.flashMsg("请先空格勾选选项（或直接输入文字答复）")
				return m, nil
			}
			// 「其他」逃生选项：不提交，引导用户在输入框填写自由文本答案。
			if answer == types.ClarifyOtherOptionID {
				m.flashMsg("请在输入框输入你的答案，回车提交")
				return m, nil
			}
			label := clarifyOptionLabel(pc.Options, answer)
			if pc.MultiSelect {
				labels := make([]string, 0, len(m.clarifySel))
				for _, id := range m.clarifySel {
					labels = append(labels, clarifyOptionLabel(pc.Options, id))
				}
				label = strings.Join(labels, "、")
			}
			m.postJSON(fmt.Sprintf("/api/sessions/%s/clarify", s.ID), map[string]string{"question_id": pc.ID, "answer": answer})
			m.clarifySel = nil
			m.inputBar.mode = inputNormal
			// 保持焦点在输入栏，方便用户继续操作。
			m.focus = panelInput
			m.flashMsg("已选择: " + label)
			return m, nil
		}
		// 非粘贴 Enter：提交输入。
		cmd := string(m.inputBar.runes)
		if strings.TrimSpace(cmd) != "" {
			m.pushHistory(cmd)
		}
		m.inputBar.histIdx = -1
		m.submitInput(cmd)
		m.inputBar.runes = nil
		m.inputBar.cursor = 0
		m.inputBar.mode = inputNormal
		m.inputBar.clearPendingImages()
		// 发送后先停止跟随底部，等待 tick 把视口滚动到刚发送的用户问题，
		// 避免长回答直接顶掉用户问题。
		m.chatPanel.followBottom = false
		m.chatPanel.pendingScrollToUser = true
		// 保持焦点在输入栏，方便用户连续输入下一条消息。
		m.focus = panelInput
		return m, nil

	case tea.KeyTab:
		// Tab 切换焦点到对话面板。
		m.focus = panelChat
		return m, nil

	case tea.KeyPgUp:
		// 输入栏聚焦时也允许翻页查看对话历史（对话区焦点下同样支持 PgUp/PgDn/j/k）。
		m.chatPanel.vp.HalfViewUp()
		m.chatPanel.followBottom = m.chatPanel.vp.AtBottom()
		m.chatPanel.anchorUser = false
		return m, nil

	case tea.KeyPgDown:
		m.chatPanel.vp.HalfViewDown()
		m.chatPanel.followBottom = m.chatPanel.vp.AtBottom()
		m.chatPanel.anchorUser = false
		return m, nil

	case tea.KeyUp:
		// 澄清选项导航（TODO #53）：↑ 上移高亮选项，优先于历史输入浏览。
		if m.clarifyNavActive() {
			if m.clarifyCursor > 0 {
				m.clarifyCursor--
			}
			return m, nil
		}
		// 向上浏览历史输入。
		h := m.sessionHistory()
		if len(h) == 0 {
			return m, nil
		}
		if m.inputBar.histIdx == -1 {
			m.inputBar.histIdx = len(h)
		}
		if m.inputBar.histIdx > 0 {
			m.inputBar.histIdx--
			m.inputBar.runes = []rune(h[m.inputBar.histIdx])
			m.inputBar.cursor = len(m.inputBar.runes)
		}
		return m, nil

	case tea.KeyDown:
		// 澄清选项导航（TODO #53）：↓ 下移高亮选项，优先于历史输入浏览。
		if m.clarifyNavActive() {
			if pc := m.pendingClarify(); m.clarifyCursor < len(pc.Options)-1 {
				m.clarifyCursor++
			}
			return m, nil
		}
		// 向下浏览历史输入。
		h := m.sessionHistory()
		if m.inputBar.histIdx == -1 {
			return m, nil
		}
		if m.inputBar.histIdx < len(h)-1 {
			m.inputBar.histIdx++
			m.inputBar.runes = []rune(h[m.inputBar.histIdx])
			m.inputBar.cursor = len(m.inputBar.runes)
		} else {
			m.inputBar.histIdx = -1
			m.inputBar.runes = nil
			m.inputBar.cursor = 0
		}
		return m, nil

	case tea.KeyBackspace:
		if m.inputBar.isMultiline() {
			// 多行内容一次性清空，避免逐字符删除长文本。
			m.inputBar.runes = nil
			m.inputBar.cursor = 0
		} else if m.inputBar.cursor > 0 {
			m.inputBar.runes = append(m.inputBar.runes[:m.inputBar.cursor-1], m.inputBar.runes[m.inputBar.cursor:]...)
			m.inputBar.cursor--
		}
		return m, nil

	case tea.KeyDelete:
		if m.inputBar.isMultiline() {
			m.inputBar.runes = nil
			m.inputBar.cursor = 0
		} else if m.inputBar.cursor < len(m.inputBar.runes) {
			m.inputBar.runes = append(m.inputBar.runes[:m.inputBar.cursor], m.inputBar.runes[m.inputBar.cursor+1:]...)
		}
		return m, nil

	case tea.KeyLeft:
		if m.inputBar.cursor > 0 {
			m.inputBar.cursor--
		}
		return m, nil

	case tea.KeyRight:
		if m.inputBar.cursor < len(m.inputBar.runes) {
			m.inputBar.cursor++
		}
		return m, nil

	case tea.KeyHome:
		m.inputBar.cursor = 0
		return m, nil

	case tea.KeyEnd:
		m.inputBar.cursor = len(m.inputBar.runes)
		return m, nil

	case tea.KeyCtrlC:
		// ctrl+c：仍有运行中会话时需二次确认，防止误杀长任务。
		return m.ctrlCQuit()

	case tea.KeyRunes:
		// TODO #53：澄清模式下数字键 1-9 / y / n 快捷选择选项，
		// 命中即消费该键（不进入普通字符插入）；其余字符仍可自由输入文字答复。
		if m.inputBar.mode == inputClarify && len(msg.Runes) == 1 && m.handleClarifyQuickKey(msg) {
			return m, nil
		}
		// 过滤控制字符：Windows 控制台 Ctrl+Space/Ctrl+@ 会产生 0x00 的 KeyRunes，
		// 粘贴文本也可能夹带 NUL；原样入库会被 PG 拒绝（invalid byte sequence 22021）。
		// 保留 \n / \t（多行粘贴的换行与代码缩进经 KeyRunes 路径进入）。
		runes := make([]rune, 0, len(msg.Runes))
		for _, r := range msg.Runes {
			if r == '\n' || r == '\t' || unicode.IsPrint(r) {
				runes = append(runes, r)
			}
		}
		if len(runes) == 0 {
			return m, nil
		}
		// 在光标位置插入输入字符（光标按过滤后的实际插入长度推进）。
		m.inputBar.insertRunes(runes)
		return m, nil
	}

	return m, nil
}

// handleClarifyQuickKey 处理澄清模式下的快捷选择键（TODO #53）：
// 条件为当前会话存在 PendingClarify 且带结构化选项。
// 空格：输入为空时操作当前高亮选项（多选=切换勾选；单选=直接提交）；已有文字时不消费。
// 数字键 1-9：单选立即提交所选选项 ID；多选切换 clarifySel 选择集（Enter 统一提交）。
// y/n：仅确认场景（Kind==confirm 或选项含 confirm/reject ID）可用，立即提交 confirm/reject。
// 返回 true 表示按键已被消费（不进入普通字符插入逻辑）。
func (m *Model) handleClarifyQuickKey(msg tea.KeyMsg) bool {
	s := m.selectedSession()
	if s == nil || s.State == nil || s.State.PendingClarify == nil {
		return false
	}
	pc := s.State.PendingClarify
	if len(pc.Options) == 0 {
		return false
	}
	r := msg.Runes[0]

	// 空格：输入栏为空时作为选项操作键；已有输入内容时按普通字符插入（自由文本可含空格）。
	if r == ' ' {
		if len(m.inputBar.runes) > 0 {
			return false
		}
		opt := pc.Options[m.clarifyCursorClamped(pc)]
		if pc.MultiSelect {
			m.toggleClarifyOption(pc, opt)
			return true
		}
		// 「其他」逃生选项：不提交，引导自由文本输入。
		if opt.ID == types.ClarifyOtherOptionID {
			m.flashMsg("请在输入框输入你的答案，回车提交")
			return true
		}
		m.submitClarifyAnswer(s.ID, pc, opt.ID, opt.Label)
		return true
	}

	// y / n：仅确认场景可用（Kind==confirm，或选项里含 confirm/reject ID）。
	if r == 'y' || r == 'n' {
		isConfirm := pc.Kind == "confirm"
		if !isConfirm {
			for _, o := range pc.Options {
				if o.ID == "confirm" || o.ID == "reject" {
					isConfirm = true
					break
				}
			}
		}
		if !isConfirm {
			return false
		}
		ans := "confirm"
		if r == 'n' {
			ans = "reject"
		}
		m.submitClarifyAnswer(s.ID, pc, ans, clarifyOptionLabel(pc.Options, ans))
		return true
	}

	// 数字键 1-9：序号须在选项范围内，超界不消费（交给普通输入）。
	if r < '1' || r > '9' {
		return false
	}
	idx := int(r - '1')
	if idx >= len(pc.Options) {
		return false
	}
	opt := pc.Options[idx]

	if pc.MultiSelect {
		// 多选：切换选择集，flash 展示当前已选（Enter 统一提交）。
		m.toggleClarifyOption(pc, opt)
		return true
	}

	// 「其他」逃生选项：不提交，引导自由文本输入。
	if opt.ID == types.ClarifyOtherOptionID {
		m.flashMsg("请在输入框输入你的答案，回车提交")
		return true
	}
	// 单选：立即提交所选选项 ID，清空输入并退出澄清模式。
	m.submitClarifyAnswer(s.ID, pc, opt.ID, opt.Label)
	return true
}

// toggleClarifyOption 切换多选选择集中的某个选项，并 flash 展示当前已选标签（TODO #53）。
func (m *Model) toggleClarifyOption(pc *types.ClarifyRequest, opt types.ClarifyOption) {
	found := -1
	for i, id := range m.clarifySel {
		if id == opt.ID {
			found = i
			break
		}
	}
	if found >= 0 {
		m.clarifySel = append(m.clarifySel[:found], m.clarifySel[found+1:]...)
	} else {
		m.clarifySel = append(m.clarifySel, opt.ID)
	}
	labels := make([]string, 0, len(m.clarifySel))
	for _, id := range m.clarifySel {
		labels = append(labels, clarifyOptionLabel(pc.Options, id))
	}
	if len(labels) == 0 {
		m.flashMsg("已取消全部选择（回车直接输入文字答复）")
	} else {
		m.flashMsg("已选: " + strings.Join(labels, "、") + "（回车提交）")
	}
}

// submitClarifyAnswer 提交澄清答复（选项 ID），清空输入并退出澄清模式（TODO #53）。
func (m *Model) submitClarifyAnswer(sessionID string, pc *types.ClarifyRequest, answer, label string) {
	m.postJSON(fmt.Sprintf("/api/sessions/%s/clarify", sessionID), map[string]string{"question_id": pc.ID, "answer": answer})
	m.inputBar.runes = nil
	m.inputBar.cursor = 0
	m.inputBar.mode = inputNormal
	m.clarifySel = nil
	m.flashMsg("已选择: " + label)
}

// clarifyOptionLabel 按选项 ID 查找展示 Label；未命中时回退返回 ID 本身。
func clarifyOptionLabel(opts []types.ClarifyOption, id string) string {
	for _, o := range opts {
		if o.ID == id {
			return o.Label
		}
	}
	return id
}

// submitInput 解析用户输入命令并路由到对应 HTTP 端点。
// 命令语义：
//   - "/new <goal>"         无需选中会话，直接创建新会话
//   - "/cancel"            终止当前运行中会话
//   - "/clarify <id> <ans>" 回复特性5 的人机澄清请求
//   - "/interrupt <text>"   特性6 抢占中断
//   - "/enqueue <text>"     特性6 队列注入
//   - "/dag trigger <id>"   立即触发 DAG
//   - "/dag new <json>"     创建 DAG（JSON 内联）
//   - 其他                  作为普通消息追加到当前会话
//
// 未选中会话时，纯文本输入自动作为新会话的 goal。
func (m *Model) submitInput(cmd string) {
	if strings.TrimSpace(cmd) == "" {
		return
	}

	// Alt+V 粘贴暂存的图片：随本条消息发送。仅普通消息（含无会话首条与
	// /new）承载；澄清答复/其他斜杠命令通道不支持，提示后丢弃。
	imgs := m.inputBar.pendingImages
	if len(imgs) > 0 {
		head := strings.TrimSpace(cmd)
		if m.inputBar.mode == inputClarify || (strings.HasPrefix(head, "/") && !strings.HasPrefix(head, "/new")) {
			m.flashMsg("图片仅支持普通消息（含 /new），本次不带图发送")
			imgs = nil
		}
	}

	// TODO #53：澄清模式下非命令输入直接作为答复提交（自由文本或选项文本），
	// 走 /api/sessions/{id}/clarify 通道；斜杠命令仍按命令解析。
	if m.inputBar.mode == inputClarify && !strings.HasPrefix(strings.TrimSpace(cmd), "/") {
		if s := m.selectedSession(); s != nil && s.State != nil && s.State.PendingClarify != nil {
			if id := s.State.PendingClarify.ID; id != "" {
				m.postJSON(fmt.Sprintf("/api/sessions/%s/clarify", s.ID), map[string]string{"question_id": id, "answer": cmd})
				return
			}
		}
		// question_id 取不到（PendingClarify 已变化）时回退普通 sendMessage 路径。
	}

	// 命令解析用去空白版本；消息发送保留原始内容，避免多行粘贴时首行缩进被吞。
	trimmed := strings.TrimSpace(cmd)
	parts := strings.Fields(trimmed)
	if len(parts) == 0 {
		return
	}

	// /new <goal...> 无需选中会话即可创建新会话。
	if parts[0] == "/new" && len(parts) > 1 {
		goal := strings.TrimSpace(strings.TrimPrefix(trimmed, "/new "))
		m.createSession(goal, imgs...)
		return
	}

	// 纯本地命令：无需选中会话即可执行（P2-1 命令模式补全）
	switch parts[0] {
	case "/help":
		m.openHelpPopup()
		return
	case "/agents":
		// 选中会话时打开 agent 拓扑面板；无会话则提示
		if m.selectedSession() == nil {
			m.flashMsg("no active session")
			return
		}
		m.toggleAgentsPopup()
		return
	}

	s := m.selectedSession()
	if s == nil {
		// 先本地预展示首条消息，确保用户按下回车后立刻在对话区看到自己的输入，
		// 避免欢迎页停留造成"第一个问题未记录"的错觉。
		m.chatPanel.pendingFirstMessage = cmd
		m.rebuildChatContent()
		m.createSession(cmd, imgs...)
		return
	}

	// 需选中会话的本地命令（P2-1）
	switch parts[0] {
	case "/status":
		// 展示当前会话状态摘要
		m.flashMsg(fmt.Sprintf("session %s: status=%s agents=%d", s.ID, s.Status, len(m.agentTreePanel.nodes)))
		return
	case "/clear":
		// 重置主对话区滚动到最新（chat 由服务端事件驱动，本地仅重置视图位置）
		m.chatPanel.followBottom = true
		m.chatPanel.anchorUser = false
		m.chatPanel.vp.GotoBottom()
		m.flashMsg("chat scrolled to bottom")
		return
	case "/topic":
		// /topic <name> [goal...]：触发话题切换（后端 HandleSessionTopic）
		if len(parts) < 2 {
			m.flashMsg("usage: /topic <name> [goal]")
			return
		}
		name := parts[1]
		goal := strings.TrimSpace(strings.TrimPrefix(trimmed, "/topic "+name))
		m.postJSON(fmt.Sprintf("/api/sessions/%s/topic", s.ID), map[string]string{"name": name, "goal": goal})
		m.flashMsg("switching topic: " + name)
		return
	case "/topics":
		// 话题列表由服务端在 topic_switch 事件中推送，内联显示为分隔线；
		// 此处打开完整记录面板便于翻阅历史话题切换点
		m.toggleLogPopup()
		return
	case "/memory":
		// /memory <query>：手动检索 Agent 记忆（POST /api/memory/search）
		if len(parts) < 2 {
			m.flashMsg("usage: /memory <query>")
			return
		}
		query := strings.TrimSpace(strings.TrimPrefix(trimmed, "/memory "))
		m.postJSON("/api/memory/search", map[string]string{"query": query})
		m.flashMsg("memory search: " + query)
		return
	}

	// /clarify <id> <answer...>：回复指定 id 的澄清问题。
	if parts[0] == "/clarify" && len(parts) >= 3 {
		id := parts[1]
		// 用 TrimPrefix 而非 Fields 拼接 answer，保留 answer 内的空格
		answer := strings.TrimSpace(strings.TrimPrefix(trimmed, "/clarify "+id))
		m.postJSON(fmt.Sprintf("/api/sessions/%s/clarify", s.ID), map[string]string{"question_id": id, "answer": answer})
		return
	}

	// /cancel — 终止当前运行中会话
	if parts[0] == "/cancel" {
		m.postJSON(fmt.Sprintf("/api/sessions/%s/cancel", s.ID), map[string]any{})
		m.flashMsg("session cancelled")
		return
	}

	// /interrupt <goal...>：以新目标抢占当前会话。
	if parts[0] == "/interrupt" && len(parts) > 1 {
		content := strings.TrimSpace(strings.TrimPrefix(trimmed, "/interrupt "))
		m.postJSON(fmt.Sprintf("/api/sessions/%s/interrupt", s.ID), map[string]string{"content": content})
		m.flashMsg("interrupted")
		return
	}

	// /enqueue <text...>：将文本注入当前会话队列。
	if parts[0] == "/enqueue" && len(parts) > 1 {
		content := strings.TrimSpace(strings.TrimPrefix(trimmed, "/enqueue "))
		m.postJSON(fmt.Sprintf("/api/sessions/%s/enqueue", s.ID), map[string]string{"content": content})
		return
	}

	// /dag trigger <id>：触发指定 id 的 DAG。
	if len(parts) >= 3 && parts[0] == "/dag" && parts[1] == "trigger" {
		m.postJSON(fmt.Sprintf("/api/dag/%s/trigger", parts[2]), map[string]any{})
		return
	}

	// /dag new <json...>：以内联 JSON 创建新 DAG。
	if len(parts) >= 3 && parts[0] == "/dag" && parts[1] == "new" {
		var d dag.DAG
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(trimmed, "/dag new "))), &d); err != nil {
			m.flashMsg("invalid dag json: " + err.Error())
			return
		}
		m.postJSON("/api/dag", d)
		return
	}

	// default: message（保留原始多行内容，包括缩进与换行；图片非空时随消息携带）
	if len(imgs) > 0 {
		m.postJSON(fmt.Sprintf("/api/sessions/%s/message", s.ID), map[string]any{
			"content": cmd,
			"images":  imgs,
		})
		return
	}
	m.postJSON(fmt.Sprintf("/api/sessions/%s/message", s.ID), map[string]string{"content": cmd})
}

// postJSON 向本地 TUI 后端发 POST 请求。
// 失败时自动重试 2 次（间隔 500ms），仅对网络/连接错误重试，4xx/5xx 不重试。
// 失败仅写 flashMsg 提示，不阻塞 TUI 主循环。
// 异步执行（T2 修复：原同步阻塞主循环最差 ~10s 冻结键盘/tick）。
func (m *Model) postJSON(path string, body any) {
	go func() {
		addr := m.httpAddr
		if addr == "" {
			addr = "http://localhost:10010"
		}
		data, err := json.Marshal(body)
		if err != nil {
			m.flashMsg("marshal error: " + err.Error())
			return
		}
		client := &http.Client{Timeout: requestTimeout}

		const maxRetries = 2
		var lastErr error
		for attempt := 0; attempt <= maxRetries; attempt++ {
			if attempt > 0 {
				// 重试前等待 500ms。
				time.Sleep(500 * time.Millisecond)
			}
			req, err := http.NewRequest(http.MethodPost, addr+path, bytes.NewReader(data))
			if err != nil {
				m.flashMsg("request error: " + err.Error())
				return
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				lastErr = err
				continue
			}
			// 排空响应体以便连接复用（Step 8）。
			_, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode >= 400 {
				m.flashMsg(fmt.Sprintf("%s returned %d", path, resp.StatusCode))
			}
			return
		}
		m.flashMsg("post error (retried): " + lastErr.Error())
	}()
}

// 异步执行（T2 修复）：成功后经 sharedState 写 pendingSelectID，由 tick handler 在
// 主循环内执行 refreshSessions + selectSession，避免后台 goroutine 直接改
// m.sessions/cursor 与 View 产生 race。sharedState 为指针共享（#47 修复），
// 写入对所有 Model 拷贝可见，不会因 bubbletea 值语义落到废弃副本上。
func (m *Model) createSession(goal string, images ...tool.ResultImage) {
	go func() {
		if m.agent == nil {
			m.flashMsg("agent facade not available")
			return
		}
		created, err := m.agent.CreateSession(context.Background(), agent.CreateRequest{Goal: goal, Images: images})
		if err != nil {
			m.flashMsg("create session: " + err.Error())
			return
		}
		// 写 pendingSelectID，tick handler 消费时在主循环内 refresh+select
		m.ensureShared().setPendingSelect(created.ID)
		m.flashMsg("session started: " + created.ID)
	}()
}

// getJSON 向本地 TUI 后端发 GET 请求并 JSON 解码到 dst。
// 失败返回 error，调用方自行处理（如显示 flash 或退回空结果）。
func (m *Model) getJSON(path string, dst any) error {
	addr := m.httpAddr
	if addr == "" {
		addr = "http://localhost:10010"
	}
	client := &http.Client{Timeout: requestTimeout}
	resp, err := client.Get(addr + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		_, _ = io.ReadAll(resp.Body)
		return fmt.Errorf("%s returned %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

// requestTimeout 是本地 HTTP 请求的超时时间。
const requestTimeout = 3 * time.Second
