# 工作流编排：打样定标与批量量产（设计报告）

日期：2026-10-16
状态：思路报告，未实现；基于 2026-10-15 讨论整理
关系：`doc/扩展设计_Agent工作流平台.md` §2 是"通用工作流平台"愿景（DAG/触发器/多角色异构节点，未实现）；本文把同一主题**收敛到"创作资产量产"场景**给出具体形态，并直接吸收其 §12.1 的成本实证。与 orch 编排页设计（`2026-09-11-agent-orch-board-design.md`）互补：编排页看单个 Agent，本文看批量生产。

---

## 0. 一句话

**Agent 打样定标，工作流量产交付**：把"标准"从对话轨迹里物化成可复核、可版本化的**配方（recipe）**文件；量产期由引擎按配方驱动**有界单位流水线**（不是 N 个完整 ReAct 子 Agent）批量产出，配套三层校验（程序/模型/人）与联系表人审。

## 1. 场景与核心判断

**用户场景**：游戏开发。多种异兽 + 炮台，逐一设计行走动作、攻击动作、特效样式。同构度极高——第一只兽（第一类产出）的模式定下来后，其余全是"同一流程吃不同输入"。

**核心判断**：这个场景天然分两段，且两段的经济学相反，必须分开对待：

| | 打样期（定标） | 量产期（交付） |
|---|---|---|
| 件数 | 1 件 × 3 类产出 | 18+ 件 × 3 类 |
| 形态 | 人机对话、反复试错 | 重复、同构、可校验 |
| 判断依据 | 人的品味（无法写成规则） | 规则 + 程序 + 抽样人审 |
| 单件成本 | 高可接受 | 必须低 |
| 失败处理 | 继续讨论改 | 重摇 / 标记 / 升级 |
| 正确载体 | **ReAct Agent**（现在就能做） | **工作流引擎**（本文新增） |

**推论**：用户直觉"Agent 把整个标准确定为工作流后交给工作流产出所有一样的东西"方向正确；但"交给工作流"不等于"再多派 N 个 Agent"，这是最容易走错的一步（见 §2）。

## 2. 反模式与实证依据：为什么量产不能是 N 个完整子 Agent

### 2.1 实证

`doc/扩展设计_Agent工作流平台.md` §12.1 的 A/B（2026-08-08，同二进制唯一变量=每叶子完成后自动派 verifier）：

| 组 | 通过率 | 耗时 | token |
|---|---|---|---|
| 验证闭环开 | 5/16 = 31% | 60min | ≈558 万（verifier 独占 71%） |
| 验证闭环关 | 16/16 = 100% | 103min | ≈284 万 |

根因一（对本场景最致命）：**ReAct 子 Agent 按累计 input 计费**——每步全量重发历史，~25 次调用即烧穿预算。

### 2.2 外推

18 件 × 3 阶段 × 多帧，若每件派一个完整 ReAct 子 Agent：单件成本是打样期的数倍，批量越大越失控；且批量越大，单件的"判断价值"越低（模式已定），烧的全是重复上下文的钱。

### 2.3 正确形态：有界单位流水线

量产单位执行 = **固定序列 + 固定次数的轻量调用**：

```
填模板 → 调生成工具 → 程序校验 → 模型评委 → （重摇 | 采纳 | 升级）
```

全程 2~4 次调用、上下文字节数可预估、不随件数增长。这与仓库既有的"轻量模型干有界活"模式同款（`bootstrap/evolver.go` 技能提取、`skill_consolidate.go`、`memory_consolidate.go`、`judgeMemoryDuplicate` 都是这个形状）。只有校验连续失败或遇到没见过的输入，才升级为一个**真子 Agent**（进 Agent 树、走邮箱、出现在编排页）——把 ReAct 的贵，花在例外上，而不是流程上。

## 3. 核心抽象：配方 / 名册 / 运行

### 3.1 配方（recipe）——"标准"的物化形态

标准不是一个提示词，而是一份**可复核、可编辑、可版本化**的结构化文件。字段设计：

