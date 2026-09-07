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

6. **Web 端 UI 重设计（浅色优先双主题 + 分组导航 + 会话双视图单页）**（2026-09-03 立项；预览图 01–16 已逐页确认；spec 与 15 任务实现计划已落稿；**代码实施完成（2026-09-03，全量构建零报错）；仅剩浏览器端视觉走查与功能冒烟待人工执行**）
   - 依据文档：
     - 设计定稿：`docs/superpowers/specs/2026-09-03-web-ui-redesign-design.md`
     - 实现计划（含全部代码）：`docs/superpowers/plans/2026-09-03-web-ui-redesign.md`
     - 视觉基准：`doc/image/redesign/01–16.png`（HTML 源稿 `doc/image/redesign/src/`）
   - 前置待决（已问未答，开工前必须先定）：
     - [x] git 策略已定：③ 全程不 commit（执行约束禁止 git 变异操作），评审改用工作区 diff 快照
   - 全局约束（所有任务遵守，详见计划 Global Constraints）：
     - 不改后端；`/api/memory/search|levels|eval` 是 ReAct 重构后的空壳桩，**禁止接入**
     - 设计 token 精确值：主色 `#4F5DFF`（dark `#6B77FF`）、主色浅底 `#EEF0FF`（dark `#232947`）、页面 `#F6F7F9`（dark `#0F1115`）、卡片 `#FFFFFF`（dark `#1A1D24`）、边框 `#E5E7EB`（dark `#2A2D35`）；语义色 成功`#16A34A`/失败`#DC2626`/警告`#D97706`/停用`#9CA3AF`；圆角 10px；弱阴影
     - 类名确定性映射表（`#0f1115→bg-page`、`#1a1d24→bg-card`、`#2a2d35→border-line`、`gray-200/300→text-ink` 等，全文见计划 §3）
     - 每任务收尾验证：`cd web && npm run build`（vue-tsc 零报错）+ 对照指定预览图
     - 新图标必须在 `web/src/main.ts` 的 import 与 icons 数组同时注册
   - 任务清单（依赖序执行；5a→6→7→8→5b 为一组耦合单元）：
     - [x] **Task 1 主题基础设施**：重写 `web/src/style.css`（`:root` 浅色 + `html.dark` 深色全套 `--bma-*`/`--el-*` 变量）；重写 `web/tailwind.config.js`（`darkMode:'class'` + 语义色映射变量 + 旧色名别名）；新建 `web/src/composables/useTheme.ts`（localStorage `bma:theme`，模块加载即应用防闪烁）；`main.ts` 注册 `DataAnalysis/FolderAdd/Moon/SetUp/Sunny`
     - [x] **Task 2 Layout 重构**：全量重写 `web/src/layout/index.vue`——顶栏（品牌 + Postgres/Redis/LLM 健康点 + 主题切换 + 设置入口）；侧栏静态 5 分组（工作台：首页/会话；项目：工作目录；资源库：技能库/插件/知识库；记忆：记忆中心/用户画像/人格配置/会话历史；系统：系统设置）；底部「工作流编排 Beta」虚线项；删原硬编码人格下拉，人格名移侧栏底部只读（`/api/status`）
     - [x] **Task 3 路由重排**：重写 `web/src/router/index.ts`——12 业务路由（新增 `projects/memory/workflow`，删除 `chat/project-prefs`）+ 2 重定向（`/chat`→`/session` 保留 query；`/project-prefs`→`/projects`）；建 `workflow` 占位页 + `projects/memory` 临时占位（防构建断裂）
     - [x] **Task 4 首页改造**（对照 01）：`dashboard/index.vue` 应用映射表 + 替换 scoped style + 三处定点（mock 提示条双主题色、趋势 SVG 容器/描边色、品牌插画保留）
     - [x] **Task 5a 文件搬迁**（另发现并修正计划遗漏：`session/components/ExecutionLog.vue` 也引用了旧 `@/views/chat` 路径）：`views/chat/components`→`views/session/chat/components`、`views/chat/utils`→`views/session/chat/utils`；删 `views/chat/index.vue`；`dashboard` 的 eventStyles 引用改 `@/views/session/chat/utils/eventStyles`；`grep -rn "@/views/chat"` 零残留
     - [x] **Task 6 对话视图**：新建 `views/session/chat/ChatView.vue`（props `session/events/agents/clarify/sending/inputTokens/outputTokens`，emits `submit/cancel/stop/interrupt/new-session/clarify-submitted`，包 ChatHeader+MessageList+ChatInput）；chat 下 7 组件纯样式换 token（ChatHeader 三个操作按钮给定点色）
     - [x] **Task 7 监控视图**（SessionLogsPanel 桥接与实际 `defineModel` 声明核对一致；唯一偏差：expandedLogId 桥接参数放宽为 `number | null | undefined` 以通过 vue-tsc）：新建 `views/session/monitor/MonitorView.vue`（中栏三 Tab：执行日志/Skill装配/日志分析；原「文件预览」Tab 移除改由右栏承担；`defineModel` 桥接 SessionLogsPanel 的 `agent/level/expandedLogId`）；监控 8 组件换 token
     - [x] **Task 8 右栏三新面板**：新建 `panels/TaskBoardPanel.vue`（角色树+任务+进度+约束，`toRef` 喂 useRoleTree/useTaskBoard）、`panels/ToolPanel.vue`（tool_call 列表，按时间戳配对同名 tool_exec 取输出，el-collapse 展开参数/输出/错误）、`panels/SessionMemoryPanel.vue`（evolution_log + learned_skills 按 `source_session` 过滤）
     - [x] **Task 5b 会话容器**（构建验证按计划推迟至 Task 8 完成后）：全量重写 `views/session/index.vue`——合并原 chat 页全部状态逻辑（openSession/SSE/submit/clarify/cancel/stop/interrupt/enqueue）与监控面板加载（board/agents/metrics/mailbox/health/logs/tokenMetrics）；左 280px 会话列表；中栏标题 +「💬 对话 | 📊 监控」分段切换（`?view=` query 同步，chat 缺省不落 URL）；右 320px 统一 5 Tab（任务看板/工具/文件/记忆/指标）；支持 `?work_dir=` 预填固化工作目录；此刻跑构建（对照 02/03/11/13/14/15）
     - [x] **Task 9 工作目录页**（对照 04）：新建 `views/projects/index.vue`——listSessions 按 `work_dir` 聚合卡片（路径等宽/会话数/运行中数/最近活跃）+ localStorage `bma:workdirs` 手工目录 + WorkDirPicker 添加；「发起新会话」→`/session?work_dir=`、「查看会话」→`/history?work_dir=`；右侧 420px 项目偏好编辑器（原 project-prefs 逻辑按目录参数加载，dirty/保存/刷新）
     - [x] **Task 10 技能库**（对照 05）：新增「内置技能」Tab（`GET /api/skills`，只读卡片 name/domain/description/tool_ref/cost/tags），Tab 序：经验技能/内置技能/进化日志；全文件映射表换 token
     - [x] **Task 11 插件页**（对照 06）：新增 `transportBadge()` 约定（`host_computer_use`→「宿主直控」warning、`computer_use`→「Docker 沙箱」info、`kind==='service'`→「HTTP 服务」；PluginInfo 无 transport 字段按 id/kind 判定）；`host_computer_use` 卡片加「直控宿主机 GUI，勿与沙箱版同时启用」提示行；换 token
     - [x] **Task 12 记忆中心**（对照 07/16）：新建 `views/memory/index.vue`——顶部项目下拉（全部项目 + 会话去重 work_dir）+ 类型 chip（全部/用户偏好/项目经验/技能新建/技能更新）；统计卡（经验技能/启用中/沉淀条目/涉及会话）；左沉淀时间线 + 右（经验技能列表 + 用户画像摘要→/profile + 项目偏好摘要→/projects）；项目归属经 `source_session→session→work_dir` 反查，无 source_session 为全局条目仅「全部项目」可见；**不接空壳 memory API**
     - [x] **Task 13 记忆分组其余页**：profile/knowledge/FeaturePlaceholder/AppCard 换 token；soul 全量重写（当前人格 + LLM + `config/soul.md` 路径 + 热切换规划中说明）；history 全量重写为表格页（搜索 + 状态/目录过滤 + `?work_dir=` 预过滤 + 行点击 →`/session?id=&view=monitor`）
     - [x] **Task 14 设置 + 工作流**（对照 08/09）：settings 全量重写（外观主题单选 useTheme、服务信息 `/api/status`、连接健康 `/api/health`、资源目录 BMA_HOME 静态说明卡）；workflow 正式占位版（SetUp 图标 + Beta 徽标 + 三项规划能力清单；注意补注册 `Share` 图标）
     - [x] **Task 15 收尾**（已删 `project-prefs/`；`@/views/chat`/`project-prefs` 零残留；hex 扫描仅余 dashboard 品牌插画 SVG 渐变与 `#1e3a8a` 圆点（计划例外），另清理计划漏掉的 `MarkdownRenderer.vue` scoped 深色硬编码；全量构建零报错。**待人工**：15.3 逐页对照预览图（含深色复查）、15.4 功能冒烟 5 项）：删 `web/src/views/project-prefs/`；残留扫描（`@/views/chat`、`project-prefs`、`#0f1115/#1a1d24/#14161a/#2a2d35/#1e3a8a` 零命中，dashboard 品牌插画渐变除外）；全量构建零报错；逐页对照预览图（含深色主题复查）；功能冒烟 5 项（`/chat` 重定向、`/project-prefs` 重定向、目录→新会话 work_dir 生效、会话页 SSE/软停止/中断/工具 Tab、刷新主题保持）
   - 执行方式：实际采用内联顺序执行（superpowers SDD 技能不在本环境可用），按依赖序 1→2→3→4∥9∥10∥11∥12∥13∥14→5→6→7→8→15 完成；全程未做任何 git 提交
   - 验收口径：12 路由 + 2 重定向全部可用；浅色默认深色可切且刷新保持；会话页双视图右栏 5 Tab 数据真实（工具=事件流、记忆=沉淀过滤、指标=四卡）；工作目录页可添加目录并带目录发起新会话；视觉与 `doc/image/redesign/` 对应页结构一致
   - 不做：TUI 不重做（"看着办"）；不引入前端测试框架与新依赖；不做后端任何改动；人格在线编辑、知识库实体功能、工作流编排实体功能均只留占位

