# 18 Goal 指令与 Flow 流程

> 本章说明两种容易混淆的编排：Goal／Loop 是“会话结束后的续跑指令”，Flow 是“显式节点图的运行”。二者共享会话、模型、工具与 HTTP 适配层，但有各自的状态和存储。
>
> 主要代码：`internal/goal`、`internal/flow`、`internal/agent/directive.go`、`internal/server`、`internal/bootstrap`；配置为 `goal.*`、`flow.*`。

## 第一部分：Goal／Loop 会话指令

### 18.1 模块边界与模式

会话指令支持三种模式：

| 模式 | 行为 |
|---|---|
| `normal` | 普通会话，回合结束后不自动续跑 |
| `goal` | 回合完成后由轻量模型评估目标是否达成，未达成则注入下一回合 |
| `loop` | 按固定间隔重复执行，直到停止或满足 `loop_until` |

Goal 不是新的 ReAct 引擎，也不替代子 Agent 树。它只在会话回合到达 `completed` 时运行 `OnTurnEnd`，对结果做后续决策；`error`、暂停或其他非完成状态不能按“正常完成”进入同一逻辑。

权威数据：

- `session_directive`：会话当前模式、目标、循环参数等；
- `goal_reminders`：到期提醒与触发状态。

`schedule_followup` 工具也可为普通会话安排延迟跟进，不依赖会话必须处于 goal／loop 模式。可配置的延迟范围为 1 分钟至 30 天。

### 18.2 入口与依赖

```text
HTTP PUT /api/sessions/:id/directive
                  │
                  ▼
           Goal Manager / Store
                  │
ReactService 回合 completed ─► OnTurnEnd
                  ├─ goal：Judge 评估
                  │      └─ 未达成 → ReactService.Send 注入续跑
                  └─ loop：写下一 goal_reminder

后台扫描 goal_reminders ─► 原子标记 fired ─► 唤醒会话
```

- 评估器使用轻量模型，避免每次续跑都调用主执行角色。
- 唤醒最终仍进入 `ReactService.Send`，复用会话的模型、工作目录、历史与工具。
- 扫描器和管理器由 bootstrap 启动；存储与 HTTP 不自行另造执行循环。

### 18.3 Goal 主流程

1. 用户通过指令 API 将模式设为 `goal` 并给出目标。
2. 普通 ReAct 回合完成，状态成为 `completed`。
3. `OnTurnEnd` 调用完成度评估器；评估输入围绕目标和当前结果，不另起一套隐藏任务状态。
4. 若判断达成，不排后续回合；若未达成，通过 `ReactService.Send` 注入续跑。
5. 每个后续回合仍走正常会话完成条件，再次触发评估。

“轻量模型判断达成”是评估机制，不是外部可验证事实的绝对保证。对需要严格验收的任务仍应使用明确测试、程序校验或人工验收。

### 18.4 Loop 与跟进提醒

Loop 每轮安排下一次提醒，`loop_until` 可提供停止边界。后台扫描到期提醒，先原子标记为 `fired`，再唤醒会话。

这个顺序的语义是**至多一次触发，失败时可能丢失本次唤醒**：如果标记成功后唤醒失败，不会把提醒回滚成未触发。因此文档和调用方不能宣称“保证送达”或“失败必然自动重试”。

默认与约束：

| 配置 | 当前值 | 含义 |
|---|---:|---|
| `goal.poll_interval_sec` | 15 | 到期提醒扫描间隔，秒 |
| `goal.min_loop_interval_sec` | 60 | Loop 最小间隔，秒，防止过密空转 |
| `goal.judge_timeout_sec` | 60 | 单次完成度评估超时，秒 |

`min_loop_interval_sec` 是最小间隔限制，不代表每次执行恰好准点；实际还受回合时长、扫描与服务状态影响。

### 18.5 API 与状态边界

| 方法与路径 | 用途 |
|---|---|
| `GET /api/sessions/:id/directive` | 查询会话当前指令模式与参数 |
| `PUT /api/sessions/:id/directive` | 设置 normal／goal／loop 或更新对应参数 |

模式切换应通过权威指令数据完成，不能只改前端显示。恢复依赖数据表扫描，而不是进程内定时器列表。

### 18.6 测试锚点与限制

`internal/goal` 测试使用内存 Store、假评估器和假唤醒器，覆盖评估、提醒与模式行为。它不证明真实模型一定能正确判断目标，也不证明唤醒失败可重投。本次未运行测试。

## 第二部分：Flow 可视化流程

### 18.7 模块边界

Flow 是有向节点图的 v1 串行解释执行器。支持六类节点：

