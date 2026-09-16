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

13. **Capability 权限所有权与公司架构治理模型**（2026-09-14 立项讨论，**判定挂起：当前是负资产**；本项是设计备忘，非待办——满足"重启条件"前不实施）
    - **动机场景**：一个 Agent 单独管理云端服务器、独占 SSH 全部权限，其他 Agent 无权限访问，但 owner 可以把权限短期借出（Rust 所有权/借用语义抽象到领域级权限）。
    - **挂起判定理由（2026-09-14 架构核查）**：
      1. 场景未出现：项目无 SSH 工具、无运维域、无凭证存储——ssh/scp/telnet/nc/ftp 反而在默认命令黑名单里（全局硬禁，配置不可削弱）；
      2. 角色职责固定 → 权限需求是静态的，静态需求用 roles.yaml 白名单已正确表达；动态借用系统的边际价值只在"运行时需要临时越权"时才为正；
      3. 租约/审批/经纪执行是一整套活动部件，自带新故障模式（申请丢 mailbox 无人批、owner 热驻槽销毁、租约中途过期）——正是要避免的"不可见异常"换一批重来；
      4. 挂载点（Dispatch 钩子、mailbox 通道、增量 migration）不会消失，晚做成本不变。
      同期已落地的小加固见变更.md 任务 159（角色白名单硬门，默认关闭）。
    - **设计备忘（重启时按此实施，勿重新发明）**：
      - **分层模型（公司架构映射）**：资源层=capability 服务持有真实凭证（任何 LLM 碰不到密钥）；职能层=domain→capability 所有权登记（**owner 挂 domain 而非 agent 实例**——员工会下班，部门不会，当值实例代行审批权）；身份层=agent 实例持有部门内角色权限子集（roles.yaml 白名单重新解释为初始所有权分配，不推翻）；协作层=申请→审批→租约→审计→归还；制度层=系统静态策略高于任何 LLM（可借性/审批分级/职责分离——如"发起支付+审批支付"禁止同域持有）。
      - **已拍板的关键决策**：① 借用执行双模并存——代办（brokered，密钥不出边界，capability 服务代执行，只回传结果）+ 借调（短期凭证签发，SSH 证书 TTL/Vault OTP 式）；申请方说明模式，owner 审批时可降级（要借调只给代办）；② 借用流=**需求方申请、owner 审批**（非 meta 集中分配、非纯人工）；③ 独占借用采**宽松语义**——&mut 借出期间 owner 自己仍可用（责任唯一性让位于运维合理性），严格独占留作借出时可选项；④ 衰减原则：只能借出自己权限的子集，默认不可再转借（reborrowable 需显式）；⑤ 借用生命周期 ≤ owner：绑 TTL 或一次派发，session 终态自动回收（挂 CleanupSessionWorktrees 同款钩子 = Drop）。
      - **最小侵入挂载点**：强制执行点= `tool.Registry.Dispatch` 执行前（与任务 159 roleToolGate、approvalHook 同款 `SetCapabilityChecker(fn)` 注入，checker nil=零开销）；申请/审批通道= mailbox MsgRequest/MsgReply（现成）；owner 审批动作= owner 角色白名单加 `capability_approve` 工具；人工会签=复用 approvalHook；存储=新增 `capabilities`/`capability_leases`/`capability_events` 三表（纯增量 migration，不动旧表）；灰度=影子模式先行（只记录"若启用会拒绝"不拦截），观察无误伤再切真拦截。
      - **worktree 收编为特例**：worktree 机制本质是文件写所有权的 clone+回收（主仓写权归 meta，副本给子 Agent，合并门=所有者回收审查，base 漂移检测=可变借用冲突检测）。重启时把"派发到 worktree"表达为 `fs.write:<副本>` 所有权转移，worktree 不再是特殊机制而是 capability 模型的一种资源类型。
    - **重启条件**（任一成立即重新评估）：① 真实出现 SSH/云资源运维域需求；② 多 Agent 需共用不可信凭证；③ 系统走向多人/多租户。
    - **明确不做**：不拆 hub-spoke——`CanCall` 禁止 domain→domain 直接派发保留，meta 仍是治理层（个位数域规模下星型协调的可靠性优于对等部门网络；借用协议架在 mailbox 之上即可，无需对等派发）。