7. **子 Agent 机制向 Claude Code / Kimi Code 借鉴**（2026-09-07 立项；①-⑤ 已落地迁出：变更.md 任务 129，含派发纪律 prompt / scout 侦察角色 / ctx_inject 注入测量 / mailbox 超 4000 runes 落盘收口 / map_sub_agents 同构批量。以下只留待办）
   - **⑥ 参数化 knobs（待③测量数据）**：30K 共享前缀、黑板摄取上限、块记忆 top3 做成 config.yaml 可调——有数据再定默认值，不拍脑袋调参。
   - **观察点**：真机多轮任务中 meta 派发 task 含确切路径比例（①效果）；scout 派发免 spec ≤5 分钟返回、无写工具记录（②验收）；ctx_inject 日志逐路 runes（③验收）。数据经几天真实会话积累后回填。

8. **执行效率与质量根治：上下文/指令/反馈三干扰源治理**（2026-09-07 立项；P0-1/P0-2/P0-3/P0-5/P1-1/P1-2/P1-3 已落地迁出：变更.md 任务 130；P0-4 测量与 mailbox 收口并入任务 129 ③④。以下只留待办与观察点）
   - **P0-4 收口 <20K（待数据）**：ctx_inject 测量数据出来后，将共享前缀/黑板/召回收口到每呼总输入 < 20K 为目标。
   - **P2-1 UGit eval 基线**：拿 2-3 个 UGit 真实大任务建回归基准（test/eval），改动前后对比 总 token / 墙钟时长 / 验收通过率——延续实证传统，不拍脑袋。
   - **验收口径**：domain 单呼均延由 57.6s 降至 ≤15s；同任务重跑总输入 token 降 ≥60%；thinking 恢复后编码任务一次通过率上升（eval 基线对比）；meta 对中小任务零派发直接完成；检验类 Agent 单任务调用 ≤20 次。
   - **观察点**（真机会话验证）：P0-2/P0-3 砍 prompt 后 08-13 身份混淆/领域越界是否复发（实证条款已保留）；P1-1 后 meta 是否"大包大揽不派发"回潮；P0-1 thinking 恢复后 domain 单呼均延变化。