```yaml
# .bma/workflows/beast_motion.yaml
# 存放：生效工作目录下 .bma/workflows/（随项目持久，不随会话清理；git 可版本化）
id: beast_motion
version: 3                      # 每次修订 +1；run 记录绑定版本，保证"这批是哪个标准下做的"
title: 异兽/炮台 动作与特效力产线

deliverable:                    # 交付契约：程序校验直接按此判，能程序判的绝不叫模型判
  type: sprite_sheet
  frames: 8
  cell: 128x128
  alpha: true
  format: png
  path: "assets/units/{{id}}/{{stage}}/"     # 路径模板（{{id}}/{{stage}} 同 map_sub_agents 的 {{item}} 风格）

stages:                         # 件内阶段，串行执行；锚点自前向后传播
  - { id: concept, anchor_out: true }                          # 概念图采纳件 → 该件的概念锚
  - { id: walk,    anchor_in: [style, family, concept], anchor_out: true }  # 行走采纳件 → 动作语言锚
  - { id: attack,  anchor_in: [style, family, concept, walk] }
  - { id: vfx,     anchor_in: [style, vfx_family], frames: 4 } # 阶段可覆写交付契约

anchors:                        # 锚点池（i2i 参考图 / 视频 first_frame）
  style:      art/style_anchor.png          # 全局风格锚：打样定标时登记，全系列恒用
  family:     art/family_{{family}}.png     # 族系锚：冰系一脉共用
  vfx_family: art/vfx_{{element}}.png

invariants: >                   # 不变量（进提示词的不变段；逐件相同）
  三头身 Q 版比例，三视角（45° 侧前），赛璐璐上色 + 硬描边；
  动作节奏明快、帧间位移均匀、脚底不打滑。
slots: [id, name, element, family, body_plan, palette]   # 变量槽（逐件替换）

params:
  size: 1024
  reroll: 2                     # 校验不过的重摇次数上限
  seed_policy: reroll_jitter    # 重摇时 seed/提示词微抖动

check:
  program: ["frames==8", "cell==128", "alpha==true", "path 符合模板"]
  judge:   ["四肢/部件交替相位正确且可循环", "轮廓与概念锚一致", "主色与族系锚一致", "无边缘白边/锯齿"]
  human:   ["动作张力（按联系表横向对比）", "与既有动作语言统一"]
```

设计要点：

- **交付契约是配方的一等公民**。产出直接是游戏工程能消费的形态（帧数/尺寸/透明通道/命名/路径），这使校验的大头变成**免费的确定性程序检查**，而不是让模型"看一眼说还行"。
- **锚点绑定与阶段传播**共同保证系列一致性。打样件有双重身份：既是交付物，又是全系列的锚。i2i `reference` / 视频 `first_frame` 机制 2026-10-14 已落地（`ui_design` 插件），配方只是把它制度化——"永远带着锚点生成"。
- **不做提示词拼接细节写死**：不变量 + 变量槽 + 锚点在执行时合成实际提示词；配方里不存"最终完整提示词"，避免逐件复制粘贴文本。

### 3.2 名册（roster）——量产的对象清单

```yaml
roster:
  - { id: ice_dragon,   name: 冰霜巨龙, element: ice,  family: dragon, stages: [concept, walk, attack, vfx] }
  - { id: frost_wolf,   name: 霜狼,     element: ice,  family: wolf,   stages: [concept, walk, attack] }      # 无特效
  - { id: flame_turret, name: 烈焰炮台, element: fire, family: turret, stages: [concept, attack, vfx] }       # 炮台无行走
```

一行一件；`stages` 允许逐件裁剪（不是每件都要全阶段）。名册是调度的坐标，也是审查 UX 的行标。

### 3.3 运行（run）——一次量产

- run = 名册 × 阶段的**表格状态机**（同 orch 编排页把"树"当权威口径的思路：状态集中、可查、可恢复）。
- **run 不是 Agent**：它是独立实体，绑定 session（工作区/事件展示），但生命周期不挂在任何 Agent 的对话状态上——这是重启续跑的前提（§5.5）。
- 表草案（新建表带 owner 列，硬约定）：

```
workflow_runs:  id, owner, session_id, workdir, recipe_id, recipe_version,
                status(pending|running|paused|done|failed|aborted),
                budget_cap, spent_estimate, created_at, finished_at
workflow_units: id, run_id, owner, item_id, stage, attempt_no,
                status(pending|running|done|needs_review|failed|skipped),
                artifact_path, check_result(jsonb), reject_reason,
                created_at, updated_at
```

- 重摇 = 新 attempt 行；被否的 attempt **只标不删**（拒绝理由是配方演化的输入，§4.4）；采纳 attempt 置标志位。unit 状态流转同时写事件账本（沿用 `session_events`/`agent_events` 纪律），监控页可见。

