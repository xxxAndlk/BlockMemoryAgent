package tui

// 面板索引常量：用于标识当前焦点所在的面板。
const (
	// panelChat 表示焦点在对话面板，可通过 j/k 等按键滚动浏览消息。
	panelChat int = iota
	// panelInput 表示焦点在底部输入栏，可直接输入命令或消息。
	panelInput
)

// 弹窗模式常量：标识当前打开的浮层面板类型，overlayNone 表示无弹窗。
const (
	// overlayNone 表示当前没有打开任何弹窗。
	overlayNone int = iota
	// overlayHelp 表示打开帮助弹窗。
	overlayHelp
	// overlayPlan 表示打开执行计划弹窗。
	overlayPlan
	// overlayAgents 表示打开 Agent 编排弹窗。
	overlayAgents
	// overlayDetail 表示打开某条消息/任务的详情弹窗。
	overlayDetail
	// overlayLog 表示打开完整记录弹窗。
	overlayLog
	// overlayDAG 表示打开 DAG 弹窗（预留）。
	overlayDAG
	// overlayRuntime 表示打开运行时信息弹窗（预留）。
	overlayRuntime
)

// 输入栏模式常量：用于切换输入栏的提示符与命令解析逻辑。
const (
	// inputNormal 表示普通输入模式，用于发送消息或执行普通命令。
	inputNormal int = iota
	// inputClarify 表示澄清答复模式，提示符为 /clarify>。
	inputClarify
	// inputInterrupt 表示抢占中断模式，提示符为 /interrupt>。
	inputInterrupt
	// inputEnqueue 表示队列注入模式，提示符为 /enqueue>。
	inputEnqueue
)

// fullHelpText 是 TUI 全局帮助文本，用户按 ? 键时通过弹窗展示。
const fullHelpText = `
	键位
	  /                聚焦底部输入栏
	  K                聚焦底部输入栏（Command Palette）
	  1 / Esc          关闭弹窗，回到对话
	  2 / p            打开 / 关闭 计划看板弹窗
	  3 / a            打开 / 关闭 Agent 编排弹窗
	  4 / l / Ctrl+L   打开 / 关闭 完整记录弹窗（可滚动查看全部输出）
	  Ctrl+B           切换右侧计划/Agent 分栏显示（自动/强制显示/强制隐藏）
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
	  对话区只显示最近记录，更早的记录按 Ctrl+L 查看完整记录
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
	  Alt+V             粘贴剪贴板图片/视频（资源管理器复制的视频文件自动识别为视频附件，插入 [image:N]/[video:N] 占位符随消息发送，服务端对视频抽帧后走图片链路；图片最多 4 张、视频最多 2 个）
	  ↑/↓               浏览历史输入

	底部快捷键栏：K Command Palette  P Plan  A Agents  L Logs  M Memory  G Git Diff  S Settings  ? Help  Ctrl+C Exit
	`