9. **对 Claude Code / Kimi Code 差距补齐六项**（2026-09-07 立项；第 7、8 项之外的剩余差距点，来源：三家机制逐项对比 + UGit 24h 会话实测）
   - 总优先级：**①②③ 与第 8 项 P0 同批**（共同目标：每呼输入 86K→<20K + 消灭串行等待）；④ 便宜见效快随手做；⑤ 单独立项（决定并行战略终局）；⑥ 搭 ⑧P2-1 eval 基线的便车。
   - **① 轮内并行工具执行**（速度）：
     - 差距：Claude Code/Kimi 一轮多 tool_calls 并行执行（同读 3 文件）；BMA `agent/react_agent.go` 同轮 tool_calls 串行执行，探索阶段"读 A+读 B+grep C"被白排串行，且轮间还要再等 LLM。
     - 改法：react_agent.go 工具执行段改 goroutine 池并发执行同轮 tool_calls，结果按原序回填消息历史；glm 支持 parallel tool_calls，协议无障碍。注意：有依赖关系的调用（模型本就会分轮发）不受影响；写操作（Edit/Write）并发时按文件路径互斥防同文件竞写。
     - 验收：探索密集任务轮次墙钟降 ≥40%；agent_events 中同轮多 tool_call 的 occurred 时间戳重叠可见。
   - **② 工具结果统一收口层**（上下文，mailbox 收口的通用化）：
     - 差距：Kimi 对超大工具结果截断头部 + "full output saved to <path>"；BMA mailbox 收口已落地（#7④，超 4000 runes 落盘送摘要+路径），但 ReadFile/SearchInFiles/RunCommand/web_search 返回仍整坨进上下文。
     - 改法：tool 执行出口加统一后处理——超阈值（默认 8K runes，可配）落盘 workspace 临时文件，上下文只留头部摘录 + 绝对路径 + "需全文读此文件"提示。与 #7④ 共用同一落盘机制与阈值配置。
     - 验收：长会话稳态输入 token 显著下降（配合③测量）；子 Agent 按路径回读全文的行为正常（不幻觉内容）。
   - **③ 陈旧工具结果驱逐**（上下文，长会话大头）：
     - 差距：Claude Code 压缩时清旧工具输出；BMA 150K 整体压缩前，几十轮前的文件内容/grep dump 全程留在历史反复重发——86K/呼 的重要构成。
     - 改法：比整体压缩更早一层的轻机制——N 轮（默认 20，可配）前的工具结果原地替换为占位符（"[ReadFile config.yaml: 已驱逐，原 2400 行]"）；信息仍在文件系统，需要时重读。先于 150K 压缩介入，与压缩不互斥。
     - 风险：模型可能对占位符幻觉旧内容——占位符文案必须明示"内容已驱逐需重读"；先对 ReadFile/SearchInFiles 两类试点，RunCommand 结果（含验证证据）暂缓驱逐。
     - 验收：长会话（50+ 轮）稳态输入降至阈值内不再触顶 150K；无"引用已驱逐内容"幻觉事故（eval 观察）。
   - **④ 截图/图片降采样**（上下文+延迟，browser/computer_use 场景）：
     - 差距：BMA `image_passthrough` 原尺寸透传截图进多模态上下文，1920×1080 截图数千视觉 token；UGit 会话 browser_* 调用 76+ 次。
     - 改法：透传前缩至长边 ≤1024（或端点推荐尺寸），原图落盘保留路径；对 computer_use 连续截图场景收益最大。
     - 验收：browser 重度会话视觉 token 占比下降可测；截图定位/断言类任务准确率不回退（降采样后文字仍可辨）。
   - **⑤ 并行域 git worktree 隔离**（单独立项，让多域并行首次真正成立）：
     - 差距：Kimi/Claude 生态用 worktree 给并行 agent 独立副本；BMA 全部 domain 共享同一 work_dir 直接写文件，并行=互踩，契约冲突→返工吃掉并行收益（第 8 项根因 4）。
     - 改法：`call_sub_agent` 加可选 `worktree: true`——dispatcher 为该 domain 建 git worktree（`.git/worktrees` 机制，可逆、不动历史），子 Agent work_dir 指向副本；完成后由 meta 或专门合并域 review diff 后合入。与 #8 P1-1 互补：耦合任务串行、独立任务 worktree 并行。
     - 风险：合并冲突处理链路是新复杂度（首版可人工合入）；worktree 生命周期管理（完成/ abandoned 清理）；Windows 路径与符号链接坑需实测。
     - 验收：两个文件级独立的 domain 真并行完成且合入无冲突；耦合任务不被误派 worktree。
     - 守卫体系适配（2026-09-07 定论：冲突不消失，只从运行期搬到合并期——机制净增不净删）：
       1. **全保留（与文件隔离无关的编排守卫）**：同域去重/接力熔断/派发限额/spec 强制门照旧。
       2. **全保留但仅服务共享路径**：spec mtime stale 拒派、职责边界 prompt（"不碰哪些文件"）——`worktree: true` 是可选项，耦合任务仍走共享主 work_dir 串行，旧守卫服务旧路径一条不删。
       3. **新增合并门**：合入前检测 base commit 漂移（他域先合入→rebase/解冲突），首版仲裁人工/meta 辅助。交付物形态=diff patch、合并门即 diff 审阅，详见第 10 项⑤。
       4. **跨域契约核对前移**：symbols/dom_ids/scripts 静态核对触发点从"兄弟域全完成后"挪到**合并前**拦截——隔离后违例在运行期不可见，合并门是唯一拦截点；核对跑在 diff 上而非全量代码。
       5. **唯一豁免**：worktree 域自身副本上的 spec mtime stale 检查可豁免（副本不会被别域改动）；其"脏读"等价物=base commit 漂移，由合并门兜底。
       6. **黑板通道升级**：隔离后兄弟域未合并代码互不可见，"偷看兄弟接口签名"隐式通道消失——接口变更类产出必须优先、及时进黑板摄取，跨域协调全靠文本通道。
   - **⑥ 效率一等指标**（度量，扩展 #8 P2-1）：
     - 差距：UGit"检验 Agent 烧 1/3 token"靠事后挖库才发现；无常态化效率视图。
     - 改法：session_logs 数据已全量，只差聚合——每会话落库/展示：每交付文件 token 成本、每派发平均轮次、校验开销占比、meta:domain token 比、单呼输入 P50/P95。web 指标页或 TUI 面板加一个视图即可，不动采集层。
     - 验收：新会话跑完即可在 UI 看到上述五项；后续所有优化项以此数据裁决。
   - 依赖关系：②③④ 同属"上下文瘦身"战役，与 #8 P0-2/P0-3/P0-4 统一设计阈值与落盘机制，避免三个收口各自为政；① 独立于上下文战役可并行开工；⑤ 依赖 #8 P1-1 的派发判据先落地（否则 worktree 会被误用于耦合任务）。
   - 不做：① 不做跨轮自动批处理（只并行模型同轮发出的调用）；③ 不驱逐 WriteSpec/审批/契约类消息（只针对工具结果）；⑤ 首版不做自动合并冲突解决（人工/meta 辅助合入）；⑥ 不新建采集管道（纯聚合查询）。