14. **会话三档控制（快速 / 探索 / 集群）+ 模式路由内核**（2026-09-14 立项讨论，同日二次讨论定稿"三档外壳+四模式内核"结构；设计备忘——第一批已落地（2026-09-15，范围与遗留见条内标注）；**2026-09-16 用户定向调整见下**；面向"普通人可用"产品目标：不同任务快速响应或大型多 Agent 完成）
    - **方向调整（2026-09-16 用户决策，第二批）**：三档全手动 `fast|daily|cluster`——`auto`（规则选档）退役，旧数据读侧映射 daily；**fast→doc_assistant**（文档助手顶层直达，补 escalate_gear；原 `chat` 角色与提示词删除）、**daily→domain**（DomainAgent 顶层直接执行，跳过 meta 专属注入，同样挂 escalate_gear）、cluster→meta 不变；默认档 `default_gear: daily`。同时新增**会话级思考强度**（off|low|medium|high，空=跟随角色默认；只覆盖本会话顶层 Agent；provider 热读取，运行中切换下一轮生效；随 MetaMemory 持久化；新端点 `POST /api/sessions/:id/thinking`）。Web 前端输入栏前置档位选择器改三档 + 新增思考强度选择器（并修复 ChatView 转发丢 gear 的既有 bug）。落地详情：`doc/变更.md` 2026-09-16「会话三档重构」。
    - **第一批落地（2026-09-15，A 组 12 任务 + 前端档位）**：档位枚举 `auto|fast|cluster` 全套——`gear` 原子字段 + `POST /api/sessions/:id/gear` 端点（枚举校验 400）+ `agent.default_gear` 配置 + MetaMemory JSONB 持久化与 restore 回填；规则选档器 `gear_selector.go`（闲聊信号→fast，**其余一律 cluster 零回归**；explore 留枚举未实现）；chat 轻量角色（`pkg/prompts` 登记 + roles.yaml `id: chat` 快模型低 thinking，fast 档直达对话跳过 roster/ledger wrapper）；meta thinking high→low；`escalate_gear` 工具 + 升档确认（复用 askUser 通道，槽占用即"稍后再试"；**只升不降**，手动切档任意向；gear 事件留痕含 manual/escalate 区分）；P0-2 steering 排队——running 非 child_wait 直连改邮箱注入 200{queued}（ErrAgentBusy 409 保留给注入失败路径，generate 前补一次 drain）；EnsureProjectDoc 异步化（cluster/auto 有界等 ≤2s，fast 不等）；翻译层①⑤（`eventStyles.ts` 中文状态短语 + `errorHints.ts` 可行动错误提示与"升集群档"按钮）；P2-6 摘要 hash 备忘（同输入复用不重调）；前端档位指示器（ChatHeader，探索档禁用标"即将上线"）。**遗留**：探索档全部切片（表格工具包 / ingest_file / run_code / P1-3 计划即产物 / P1-4 依赖门排队 / P1-5 合并门放宽 / C-2 里程碑升格 / C-3 整合纪要）按档后续交付；选档器大模型化与自动降档维持"明确不做"。
    - **动机**：用户真实工作流 = 大任务先出大体设计 → 使用中一点点发现小缺陷再纠正；现状这套链路极其繁琐且执行时间久，平台用户不会等。日常三大场景验证：持续修改一份 Excel、读一份超大型文件、陪聊完善创意——三者形状完全不同，系统现在只认识 project 一种形状，每个日常需求都被迫穿重装。
    - **核心缺陷判定（2026-09-14 双路探查，均有 file:line 证据）**：所有任务被迫走同一条最重链路——12K 字符 meta 系统提示 + 40+ 工具 schema + thinking:high（实测单轮 22-40s，roles.yaml:38-40 注释自证），外加 WriteSpec 强制门 + submit_plan 计划确认门；**没有慢系统之外的快通道，也没有跑起来之后的纠偏通道**。
      1. **缺快通道**："轻任务直达"只是 meta 提示词纪律（meta_agent.go L131+），判断本身仍由最贵模型做；开工前强制 WriteSpec（dispatcher.go:2320，仅 scout/light 豁免）+ 计划确认（plan_confirm.go:33）多付 1-N 次 LLM 往返；【近期事件】超 8 条后每个 loop 迭代同步调一次轻量摘要 LLM（pipeline.go:357，120s 超时无缓存）；主循环同步写放大（每消息 Redis 2s 超时 message_log.go:38、每记忆事件 PG INSERT 5s 超时 pipeline.go:427）；SSE 500ms 轮询降采样把 token 流压成 2fps（stream_http.go:64）。
      2. **缺纠偏通道**：执行中子 Agent 拒收新消息（ErrAgentBusy，service_react.go:722）；对 meta 的注入只在无 tool_calls 的 drain 检查点生效（react_agent.go:1842），单次长 LLM 调用期间用户完全失声；纠正小缺陷只能 stop → ReviveWithMessage 拿原任务+上轮结果当种子重跑（service_react.go:1087）——纠偏代价=重做。
      3. **计划是门槛不是产物**：write_plan 是可选工具靠提示词自觉（plan.go:52 DAG 校验在代码层，但触发无强制）；计划确认 10min fail-open（无人值守形同虚设）；用户看到的不是可交互修改的设计稿而是倒计时审批框。
      4. **大任务并行交付链断**：worktree 合并门 base 漂移即拒（worktree.go:286-288），多域并行写码除第一个合入者外全部人工 rebase/重派，且 worktree 与热驻互斥（重派丢上下文）；依赖门是拒绝非排队（plan.go:118），靠 meta 用 22-40s 轮次反复重试推进；协作状态（mailbox/看板/契约）全在进程内存，重启即丢（单节点部署下非 P0）。
    - **统一设计框架：三档用户控制（外壳）+ 四模式能力组装（内核）**。三档与四模式是两条正交轴——档位=预算轴（用户愿意动用多大阵仗、等多久），模式=能力轴（任务需要什么工具与提示词）；**用户可见层用档位（普通用户零解释成本，直接管理时间与成本预期；行业佐证：Auto 模型选择/Deep Research 按钮/thinking 开关给用户的都是"档位"而非"模式"），能力模式退为内核实现**。
      - **用户可见三档**：
        - **快速档**：秒回问答，单轻量 agent，无门
        - **探索档**：边聊边改——steering 可打断、工件/文档能力、计划可见可改不强制审批
        - **集群档**：大任务多 Agent 编排，spec/计划门全装，预期 5 分钟级
      - **默认自动选档，三档为实体控制**：进来先自动选档（规则先行、小模型补充，误判成本=多一次交接而非失败）；档位指示器常驻可见、一键切换；升级信号明确时主动问一次（"这像大任务，预计 5-10 分钟，切集群档？"），降级不问；中途可升档（快速档聊成"帮我做个网站"→ 升集群档），反向不自动降级。纯手动死穴=普通用户系统性误判任务复杂度（选快档得浅答案、选集群档空等），故自动为默认、手动为覆盖。
      - **档位→内核模式映射表**（内核模式 = session 元数据 + 预设参数包：提示词集/工具白名单/门控开关/上下文策略；与 roles.yaml 角色白名单体系天然兼容，模式只是再加一层会话级预设）：
      | 档位 | 内核模式 | 提示词 | 工具白名单 | 门控 | 上下文策略 |
      |---|---|---|---|---|---|
      | 快速 | chat | soul.md+用户画像 ~3K | ≈空 | 无 | 全量历史 |
      | 探索 | artifact / document | 中量+工件操作说明 | 工件工具包 / ingest·search·read | 无（计划可见可改不强制） | 激进驱逐文件即状态 / 检索替代历史 |
      | 集群 | project | 现有 meta 全装 | 现有 40+ | spec+计划确认 | 现有金字塔压缩 |
    - **三大日常场景映射**（断点均已核实；场景→内核模式→档位）：
      - **持续修改 Excel → artifact 模式（探索档）**：现状表格能力为零（全仓库无 xlsx/excel/openpyxl，只能 RunCommand 写 Python 脚本，"第三列求和"都走写脚本→执行→读输出重型链路）；方向=表格工具包（read_range/set_cells/add_chart/list_sheets 结构化操作，同构扩展 WriteFile/EditFile 工件范式）+ 文件即状态（表格工具输出标记可驱逐，复用现有陈旧工具结果驱逐——10 轮前的读取结果毫无价值，当前文件状态重读即可）+ 会话粘性（热驻 domain 复用，跳过 meta 重路由）。
      - **读超大型文件 → document 模式（探索档）**：现状 `search_knowledge` 只能搜已入库知识，**没有摄取入口**；方向=新增 `ingest_file` 工具（文本提取 PDF/docx/纯文本 → retriever.Chunk 切块器已有 → embed → pgvector 已有，bootstrap.go:550-553 检索端已接线），摄入后问答走 search_knowledge 一次工具调用而非多 Agent 编排；"通读全文写总结"这类检索答不了的才动 map_sub_agents（32 项同构批量现成）——把编排题变成工具题。
      - **陪聊完善创意 → chat 模式（快速档）**：现状每句"再大胆一点"都过 12K meta + 40 工具 + thinking:high，22-40s 回一句话；方向=soul.md+画像极简注入、工具白名单≈空、thinking low、可打断（steering 价值最大场景）；evolveSession 用户偏好沉淀现成，恰好服务"懂我"。
      - 顺带覆盖：批量处理（100 张图压缩）map_sub_agents 能力已有，缺前端表达入口（纯 UI）；挂机长任务 Pause/Resume/软停止已有，缺进度聚合视图（看板数据已有前端未消费）。
    - **UX 翻译层补充包（2026-09-14 三次讨论增补）**：主干决定系统跑多快，这六件决定用户等的时候慌不慌、拿到结果顺不顺；全部挂载式前端/薄层工作，随档位切片顺带交付。
      1. **进度说人话**（三档通用）：事件流 32 种事件映射为自然语言状态（"正在读取报表→正在分析→正在生成图表"）；纯前端/轻映射层，数据全现成——大任务等待期焦虑主要来自"不知道它在干嘛"。
      2. **首会话体验缺口**（实测缺陷）：EnsureProjectDoc 在创建会话的 HTTP 同步路径内可能调 LLM 扫描项目，首个会话阻塞数秒~数十秒，新用户第一印象杀手；改异步——会话先建、后台扫描、好了再注入。
      3. **工件交付一等公民**（探索档）：ShowArtifact 已有（.bma/artifacts 落盘+展示），但"改完的 Excel 在哪、怎么下载、改了几版"对普通人不直观；文件类产出需明确预览/下载/版本入口。
      4. **撤销按钮**（探索档）：RestoreFile 已在 meta 白名单（文件快照恢复），升级为普通用户可见的"撤销上一步"；改 Excel 改坏了能一键回退，是普通人敢用的前提，比权限设计更能建立信任。
      5. **失败说人话**（三档通用）：LLM 超时上限 1800s、工具原始错误文本 → 可行动提示（"超时了 → 重试 / 升集群档"）；错误→行动翻译层。
      6. **run_code 执行器**（探索档底座）：代码作为参数直传（stdin/专用执行器）不经 shell 字符串，跨 shell 引号转义地狱（fixed_roles.go:36 `node -e` 软纪律的根因）从根上消失，且无需落临时文件；落地后"内联命令禁令"自然退役——该纪律是"用纪律补工具缺口"，不升级为代码级硬禁（沙箱黑名单 sandbox.go:40-43 只放 rm/ssh 类真危险命令，现状粒度正确；事实核查：代码层从未禁用 python -c，软纪律仅针对 code_assistant 的 node -e）。
    - **档位切换与记忆治理（2026-09-14 四次讨论增补）**：回答"低档接了大任务怎么办 / 中途切档记忆怎么约束 / 集群档记忆分散怎么优化"三问。
      - **A. 低档接大任务：升档机制**。关键决策=**升档判断交给正在干活的 agent 自己，不押进入口选档器**（入口只看到第一句话，agent 两轮后就知道任务多大，它才是最准的选档器）。
        1. 快速/探索档 agent 配唯一"逃生舱"工具 `escalate_gear(reason, task_brief)`：发现需要集群能力（多文件/多步骤/要派子 agent）时生成结构化交接摘要并触发升档确认；
        2. **升档 ≠ 重开会话**：复用 ReviveWithMessage 种子机制（原任务+已探明结论+交接摘要），快速档已做的调研转成集群档 meta 的初始上下文而非垃圾；
        3. 升档问一次、降档不问、**只升不降防乒乓**（任务不在两档间横跳）；
        4. 风险天然低：快速档 ≈ 无工具，接了大任务造不成破坏，只会答"这需要集群档"——软性失败不是事故；
        5. 入口选档器职责收窄为"第一眼像什么"，判断错了有 agent 自救兜底。
      - **B. 切档记忆约束：底账唯一、视图随档、交接摘要当锚点**。
        1. 底账不分档：PG agent_messages 历史与压缩状态是全档共享同一份账本，切档零数据迁移；
        2. 压缩金字塔本质是 Assemble 时的视图变换而非销毁——切档=换镜头不是搬数据，chat 全量历史与 project 压缩视图作用于同一份底账；
        3. chat 滚长后升集群档：首次 project 视图 Assemble 必触发一次大压缩（120s 摘要）——让升档交接摘要**兼任压缩种子**，一份成本两用；
        4. 切档=换系统提示词=前缀缓存作废，升档后第一呼全价——一次性成本，知情接受；
        5. 工件状态传递：探索档"文件即状态"，升集群档后子 agent 可能拿 worktree 副本改同一文件——交接摘要必须带**文件路径清单**（不是内容），经 spec 通道下发；
        6. 用户画像/soul 各档恒定；技能召回按新档白名单重跑一次（廉价）；
        7. 不自动降档再立一功：直接规避"集群态分布式上下文降回单人对话"这个无解的有损压缩——不让发生就不用解决。
      - **C. 集群档记忆分散：四步收敛**（定性：子 agent 私有上下文一半是隔离性的代价不该消灭，一半是真缺口；全部有现成挂载点）。
        1. **分层契约显式化**：三层已有——子 agent 私有工作记忆（transient）/ 共享事实层（黑板/WriteSharedMemory/PG 块记忆）/ 会话沉淀层（evolveSession）；缺的是纪律强制：**任何需跨 Agent 的信息必须显式升格到共享层，无隐式通道**——写进角色提示词+工具描述背书；
        2. **升格时机从完成时提前到里程碑**：现状子 agent 跑完才事实提取落库（saveBlockMemory），30 分钟的子 agent 第 5 分钟的关键事实兄弟与 meta 要等 25 分钟；启用 mailbox 里**预留未用的 MsgMilestone 类型**，子 agent 关键节点向黑板/共享记忆写中间事实——纯提示词纪律+钩子，侵入度低；
        3. **整合成本从 meta 轮次挪到后台轻模型**：N 个子回传 = meta 花 N 个 22-40s 轮次逐条消化；map_sub_agents aggregate=true 已证聚合回传可行——推广到异构：全部完成后先用轻量 LLM 把 N 份摘要预合并成一份"整合纪要"，meta 一轮看一份——集群档最实在的提速；
        4. **推送改拉取**：与其往每个子上下文塞共享内容（token 税），不如子 agent 轮间 drain 邮箱信号后按需拉取黑板切片——即 TODO#2"mailbox 降级为信号层"既有设计，两案合并实施。
        依据文档：doc/计划_共享记忆一致性.md（实施前先读，勿重新发明）。
      - **D. 两个近零成本优化点**：
        1. **模型随档分级**：快速档绑便宜快模型、集群档绑强模型；models.json 已有按角色绑定（model_ref），档位映射角色后基本纯配置——延迟成本双赢，P0-1 顺手做；
        2. **升档事件留痕**：每次 escalate/手动切档记一条 agent_events——选档器规则与"升档问一次"话术的调参数据源，没有它就是拍脑袋迭代。
    - **分阶段落地（按档交付，每档独立可用独立验收；侵入度标注，全部挂载式新增不拆主干）**：
      - **按档切片**：快速档 = P0-1 + 模型随档分级（D-1）；探索档 = P0-2 + P1-3 + 表格工具包 + ingest_file + run_code + 翻译层③④⑥；集群档 = P1-4 + P1-5 + 记忆治理 C-2/C-3；横向（全档）= 升档机制 A + 切档约束 B + 记忆分层契约 C-1 + 升档留痕 D-2 + 翻译层①②⑤ + P2-6 成本治理。
      - **P0-1 chat 模式 + thinking 降级**（侵入度低，大部分配置级）：meta 的 thinking:high 先在 roles.yaml 降下来立刻见效；chat 模式=瘦身 general agent（小提示词/工具子集/thinking low）绕开 meta 全装。
      - **P0-2 steering 纠偏通道**（侵入度中，三场景共同地基）：用户新消息从 drain 检查点升级为一级事件——loop 每次 LLM 调用前 drain 邮箱（现仅无 tool_calls 分支），进一步可 cancel 当前 LLM 调用注入消息带新指令重试；子 Agent ErrAgentBusy 改邮箱排队；复用 suspendGate/StopContext/sanitizeToolPairing 现有机制。
      - **P1-3 计划即产物**（侵入度中）：多步任务 plan-first 从提示词纪律升级为代码门；计划确认从倒计时审批框改为可交互设计稿（用户能改能批注，fail-open/fail-closed 按模式可配——project 模式可 fail-closed，chat 模式无门）。
      - **P1-4 依赖门排队化**（侵入度中）：checkDepGate 拒派改登记挂起，依赖完成 notify 事件自动唤醒；dispatcher 已有 notify channel 可复用。
      - **P1-5 合并门放宽**（侵入度中，worktree.go 单点）：base 漂移先试自动 rebase/三方合并，真冲突才升级人工。
      - **P2-6 成本治理**（侵入度低）：事件摘要加缓存或调阈值、压缩摘要后台预跑；SSE 500ms 轮询改事件驱动推送。
    - **可复用地基清单（实施时先查这里，勿重新发明）**：陈旧工具结果驱逐与 8000 runes 落盘（react_agent.go）；retriever.Chunk + search_knowledge + pgvector 混合检索（bootstrap.go:550-553，KnowledgeTypeExternalKB 已预留）；map_sub_agents 32 项同构批量+聚合回传；domain 热驻 LRU；suspendGate/StopContext/Pause/Resume/软停止；mailbox MsgRequest/MsgReply；DAG 校验（plan.go:52，注意 internal/dag/dag.go 是 cron 会话调度器与编排无关）。
    - **风险与对策**：① 选档器误判→低档必须可中途升档（快速档兜底升级集群档），不追求选档准确；② 打断一致性→残缺 tool 配对由 sanitizeToolPairing 兜底，**真正的难点是打断有副作用的工具调用（文件写一半）——靠工具幂等/事务性兜底，不能靠 loop**；③ fail-closed 悖论（用户不点确认永不动）→ 不选边，按档配门（集群档可 fail-closed，快速档无门）。
    - **验收口径**（启动后补实测基线）：chat 模式简单问答首 token ≤5s（对照现状 meta 路径 22-40s）；运行中会话用户插话在下一 LLM 调用前生效（不再 ErrAgentBusy/失声）；Excel 场景 10 轮连续小改不触发每轮隐藏摘要 LLM（历史驱逐生效）；500 页 PDF ingest 后问答不走 map 派发；多步任务计划以可编辑产物呈现而非倒计时框；首会话创建响应 ≤1s（EnsureProjectDoc 异步化后，后台扫描不阻塞建会话）；探索档数据处理任务零 shell 内联命令（run_code 直传，无转义失败重试轮）；快速档接大任务触发 escalate 后集群档首呼即带交接摘要与文件路径清单（已做调研零重做）；集群档 N 子回传 meta 整合 ≤2 轮（整合纪要生效，对照现状 N 轮逐条消化）。
    - **明确不做**：不拆 hub-spoke 拓扑（同 #13 决策）；不做邮箱/看板持久化（单节点部署下是负资产，重启失忆留作已知限制，咬人再按 #13 重启条件评估）；不引入完整 DAG 引擎或重写编排层；不做选档器大模型化（规则先行，小模型仅补充）；不自动降档（规避集群态→单人态的无解有损压缩）；不建分布式记忆织物（单会话派发上限 30、建议并行域 2-3 个，四步收敛足够）；探索档不做通用 Office 全家桶，表格先行（docx 已有插件路径）；不把内联命令纪律升级为代码级硬禁（沙箱黑名单只放真危险命令，python -c 从未被代码层禁用，保持现状粒度）。