## 4. 打样定标（Phase A：Agent 的活，今天就能做）

### 4.1 打样对话

正常会话（daily/cluster 档），用 `ui_design__od_image_generate` / `od_video_generate` 出图/出视频（多帧用 `prompts` 数组一次批量提交，桥内并发生成），带锚点 i2i 锁定，逐轮与用户确认。

### 4.2 冻结时刻：配方从轨迹提取（关键机制）

用户说"就这个，其余照这样做"时，系统**从会话轨迹里提取配方，不让模型凭记忆重写**：

- **输入**：成功且被采纳的工具调用序列（完整的 `od_image_generate` 参数）、被采纳产物路径、用户确认点（哪一轮说"可以"）。
- **步骤**：抓轨迹 → 登记锚点（采纳产物进锚点池，如概念图 → `concept` 锚、风格样图 → `style` 锚）→ 轻量模型参数化（把提示词切成"逐件变动段/不变段"，产出变量槽）→ 生成 YAML 草案 → **用户过目、可编辑** → 落盘 `.bma/workflows/`。
- **为什么不让模型凭空写**：模型重写的配方"看着合理但和实际做出来的东西漂移"——它既记不住被淘汰的 17 个候选，也不知道哪条约束是用户当场提出的。从轨迹提取保证 **配方 = 实际被确认的那次做法**。
- **入口**：v1 做对话内显式指令（"把这套标准存成工作流"）；工作区挑采纳产物冻结为配方作为后续入口。

### 4.3 配方演化

批量审查里的驳回原因（§6.3）回灌为配方新版本：如"边缘白边"→ 负向词补充 + 校验单加一条。run 绑定配方版本 → "哪批是哪个标准下做的"永远可查。

## 5. 量产执行（Phase B：引擎的活）

### 5.1 单位执行序列

```
for unit in units(run):                     # (件 × 阶段)
    prompt  ← 合成(recipe.invariants, recipe.slots[item], anchors)
    art     ← 调生成工具(prompt, 锚点)        # 多帧一次调用（prompts 数组）
    if     !程序校验(art, recipe.deliverable):   重摇或降级文本说明
    elif   !模型评委(art, recipe.check.judge):   attempt+1 重摇（上限 reroll）
    else:  采纳 → 登记 artifact →（阶段 anchor_out 则入锚点池）→ 下阶段
```

### 5.2 调度与并发

- **件间并行**：单位互不依赖（族系锚共享只读）。并发宽度受生成桥/守护进程容量约束，引擎侧保守可配（先 2~4 件并行，按观测调）。
- **件内阶段串行**：锚点传播要求（概念 → 行走 → 攻击以行走为锚），阶段间天然有依赖，不并行。
- **升级通道**：校验连续失败 / 未知输入 → ①标 `needs_review`（人审）或 ②派真子 Agent（走 `Dispatcher` 既有入口，自动继承并发池准入、Agent 树、邮箱语义——升级出的 Agent 自然出现在编排页）。
- 生成单位不是子 Agent，**不占** `max_concurrent_sub_agents` 池；该池只为升级出来的例外服务。

### 5.3 引擎落点（包分层）

- 新包 `internal/domain/workflow`（domain 层，不得 import `server`/`tui`/`agent`）。
- 依赖：`domain/tool.Registry`（直接调生成工具）、`model`（轻量模型评委/参数化）、`store`（run 表）、`domain/subagent.Dispatcher`（升级派发，保持池/树/邮箱语义一致）。
- `server` 适配层出端点；`bootstrap` 装配；前端消费。
- 实现期待定：引擎侧工具调用与 roleToolGate / 审批链的语义对齐（预挂机制 `MountPreApprovedForScope` 已有"按 scope 放行工具"的先例，可复用该思路，见 §11）。

### 5.4 预算与熔断

- 开跑前估算：单位数 × 每单位调用数 × 单价 → 展示给用户 → 硬上限（超限暂停，不静默烧钱）。
- **早期熔断**：前 k 件校验失败率超阈值（如 5 件里 3 件败）→ 停run，结论"配方有问题"→ 回打样期改配方，而不是把 18 件烧完。这是 §12.1 教训的直接制度化。

### 5.5 运行状态与续跑

