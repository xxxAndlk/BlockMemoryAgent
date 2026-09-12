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

9. **对 Claude Code / Kimi Code 差距补齐六项**（2026-09-07 立项；①-⑥ 全部落地迁出：变更.md 任务 131——① 轮内并行工具执行（原序串行回填+写路径互斥）、② 工具结果统一收口（.bma/tool_outputs/ 落盘+摘录）、③ 陈旧工具结果驱逐（ReadFile/SearchInFiles 请求视图占位符化）、④ 截图降采样（image_max_edge=1024 box-filter）、⑤ worktree 隔离派发+合并门（worktree=true / merge_worktree review|merge|reject，含守卫体系六条适配：守卫全保留/spec stale 豁免降级警告/新增 base 漂移+契约检查合并门/契约核对前移合并期跑副本/黑板通道提示词升级）、⑥ 效率一等指标（五项指标+支路成本表，web 效率审计 Tab）。以下只留观察点）
   - **观察点**（真机会话验证 + eval 基线）：① 探索密集任务轮次墙钟降 ≥40%（agent_events 同轮 occurred 重叠可见）；②③ 长会话稳态输入 token 下降、无"引用已驱逐内容"幻觉；④ browser 重度会话视觉 token 占比下降且截图定位准确率不回退；⑤ 两文件级独立 domain 真并行合入无冲突、耦合任务不被误派 worktree（挂死工具/长 MCP 操作的巡检误杀同批观察，见 #10②）；⑥ 五项指标作为后续 ②③④ 收口效果的裁决数据源。
   - 不做（沿用原决策）：① 不做跨轮自动批处理；③ 不驱逐 WriteSpec/审批/契约类消息；⑤ 首版不做自动合并冲突解决（base 漂移即拒，人工 rebase/重派）；⑥ 不新建采集管道（纯聚合查询）。

10. **Hermes / Codex 借鉴七项**（2026-09-07 立项；①-⑦ 全部落地迁出：变更.md 任务 131——① 三层分级显式化 + 每呼 stable hash 日志（`[cache] stable_hash=…`，变化 WARN）；② stall 判定活动证据化（activityEvidence 结构 + llm_start/llm_end/tool:<名>/stream/user_wait/descendant 六类 kind，盲 keepalive 不再刷新 lastTS；scanStuck 三判定：in-flight LLM 不杀 / 挂死工具到点杀 / 步间静默杀；TUI+web 显示 `in <tool> · active Xs ago`）；③ 子 Agent 审计面（并入 #9⑥：效率审计 Tab 支路成本表 + 逐轮事件下钻回放 + 手动 pause 入口）；④ 会话级 stopCtx 中断传播（stop 硬取消+软停销毁路径均 cancel，子 ctx 挂 stopCtx，全树 ≤10s 终止；**偏离 TODO 字面**：domain 命中后走既有软停收尾，热驻转 Idle 保留复用而非全部 interrupted 硬销毁——用户拍板）；⑤ worktree diff 交付 + 合并门（并入 #9⑤，merge_worktree review 全量 diff 20K 截断 / merge / reject 回信续改）；⑥ 三级信任模式 suggest|auto-edit|full-auto（会话级 + HTTP/`/approval`/web 下拉切换，下一工具调用生效；**suggest 档收窄为变更类动作**——读类免审批防审批风暴，对齐 Codex 实际行为；默认 full-auto 不改现状）；⑦ AGENTS.md/CLAUDE.md 自动注入（≤4K runes【项目自述】槽，mtime 缓存，子 Agent 前缀首位 + meta envBlock 双路）。以下只留观察点）
   - **观察点**（真机会话验证）：① 同会话 stable hash 恒定（日志）+ 缓存端点命中率可观测；② 塔防类长任务零误杀（对照任务 112/115 根因）、mock 挂死 Agent 阈值内被杀、UI slow/stuck 可分辨——RunCommand/MCP 长操作误杀为重点观察项（阈值可调）；③ 会话页支路成本表出数、逐轮回放可用；④ stop 后全树 ≤10s 终止、暂停恢复续跑、热驻 stop→Idle→下条消息复用；⑤ 合并门 diff 完整可见、契约违例回指责任域、驳回续改复用上下文；⑥ 三档行为各如定义、中途切换下一呼生效；⑦ 有 AGENTS.md 仓库派发首呼含项目自述、mtime 失效正确。
   - **不做**（沿用原决策）：Hermes 派发级 max_iterations 不引入（与 token 预算 + 墙钟 + 停滞预警三道闸重复）；Codex OS 级沙箱（Landlock/Seatbelt）不上（Windows 宿主性价比低，computer_use 已有 docker 路径）；云端任务 fleet / 多平台网关 / trajectory 导出不碰。watchdog（TODO#3）退役仍按其数据门槛单独决策——本项②只换判定内核，巡检循环保留。