15. **提示词结构工程：集群档自身瘦身（构成计量 → 工具收窄 → 延迟加载 → 手册按需）**（2026-09-14 立项讨论；设计备忘——第一批已落地（2026-09-15，范围与遗留见条内标注）；#14 的姊妹篇：#14 解决"别什么任务都穿重装"，本项解决"集群档那身重装本身太重"）
    - **第一批落地（2026-09-15）**：①构成计量——`react_agent.go` 每轮 `[prompt-stats]` debug 一行（sys/tools/hist/dyn）+ 首轮 info 分段明细（persona/env/role_base/discipline/skill），pipeline Assemble 段（compressed/events/filemap）同样计入；②meta 工具收窄 **40→22**（roles.yaml：去直接执行类 WriteFile/RunCommand/ReadMedia、角色管理类、项目刷新与记忆类、插件管理类；保留 ReadFile/EditFile/RestoreFile + 新增 SearchInFiles 收尾小改四件）+ **roleToolGate 默认开启**（config `role_tool_gate_enabled: true`；硬门闭包 = 角色静态 tools ∪ 该角色可见插件工具并集——防 meta 的 web_search 被误拦；逃生舱改回 false 即回软过滤）；⑤版本钉 `prompts.Version`（现值 20260915-1，语义改动必须 bump）+ 启动日志一行；⑥成本可见（TokenMetricsCard 会话累计行：总 token/耗时/轮次，复用 `GET /sessions/:id/efficiency`）。**遗留（维持"不抢跑"）**：③工具 schema 延迟加载（load_tool 两级制）、④角色手册按需注入；⑤金标准回归 eval 仍人工跑（test/eval 需真实 LLM 花费，不进 CI）。
    - **动机与根因**：#14 三档落地后快速/探索档绕开了 meta 全装，但集群档映射的仍是"现有 meta 全装"——12K 字符系统提示 + 40 工具 schema（实测单轮 22-40s，roles.yaml:38-40 注释自证）的每轮固定成本原封不动，且被**轮次数 × Agent 数双重放大**（meta 派发+N 轮消化+汇总，每个子 Agent 各背一份全装提示词）。集群档"笨重"的本质不是 Agent 多，是每轮推理的门票钱太贵。
    - **核心判定**（估算，待第 1 条计量验证）：40 个工具 JSON schema 按每个 200-500 token 估算 = 8K-20K token，**可能超过系统提示本身**，且每轮、每个 Agent 全量重发——工具 schema 是比角色正文更大的砍刀目标。
    - **改进动作（按收益/成本比排序，全部挂载式不拆主干）**：
      1. **前置件：提示词构成计量**（侵入度低，一切瘦身的前置）：现有 stable hash 日志（任务 131①）只回答"缓存变没变"，回答不了"哪块占多少"；在 Assemble 处加构成报表——身份段/工具 schema/记忆块/动态段各占多少 token、按角色输出。没有它，后续刀法砍多少、砍得对不对都是拍脑袋。
      2. **meta 工具收窄：摘重留轻，不是全摘**（零代码纯配置，最该先做）：meta 职责是编排，不该直接跑重执行；用刚落地的 roleToolGate（默认关闭，变更.md 任务 159）给 meta 配 ~15 个编排工具白名单（派发/邮箱/计划/spec/看板）——**摘重执行（RunCommand/run_code/测试类），留轻读写四件（ReadFile/EditFile/RestoreFile/搜索类）用于收尾小改**；schema 占用砍半以上；附带收益：40 选 1 → 15+4 选 1，工具选择准确率上升；防 meta 顺手把大活自己干了的行为漂移，靠工具描述写明"仅收尾小改" + 提示词纪律背书。
      3. **工具 schema 延迟加载**（收益最大，侵入度中高，放集群档切片后期）：两级制——常驻核心工具 + 其余只挂"目录"（名称+一行描述，数百 token），模型需要时调 `load_tool(name)` 把完整 schema 注入后续轮次（MCP tool search 同思路）；loop 需支持工具集轮间动态扩。先用第 2 条顶着，本条不抢跑。
      4. **角色手册按需注入**（侵入度中）：fixed_roles.go 长角色文拆成核心身份（恒定 ~1-2K）+ 场景手册（按任务检索注入 1 篇）；机制全现成——skills.yaml 声明层 + AGENTS.md ≤4K runes 注入槽（任务 131⑦）即"索引常驻、正文按需"范式，角色文拆块挂同一注入机制，不发明新轮子。
    - **收尾小改双路径（2026-09-14 场景追问增补，修正第 2 条的一刀切表述）**："任务完成后用户说改一行字"是最高频跟进动作，一刀切全摘执行工具会把它逼上"派发子 Agent 改一行字"的最重链路，恰与本项目标背道而驰；两条路：
      1. **热驻直达（首选）**：集群档完成后的时间窗口内，跟进消息不过 meta，直接投递热驻中的干活 worker（上下文完整，一次 LLM 调用 + EditFile 完事；忙时靠 #14 P0-2 邮箱排队，不再 ErrAgentBusy），结果按既有里程碑/事实机制落库抄送共享层；机制不新造 = #14"会话粘性（热驻 domain 复用，跳过 meta 重路由）"从探索档推广到集群档收尾；路由判断用规则（同会话 + 完成时间窗 + worker 空闲），不用 LLM；
      2. **meta 轻读写兜底**：热驻过期（LRU 挤出/重启）时 meta 用保留的轻读写四件自己动手，一轮解决；判断为多文件/要跑命令才升级派发。
    - **配套两点**：
      5. **提示词回归评估**：角色提示词版本钉住 + 关键场景金标准回归（test/eval 目录现成），拆提示词后跑一遍防角色行为悄悄退化——瘦身的质量兜底。
      6. **用户侧成本可见**（翻译层⑦，补入 #14 UX 翻译层）：P2-6 是系统侧成本治理，普通用户要的是"这单花了多少、为什么慢"——会话级 token/耗时摘要，事件流数据现成，纯前端聚合。
    - **验收口径**（实施时补实测基线）：meta 白名单收窄后单轮提示词 token 降幅 ≥40%（构成报表前后对照）且编排决策不回归（金标准 eval 全绿）；集群档典型任务（3 子 Agent）端到端 token 成本可量化下降（效率审计 Tab 支路成本表对照，任务 131⑨现成）；load_tool 落地后非常驻工具首次调用成功率不劣于现状（目录描述足以让模型选对工具）；角色手册拆分后叶子角色关键场景 eval 全绿。
    - **明确不做**：不做 worker"完成即冻结、唤醒从摘要恢复"——与 domain 热驻 LRU 复用直接冲突，热驻已控制常驻规模，单节点部署下是负资产；不重做前缀缓存机制——三层分级 + stable hash 已做（任务 131①），本项只砍构成；工具收窄不做硬编码——走 roleToolGate 配置，角色职责边界调整是配置事不是代码事；快速/探索档不做手册按需——那两档提示词本来就小，不值得检索层开销。
    - **与 #14 的关系**：#14 集群档切片（P1-4/P1-5/C-2/C-3）解决轮次数与协调拓扑，本项解决单轮门票钱；两者独立落地互不阻塞，验收共用同一份构成报表（第 1 条）。
