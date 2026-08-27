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
