package tui

// Panel indices.
const (
	panelChat int = iota
	panelInput
)

// Overlay modes.
const (
	overlayNone int = iota
	overlayHelp
	overlayPlan
	overlayAgents
	overlayDetail
	overlayLog
	overlayDAG
	overlayRuntime
)

// Input modes.
const (
	inputNormal int = iota
	inputClarify
	inputInterrupt
	inputEnqueue
)

// Full help text.
const fullHelpText = `
键位
  /                聚焦底部输入栏
  K                聚焦底部输入栏（Command Palette）
  1 / Esc          关闭弹窗，回到对话
  2 / p            打开 / 关闭 计划看板弹窗
  3 / a            打开 / 关闭 Agent 编排弹窗
  4 / l / Ctrl+L   打开 / 关闭 完整记录弹窗（可滚动查看全部输出）
  m                Memory 面板占位提示
  g                Git Diff 占位提示
  s                Settings 占位提示
  ?                帮助
  Esc              关闭弹窗 / 离开输入栏 / 回到对话区
  ctrl+c           退出 TUI（任意状态下生效）

对话面板
  j/k  按 item 上下滚动  g/G 首/尾  PgUp/PgDn 半屏滚动  Enter 展开详情
  Home/End  跳到首/尾
  鼠标滚轮可在对话区滚动
  发送消息后焦点停在输入栏，按 Esc 可切回对话面板用 j/k 滚动；
  或随时按 Ctrl+L 打开完整记录面板查看全部输出

输入栏
  /new <goal>       创建新会话（首次启动对话）
  直接回车          向当前会话发送消息
  /clarify id ans   答复澄清
  /cancel           终止当前运行中会话
  /interrupt goal   抢占中断
  /enqueue text     队列注入
  /topic <name> [goal]  切换/创建话题
  /topics           打开完整记录翻阅话题切换点
  /memory <query>   手动检索 Agent 记忆
  /agents           打开 Agent 编排面板
  /status           显示当前会话状态
  /clear            重置主对话区滚动到底部
  /help             打开帮助
  /dag trigger <id> 触发 DAG
  /dag new <json>   创建 DAG
  Alt+Enter         输入换行（多行）
  ↑/↓               浏览历史输入

底部快捷键栏：K Command Palette  P Plan  A Agents  L Logs  M Memory  G Git Diff  S Settings  ? Help  Ctrl+C Exit
`