16. **跨场景盲区七条（并发锁 / 模型降级 / 隐性信号 / 重启讣告 / 重连续播 / 数据遗忘 / 档位漂移）**（2026-09-14 立项讨论；设计备忘——第一批已落地（2026-09-15，范围与遗留见条内标注）；#14/#15 未覆盖的横向坑，按杀伤力排序，每条带最轻对策）
    - **第一批落地（2026-09-15，七条全做）**：1.乐观锁——EditFile/WriteFile 可选 `expected_mtime`（写入前 stat 比对秒级容差；冲突=工具结果级错误并列出当前 mtime 提示重读，不中止循环；未提供保持 advisory）；2.fallback 链——models.json `role_bindings[].fallback`，主模型出错（非 ctx 取消）依序试备胎全败返最后错误；观察回调由 agent 层注入（会话事件 + slog，model 包不反向依赖 agent）；3.隐性信号——`gear_signals.go` 只落 `gear_signal` 事件不改行为（fast 终态 ≤5min 后行动动词追问 / cluster 启动 ≤30s 取消）；4.重启讣告——interrupted 恢复分支 best-effort 查最近 agent_events/最后派发拼"上次任务断在 X"System 事件（nil-DB 静默）；5.重连续播——`streamSession` 页面可见期间持续重试（指数退避封顶 30s）、隐藏暂停、网络恢复即连；断档由快照全量替换兜底**零新端点**；恢复可见/联网时再拉会话与面板数据；6.按源删除原语——`DeleteBySource`（DELETE WHERE meta->>'source'）落库层原语，ingest_file 未做不接工具；7.热驻槽带档位——domainSlot 记 gear、复用比较不符跳过（空 gear 按匹配处理防误杀存量槽）。**维持明确剔除**：成本熔断/预算墙不做。
    1. **跨会话并发改同一文件无锁**（杀伤力最高）：三档设计全是单会话视角，普通人真实用法是多会话并行——会话 A 集群档子 Agent 改 `report.xlsx`、会话 B 探索档也在改 = 后写覆盖前写，无检测无提示；worktree 隔离只管派发内部，跨会话是真空。最轻对策：编辑类工具加乐观锁（EditFile 携带读取时 mtime/版本，写入前校验，变了报冲突而非覆盖），工具层单点改，侵入度低。
    2. **模型故障无 fallback 链**：#14 D-1 模型随档分级后坑被放大——快速档绑的便宜模型限流/宕机 = 最高频入口直接瘫痪。models.json 支持按角色配 fallback chain（主挂切备胎继续跑 + agent_events 留痕）；翻译层⑤只翻译错误，不解决停摆。
    3. **选档误判的隐性负反馈信号缺失**：#14 D-2 升档留痕是主动数据，用户被误判档时不点报错、只会沉默流失。被动信号两条：快速档答完立刻被追问"能帮我实际改一下吗" ≈ 选低了；集群档启动 30s 内被取消 ≈ 选高了；落 agent_events，是选档器迭代的真实数据源。
    4. **重启蒸发无讣告**：#14 明确不做协作状态持久化（正确，单节点负资产），但集群档 5-10 分钟任务跑一半进程重启 = 无声蒸发。只做讣告、不做全持久化：集群档关键里程碑复用 D-2 落 agent_events，重启后扫到未完成态主动告知"上次任务断在 X，要续吗"——几行代码换用户信任。
    5. **锁屏/断线重连续播**：移动端锁屏 10 分钟 SSE 早断；session_events 已落库（migrations 003），缺的是前端重连后补播断档期间事件——纯前端 + 薄接口；不做则"挂机长任务"对移动端用户等于黑盒。
    6. **ingest 数据的归属与遗忘**：#14 `ingest_file` 切块向量化进 PG 后，"忘掉这份文件"删不了；低成本预留——摄取时块记录带来源文件/会话字段 + `forget_file` 按来源删块；设计时不留，上线后补不动。
    7. **热驻实例的档位漂移**（小坑，一行对策）：热驻 LRU 里的 domain agent 带旧档提示词与工具集，切档后若被复用即串味；对策 = 热驻键带档位/模式，切档自然不命中、自然新建。
    - **明确剔除**：成本熔断/预算墙不纳入（2026-09-14 拍板）——stall 判定（任务 131②）已覆盖"卡住不动"，"活蹦乱跳烧钱"场景现阶段不防，咬人再议。
