## 待完成 / 开放项

> 已完成项全部迁出：明细见 doc/变更.md 与 git 历史。本文件只保留待办。

1. **ReAct 端到端集成测试**（主体完成，剩余补充项）
   - 现状：`test/coding/*`、`test/api/*` 全部跑通；mock SSE 已支持真实增量语义（工具参数/文本跨 delta 分片，`stream_fragment_test.go` 全栈验证拼接，2026-08-27）；端点覆盖含 profile / plugins / metrics / timeline / activity。
   - 剩余：随新端点持续补 e2e；mock 未覆盖多 chunk 并行 tool_calls（index>0 拼接）。

2.  **黑板剩余两件**（Phase B 已落地：scope 标注+兄弟产出摄取+salvage 统一收口；以下是残余）
    - **子 Agent 上下文按 scope 切片注入**：替代父注入全量 global spec——08-13 身份混淆根因，renderSpecPrefix【范围】锚定已部分做，扩展为按 scope 切片注入（ReActAgent Assemble 经黑板取相关兄弟产出+fact 切片）。改坏会复发身份混淆老病，与提示词修复强耦合。
    - **mailbox 降级为信号层**：仅传"有新产出"信号+黑板 key 指针，父 drain 后按指针取黑板；不删 mailbox。级联影响 salvage/校验/打捞链路，回归面大。
    - 验收口径沿用原文：同域重派不再发生；DomainAgent 等兄弟时读黑板决收口而非空转叙事；TUI 可观测黑板检索命中。
    - 建议：等真机多轮塔防 token/质量数据再决定是否做（当前瓶颈不在通信 token）。

3.  **去调度硬化件：仅剩心跳 watchdog 退役**
    - 已退役：探索预算、连杀指纹、同域去重 findPendingDomainSibling、等待叙事空转门（2026-08-14 批次）；spec 强制门+轮/token/墙钟终止条件按决策永久保留。
    - 剩余：删 patrol/killStuckSubAgent。退役条件=连续 N 个塔防任务零 HEARTBEAT KILL（误杀根因已修两次：08-27 任务 112 热驻漏注 ActivityReporter、115 流首块前 300s 看门狗，均尚无真机观察数据）。
    - 动作：重启 TUI 跑塔防拿观察数据→达标后删 watchdog（纯减法，含测试）。

4.  **子 Agent 收尾事实提取异步化**（同步阻塞 DONE 通知，实证最长 69s；常规 4-7s 无感）
    - 方向：事实提取挪异步 goroutine + 落库完成事件，摄取侧按事件等待而非假设 DONE 即可见。
    - 复杂点：黑板兄弟摄取依赖事实落库顺序，异步引入"已 DONE 但事实未落库"可见性竞态；失败打捞/校验分层消费同一结果需专门设计。
    - 记账缺口已修（2026-08-27 实证 lightweight llm_input/llm_output 已写 session_logs，model= 有值）。
    - 建议：缓做——长尾只在评测出现，收益<竞态风险；除非实测频繁伤交互。