| 节点 | 职责 |
|---|---|
| `start` | 流程入口与初始输入 |
| `llm` | 调用模型生成文本／结构输出 |
| `tool` | 经工具注册表执行工具 |
| `branch` | 根据表达式／条件决定下一跳并剪枝 |
| `approval` | 挂起等待人工通过或拒绝 |
| `answer` | 形成最终答案并成功结束 run |

Flow 不是 Workflow 的 UI 别名：Flow 以“节点”为执行单位，Workflow 以“件 × 阶段”的 attempt 为单位；二者表结构、API、调度与人审语义均不同。

### 18.8 定义与运行真相源

- 流程定义文件：`<workdir>/.bma/flows/<id>.json`。
- 创建 run 时把定义快照写入 `flow_runs.flow_json`，后续编辑定义不应悄悄改变已经创建的 run。
- 节点轨迹写入 `flow_node_runs`。
- 文件是定义真相源，数据库是运行真相源；不能只读当前文件还原始运行定义。

### 18.9 执行与分支

v1 按当前节点选择下一节点，串行解释执行：

```text
start → llm/tool → branch ─┬─ 被选路径 → approval → answer
                           └─ 未选路径剪枝
```

- `llm` 通过模型层执行，`tool` 通过 `tool.Registry` 执行。
- `branch` 决定下一跳，未被选择的分支被剪枝。
- `answer` 成功即完成整个 run。
- 工具失败直接导致 run 失败；当前没有通用节点重试与失败分支机制。
- 单节点默认超时 120 秒，由 `flow.node_timeout_ms: 120000` 配置。

不要把 branch 画成并发 fork：当前是路径选择，不是并行执行所有 outgoing edge。

### 18.10 审批语义

执行到 `approval` 时：

1. run 进入 `waiting_approval`，保留当前节点和运行快照；
2. 用户通过节点裁决端点提交通过或拒绝；
3. 通过后继续后续节点；
4. 拒绝则 run 失败，而不是“跳过审批继续执行”。

审批状态属于 run，不等于普通会话的 `awaiting_clarify`，也不等于 Workflow 单元 `needs_review`。

### 18.11 并发、恢复与限制

- `flow.max_active_runs: 8`：默认最多 8 个活跃 run，超出的等待提升。
- 服务重启时，把 `running` 节点复位为 `pending`，恢复 `running` run。
- `pending` run 按 FIFO 提升到活跃槽。
- 恢复依赖 run 中的定义快照与节点轨迹，不重新读取最新定义覆盖旧 run。
- 节点超时和数据库恢复不等于外部工具副作用可回滚；有副作用的 tool 节点仍应设计幂等。

### 18.12 HTTP API

当前 8 个 `/api/flow/*` 端点按职责如下：

| 方法与路径 | 用途 |
|---|---|
| `GET /api/flow/flows` | 列出当前工作区的流程定义 |
| `GET /api/flow/flows/:id` | 读取定义详情 |
| `PUT /api/flow/flows/:id` | 保存／更新定义 |
| `POST /api/flow/runs` | 基于定义快照创建 run |
| `GET /api/flow/runs` | 查询 run 列表 |
| `GET /api/flow/runs/:id` | 查询 run 与节点轨迹 |
| `POST /api/flow/runs/:id/abort` | 中止运行 |
| `POST /api/flow/runs/:id/nodes/:nid/decide` | 对审批节点提交裁决 |

Web `/flow` 提供列表与运行入口，`/flow/edit` 提供可视化编辑器。路由与参数结构以 `internal/server` 当前注册和前端 API 客户端为准。

### 18.13 配置、降级与测试

| 配置 | 当前默认 | 影响 |
|---|---:|---|
| `flow.max_active_runs` | 8 | 全局活跃 run 闸与 pending 排队 |
| `flow.node_timeout_ms` | 120000 | 单个 llm／tool 等节点执行超时 |

已有 `internal/flow` 测试源码覆盖定义校验、分支、审批、工具、变量和中止。尚未覆盖或当前未实现的语义不能推断为存在，尤其是通用重试、失败分支、并行 fork、外部副作用回滚和审批超时自动策略。本次未执行测试。

## 18.14 修改检查清单

- 改 Goal：检查 `OnTurnEnd` 是否仍只接收 completed、唤醒是否复用 ReactService、提醒 fired 语义是否被误改。
- 改 Loop：检查最小间隔、loop_until、重启扫描与至多一次语义。
- 改 Flow 定义：检查保存文件与 run 快照的边界，旧 run 不应被新定义污染。
- 改节点类型：同步 definition 校验、执行器、前端编辑器、节点轨迹和测试。
- 改审批：不要混用 Flow `waiting_approval`、Workflow `needs_review` 和会话澄清状态。
- 改恢复：检查 running 节点复位、FIFO 提升与外部工具副作用，不能承诺无条件 exactly-once。