17. **领域注册表：领域精准拆分与跨会话复用（domain_profile + 文件事实反向注册 + 记忆成链）**（2026-09-14 立项讨论，explore 探查定案；设计备忘——第一批已落地（2026-09-15，范围与遗留见条内标注）；目标场景：今天做完支付模块 → 明天新会话"再改下支付"秒级命中原领域、复用块记忆链；领域跨子项目精确寻址文件，新增文件自动并入）
    - **第一批落地（2026-09-15，方案五件全做）**：①domain_profile 复用 global_knowledge（`KnowledgeTypeDomainProfile` + `UpsertDomainProfile`，meta={domain,display_name,aliases,files[],subproject,created_at,last_used,source}，embedding=goal 语义指纹，**零新表**）；②派发匹配——`SetDomainProfileHook`（dispatcher 与 store 解耦，bootstrap 接线）：任务文本 + spec FileList 提取路径 ∩ 档案 files → 命中归一化 + 冷复活种子注入（档案摘要/文件清单/记忆链拼进 task，逐文件 stat 剔除已删）；未命中照旧自由命名；**热驻/同名复用路径先于档案匹配**（档案只管跨会话冷复活）；③记忆成链——`QueryBlockMemoryByDomain`（按 meta->>'task_domain' + archived=false，created_at 倒序 LIMIT n）；④文件清单自动维护——saveRawBlockMemory/saveFacts 收尾旁路 union files_modified（幂等去重，失败仅日志）；⑤DomainPartition 种子导入——EnsureProjectDoc 成功后逐分区 upsert（source="project_md"），RefreshProjectDoc 同步更新。**遗留（维持"明确不做"）**：merge_domains 人工修正入口（先文档级流程）、领域档案 UI 管理页。
    - **现状三缺口**（2026-09-14 探查，均有 file:line 证据）：
      1. **"同一个领域"没有权威身份**：`call_sub_agent` 的 domain 是 LLM 自由命名（dispatcher.go:2343），无注册表无归一化；系统另有第二套结构化拆分 PROJECT.md `DomainPartition{Name,Purpose,Files}`（project.go:168-172），只作提示词注入（react_agent.go:1782-1784），**两套命名互不校验**；同名复用只认字符串精确匹配。
      2. **领域↔文件映射只活在易失载体里**：块记忆 meta 已落 `task_domain` 与 `files_modified`（dispatcher.go:5033-5044，新增文件同样被 recordFileWrite 记录，dispatcher.go:692-734），**事实数据都在，但没有任何一条 SQL 按领域名反查**——knowledge_store.go 全部查询以 session_id 为根（:263-291）；spec 的 FileList 键绑死 parentID（含 session 前缀，spec.go:288-294），新会话不可寻址。
      3. **跨会话复活路径为零**：同名 domain 复用要求同会话+同父+热驻池命中（dispatcher.go:2437-2500），重启后池空只能新建；agent_tree_nodes 持久化了 domain 名（agent_tree_store.go:147）"可见"但不可"复活"为可复用实体。
    - **题眼：精准复用靠文件集合事实，不靠名字**。LLM 自由起名必然漂移（"支付模块"/"支付"/"payment"），指望命名一致是系统里最不可靠的假设；匹配主键 = **文件路径重叠度**（确定性、毫秒级、零 LLM 调用），领域名 = 展示层标签，语义向量 = 无文件信号时的兜底。
    - **方案五件（全部挂载式，不动派发主干）**：
      1. **领域档案 domain_profile**：复用 global_knowledge 加 `knowledge_type='domain_profile'`（pgvector + KnowledgeStore.Save/Search 全现成，零新表）；meta = {显示名/别名集/**文件清单**/子项目路径/创建与最后使用时间}，embedding = goal 语义指纹；**首次派发完成时从 files_modified 事实反向注册**——档案是事实的沉淀，不是声明。
      2. **匹配流程（dispatcher 轻 hook）**：meta 派发"支付模块" → 任务涉及路径 ∩ 档案文件清单 → 命中即复用领域身份；同会话走现有热驻/同名复用路径，**新会话冷复活** = 新 agent 实例 + 注入档案摘要/文件清单/记忆链当种子；匹配 = 一次 PG 查询 <100ms，不是一轮 LLM；未命中 → 新领域，完成时注册。
      3. **块记忆成链**：补一条按 `meta->>'task_domain'` 精确召回、按 created_at 排序的 SQL（索引模式照抄 idx_global_knowledge_domain，001_init.sql:66）——"某某时间支付逻辑完成"→"某某时间支付完成 xxx 修改"按时间自然成链，复活时整条召回。
      4. **文件清单自动维护 = 跨子项目与新增文件的答案**：saveBlockMemory 落库旁路把 files_modified **union 进档案清单**（幂等去重），新增文件自动并入；清单是路径数组不限单根，天然支持多子项目（session 单 work_dir 只是"打开项目"的锚点）；复活时 stat 校验剔除已删路径防档案腐化（照抄 spec.go:275-282 的 stat mtime 模式）。
      5. **两套命名合一**：PROJECT.md DomainPartition 从提示词材料升级为注册表**种子来源**（EnsureProjectDoc 生成时导入）；派发命名在 dispatcher 侧归一化匹配注册表——meta 照常随口起名（不改提示词纪律，改了也守不住），归一化代码做；人改 PROJECT.md，档案随 RefreshProjectDoc 更新，可修可并。
    - **拆分精准度：宽进、校准、可修**。派发时不设卡（避免新摩擦）；dispatcher 归一化——文件重叠超阈值 → 回执"已并入已有领域 X"；完成时按 files_modified 事实校准档案，**越用越准**；提供 merge_domains 级人工修正入口（先文档级流程，工具后补）。
    - **侵入度**：档案注册/匹配/更新 = 中（挂 KnowledgeStore 旁路）；反查 SQL = 低；dispatcher hook = 低-中；派发流程、黑板、热驻全部不动。
    - **与既有设计的关系**：#15 热驻直达是"分钟级"复用，本项是"跨会话永久级"复用——同一两级缓存连续体（热驻池 → 领域注册表），不重复建设；同时补上 #14 C-1 分层契约里"共享事实层"一直缺的持久化实体；领域复用后同领域跨会话并发概率上升，#16-1 乐观锁成为前置依赖。
    - **验收口径**：第一天完成支付模块 → 新会话"再改下支付"零 LLM 调用命中档案（<100ms）、注入记忆链含上次"支付逻辑完成"条目；再次完成后块记忆新条目按 task_domain 反查成链；领域档案文件清单含多子项目前缀路径、修改中新增文件完成时自动并入；PROJECT.md 重组后档案跟随更新；复活时已删文件从清单剔除不注入。
    - **明确不做**：不改 meta 自由起名入口（归一化在 dispatcher 侧做，提示词纪律不可靠）；不做全量依赖图分析（clusterByDeps 启发式够用，project.go:675）；不接 #13 权限所有权（仍挂起）；不做领域档案的 UI 管理页（先跑通机制，可视化后补）。