- 后端重启/发版是常态（线上重启窗口约 1 分钟）——**重启不丢批量**是硬要求：
  - 已 `done` 的 unit 不重跑；`running` 的 unit 标回 `pending` 重跑（幂等键 = run+unit+attempt，重跑生成新 attempt）；
  - 启动时扫描 `status=running` 的 run 恢复调度；
  - 运行期间用户可 pause/resume/中止（复用会话级 pause/resume 软停止的既有交互）。
- 不用任何内存态做权威——权威在表 + 事件账本（对齐"底账可查"纪律）。

## 6. 校验体系（钱花在刀刃上）

| 层 | 谁判 | 成本 | 判什么 |
|---|---|---|---|
| 程序校验 | 确定性代码 | 免费 | 帧数/尺寸/alpha/命名/路径符合交付契约；能程序判的绝不叫模型判 |
| 模型评委 | 轻量模型 | 便宜 | 按 `check.judge` 逐条 + 锚点图对照，输出结构化 `{pass, reasons}`；视频类抽关键帧当图判 |
| 人审 | 人 | 贵，**批量级** | 品味类（动作张力、与既有语言统一）；按**联系表**审，不逐件翻卡片 |
| 漂移检测 | embed | 便宜 | 采纳产出嵌向量 vs 锚点/组质心距离，离群自动标记（复用既有 embed 端点与余弦比对模式） |

**联系表（contact sheet）**：同批同类产出拼成一张对比图，标准件置顶/左上。艺术审查的真实动作就是"横向对比"——逐件看卡片反而看不出风格漂移（"都还行"），并排看立刻暴露"第 7 只比例偏了"。流程：模型初筛 → 只把被标记/存疑项交人审 → 人审在联系表上逐格 accept / reject+理由 / 重摇。

## 7. UX 与页面

- `/workflow` Beta 占位页（`web/src/views/workflow/index.vue`）的正式定位：**配方库（列表 + 版本 diff）+ 运行列表 + 运行详情（名册×阶段网格 + 联系表审查流 + 暂停/续跑/重摇控制）**。
- **不做拖拽 DAG 编辑器**——本场景配方是"提取出来 + 改 YAML"，画布编辑是低价值部分；不做触发器/cron/webhook 触发（那是通用平台愿景的部分，本场景用不上）。
- 对话页是打样与例外处理的入口；工作流页是量产管理入口；产物在（项目）工作区，`/api/sessions/:id/workspace/*path` 已可流式服务，`ArtifactCard` 已可渲染。
- run 终态复用既有 webhook 通知（`notify.webhook_url`，`completed/error`）。
- TUI：v1 只做运行列表 + 状态，审查交互归 Web。

## 8. 复用清单 / 新增清单 / 明确不做

### 复用（实施时先查这里，勿重新发明）

| 能力 | 现状 | 在本设计中承担 |
|---|---|---|
| i2i `reference` + 视频 `first_frame` | 2026-10-14 已落地（ui_design 插件） | 系列一致性锁定的核心装置 |
| `map_sub_agents` 同构批量 | 已完成（重型 6 件/波、侦察类 32 件/波、聚合回传、`{{item}}` 模板） | Phase 0 直接当量产引擎用；Phase 1 引擎吸收其模板/聚合语义 |
| 子 Agent 全局并发池 | `max_concurrent_sub_agents`（默认 8，FIFO 排队） | 升级通道的准入（生成单位不占用） |
| artifact + 工作区流式服务 | `ShowArtifact` / `GET /workspace/*path` / `ArtifactCard` | 产物登记、预览、审查载体 |
| ask_user 三 hook（单题/批量/审批）+ `report_text` | 已落地 | 批量关键节点的审批门/确认 |
| pause / resume / 软停止 | 已落地 | run 的暂停/续跑交互 |
| 轻量模型有界任务模式 | evolver / skill_consolidate / memory_consolidate | 单位执行与评委的成本形状先例 |
| embed | llama-server 端点已接线 | 漂移检测 |
| 事件账本 + 监控页 | session_events/agent_events + `eventStyles.kindLabel()` | unit 状态流转留痕（新 kind 记得补映射） |
| webhook 通知 | `notify.webhook_url` | run 终态外呼 |
| board 进度数据 | 已写穿，前端未消费（TODO #7 遗留） | 运行详情进度展示可参考 |

### 新增