11. **任务完成后提示词三小项 + 提示词正文日期清扫**（①-④ 全部落地迁出：变更.md 任务 132——① assistant_template 补【交付】段、② domain 终答正文 ≤500 字符超出落盘留路径、③ 五叶子公共段收敛为 `pkg/config/leafCommonBlock` 单一来源加载期拼接（含 doc_assistant 自检口径泛化修正）、④ 提示词正文 27 处日期/迭代编号清扫（`#` 注释行保留）。以下只留观察点）
   - **观察点**（真机会话验证）：① 临时助手回传摘要含路径+证据（真机抽查 mailbox）；② domain 摘要超 4000 runes 截断落盘发生率下降（ctx_inject/mailbox 日志）；③ 塔防冒烟确认五叶子公共段泛化（自检口径/兄弟文件句统一）无行为回潮；④ 后续公共段修改只动 leafCommonBlock 一处即五叶子同步。

12. **Agent 编排页：层级树图 + 单 Agent 对话页 + 用户直连**（2026-09-11 立项；设计已批准，spec 与 14 任务实现计划已落稿；**代码实施完成（2026-09-11），后端全量测试 + 前端 vue-tsc/vite 构建零报错；仅剩带真实会话的手动验收清单待人工执行**）
    - 依据文档：
      - 设计定稿：`docs/superpowers/specs/2026-09-11-agent-orch-board-design.md`
      - 实现计划（含全部代码）：`docs/superpowers/plans/2026-09-11-agent-orch-board.md`
    - 实施记录（2026-09-11）：Task 1-14 全部落地——`child_wait` 展示态（不续命不冒泡）、mailbox 双写 `agent_events` 留痕、Redis 消息热层（TTL 24h/cap 500/nil 降级）、消息热写 + 子 Agent 终态全量落 PG、`GET/POST /agents/:aid/messages|message`、`Tree.Reopen` + `InjectUserMessage`/`ReviveWithMessage`、两段提示词规程、前端树图/对话面板/编排主视图/看板时间线（删 `useRoleTree.ts`）。两处偏离计划：① `ErrInvalidSessionState` 全局映射仍是 400，直连不可用态改由新哨兵 `ErrAgentNotDirectable`（包装前者）映射 409；② 新增测试 `TestAgentMsgRedisStore_NilNoOp` 促使 `BeforeMsg`/`AfterMsg` 补 nil 守卫。
    - 评审修复（2026-09-11，代码评审 15 项发现逐条核实后修）：**路由可达性**——`%2F` 转义的实例 ID 在默认 gin 下全部 404（`/agents/:aid/*` 对任何子 Agent 不可用，含既有 pause/cancel/events），修法为新 DefaultRouter 开 `UseRawPath`+`UnescapePathValues`，护栏 `server/routes_agentparam_test.go`；**复活直连 ctx 缺 sessionID**（treeFn("") 取空树 → 恒失败）→ `MessageAgent` 注入 `tool.WithSessionID`；**复活后邮箱已 Purge 永久关闭**（子 Agent 回传/再注入全死信）→ 新增 `Mailbox.Reopen`；**child_wait 被后代冒泡秒刷**（等子态与发送闸门形同失效）→ `activityEvidence.waitingChildren` 粘滞（冒泡只续命不换 kind）；**复活 seq 从 0 重编号撞旧游标** → 复活前 `MessageLogger.Clear` 清热层；**热驻 domain 主路径（默认开启）与 resume 路径漏接消息日志/终态落库** → 补注入 + 终态存；**读侧热层非空即不回退 PG** → 改为热层不足时按 seq 合并 PG（增量窗口仍只读热层）；**mailbox 留痕同步 PG 写阻塞发送方** → 改 goroutine 异步；**留痕取最早 100 条** → 改取最近 100；**复活脚手架漂移** → 补 lastWrites/heldSkills 清理，`subMeta`/`activity` 收尾改 `CompareAndDelete`（防旧 run 收尾误删复活条目）；前端：`before_seq=0` 上翻重复前插、看板「打开编排页」只改 URL 不切视图（补 URL→ref watcher）、`active` 状态渲染为灰点/英文原值、`fetchJson` 丢弃后端纯文本错误体（409 提示只剩 "409 Conflict"）。
    - 前置待决（已定）：
      - [x] git 策略：全程不 commit（2026-09-11 用户拍板）——执行者跳过计划各任务末尾的 Commit 步骤，评审用工作区 diff
      - [x] 执行分工：规划者只出计划，代码实施由其他执行者按本计划逐任务执行
    - 全局约束（所有任务遵守，详见计划 Global Constraints）：
      - 零新依赖；不动 `orchestrator.Status` 枚举、SSE 通道、压缩金字塔、mailbox 投递语义
      - 观测写（Redis 热写 / trace 留痕 / 终态落库）一律 best-effort，失败仅记日志不阻塞 ReAct 主循环
      - Redis 不可用全链路降级：写侧跳过、读侧回退 PG，端点不报错
      - 后端测试不依赖真实 PG/Redis（nil-DB no-op + fake 模式）；前端验收 = `cd web && pnpm build`（vue-tsc）+ 手动清单
      - 新代码注释中文，说明"为什么"
    - 任务清单（依赖序执行，格式 Files/Interfaces/checkbox steps，详见计划）：
      - [ ] **Task 1** `child_wait` 展示态：`waitForChildren` 上报等子信号（只换 kind，不刷 lastTS、不冒泡）
      - [ ] **Task 2** mailbox trace 钩子：`WithTrace` + Send 重构，邮件双写 `agent_events`（type=mailbox）留痕
      - [ ] **Task 3** Redis 热层：`redis_agentmsg.go` AgentMsgEntry/AgentMsgRedisStore（TTL 24h、cap 500、nil 安全）
      - [ ] **Task 4** MessageLogger 接线：ReActAgent 每条入史消息热写 Redis + dispatcher 五处终态 saveTerminalHistory 全量落 PG
      - [ ] **Task 5** 查询端点：`GET /sessions/:id/agents/:aid/messages`（热层优先 PG 回退，合并分页 + 留痕）
      - [ ] **Task 6** 复活与注入内核：`Tree.Reopen`（仅终态可复活）+ `InjectUserMessage`（投邮件 + pokeParent 唤醒）+ `ReviveWithMessage`（同 ID 重跑，种子=原任务+上轮结果+用户消息，通知父"复活返工"）
      - [ ] **Task 7** 直连端点：`POST /sessions/:id/agents/:aid/message` 状态机路由（waiting 注入/终态复活/running 409 ErrAgentBusy/paused·idle·meta 拒绝）
      - [ ] **Task 8** 提示词规程：DomainAgent 加【用户直连消息】处置段（先盘点自身+下游→重派/新派/纳入/答进度），MetaAgent 加复活返工感知段
      - [ ] **Task 9** 前端 API 层：AgentMessageItem/AgentMailItem/AgentConversation 类型 + getAgentMessages/sendAgentMessage
      - [ ] **Task 10** 树图基础：useTreeLayout（tidy 两遍法，零图库）+ AgentTreeNode（状态点/徽章/活动小字/hover 中断终止）+ AgentTreeCanvas（SVG 贝塞尔边 + 缩放拖拽适应）
      - [ ] **Task 11** 对话面板 AgentChatPanel：消息流（用户直连高亮/mailbox 灰卡/任务输入/assistant+思考+工具 chips/tool 折叠）+ 留痕 tab + 三态发送框（waiting 可发/running 禁用转圈/终态可发复活）
      - [ ] **Task 12** 编排主视图 OrchView + index.vue 接线（`?view=orch`、选中态 `?agent=` 同步、第三个切换按钮 🌳 编排）
      - [ ] **Task 13** 看板时间线化：useGoalTimeline（user/派发/完成/失败里程碑 + board.tasks 兜底）+ 任务目标折叠展开 + 旧 el-tree 编排区块换"打开编排页 →"入口 + 删 useRoleTree.ts
      - [ ] **Task 14** 总验证：后端全量测试 + 前端构建 + 手动清单 6 项（树图/三态发送/复活/中断终止/Redis 降级/中断恢复连续性）+ CLAUDE.md·TODO.md 最小补记
    - 验收口径：编排页树图层级与连线正确、点击任意 Agent 看全量对话与交互留痕；waiting 注入即时唤醒、终态复活重跑父知悉、running 发送禁用并可中断终止；Redis 停掉对话页仍可读（PG 回退）；看板目标时间线随事件增长且 events 空时有兜底
    - 不做：不引入图布局库（tidy 自绘）；mailbox 留痕不建新表（复用 agent_events）；复活不回溯父状态（父经邮件感知）；不扩 SSE（对话页 3s 轮询增量）；paused/idle 不可直连（paused 走监控页恢复，idle 经 MetaAgent 派发）；meta 不开放直连（走主对话通道）