5.  **UI 测试工作流：多步桌面点击场景测试（先询问后演示）+ 上下文压缩 auto/manual 双模式**
    - 背景（原始需求原话）：测试工作流添加自动桌面点击测试，分多个测试——第一个测试点击某个按钮查看反应，第二个测试滑动某个桌面再点击某个按钮的一套业务流程；每次测试流程启动前询问是否开始演示，点击后才操控电脑演示。另：上下文压缩支持自动与手动两种模式——当前为自动压缩（到达某个上下文值就自动压缩并保持在阈值附近），手动模式则和其他工具一样由用户自己手动管理上下文。
    - 子项 1 设计（多步桌面点击场景测试，走既有 computer_use 插件与审批链，零新通道）：
      1. 前置：computer_use 插件默认 disabled（`plugins.yaml`），此类任务会话显式启用（Agent 经 plugin_enable 或用户 HTTP API / 对话批准安装门）；容器沙箱场景起 `docker compose --profile computer`（Xvfb + noVNC），本机场景直接操控用户桌面。
      2. 场景进 spec：MetaAgent 把 UI 交互验证需求写成有序场景清单 `[{场景名, 步骤: [{动作: click|swipe|type|wait|screenshot, 目标: 元素自然语言描述/坐标区域}, ...]}]`——落在 WriteSpec 的 acceptance/scene 维度（对齐 verify_levels 与场景化截图的场景概念），人可在派发前审改。
      3. 启动门（先询问后演示）：每个场景执行前经 ask_user 结构化确认（复用选项协议 Kind=confirm，选项=开始演示/跳过该场景/终止全部测试）；批准后 agent 逐步骤调 computer_use 工具（screenshot 定位 -> 动作执行 -> 截图对比）；未批准场景标 skipped 不执行。
      4. 证据与判定：每步截图落会话临时目录并把绝对路径写进产出；汇总为「场景 N：步骤 M 成功/失败 + 截图证据引用」的视觉验证证据——对接 acceptance 计分的 screenshot/probe 证据扫描与冒烟层 verify_missing 反馈通道：场景有败则算未通过打回责任域，修好后只重跑失败场景。
      5. 安全边界不变：computer_use 全部工具 `Destructive()=true` 审批守卫链保持兜底（启动门是流程性预告，单步拦截仍靠审批链）；角色白名单仅限显式启用者。
    - 子项 1 测试：mock computer_use MCP server 下——三场景清单顺序执行、启动门选跳过该场景被标 skipped、步骤失败截断后续并回传失败场景号；真实环境人工冒烟（登录 -> 滑动 -> 点按钮两场景）。
    - 子项 1 验收：发起带 UI 交互验收的任务时，日志可见逐场景 ask_user 确认 -> 截图证据落盘 -> 完成/打回结论；插件未启用/沙箱未起时给明确不可用原因而非挂起等待。
    - 子项 2 设计（上下文压缩 auto/manual 双模式）：
      1. 现状：仅 auto——每 Agent 150K 阈值即触发压缩（任务 45），保留近 10 条、其余压成摘要块，压完回落阈值附近；无手动入口。
      2. 配置：`config.yaml` 加 `context_compression_mode: auto|manual`（默认 auto，不改现状语义）。
      3. manual 语义：Pipeline 不再按 token 阈值自动压缩；新增 TUI 命令 `/compact [keep_n]` 触发即时压缩（keep_n 缺省取默认近 10 条）；TUI 状态栏常驻显示当前会话 token 用量 / 150K 百分比，>=80% 黄色提示建议 /compact。触达硬上限（LimitReached 暂停）时暂停文案追加「输入 /compact 压缩上下文后继续」引导。
      4. HTTP 同步暴露 `POST /api/sessions/{id}/compact`，web 端按钮走同一通路。
    - 子项 2 测试：manual 模式下长会话超阈值不自动压缩 + /compact 后窗口收缩摘要生成、首条 user 与看板注入段保留；auto 模式回归零变化；HTTP compact 对非活跃会话幂等。
    - 子项 2 验收：双开关切换行为各自正确；manual 模式长跑塔防会话中用户 /compact 后 MetaAgent 继续编排不丢目标。
    - 不做：不做场景自动录制回放（首版场景清单全由 MetaAgent 写、人审后执行）；不做鼠标轨迹拟人化；manual 模式不做 per-Agent 差异化阈值（沿用统一 150K）；不做跨会话压缩结果复用。