1. **配方提取器**（对话内指令 → 轨迹 → YAML 草案 → 确认落盘）。
2. **运行引擎**（状态机 + 有界执行器 + 三层校验 + 预算熔断 + 续跑）。
3. **表**：`workflow_runs` / `workflow_units`（带 owner 列）。
4. **端点**：配方 CRUD、run 启动/暂停/中止/续跑、run 状态、unit 操作（accept / reject+理由 / 重摇 / skip）；详见实现期 API 汇总。
5. **前端工作流页**（替换占位页）。

### 明确不做（YAGNI）

- 通用 DAG 编排引擎 / 拖拽编辑器 / 触发器（cron/webhook/git 事件）——通用平台愿景的部件，本场景无需求；
- 每件一个完整 ReAct 子 Agent；
- 跨角色混批（对齐 `map_sub_agents` 的克制原则）；
- 物理删除任何 attempt/产物（只标不删）。

## 9. 分期落地

### Phase 0：零后端改动，验证配方格式（今天可启动）

1. 正常对话打样第一只异兽（concept → walk → attack → vfx，ui_design 插件 + 锚点 i2i），验收采纳。
2. Agent 协助手写配方 YAML（§3.1 草案即格式起点），人工审一遍措辞。
3. `map_sub_agents` 按配方批量：一行一件、task_template 引用配方路径与件参数（重型 6 件/波，名册分批）。
4. 联系表人工拼（截图/前端拼贴皆可）→ 批量审查 → 重摇个别件。
5. **目标**：跑 5~10 件，验证三件事——配方字段是否够用、校验单措辞是否可判、审查节奏是否成立。
6. 局限明示：无结构化 run 状态、无续跑、无预算、单件是全量 ReAct 成本——只适合首波小批量试产。

### Phase 1：引擎

配方提取器 + run 表 + 有界执行器 + 三层校验 + 预算熔断 + 续跑 + 端点。验收：断进程重启后续跑不重不漏；预算上限确实拦截；50 件批次的单件成本显著低于 Phase 0。

### Phase 2：审查与演化

联系表 UX + 漂移检测 + 配方演化（驳回原因回灌版本）+ 配方库化（跨项目复用，届时再议存 global_knowledge 还是独立目录）。

### 验收指标（Phase 0 起记录，用于验证本设计成立）

- 单件均摊成本（token/调用数）随批量上升是否基本持平（有界流水线成立的标志）；
- 一次通过率（免重摇比例）与重摇收敛率；
- 人审时间/件（联系表是否真的降低审查成本）；
- 漂移率（离群标记命中人工驳回的比例）。

## 10. 开放问题（需拍板）

1. **交付物形态**：动作产出是视频（i2v → 转 sprite sheet）还是直接关键帧序列？定了才能写死交付契约与程序校验（也决定生成工具选型）。
2. **锚点传播链**：攻击/特效是否以行走采纳件为锚（动作语言统一）？影响阶段依赖图与并行度。
3. **配方归属**：项目工作区（本项目复用）vs 全局库（跨项目复用）——影响存储与可见性设计；本文默认前者。
4. **联系表生成位置**：前端 CSS 拼（零后端）vs 后端拼图（可存档、可外发）——影响审查归档。
5. **引擎工具权限门**：引擎侧调用生成工具时，roleToolGate/审批链按何种 scope 放行（预挂机制思路复用？）——实现期第一个要定的接线问题。
6. **并发与费用闸**：同时跑多个 run 的额度策略（全局一个活跃 run？可配并发 run 数？）。

## 附：术语表

| 术语 | 含义 |
|---|---|
| 打样 / 定标 | 第一件（第一类产出）的人机迭代期；产出被确认时"冻结"为标准 |
| 配方 recipe | 标准的结构化物化：交付契约 + 变量槽 + 不变量 + 锚点 + 校验单 + 参数；工作区 YAML，带版本 |
| 名册 roster | 量产对象清单（逐件：id/族系/阶段子集） |
| 运行 run | 名册 × 配方的一次量产执行；独立实体（非 Agent），可暂停/续跑 |
| 单位 unit | run 的最小执行格：(件, 阶段)；有界流水线执行 |
| 锚点 anchor | 锁一致性的参考素材（i2i reference / 视频 first_frame）；分风格锚/族系锚/件内锚 |
| 联系表 contact sheet | 同批同类产出拼图对比的批量审查界面形态 |