10. **Hermes / Codex 借鉴七项**（2026-09-07 立项；来源：hermes-agent 官方架构/delegation 文档 + Codex 机制查证。与 Claude/Kimi 的"紧循环"优势不同，这两家强在工程化外围，是重架构系统的对标对象。原评审 8 条中第 4 条 max_iterations 不引入，见文末不做）
   - 排序与依赖：⑦ 最便宜先做；① 依赖 #8 P0-5 已落地的端点缓存结论；② 是 TODO#3 watchdog 退役的前置替代；③ 与 #9⑥ 同批；④ 独立小改；⑤ 排进 #9⑤ 内实施；⑥ 独立。
   - **① system prompt 三层分级 + 缓存破坏纪律**（承接 #8 P0-5 缓存查证，升级为缓存友好架构）：
     - 改哪里：`agent/react_agent.go` Assemble 注入顺序；`dispatcher.go` buildSharedPrefix（:3868）/injectScopedRecall；`blackboard_uptake.go`；PersonaInjector。
     - 方案：消息结构显式分三层——**stable**（身份/角色职责/工具纪律，会话内逐字节不变）→ **context**（项目概览/AGENTS.md/spec 槽）→ **volatile**（黑板摄取/记忆召回/墙钟预警/时间戳，固定尾部）。纪律：会话中途任何代码路径不得改动 stable 段（唯一例外：用户显式切模型）；每呼 slog 记录 stable 前缀 hash，同会话内 hash 变化即告警（防回归断言）。端点支持显式缓存（cache_control）时断点打在 stable|context 边界；仅隐式前缀缓存的端点靠本分层即可获得命中率。
     - 验收：同会话全部调用的 stable hash 一致（日志可查）；支持缓存端点的命中率可观测；每呼输入 P95 与 #9⑥ 联动下降。
   - **② stall 判定活动证据化（slow vs stuck）**（TODO#3 watchdog 退役的替代方案，替代心跳计数）：
     - 改哪里：`dispatcher.go` patrol/scanStuck（:767）/wallClockWarnLadder（:2206）；`react_agent.go` stagnationGuard（:737）；ActivityReporter（任务 112 已修热驻漏注）。
     - 方案：每 Agent 维护活动证据流——last_llm_start/end、last_tool_call/exec 时间戳 + 当前所处位置（工具名/LLM 呼）。**stall = 无在飞 LLM 调用 且 无活动事件 > 阈值**（默认 450s 可配，对齐 Hermes child_timeout）；"quiet but in-LLM"（慢思考单呼可达 25min）只展示不杀。TUI/web 每 Agent 显示 `in <tool> · active Xs ago`，一眼分辨慢与卡。预警文案沿用 #8 P1-2 建设性口径（改提示不收尾）。
     - 验收：塔防类长任务零误杀（对照任务 112/115 两次误杀根因）；mock 挂死 Agent 阈值内被杀；UI 可分辨 slow/stuck。
   - **③ 子 Agent 审计面**（并入 #9⑥ 同批实施）：
     - 改哪里：web 指标页 / TUI 面板；agent_tree_nodes + session_logs + agent_events 聚合查询（不动采集层）。
     - 方案：按支路 rollup——in/out tokens、轮次、墙钟、FilesModified 清单（派发结果已带）、校验开销占比；**下钻**：agent_events 支撑单支路逐轮事后回放；单支 kill（cancel_agent 已有）/pause（token 预算暂停已有，暴露手动入口）。目标：UGit"检验 Agent 烧 1/3 token"级失控当日可见，不再靠事后挖库。
     - 验收：会话页可见逐支路成本表；点击支路可逐轮回放；kill 单支不影响兄弟支路。
   - **④ 中断传播语义**（stop 传播 / pause 不传播）：
     - 改哪里：`dispatcher.go` :2127 子 ctx 重建（context.Background()）、react_agent 取消链、TUI stop/interrupt 通路。
     - 方案：区分两类取消——**用户 stop/interrupt 传播**：子 Agent ctx 挂到新设会话级 stopCtx（仍不从父 run ctx 派生，保持父取消不波及子的热驻语义），命中后子状态=interrupted 且巡检不救活；**内部 pause（token 预算触顶）/父自身结束不传播**，维持 ResumePaused 续跑。修的是漏洞：现状用户按停止后子 Agent 可能继续烧 token。
     - 验收：TUI 停止后全部活跃子 Agent ≤10s 终止且台账状态=interrupted；token 触顶暂停不影响子；暂停恢复后子正常续跑。
   - **⑤ worktree 交付物 = diff，合并门 = diff 审阅**（并入 #9⑤ 实施，已回写 #9⑤ 守卫适配 3/4 条）：
     - 方案：worktree 域完成时产出 `git diff` patch 文件落盘 + 变更摘要；合并门（web/TUI）逐文件展示 diff（+/-），契约核对跑在 diff 上（#9⑤ 前移条款）；人/meta 批准才合入；**驳回路径**：patch + 修改意见回热驻的 worktree 域续改，不冷启动重派。
     - 验收：合并门可见完整 diff；契约违例在 diff 上被拦截并回指责任域；驳回续改复用该域上下文（reuse_agent_id）。
   - **⑥ 三级信任模式**（恢复审批链为 Codex 式分级，替代当前全信任裸奔）：
     - 改哪里：`config.yaml` tool_approval_*；审批链（destructive 守卫链保留复用）；TUI `/approval` 命令 + web 会话设置；派发链不感知。
     - 方案：会话级三档——**suggest**（全部动作逐条审批）/ **auto-edit**（文件读写免审批，shell + destructive:true 工具落审批）/ **full-auto**（现状全信任）。**默认维持 full-auto 不改现状**（UGit 类任务零回归）；切换即时生效（下一工具调用起）。实现只加模式判定层，不动审批链本体。
     - 验收：三档行为各如定义；auto-edit 下 EditFile 直通、RunCommand 落审批；中途切换下一呼生效。
   - **⑦ AGENTS.md 自动注入**（零成本冷启动加速，UGit 类成熟仓库直接受益）：
     - 改哪里：`dispatcher.go` buildSharedPrefix（:3868）/runSubAgentOnce 前缀拼装；meta 会话启动同理。
     - 方案：派发时检测 work_dir 根部 `AGENTS.md` 或 `CLAUDE.md`（优先 AGENTS.md），截断 ≤4K runes 注入任务前缀【项目自述】槽；按 session + 文件 mtime 缓存，不变不重读；根部无文件则零开销不注入。子 Agent 冷启动直接获得目标项目的构建/测试/约定信息，对冲"重复探索"根因。
     - 验收：UGit 派发首呼 prompt 含项目自述；无文件时零开销；mtime 变化后新派发拿到新版（缓存正确失效）。
   - **不做**：Hermes 派发级 max_iterations 不引入（与既有 token 预算 + 墙钟 + 停滞预警三道闸门重复，再加一道只增配置复杂度）；Codex OS 级沙箱（Landlock/Seatbelt）不上（Windows 宿主 + 全信任主场景性价比低，computer_use 类需求已有 docker 沙箱路径）；云端任务 fleet / 多平台网关 / trajectory 训练数据导出均不碰（超出当前阶段）。