6. **Web 端 UI 重设计（浅色优先双主题 + 分组导航 + 会话双视图单页）**（2026-09-03 立项；预览图 01–16 已逐页确认；spec 与 15 任务实现计划已落稿；**待执行**）
   - 依据文档：
     - 设计定稿：`docs/superpowers/specs/2026-09-03-web-ui-redesign-design.md`
     - 实现计划（含全部代码）：`docs/superpowers/plans/2026-09-03-web-ui-redesign.md`
     - 视觉基准：`doc/image/redesign/01–16.png`（HTML 源稿 `doc/image/redesign/src/`）
   - 前置待决（已问未答，开工前必须先定）：
     - [ ] git 策略三选一：① 建 feature 分支 + 逐任务 commit（同上一特性 SDD 做法，评审包依赖 commit 区间）；② 留 main + 逐任务 commit；③ 全程不 commit，评审改用工作区 diff 快照
   - 全局约束（所有任务遵守，详见计划 Global Constraints）：
     - 不改后端；`/api/memory/search|levels|eval` 是 ReAct 重构后的空壳桩，**禁止接入**
     - 设计 token 精确值：主色 `#4F5DFF`（dark `#6B77FF`）、主色浅底 `#EEF0FF`（dark `#232947`）、页面 `#F6F7F9`（dark `#0F1115`）、卡片 `#FFFFFF`（dark `#1A1D24`）、边框 `#E5E7EB`（dark `#2A2D35`）；语义色 成功`#16A34A`/失败`#DC2626`/警告`#D97706`/停用`#9CA3AF`；圆角 10px；弱阴影
     - 类名确定性映射表（`#0f1115→bg-page`、`#1a1d24→bg-card`、`#2a2d35→border-line`、`gray-200/300→text-ink` 等，全文见计划 §3）
     - 每任务收尾验证：`cd web && npm run build`（vue-tsc 零报错）+ 对照指定预览图
     - 新图标必须在 `web/src/main.ts` 的 import 与 icons 数组同时注册
   - 任务清单（依赖序执行；5a→6→7→8→5b 为一组耦合单元）：
     - [ ] **Task 1 主题基础设施**：重写 `web/src/style.css`（`:root` 浅色 + `html.dark` 深色全套 `--bma-*`/`--el-*` 变量）；重写 `web/tailwind.config.js`（`darkMode:'class'` + 语义色映射变量 + 旧色名别名）；新建 `web/src/composables/useTheme.ts`（localStorage `bma:theme`，模块加载即应用防闪烁）；`main.ts` 注册 `DataAnalysis/FolderAdd/Moon/SetUp/Sunny`
     - [ ] **Task 2 Layout 重构**：全量重写 `web/src/layout/index.vue`——顶栏（品牌 + Postgres/Redis/LLM 健康点 + 主题切换 + 设置入口）；侧栏静态 5 分组（工作台：首页/会话；项目：工作目录；资源库：技能库/插件/知识库；记忆：记忆中心/用户画像/人格配置/会话历史；系统：系统设置）；底部「工作流编排 Beta」虚线项；删原硬编码人格下拉，人格名移侧栏底部只读（`/api/status`）
     - [ ] **Task 3 路由重排**：重写 `web/src/router/index.ts`——12 业务路由（新增 `projects/memory/workflow`，删除 `chat/project-prefs`）+ 2 重定向（`/chat`→`/session` 保留 query；`/project-prefs`→`/projects`）；建 `workflow` 占位页 + `projects/memory` 临时占位（防构建断裂）
     - [ ] **Task 4 首页改造**（对照 01）：`dashboard/index.vue` 应用映射表 + 替换 scoped style + 三处定点（mock 提示条双主题色、趋势 SVG 容器/描边色、品牌插画保留）
     - [ ] **Task 5a 文件搬迁**：`views/chat/components`→`views/session/chat/components`、`views/chat/utils`→`views/session/chat/utils`；删 `views/chat/index.vue`；`dashboard` 的 eventStyles 引用改 `@/views/session/chat/utils/eventStyles`；`grep -rn "@/views/chat"` 零残留
     - [ ] **Task 6 对话视图**：新建 `views/session/chat/ChatView.vue`（props `session/events/agents/clarify/sending/inputTokens/outputTokens`，emits `submit/cancel/stop/interrupt/new-session/clarify-submitted`，包 ChatHeader+MessageList+ChatInput）；chat 下 7 组件纯样式换 token（ChatHeader 三个操作按钮给定点色）
     - [ ] **Task 7 监控视图**：新建 `views/session/monitor/MonitorView.vue`（中栏三 Tab：执行日志/Skill装配/日志分析；原「文件预览」Tab 移除改由右栏承担；`defineModel` 桥接 SessionLogsPanel 的 `agent/level/expandedLogId`）；监控 8 组件换 token
     - [ ] **Task 8 右栏三新面板**：新建 `panels/TaskBoardPanel.vue`（角色树+任务+进度+约束，`toRef` 喂 useRoleTree/useTaskBoard）、`panels/ToolPanel.vue`（tool_call 列表，按时间戳配对同名 tool_exec 取输出，el-collapse 展开参数/输出/错误）、`panels/SessionMemoryPanel.vue`（evolution_log + learned_skills 按 `source_session` 过滤）
     - [ ] **Task 5b 会话容器**：全量重写 `views/session/index.vue`——合并原 chat 页全部状态逻辑（openSession/SSE/submit/clarify/cancel/stop/interrupt/enqueue）与监控面板加载（board/agents/metrics/mailbox/health/logs/tokenMetrics）；左 280px 会话列表；中栏标题 +「💬 对话 | 📊 监控」分段切换（`?view=` query 同步，chat 缺省不落 URL）；右 320px 统一 5 Tab（任务看板/工具/文件/记忆/指标）；支持 `?work_dir=` 预填固化工作目录；此刻跑构建（对照 02/03/11/13/14/15）
     - [ ] **Task 9 工作目录页**（对照 04）：新建 `views/projects/index.vue`——listSessions 按 `work_dir` 聚合卡片（路径等宽/会话数/运行中数/最近活跃）+ localStorage `bma:workdirs` 手工目录 + WorkDirPicker 添加；「发起新会话」→`/session?work_dir=`、「查看会话」→`/history?work_dir=`；右侧 420px 项目偏好编辑器（原 project-prefs 逻辑按目录参数加载，dirty/保存/刷新）
     - [ ] **Task 10 技能库**（对照 05）：新增「内置技能」Tab（`GET /api/skills`，只读卡片 name/domain/description/tool_ref/cost/tags），Tab 序：经验技能/内置技能/进化日志；全文件映射表换 token
     - [ ] **Task 11 插件页**（对照 06）：新增 `transportBadge()` 约定（`host_computer_use`→「宿主直控」warning、`computer_use`→「Docker 沙箱」info、`kind==='service'`→「HTTP 服务」；PluginInfo 无 transport 字段按 id/kind 判定）；`host_computer_use` 卡片加「直控宿主机 GUI，勿与沙箱版同时启用」提示行；换 token
     - [ ] **Task 12 记忆中心**（对照 07/16）：新建 `views/memory/index.vue`——顶部项目下拉（全部项目 + 会话去重 work_dir）+ 类型 chip（全部/用户偏好/项目经验/技能新建/技能更新）；统计卡（经验技能/启用中/沉淀条目/涉及会话）；左沉淀时间线 + 右（经验技能列表 + 用户画像摘要→/profile + 项目偏好摘要→/projects）；项目归属经 `source_session→session→work_dir` 反查，无 source_session 为全局条目仅「全部项目」可见；**不接空壳 memory API**
     - [ ] **Task 13 记忆分组其余页**：profile/knowledge/FeaturePlaceholder/AppCard 换 token；soul 全量重写（当前人格 + LLM + `config/soul.md` 路径 + 热切换规划中说明）；history 全量重写为表格页（搜索 + 状态/目录过滤 + `?work_dir=` 预过滤 + 行点击 →`/session?id=&view=monitor`）
     - [ ] **Task 14 设置 + 工作流**（对照 08/09）：settings 全量重写（外观主题单选 useTheme、服务信息 `/api/status`、连接健康 `/api/health`、资源目录 BMA_HOME 静态说明卡）；workflow 正式占位版（SetUp 图标 + Beta 徽标 + 三项规划能力清单；注意补注册 `Share` 图标）
     - [ ] **Task 15 收尾**：删 `web/src/views/project-prefs/`；残留扫描（`@/views/chat`、`project-prefs`、`#0f1115/#1a1d24/#14161a/#2a2d35/#1e3a8a` 零命中，dashboard 品牌插画渐变除外）；全量构建零报错；逐页对照预览图（含深色主题复查）；功能冒烟 5 项（`/chat` 重定向、`/project-prefs` 重定向、目录→新会话 work_dir 生效、会话页 SSE/软停止/中断/工具 Tab、刷新主题保持）
   - 执行方式：SDD 子代理逐任务（实现者 + 任务评审 + 终审），进度台账 `.superpowers/sdd/progress.md` 另起本节
   - 验收口径：12 路由 + 2 重定向全部可用；浅色默认深色可切且刷新保持；会话页双视图右栏 5 Tab 数据真实（工具=事件流、记忆=沉淀过滤、指标=四卡）；工作目录页可添加目录并带目录发起新会话；视觉与 `doc/image/redesign/` 对应页结构一致
   - 不做：TUI 不重做（"看着办"）；不引入前端测试框架与新依赖；不做后端任何改动；人格在线编辑、知识库实体功能、工作流编排实体功能均只留占位