18. **全项目缺口复查八条（装机 / 数据生命周期 / 质量门 / 提示注入 / 通知 / 绑定地址 / 多端 / 多租户预留）**（2026-09-14 全项目探查定案，均有 file:line 证据；设计备忘——第一批已落地（2026-09-15，范围与遗留见条内标注）；视角：普通人可用的平台，不只编排链路）
    - **第一批落地（2026-09-15，八条轻对策）**：1.装机——install.ps1 交互填 OPENAI_API_KEY（占位符跳过）+ 自动生成 BMA_API_TOKEN + 启动后探活 /api/health 与 /api/capabilities 逐项 [OK]/[FAIL]（含修复提示）；能力自检端点 `GET /api/capabilities`（llm/postgres/redis/embed/plugins/workdir 六项，全部廉价检查**不发真实 LLM 调用**——严格启动模式本身即连通性证明；每项 {ok,missing,detail,hint}）+ settings 页能力自检面板；2.数据生命周期——`data.*` 配置：`knowledge_archive_days`（0=关，opt-in）每日 tick 归档 block_memory/external_kb；logs/tool_outputs 保留期清理（0=默认 30/14 天，负数=关）启动+每日；会话导出 `GET /api/sessions/:id/export`（zip：session_history/events/agent_messages/agent_events 四份 JSON + workspace/.bma 产物树，2000 文件/64MB 上限）；记忆导出 `GET /api/export/memory`（未归档 global_knowledge JSONL + user_profile.md + skills_learned/）；**导入端点后置**；3.最小 CI——`.github/workflows/ci.yml` push/PR 即跑 backend vet+test 与 web 构建；**nightly integration 未配：test/ 模块已零 Go 文件（b35f87e 移除 test/api、test/coding），待集成测试重建后再补；-race 需 C 编译器不开**；4.untrusted 围栏——`tool.WrapUntrusted`（`<untrusted_data>` 围栏 + 围栏标记转义防逃逸）接入 mcpbridge 与 HTTPGet/HTTPPost（只包外部源，本地工具不包）+ meta/domain 提示词【数据围栏纪律】（Version bump）；5.通知——`notify.webhook_url`（空=关）+ `webhook_events`（默认 [completed,error]；实际状态枚举无 failed）60s 同会话同状态去重、3s 超时 best-effort；前端 document.hidden ∧ 终态 → 浏览器 Notification（settings 页申请权限）；6.默认绑 `127.0.0.1:10010`（局域网需显式 `HTTP_ADDR=:10010`，.env.example 已注明）；7.移动端——history 页 <768px 卡片化（md 断点卡片列表，桌面表格不动；只做列表，页面级移动重构维持不做）；8.owner 纪律写入 CLAUDE.md 硬约束（凡新建表/新字段统一带 owner 列）。**遗留**：导出恢复（导入）端点、nightly integration job、装机向导的插件 docker build 显式清单——均后置。
    - **先记录已核实无缺口的四件**（防重复检查）：migrations 启动自动幂等执行，代码内嵌 DDL 为准（bootstrap.go:167,881-893，schema.go 全 CREATE IF NOT EXISTS），migrations/*.sql 仅手动同步路径；技能学习闭环完整接线——会话末 SessionEvolver 产出技能包落文件+PG+向量（evolver.go:29-191），Meta/子 Agent 派发时向量预筛 top-3 注入（bootstrap.go:641-671），管理 API 齐全；TUI 与 web 共享同一 bootstrap/编排链路（cmd/tui/main.go:157 复用 bootstrap.Build）——三档落地时 TUI 自动继承，无需单独设计；密钥分发模板补缺合理（install.ps1:21 无 .env 才从 example 复制、不覆盖现场，skills_learned 同理保留）。
    - **八条缺口（按对普通人的杀伤力排序，每条带最轻对策与侵入度）**：
      1. **装机链路多处手动断点，普通人第一公里就翻车**（探查判定最致命 #1）：首装 .env 只给模板，LLM key 必填、BMA_API_TOKEN 空则 fail-fast 拒启（config.go:506-507）；插件能力还要手动 docker build 四个镜像 + compose 起服务（Makefile:59-64、plugins.yaml），缺了只在日志打一行、工具从 Schema 静默摘除（manager.go:284-291,519）——用户装完大概率"起不来或能力残缺而不知"。对策：装机向导（install.ps1 交互式填 key + 连通性校验）+ **能力自检面板**（启动后一页列清每个能力可用/缺什么/怎么补，把静默缺席变显式信号）；联动 #14 翻译层⑤。侵入度：低-中（脚本 + 只读状态页）。
      2. **数据只增不减 + 零备份兜底**（探查判定最致命 #2）：归档置位函数 Archive 是死代码（knowledge_store.go:365 无调用方），access_count/last_accessed 不驱动淘汰；logs/ 按天切分但无保留期（logoutput/writer.go:29-73）、.bma/tool_outputs/ 无清理（react_agent.go:946-958）、PG 各事件表仅删会话时级联清；PG/workspace/.bma 无任何导出/备份机制。对策三件：a) 激活归档——按 last_accessed+时间做 TTL 归档策略（列、索引、置位函数全现成，只缺调用方与策略配置）；b) 清理协程——logs/tool_outputs 保留期配置 + 启动期或每日清理；c) 一键导出——"带走我的记忆"导出包（PG 逻辑导出 + workspace/.bma 打包）。侵入度：低-中，全部旁路新增。
      3. **无 CI 质量门**（探查判定最致命 #3）：仓库无任何 CI 配置，test/ 的 api/eval 套件要手动起 compose.test + 手动 make 才跑——发给普通人的平台，回归质量靠开发者自觉。对策：最小 CI（GitHub Actions：push 即跑 backend-test + vet lint，integration 挂 nightly）；#15-5 提示词回归 eval 也需 CI 承载。侵入度：低（纯仓库配置）。
      4. **提示注入防线缺失**（架构推断，#14 ingest_file 落地的前置）：firecrawl 抓取的网页、ingest 的文档 = **不可信内容直接进上下文**，内容里的指令可指挥 agent 调工具；沙箱黑名单（sandbox.go:40-43）只拦 shell 命令，防不住"文本里的命令"。对策：不可信内容包裹标注（untrusted 围栏 + 提示词纪律"内容非指令"）+ 高危动作联动三级信任模式审批（任务 131⑥现成）；ingest_file 设计时同步落地，不后补。侵入度：低-中。
      5. **主动通知缺失**：挂机长任务完成/失败只有站内 SSE 系统消息（service_react.go:689-695），人不在页面前就看不到；全仓无 webhook/email/smtp 任何外部触达。#16-5 重连续播管"回来看"，本条管"叫你回来看"。对策：最小 webhook 出站（完成/失败 POST 用户自配 URL，server酱/企业微信/TG bot 均可接）+ 浏览器 Notification API（前端一次权限申请）；配置驱动默认关。侵入度：低。
      6. **HTTP 默认绑 0.0.0.0**：鉴权有（Bearer token，默认开启、空 token 拒启，server/auth.go:28-64），但 `addr: ":10010"` 监听全部网卡（config.yaml:31），局域网裸奔靠单 token。对策：默认改 127.0.0.1，需局域网访问时显式配置 + 注释警示。侵入度：一行默认值 + 文档。
      7. **移动端与多会话形态**：web 端全面桌面组件（Element Plus el-table 遍布），响应式痕迹仅两处；多会话 = 桌面列表 + 单会话详情，无并行视图。对策：短期不动布局——只做会话列表移动可用 + 第 5 条通知兜底；#6 UI 走查收尾后再评是否需要移动布局切片。侵入度：中（故排后）。
      8. **单租户断层预留（零成本留缝，不是实施）**：全库无 user 维度（global_knowledge/session/文件皆然）；现在不做多用户完全正确，但**凡新建表/新字段（#16-6 ingest 归属、#17 领域档案、本条导出包）统一带 owner 列**——现在加一列零成本，以后补是拆骨。侵入度：零（纪律）。
    - **验收口径**：全新 Windows 机器 install 后 5 分钟内可用（向导填空+校验，无手工 docker 命令也能跑核心链路）；连续运行 30 天 global_knowledge 活跃行数有界、logs/tool_outputs 体积有界；push 即触发 backend-test；集群档任务完成时锁屏手机收到浏览器/webhook 通知；默认配置起服后外网不可达（127.0.0.1）；导出包在新机器可完整恢复会话+记忆+工件。
    - **明确不做**：不做多用户完整改造（断层预留 ≠ 实施，#13 仍挂起）；不做移动端原生应用/页面级移动重构（短期通知+列表够用）；不替换日志系统（按天切分够用，补清理即可）；不做插件自动构建编排（docker build 保留手动，向导里给显式清单即可——自动化构建链是另一个项目）。

19. **经验沉淀治理四件套（技能目录收敛 / 写入门 / 库存整理 / 块记忆修复）**（2026-09-16 落地，动机与实现见 `doc/变更.md` 末条；此处只记观察点与遗留）
    - **落地**：A meta 提示只列经验技能 top-N（`skills.meta_catalog_top`，配置 `list_skills(query=)` 检索其余）；B 满库拒新建（`skills.max_count`，evolution_log `skill_dropped`）；C 每日/手动整理（`skills.consolidate_threshold` + `POST /api/skills/consolidate` + Web「立即整理」）；D 块记忆写入近邻去重（同域 cosine ≤0.15 更新既有行）+ `last_accessed` 修复（INSERT/BumpReuse）+ 事实提取头 4000/尾 2000 + `agent.block_memory_facts_max`。
    - **第二轮补丁（同日晚，用户定向"只沉淀改动的关键逻辑与信息"）**：块记忆触发门 `hasSubstantiveChange`（无文件改动+纯只读任务不沉淀）+ 提取失败/为空不回退原文 + 提取 prompt 跨任务复用判据 + salvage 失败原文不落库；存量 13 条 `[failure` 垃圾行 archived。
    - **观察点**：块记忆日增量是否显著下降（基线 1067 行 / ~35 天 ≈ 30 行/天）；沉淀是否只剩真改动结论；技能库增长曲线是否被压平（`learned_skills` enabled 数 / evolution_log skill_dropped 频次）；整理质量（skill_merge 是否误并、skill_archive 是否误归档——两者都可手工恢复：enable + 文件仍在 `config/skills_learned/`）；块记忆增长速率（1067 行基线，观察增量是否显著放缓）。
    - **遗留**：B 的「与 project_lessons 跨库判重」未做（项目经验无向量）；技能归档不自动恢复；块记忆**存量行**的去重与 `last_accessed` 回填未做（仅新写入生效——若要让 `knowledge_archive_days` 立即生效，需先手工 `UPDATE global_knowledge SET last_accessed = created_at WHERE last_accessed IS NULL`）；块记忆批量整理未做。
