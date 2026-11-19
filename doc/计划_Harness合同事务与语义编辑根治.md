# BMA Coding Harness 根治实施计划：任务合同、事件投影、事务执行、语义编辑与独立验收

- 文档状态：**待执行，不代表任何功能已经实现**
- 制定日期：2026-11-18
- 执行对象：负责修改 BlockMemoryAgent 仓库的执行 Agent
- 事故审计版本：桌面后端 `20261118-115416-9dbc885`，commit `9dbc885`
- 本计划核对源码时的仓库基线：`main@a7905c2`
- 事故会话：`session-1794974762351783800-1d9520d6-1`
- 审计材料：`workspace/harness-audit-20261118/`（本地审计材料，不得打包或提交敏感业务源码）

> 执行前先重新核对当前分支与符号。本文中的文件路径是改造挂点，行号不是稳定 API；后续代码变化时以函数名、类型名和测试为准。

---

## 0. 给执行 Agent 的硬性指令

### 0.1 目标不是修一个 CSS 案例

本计划要解决的是一类系统性问题：

1. 长会话中当前任务与历史任务混杂，用户纠正没有形成明确状态迁移；
2. 已确认的非目标约束没有随任务继续生效；
3. 模型可通过不同工具入口获得不同的写入权限与审计语义；
4. 生产代码依赖正则或任意脚本盲写，缺少唯一目标与最小源码范围编辑；
5. 验收只证明构建或自建夹具通过，不能证明“目标完成且非目标不变”；
6. token、模型调用和测试助手成本没有按“最终被用户接受的逻辑任务”核算。

**禁止把本计划缩减为以下伪修复：**

- 只在系统提示词里增加“不要修改无关内容”；
- 只增加 `margin/padding/min-width` 等 CSS 专用规则；
- 只扩大或缩短聊天历史窗口；
- 只在 `RunCommand` 增加几个字符串黑名单；
- 只检查 `git diff --stat` 或文件数量；
- 只让测试助手再检查一次，或把同一个模型的自述当作独立验收；
- 为追求 token 降幅而删除当前合同、用户不变量或必要源码上下文。

### 0.2 工程纪律

1. 遵守根目录 `CLAUDE.md` 和 `docs/superpowers/plans/dependency-rules.md`。
2. Go 命令统一使用 `GOTOOLCHAIN=local`；Windows Git Bash 使用 Unix shell 语法。
3. 新增持久化表和字段必须同期携带 `owner`，并保证会话级数据可按 owner 隔离。
4. 事件底账 append-only。投影可以更新；重规划、压缩、回滚不得物理删除历史事件。用户显式删除会话的既有级联清理例外保持不变。
5. 不修改 `.env`、模型密钥、用户配置、用户业务仓库和桌面安装目录；部署与桌面更新必须另行取得 luokun 明确指令。
6. 默认不运行真实付费 LLM、不生成付费媒体、不访问生产数据。真实模型评测单独报告花费与模型版本。
7. 不通过修改既有测试断言来掩盖行为回归。确需改变公开行为时，先记录兼容性依据，再新增迁移测试。
8. 不执行 `git commit/push/reset/rebase`，除非 luokun 另行明确要求。每个工作包交付可审查的 diff。
9. 每个工作包先写能够证明旧缺陷的测试，再实现；测试失败时停在该工作包，不跨层堆叠后续改造。
10. 最终只报告实际运行过的命令和结果。环境依赖缺失、未运行、跳过、flake 必须明示，禁止把“写了测试”写成“测试通过”。
11. 不把事故业务项目源码复制进仓库。回归测试使用脱敏、最小化的合成 Vue/Go fixture。
12. 每个工作包控制在可独立回退的变更集内；不得在一个巨大改动里同时替换状态、执行、编辑和验收全部机制。

### 0.3 完成定义（Definition of Done）

一个工作包只有同时满足以下条件才算完成：

- [ ] 功能代码与负面测试均已实现；
- [ ] 受影响包测试通过；
- [ ] 无静默降级、无未记录的兼容行为变化；
- [ ] 失败路径有结构化事件，能够回答“谁、基于哪个合同版本、做了什么、实际改了什么、为何提交或回滚”；
- [ ] 不破坏旧会话恢复、dirty worktree、未跟踪文件和用户外部修改；
- [ ] 配置默认行为符合本计划的灰度顺序；
- [ ] 文档记录了实际测试命令、结果、未验证项和已知限制。

---

## 1. 事故证据与根因边界

### 1.1 已确认事实

事故会话约 5 小时，包含 22 个用户轮次和 702 个事件。聚合指标为 268 次调用、14,150,610 输入 tokens、221,432 输出 tokens；原始日志另有 288 次 LLM 调用口径，报告中不得混用。

关键轮次：

- 第 15 轮：用户只要求“下单信息的收货人这个标题改成和其他标题一样长度”，模型通过 `RunCommand → Node → fs.writeFileSync` 把两个页面的共享标签列宽从 `4.2em` 改为 `5em`。本轮使用 5 次调用、237,780 输入 tokens。
- 第 20 轮：用户明确商品名称前增加 30px、下单时间前增加 60px。
- 第 21 轮：用户只要求修省略，模型同时修改三列的 `min-width/width/flex`；本轮 35 次调用、1,352,936 输入 tokens。
- 第 22 轮：用户指出下单时间位置和此前距离被破坏。

### 1.2 不是“现有能力全部不存在”

执行 Agent 必须保留并复用以下已有基础：

- `backend/internal/agent/session_react.go`：事件、消息、History、决策缓存和会话运行时；
- `backend/internal/agent/decision_ledger.go`：独立决策账本，已有 active/superseded 语义；
- `backend/internal/domain/memory/pipeline.go`：上下文装配、压缩视图、文件小地图、事件持久化；
- `backend/internal/team/contracts.go`：C-06 的 `TaskContract`，已有 Acceptance、ReadSet、WriteSet 等字段；
- `backend/internal/team/context.go`：来源、信任等级、冲突裁决和不可变上下文清单；
- `backend/internal/domain/tool/registry.go`：统一 Dispatch、默认关闭的角色工具硬门、危险操作审批和文件后置钩子；
- `WriteFile/EditFile` 的 `.bma/snapshots`；
- `backend/internal/domain/subagent/worktree.go`：并行 worktree、patch、review、merge/reject、base 漂移处理；
- `service_react.go` 的 AcceptanceRunner 和 SessionEvolver；
- 长期记忆默认 `shadow` 和受控晋升机制。

### 1.3 根本缺口

现有能力分别服务于不同子系统，但缺少共同的一次执行事实：

```text
当前逻辑任务是什么
→ 当前合同版本是什么
→ 本轮允许操作的资源是什么
→ 哪些历史约束仍活跃
→ 实际产生了哪些变化
→ 哪些验证证明目标和不变量同时满足
```

此外：

- `Goal` 是最初目标，不能单独表达当前任务；
- 决策账本最近指令窗口不是代码状态合同；
- C-06 `TaskContract` 服务自认领计划，`InitPlan` 还有全量替换投影语义，不能直接当作顶层 LogicalTask Store；
- `Registry.Dispatch` 是正确挂点，但还没有统一的合同资源授权、前后检查点和实际变更审计；
- 顶层首跑与续跑虽然都能进入验收，但传的是裸 goal 文本，不是任务 ID 和合同版本；
- 每个微任务收尾触发 Evolver，不等于任务已被用户接受。

---

## 2. 目标架构与不可破坏的不变量

### 2.1 目标形态

```text
用户输入 / 运行中纠正
        │
        ▼
TaskManager ────────► LogicalTask / ContractRevision
        │                     │
        │                     ├──── TaskProjection（当前状态）
        │                     ├──── DecisionProjection（决定与否决）
        │                     └──── CodeProjection（基线、修改、验证）
        ▼
ContextAssembler：权威合同 + repo map + 当前锚点 + 短期工作集
        │
        ▼
模型提出 Action / PatchPlan
        │
        ▼
Registry.Dispatch 唯一生产执行入口
        │
        ├─ resolve resources
        ├─ authorize contract + policy
        ├─ checkpoint
        ├─ execute
        ├─ observe real mutation
        ├─ verify scope + invariants
        └─ commit / rollback
        │
        ▼
分层独立验收 → verified / delivered-unverified → 用户接受
        │
        ▼
受控 Evolver / Memory Promotion
```

### 2.2 系统级不变量

以下要求属于架构不变量，必须用测试保护：

- **I-STATE-1**：当前合同和活跃不变量永远不参与有损摘要。
- **I-STATE-2**：历史消息、摘要、看板均为视图或投影，不得反向覆盖权威任务事件。
- **I-STATE-3**：模型不能通过工具调用自行扩大合同写范围；扩大范围必须产生新合同版本。
- **I-EXEC-1**：所有生产写入只有一个 Action pipeline，不允许旁路。
- **I-EXEC-2**：文件越界判断包含路径、语义目标和不变量，不只是文件名。
- **I-EXEC-3**：检查点失败时，强制模式下的生产文件写入 fail closed。
- **I-EXEC-4**：回滚只能回退本 Action 的变化，不得 `git reset --hard`、不得清空用户 dirty/untracked 文件。
- **I-EDIT-1**：结构化编辑必须验证唯一目标和预期指纹，只返回最小 source-range TextEdit。
- **I-VERIFY-1**：构建通过不等于验收通过；必需证据缺失只能为 unverified。
- **I-VERIFY-2**：验收规则和用户守护测试不在执行 Agent 可编辑范围内。
- **I-LEARN-1**：未验证或正在被用户纠正的任务不得产生正面技能或晋升长期记忆。
- **I-COST-1**：成本按 LogicalTask 聚合所有 Agent、验收和 Evolver，而不是只看单次主模型请求。

---

## 3. 核心数据模型与状态机

### 3.1 不直接修改 C-06 的兼容语义

保留 `team.TaskContract` 现有 JSON 和自认领行为。新增 `LogicalTaskContract`，可以复用或嵌入通用字段，但不得改变 C-06 的 PlanVersion、InitPlan、Claim/fencing 语义。

建议新增：

- `backend/internal/team/logical_task.go`：纯类型、规范化、状态迁移；
- `backend/internal/team/logical_task_store.go`：存储接口；
- `backend/internal/store/logical_task_store.go`：PG 实现；
- `migrations/020_logical_tasks.sql`：文档性迁移副本；
- `backend/internal/store/schema.go` 或独立 schema 文件：幂等 Ensure 入口。

### 3.2 建议类型

```go
type LogicalTaskContract struct {
    SchemaVersion   int              `json:"schema_version"`
    LogicalTaskID   string           `json:"logical_task_id"`
    SessionID       string           `json:"session_id"`
    Revision        int64            `json:"revision"`
    ParentRevision  int64            `json:"parent_revision,omitempty"`
    RelatedTaskIDs  []string         `json:"related_task_ids,omitempty"`
    Goal            string           `json:"goal"`
    OriginEventID   int64            `json:"origin_event_id,omitempty"`
    Targets         []SemanticTarget `json:"targets,omitempty"`
    ReadSet         []ResourceRule   `json:"read_set,omitempty"`
    WriteSet        []ResourceRule   `json:"write_set,omitempty"`
    ExcludedSet     []ResourceRule   `json:"excluded_set,omitempty"`
    Invariants      []Invariant      `json:"invariants,omitempty"`
    Acceptance      []AcceptanceRule `json:"acceptance,omitempty"`
    BaseCheckpoint  string           `json:"base_checkpoint,omitempty"`
    Status          TaskStatus       `json:"status"`
    ContentHash     string           `json:"content_hash"`
}

type SemanticTarget struct {
    ID          string `json:"id"`
    File        string `json:"file,omitempty"`
    Language    string `json:"language,omitempty"`
    Kind        string `json:"kind,omitempty"` // symbol/template_node/css_declaration/api/...
    Anchor      string `json:"anchor,omitempty"` // 适配器解析用的结构锚点，不是任意正则
    Fingerprint string `json:"fingerprint,omitempty"`
}

type ResourceRule struct {
    Kind     string `json:"kind"` // path/glob/semantic_target/tool/network/database
    Value    string `json:"value"`
    Effect   string `json:"effect"` // allow/ask/deny；ExcludedSet 恒为 deny
    Source   string `json:"source"` // user/resolver/policy/legacy
    Revision int64  `json:"revision"`
}

type Invariant struct {
    ID           string          `json:"id"`
    Kind         string          `json:"kind"` // file_hash/text_range/ast_node/dom_metric/api_schema/test
    Target       SemanticTarget  `json:"target"`
    Expected     json.RawMessage `json:"expected"`
    BaselineRef  string          `json:"baseline_ref,omitempty"`
    SourceEvent  int64           `json:"source_event_id,omitempty"`
    SourceTaskID string          `json:"source_task_id,omitempty"`
    Status       string          `json:"status"` // active/superseded/revoked
    Verification string          `json:"verification"`
}

type AcceptanceRule struct {
    ID         string   `json:"id"`
    Kind       string   `json:"kind"` // static/unit/integration/dom/visual/api/manual
    Required   bool     `json:"required"`
    TargetIDs  []string `json:"target_ids,omitempty"`
    InvariantIDs []string `json:"invariant_ids,omitempty"`
    Spec       json.RawMessage `json:"spec"`
}
```

要求：

- `ContentHash` 对规范化后的权威字段计算，不包含 CreatedAt 等展示字段；
- 目标匹配规则 deny 优先，任何未知资源默认 ask/enforce 下拒绝；
- 空 WriteSet 不表示“全部允许”，而表示“尚未解析，不能执行生产写入”；
- Invariant 不可只存自然语言，必须有至少一种机器可验证 Kind；无法验证时任务最终只能 unverified；
- 所有 ID、Revision、ContentHash 进入 ActionContext 和验收证据。

### 3.3 逻辑任务划分：采用保守策略，不增加每轮意图分类调用

第一版不得为了判断“新任务还是纠正”额外调用一次 LLM。采用确定性、保守规则：

1. 新会话首条用户输入创建根 LogicalTask。
2. 同一次运行中的用户纠正/邮箱注入，对当前未终态任务产生 `refine` 和新 Revision；旧 Revision 保留。
3. 已完成交付后的新用户输入默认创建新的 LogicalTask，并记录 `RelatedTaskIDs`。
4. 新任务只继承以下不变量：
   - 来源任务与当前任务具有相同工作区和已解析的语义目标重叠；
   - 来源不变量状态为 active；
   - 新用户输入没有显式撤销它；
   - 继承时保留 SourceTaskID、SourceEvent 和 BaselineRef。
5. 无法判断是否仍相关时，**保留约束并标记需确认，不得静默丢弃**；只有明确冲突时才要求用户裁决。
6. 用户说“保持之前距离”“不要动订单号”等时，生成或强化不变量；后续“修省略”不得隐式撤销。
7. 用户明确“撤销/不要之前的方案/重新做”时，产生 `supersede/revoke` 事件，不删除旧合同。

这意味着一个 LogicalTask 可以只对应一次用户提交，也可以因运行中纠正包含多个 Revision。不要在第一版实现复杂的主题猜测器。

### 3.4 状态机

```text
draft
  → active
  → blocked / awaiting_clarification / awaiting_verification
  → verified | delivered-unverified | failed | cancelled
  → accepted（仅显式用户接受）
  → superseded（被新任务或新 Revision 替代，历史仍保留）
```

约束：

- `verified` 是机器证据状态，`accepted` 是用户决定，二者不能混用；
- 没有显式接受入口时，旧任务可停留在 verified，Evolver 不得自动把它当 accepted；
- 首轮需要在会话 API/UI 增加轻量“接受结果”动作，或提供等价显式控制事件；未接线前保持学习关闭；
- 运行中用户纠正立即使旧 Revision 对后续 Action 失效，正在执行的 Action 通过 fencing 决定完成或回滚，不能继续按旧权限写入。

### 3.5 存储与事件

建议三张新表：

1. `logical_tasks`：当前投影，含 owner、session_id、logical_task_id、revision、status、contract JSONB、base_checkpoint、created_at、updated_at；
2. `logical_task_events`：append-only，含 owner、event_id、session_id、logical_task_id、revision、kind、payload、origin_event_id、idempotency_key、created_at；
3. `action_events`：append-only，含 owner、action_id、session_id、logical_task_id、contract_revision、seq、phase、tool、request_ref、before_checkpoint、after_checkpoint、mutation_ref、error_category、payload、created_at。

要求：

- `(session_id, logical_task_id)` 为当前投影键；
- `logical_task_events.idempotency_key` 唯一，保证重试幂等；
- `(action_id, seq)` 唯一，Action 阶段事件只追加；
- 新表全部有 owner 和必要索引；
- 会话显式删除时纳入既有级联/清理清单；
- 旧会话首次迁移时建立 `legacy-root` 或按首条新输入建立任务，不回填伪造的“用户已接受”状态；
- `Goal/Messages/History/session_decisions` 暂时双写保留，作为旧 UI 和回退路径，不做一次性破坏性迁移。

---

## 4. 工作包 WP0：回归基线、测试设施与指标口径

### 4.1 改动范围

建议新增：

- `test/coding/harness_contract_replay_test.go`
- `test/coding/testdata/harness-scope/`（脱敏 Vue/Go fixture）
- `backend/internal/agent/harness_metrics.go` 或现有效率统计的扩展文件
- `doc/eval/harness_contract_baseline_20261118.md`

复用 `test/fixtures` 的 MockLLM 和工作区设施，不另起一套测试服务器。

### 4.2 必做工作

1. 将第 15 轮抽象为合成 fixture：
   - 两个 Vue 页面；
   - 一个“收货人”目标；
   - 多个共享标签规则；
   - 捕获的动作模拟把两个文件的共享列宽统一改掉。
2. 将第 20～22 轮抽象为连续任务：
   - 先建立 30px/60px 两个不变量；
   - 再模拟只修省略却修改三列 flex/min-width；
   - 断言旧实现缺少目标与不变量保护。
3. 固化指标口径：
   - `raw_llm_calls`：原始 LLM 请求；
   - `aggregated_agent_calls`：按 Agent/模型聚合；
   - `accepted_task_input_tokens/output_tokens`：归属 LogicalTask 的所有调用；
   - `rework_tokens`：首次交付后至 accepted 前的新增消耗；
   - `correction_turns`、`scope_violations`、`verification_escapes`。
4. 基线文档记录事故版本、会话 ID、调用口径和输入/输出数据，不记录密钥、完整业务源码或无关隐私。
5. 为后续对照保存机器可读 JSON，字段包含 task_id、模型版本、prompt/context hash、输入输出、时延、验证结果和用户纠正。

### 4.3 验收

```bash
cd test
GOTOOLCHAIN=local go test ./coding -run 'TestHarnessContractReplay|TestHarnessMetrics' -count=1
```

- 旧架构下负面用例必须能够复现，不得写成一开始就被跳过；
- 新功能尚未实现的后续测试可放在对应 WP，不用 false/skip 伪造 WP0 全绿；
- 基线 token 数字与审计 JSON 一致，明确 268 与 288 的不同口径。

---

## 5. 工作包 WP1：LogicalTask、事件投影与顶层会话绑定

### 5.1 改动范围

主要挂点：

- `backend/internal/team/logical_task.go`
- `backend/internal/team/logical_task_store.go`
- `backend/internal/store/logical_task_store.go`
- `backend/internal/agent/session_react.go`
- `backend/internal/agent/service_react.go`
- `backend/internal/agent/decision_ledger.go`
- `backend/internal/bootstrap/bootstrap.go`
- `backend/internal/server/session_http.go`（只在需要接受动作/查询时扩展）
- `migrations/020_logical_tasks.sql`

### 5.2 实现 TaskManager

TaskManager 放在 `internal/team` 的纯业务层或符合依赖方向的新 runtime 组件中，PG/文件系统通过接口注入。职责仅限：

- Create/Refine/Supersede/Revoke；
- ResolveActiveContract；
- InheritInvariants；
- Accept/RejectVerification；
- RebuildProjection(events)；
- LegacyFallback。

不要在 TaskManager 中调用工具、执行命令或直接 import `internal/agent/server`。

### 5.3 绑定首跑、续跑和运行中纠正

1. `runSession`：创建 LogicalTask，将 `task_id + revision` 注入 runCtx。
2. `resumeSession`：按第 3.3 节规则创建新任务或 Revision。
3. `injectUserMessageToRunningSession`/邮箱用户消息：在下一个主循环边界先更新合同，再让模型继续。
4. `acceptGoal` 迁移为 `AcceptanceRequest{TaskID, Revision, ContractHash}`；旧裸 goal 仅作显示和 legacy fallback。
5. `RunWithHistory` 保持兼容，可新增带 ExecutionContext 的入口，避免一次性改坏所有调用方。
6. 当前合同经 provider 注入上下文；决策账本继续保留，但不再作为唯一当前任务事实。
7. 恢复会话时从事件和投影恢复 task/revision；发现合同 hash 不一致时 fail closed，不继续沿用内存旧权限。

### 5.4 配置灰度

在 `backend/internal/config` 增加独立配置，不复用 C-06 开关：

```yaml
agent:
  logical_task_mode: shadow   # off|shadow|enforce
  action_transaction_mode: off # off|shadow|enforce，WP2 使用
  semantic_edit_mode: off      # off|shadow|enforce，WP4 使用
  contract_verification_mode: shadow # off|shadow|enforce，WP5 使用
```

默认只观察，不能直接把所有存量会话切 enforce。非法值回落最保守 shadow/off 并告警，测试默认值和非法值。

### 5.5 必测场景

- 首跑、续跑、运行中纠正均绑定正确 task/revision；
- 第 20 轮不变量在第 21 轮仍 active；
- 新指令明确撤销时只撤销对应约束；
- 无关旧任务不被全部注入；
- 压缩和进程重启后合同逐字/hash 不变；
- 相同 idempotency key 重放不产生重复合同；
- 旧会话无合同能走 legacy fallback，但不伪造 accepted；
- C-06 既有 Claim、InitPlan 和 TaskContract JSON 测试不变通过。

### 5.6 阶段命令

```bash
cd backend
GOTOOLCHAIN=local go test ./internal/team ./internal/store ./internal/agent ./internal/config ./internal/bootstrap -count=1
GOTOOLCHAIN=local go test ./internal/domain/subagent ./internal/domain/memory -count=1
```

PG 真库测试使用隔离测试实例，验证迁移幂等、owner、事件重放和并发 Revision 冲突；不使用开发数据库。

---

## 6. 工作包 WP2：统一 Action 授权、检查点与实际变更审计

### 6.1 执行管线

在 `backend/internal/domain/tool` 新增中立 Action 类型和 pipeline，不新建第二个 Registry：

```text
ActionRequested
→ ResourceResolved
→ Authorized / ApprovalRequired / Rejected
→ CheckpointCreated
→ Executed
→ MutationObserved
→ ScopeVerified
→ Committed | RolledBack | ManualRecoveryRequired
```

建议文件：

- `backend/internal/domain/tool/action.go`
- `backend/internal/domain/tool/action_pipeline.go`
- `backend/internal/domain/tool/action_policy.go`
- `backend/internal/domain/tool/mutation_manifest.go`
- `backend/internal/domain/checkpoint/`（shadow Git 实现，不 import tool，避免循环）
- `backend/internal/store/action_event_store.go`

`Registry.Dispatch` 保留工具查找、角色硬门、危险审批和结果填充，将 `t.Execute(ctx,args)` 包进统一管线。不要把管线只接到 `toolRegistryAdapter`，否则程序化调用和其他 Dispatch 调用方会绕过。

### 6.2 ActionContext

至少携带：

```text
ActionID / SessionID / AgentID / RoleID
LogicalTaskID / ContractRevision / ContractHash
WorkDir / StopContext / TrustMode
RequestedResources / ResolvedResources
BeforeCheckpoint / IdempotencyKey
```

兼容要求：

- 无 LogicalTask 的 legacy 调用仍可运行；
- shadow 下只记录“将会拒绝/回滚”；
- enforce 下无合同或合同不完整时，生产源文件写入默认拒绝或进入明确审批，不得视为全允许；
- 旧角色工具硬门与危险命令审批继续生效，Action 授权不能替代组织级 deny。

### 6.3 资源授权

统一规则：

- 路径 canonicalize 后匹配，处理 Windows 大小写、相对路径、`..`、符号链接与外部目录；
- 多资源任一 deny 即 deny；任一 ask 且未批准则 ask；全部 allow 才执行；
- ExcludedSet 永远优先；
- 验收目录、合同事件、检查点内部文件对普通执行角色 deny；
- 语义目标越界即使路径在 WriteSet 内也算 violation；
- 子 Agent 的权限必须是父任务权限的子集，不能通过自定义角色重新获得全集；
- 插件工具必须声明只读/文件写/网络/数据库/外部副作用能力；未声明能力按 unknown 处理；
- `merge_worktree` 也是 Action，必须校验主任务合同、目标 patch 资源和 base 漂移，不得绕过主工作区保护。

### 6.4 Shadow Git 检查点

采用独立 git-dir：

```text
git --git-dir=<BMA数据目录>/checkpoints/<workspace-hash>.git \
    --work-tree=<用户工作区> ...
```

要求：

- 不修改用户 `.git`、index、branch、stash 和提交历史；
- 基线包含 dirty 和 untracked 文件；
- 默认只排除 BMA 自身检查点内部目录与嵌套 worktree，排除规则必须可审计；
- 对 node_modules、dist 等大型目录先做基准测量；任何性能排除都要显式配置并说明恢复边界，不能悄悄宣称“完整快照”；
- 保留二进制、权限位和行尾；
- 修改前创建 before，成功/失败/超时后都观察并创建 after；
- 恢复使用选择性 patch/三方合并，检测快照后用户或其他 Agent 的新修改；
- 不使用 `git reset --hard`、`git clean -fd` 或直接覆盖整个工作区。

性能策略：

- 保证只读工具不创建检查点；
- 可证明只读的 Shell 走快速路径；
- 其他命令按修改型处理，先 checkpoint 再执行；
- 大仓库记录快照 p50/p95、磁盘增长和文件数；超过配置预算时暂停强制上线，而不是删除安全步骤。

### 6.5 MutationManifest

每次 Action 后生成：

```json
{
  "added": [],
  "modified": [],
  "deleted": [],
  "renamed": [],
  "mode_changed": [],
  "semantic_changes": [],
  "untracked_before": [],
  "external_drift": [],
  "checkpoint": "...",
  "content_hashes": {}
}
```

实际变更以文件系统/shadow Git diff 为准，不采用模型声明。WriteFile/EditFile 现有 `.bma/snapshots` 可暂时保留双写，等新检查点完成恢复等价与崩溃测试后再决定是否去重，不得提前删除旧安全网。

### 6.6 RunCommand 的明确边界

1. 对 Bash/PowerShell 做子命令与 `cd` 目标解析；无法安全解析、动态 eval、语法错误时标记 unknown。
2. enforce 下生产源文件默认要求结构化编辑；RunCommand 写源码需要专门 `source_script_write` 能力或明确审批。
3. 即使命令被允许，仍要比较 before/after，越界文件变更回滚本 Action。
4. 测试/构建允许产生声明过的临时或构建输出，但这些路径也必须进入 MutationManifest。
5. 网络、数据库、进程、部署等副作用不能由文件检查点回滚，必须单独审批或沙箱化；结果事件明确 `external_side_effects_not_reverted`。
6. 字符串扫描不是完整 OS 沙箱，文档和 UI 不得宣称“所有命令均可撤销”。

### 6.7 必测场景

- WriteFile、EditFile、RunCommand 三条路径同一越界规则均生效；
- Node 脚本修改两个文件，只有一个获授权时，实际变化被识别并回滚；
- 命令非零退出/超时后仍生成 MutationManifest；
- 同路径不同 AST 目标的语义越界能被发现（语义适配器未上线时记录 unverified，不虚报通过）；
- dirty、untracked、二进制、CRLF、重命名、删除均正确恢复；
- Action 执行中用户外部编辑不会被回滚覆盖；
- 两个 Action/子 Agent 并发，stale contract/base drift 被拒绝；
- checkpoint 创建失败时 enforce 不写生产文件；
- 验收文件和 `.bma/checkpoints` 被模型修改时拒绝；
- 插件 unknown capability 不被当成只读；
- shadow 只记录，不改变旧行为；enforce 才阻断。

### 6.8 阶段命令

```bash
cd backend
GOTOOLCHAIN=local go test ./internal/domain/tool ./internal/domain/checkpoint ./internal/domain/subagent ./internal/store -count=1
GOTOOLCHAIN=local go test ./internal/agent ./internal/bootstrap -count=1
```

有 C 编译器时对 tool/checkpoint/subagent/team 跑 `-race`；没有则明确记录未运行原因，普通并发测试仍必须执行。

---

## 7. 工作包 WP3：真实页面基线与合同验收先行

> WP3 放在语义编辑前，是为了先让“间距破坏”变成可测事实。不要把所有视觉验证推迟到最后。

### 7.1 AcceptanceRequest 改造

保留旧 `AcceptanceRunner` 适配器，新增合同化请求：

```go
type AcceptanceRequest struct {
    SessionID      string
    LogicalTaskID  string
    Revision       int64
    ContractHash   string
    WorkDir        string
    BaseCheckpoint string
    CandidateCheckpoint string
    FinalText      string
}
```

首跑与续跑统一走该请求，不再分别传 `session.Goal` 或本轮裸 input。

### 7.2 分层验证

```text
L0 deterministic：合同版本、路径/语义范围、实际 diff、不变量
L1 static：格式化检查、lint、typecheck、编译
L2 focused：相关单元/集成/API 测试
L3 real behavior：真实应用、真实 DOM 几何、截图、交互
L4 broad：完整构建和必要回归
```

规则：

- 微任务不默认跑完整模型测试助手；L0/L1 由确定性 runner 完成；
- 只有合同要求或风险升级时才进入更昂贵的 L2～L4；
- Required 证据缺失为 `delivered-unverified`，不是 PASS；
- 验证命令退出码、环境失败、行为失败分开分类；
- 基线和候选使用相同数据、视口和浏览器版本；
- DOM 验证记录具体指标，例如两个元素边界的差值，而不是让模型看截图猜；
- 静态 HTML 夹具只可作为单元测试，不可冒充真实页面验收；
- 测试助手不能编辑合同、守护测试或验收证据。

### 7.3 验收规则保护

建议把项目验收规则放在 `.bma/verification/`，通过 Action policy 对执行角色 deny 写入。这里是 harness 强制逻辑只读，不冒充操作系统 ACL；RunCommand 事后 diff 也必须检查该目录。

### 7.4 必测场景

- 构建成功但 60px 间距改变，验收失败；
- 构建成功但非目标 AST 节点改变，验收失败；
- 真实浏览器不可用时返回 unverified 并说明缺证据；
- 测试助手试图修改守护测试被拒绝；
- 环境 EBUSY 不被误判为代码行为通过或失败；
- 同一任务修正后合同 Revision 改变，旧验收证据不能复用；
- Evolver 在 verified 但未 accepted 时仍不运行。

---

## 8. 工作包 WP4：语义 PatchPlan 与最小源码范围编辑

### 8.1 统一协议

新增 `backend/internal/domain/edit`：

```go
type PatchPlan struct {
    TaskID       string
    Revision     int64
    Operations   []PatchOperation
}

type PatchOperation struct {
    Target            SemanticTarget
    Operation         string
    Value             json.RawMessage
    ExpectedMatches   int
    ExpectedFingerprint string
    Preconditions     []InvariantRef
    PreserveInvariantIDs []string
}

type TextEdit struct {
    Path       string
    StartByte  int64
    EndByte    int64
    NewText    string
    BeforeHash string
    AfterHash  string
}
```

执行步骤：

```text
PatchPlan 校验
→ 语言适配器解析
→ 唯一目标与 fingerprint 校验
→ 影响消费者分析
→ 生成 TextEdit[]
→ Action authorize/checkpoint
→ 按 byte range 应用
→ 重新解析与 diff/Invariant 验证
```

禁止：

- 用 AST 重新打印整个文件；
- `ExpectedMatches > 1` 时自动全部替换；
- 解析失败时静默回退到正则盲写；
- 把两个页面的相同规则自动理解成“用户要求同步”；
- 模型直接提交任意 JavaScript/Python 作为默认生产编辑方式。

### 8.2 语言适配器顺序

第一批：

1. Vue SFC/template：`@vue/compiler-sfc`、必要时 compiler-dom；
2. CSS：PostCSS 规则与声明定位；
3. TS/JS：TypeScript compiler API；
4. Go：`go/parser`、`go/ast`、`go/token`，先做声明/函数/类型级最小编辑；
5. 引用、诊断和跨文件 WorkspaceEdit：逐步接 LSP/Volar/gopls。

Tree-sitter/ast-grep 可作为通用结构搜索与 codemod 补充，但不是首批唯一语义事实源。不要为了统一而抹平各语言编译器已有的准确语义。

### 8.3 Node worker 与桌面自包含

这是必须同时完成的交付项：

- 新增独立 `tools/semantic-edit/` package 和 lockfile；
- 直接声明并固定 `@vue/compiler-sfc`、`postcss`、`typescript` 等版本，不依赖 web 项目的传递依赖；
- worker 通过 stdio JSON-RPC 接收结构化请求，带 request ID、超时、取消和输出上限；
- worker 只返回 TextEdit/诊断，不直接写用户文件；
- 禁止 eval、动态加载用户指定模块和任意命令参数；
- Go 侧懒启动、健康检查、崩溃重启和并发序列化；
- 开发环境显式配置 worker 路径；
- `bma-app.ps1`、安装器和 payload 指纹同时纳入 worker、生产 node_modules 与启动配置；
- 使用桌面自带 Node，不在用户机器运行 npx 下载依赖；
- worker 不可用时 semantic enforce fail closed，legacy 路径只能按明确兼容策略/审批运行，不能静默正则写。

### 8.4 事故案例在新协议下的预期行为

“修改收货人标题”：

- 解析到一个模板节点及其有效样式目标；
- 发现拟修改的是共享标签列宽而非目标标题时，scope resolver 拒绝；
- 如果目标确有多页面消费者且用户未明确多页面，返回多个候选而不是自动同步；
- 最终 diff 只包含获授权节点/声明。

“修省略”：

- PatchPlan 必须声明 PreserveInvariantIDs 为此前的 30px/60px；
- 若修改 flex/min-width 导致这些 DOM 指标改变，L0/L3 拒绝；
- 不允许用“构建通过”覆盖不变量失败。

### 8.5 必测场景

- 同文件两个相似节点，只有目标节点改变；
- 共享 CSS 多消费者，未授权消费者文本逐字不变；
- 目标数 0、2、指纹过期、解析错误均拒绝；
- 未修改区域 byte-for-byte 相同，注释、空白、CRLF 不扩散；
- Go 跨文件引用修改有诊断，未授权文件不进入 WorkspaceEdit；
- worker 超时、崩溃、 malformed response、版本不匹配均安全失败；
- 桌面离线安装包不访问 npm 也能执行 worker 冒烟。

---

## 9. 工作包 WP5：上下文重建、工具按需发现与成本治理

### 9.1 请求上下文的新顺序

修改 `backend/internal/agent/react_agent.go` 与 `backend/internal/domain/memory/pipeline.go` 的装配协作，不删除现有压缩能力：

1. 稳定系统提示和稳定核心工具；
2. 当前 LogicalTask 合同及 active Invariants；
3. 当前代码状态和 BaseCheckpoint；
4. 按任务相关性排序的 repo map；
5. 当前目标源码锚点；
6. 最近未完成动作与短期工作集；
7. 可压缩的旧叙事；
8. 当前时间等尾部动态信息。

实现方式优先采用 provider/接口注入，避免 domain/memory 新增对 agent/server 的反向依赖。现有 `ContextEngine` 的“内容替换、消息条数不变”约束如果成为合同消息增删障碍，应显式升级接口契约并更新所有引擎与 Quarantine 测试，不要绕过检查。

### 9.2 权威内容与可压缩内容分离

- LogicalTask、Invariant、Action 状态、验收规则永不摘要；
- 旧聊天、重复探查、命令原始输出可摘要或外置；
- 摘要带 source event range 和 content hash，能回查原事件；
- 压缩后重新注入的是当前合同，不是上一次压缩时的旧合同；
- 文件小地图升级为按依赖/引用相关性的 repo map，已有 mtime 缓存继续复用；
- 缓存稳定性按段统计，避免每轮重建相同工具 schema 和合同前缀。

### 9.3 ToolCatalog / SearchTools

在现有 Registry 上增加本地目录，不绑定特定模型厂商：

- 核心工具保持稳定 schema；
- 低频/插件工具延迟加载；
- SearchTools 支持名称、描述、能力标签的 BM25/简单索引即可，第一阶段不强制向量模型；
- 工具发现后只在当前工作集加载所需 schema；
- Dispatch 权限不依赖“是否已发现”，未发现工具仍必须过同一授权；
- 工具定义少且全部高频时不启用搜索，避免多一步延迟。

### 9.4 Observation externalization 与程序化编排

- 大文件、长日志、测试结果写入 `.bma/artifacts/observations/`；
- 模型接收摘要、大小、content hash、引用句柄和必要片段；
- 引用内容访问遵循原资源权限和敏感信息策略；
- 可确定的批量读取、统计、过滤、测试结果汇总由后端 runner 完成，不把中间结果逐次塞回主模型；
- 不允许把程序化编排变成新的任意 Shell 写入旁路，内部调用仍进入 Action pipeline。

### 9.5 成本与循环预算

- 顶层也必须有 LogicalTask/turn 的硬预算，不能继续只靠 API 报错；
- 预算暂停产生可恢复事件，不伪造成用户消息；
- 预算包括主 Agent、子 Agent、测试助手、摘要和 Evolver；
- 记录 cache hit/miss，但成本评价不能只看缓存后账单，要同时看总输入和重复工作；
- 简单微任务默认“一次主模型 + 确定性编辑/验证”，复杂任务才 Plan 或调用额外模型；
- Evolver 只在 accepted 后运行，禁止每个小轮次固定进化；
- 无进展检测基于重复动作、相同 diff、相同错误和预算，不误杀正常的“修改→测试→修复”。

### 9.6 必测场景

- 压缩、恢复、重启后合同和不变量无损失；
- 工具未加载 schema 时直接幻觉调用仍被权限层处理；
- 大日志只把摘要送入模型，全文可按权限回读；
- 核心前缀在多轮保持稳定，合同更新只使必要段失效；
- 简单修改不启动规划模型、测试模型和 Evolver 的固定三连；
- 达到任务预算暂停后可恢复，旧 Action 不重复执行；
- 同一 LogicalTask 的所有成本能完整聚合。

---

## 10. 工作包 WP6：受控学习、API/UI 与灰度发布

### 10.1 Evolver 与长期记忆

修改 `service_react.go` 的调用时机：

```text
verified + 用户显式 accepted
→ 生成候选
→ 去重/冲突/撤回检查
→ shadow 记录
→ 按现有人工或评测策略晋升
```

- 用户纠正中的任务、failed、unverified、cancelled 不产生正面技能；
- 可以将失败原因进入独立负面经验候选，但不得自动注入为“推荐做法”；
- 保持 memory.write_mode 默认 shadow；
- 旧会话迁移不自动生成 accepted，也不补写历史技能。

### 10.2 最小 UI/API

只增加执行和审计需要的功能：

- 当前 LogicalTask、Revision、状态；
- 本任务允许范围与活跃不变量；
- Action 实际 diff、越界原因、检查点；
- Restore Files / Restore Task / Restore Both；
- 显式接受结果；
- verified 与 accepted 分开展示；
- unverified 的证据缺口。

不得在第一版做复杂项目管理后台。前端改动后运行 `cd web && npm run build`，并做必要浏览器冒烟。

### 10.3 灰度顺序

1. `logical_task_mode=shadow`：只观察任务划分与继承；
2. `action_transaction_mode=shadow`：只记录越界和将回滚内容；
3. 打开检查点，先验证恢复与性能；
4. 对完整新合同任务启用 Action enforce，legacy 保持兼容/审批；
5. 启用合同验收 enforce；
6. 语义编辑先 Vue/CSS 小范围 enforce，再 TS/Go；
7. 最后调整 Evolver 和长期记忆晋升。

每个阶段都保留独立回退开关。关闭新功能不得删除事件、检查点或旧会话数据。

### 10.4 桌面发布前置条件

- 后端全量测试与集成测试达到本计划门槛；
- 离线 worker 冒烟通过；
- 检查点磁盘位置不会进入安装包；
- `.env`、models、用户配置、workspace、logs 不被升级覆盖；
- 安装包排除审计材料和用户业务源码；
- 健康检查确认新版本进程，而不是旧进程占用端口；
- 已验证上一版回滚；
- 部署动作另行向 luokun 报告，不由执行 Agent 自行更新正在使用的 BMA。

---

## 11. 通用回归矩阵

| 编号 | 场景 | 必须断言 |
|---|---|---|
| R-01 | 单页面单目标样式 | 只有目标声明变化 |
| R-02 | 同文件多个相似标题 | expected_matches=1，其余逐字不变 |
| R-03 | 两页面共享规则 | 未明确多目标时不得自动同步 |
| R-04 | 已确认间距后修省略 | 目标效果通过且间距不变量通过 |
| R-05 | 用户撤销旧方案 | 旧约束 superseded，历史仍可查 |
| R-06 | 长会话多次压缩 | 当前合同/hash/Invariant 不丢失 |
| R-07 | 进程重启恢复 | task/revision/action 水位一致，不重复写入 |
| R-08 | Go API 跨文件修改 | 引用诊断与授权范围同时满足 |
| R-09 | 重命名符号 | 未授权引用文件不变 |
| R-10 | dirty worktree | 用户已有修改完整保留 |
| R-11 | untracked 文件 | 快照与恢复不丢失 |
| R-12 | 用户执行期间外部编辑 | base drift 被检测，不覆盖新内容 |
| R-13 | 并行子 Agent 写同一资源 | 序列化、拒绝或三方合并，不能静默覆盖 |
| R-14 | worktree HEAD 漂移 | 既有合并门语义不回退 |
| R-15 | Node/Python Shell 写源码 | 与实际 WriteFile 同规则，越界可回滚 |
| R-16 | 动态 eval/错误 Shell 语法 | unknown，不当安全只读 |
| R-17 | 命令超时/非零退出 | 仍观察变更，不错误 commit |
| R-18 | checkpoint 失败 | enforce 下源文件零写入 |
| R-19 | 二进制/CRLF/权限位 | 内容与元数据按支持矩阵恢复 |
| R-20 | 大仓库 | 快照 p50/p95 与磁盘增长有实测，不假设零成本 |
| R-21 | 插件未知能力 | 不当只读，不承诺可回滚外部副作用 |
| R-22 | 验收规则被 Agent 修改 | 拒绝并保留证据 |
| R-23 | 构建通过但非目标变化 | 验收失败 |
| R-24 | 浏览器缺失 | unverified，不伪造视觉 PASS |
| R-25 | 环境构建失败 | 与行为回归分类不同 |
| R-26 | 工具 schema 未加载 | 执行权限仍完整生效 |
| R-27 | 任务预算耗尽 | 可恢复，不重复 Action，不烧空循环 |
| R-28 | 旧会话无 LogicalTask | 兼容 fallback，不伪造 accepted |
| R-29 | PG 事件重放 | 投影一致、幂等、owner 隔离 |
| R-30 | 用户显式删除会话 | 新事件/投影按既有删除约定清理 |

---

## 12. 测试分层与命令

### 12.1 每次工作包

```bash
cd backend
GOTOOLCHAIN=local go test ./internal/team ./internal/domain/tool ./internal/domain/memory ./internal/domain/subagent ./internal/agent ./internal/store ./internal/config -count=1
```

按实际改动增减包；不得只跑新测试而忽略同包既有测试。

### 12.2 每个 Phase 结束

```bash
cd backend
GOTOOLCHAIN=local go build ./...
GOTOOLCHAIN=local go vet ./...
GOTOOLCHAIN=local go test ./... -count=1

cd ../test
GOTOOLCHAIN=local go test ./... -count=1
GOTOOLCHAIN=local go test -tags=integration ./... -run='^$' -count=1

cd ../web
npm run build
```

真实 PG/Redis 集成：

```bash
cd test
GOTOOLCHAIN=local go test -tags=integration ./... -count=1
```

只能使用隔离测试实例。现有约定是测试 PG 55432/Redis 56380，不使用开发实例 5432/6380；桌面自带实例也可能使用 55432/56380，执行前必须核实并隔离，不得默认连接正在使用的桌面数据。

### 12.3 真实模型评测

- 仅在 deterministic 全绿后运行；
- 固定模型、温度、任务集、预算和重复次数；
- 报告模型版本、时间和实际花费；
- 不用单次成功证明稳定性；
- 新旧版本使用相同任务和预算；
- 真实模型评测失败不能通过删除场景或放宽 oracle 解决。

---

## 13. 量化指标与发布门槛

### 13.1 正确性硬门

| 指标 | 口径 | 门槛 |
|---|---|---|
| 未授权路径修改率 | 未授权路径变更任务数/修改任务数 | deterministic 为 0 |
| 未授权语义修改率 | 未授权 AST/DOM/API 目标变更数/修改任务数 | deterministic 为 0 |
| 活跃合同保持率 | 压缩/重启后正确合同项/应保留项 | 100% |
| 用户原修改保留率 | 回滚后 dirty/untracked/外部新修改保留项 | 100% |
| 必需证据完整率 | PASS 中 Required 证据完整数/PASS 数 | 100% |
| 守护测试篡改 | 执行 Agent 对验收规则的实际写入 | 0 |
| 严重验证逃逸 | PASS 后发现非目标回归 | 发布前已知案例为 0 |

### 13.2 效率对照指标

```text
cost_per_accepted_task
= 主 Agent + 子 Agent + 摘要 + 测试助手 + Evolver 的全部 tokens / accepted tasks

rework_ratio
= 首次交付后至 accepted 的 tokens / 该任务总 tokens

correction_rate
= 纠正型用户轮次 / LogicalTask 数

first_pass_acceptance
= 无纠正即 accepted 的任务数 / accepted tasks

latency_p50/p95
= 用户提交至 accepted 或明确 unverified 的时间
```

发布比较原则：

- 正确性硬门先满足，再比较 token 和时延；
- 成功率、首次接受率、非目标回归不得比基线差；
- 同时报告总输入、缓存后费用、模型调用数、p50/p95，不用单一“账单下降”掩盖质量下降；
- 不预先承诺 Anthropic 或其他项目的固定降幅；
- 若 token 降低但 correction_rate 或 verification_escape 上升，视为失败；
- 若快照/验证使时延显著增加，先优化增量和分层调度，不删除安全步骤。

---

## 14. 推荐执行顺序与依赖

```text
WP0 回归基线与指标
  ↓
WP1 LogicalTask / Store / 顶层绑定
  ↓
WP2 Action 授权 + Shadow Git + MutationManifest
  ↓
WP3 合同验收与真实 DOM 基线
  ↓
WP4 语义 PatchPlan + 桌面 worker
  ↓
WP5 上下文重建 + ToolCatalog + 成本治理
  ↓
WP6 accepted 后学习 + UI/API + 灰度发布
```

说明：

- WP1 与 WP2 的部分接口可并行设计，但 WP2 enforce 必须等待 WP1 能稳定提供合同；
- WP3 的基本 L0/L1 可与 WP2 同步，真实 DOM 不变量必须在 WP4 大规模语义编辑之前可用；
- WP5 可以提前做 repo map 的只读原型，但不得通过提前裁掉历史来“验证”新合同；
- Evolver 的最后接线不能提前，否则系统会把尚未纠正完成的错误方案学习为经验。

---

## 15. 风险与明确限制

1. **意图划分不能百分之百自动推断。** 第一版采用保守继承与澄清，不用额外 LLM 猜测替代用户事实。
2. **文件事务不等于全系统事务。** 网络、数据库、部署和外部进程副作用需要沙箱/审批，不能承诺统一撤销。
3. **Shell 分析不是完整安全边界。** 必须有事后文件观察和可选 OS 沙箱；unknown 不得视为安全。
4. **大仓库 checkpoint 有成本。** 必须实测并公开排除项，不能为性能静默漏掉用户文件。
5. **AST 不天然保证最小编辑。** 必须只应用 source-range TextEdit，并断言未修改区域逐字不变。
6. **LSP/编译器也不能理解所有产品意图。** 用户不变量和真实行为验收仍是独立层。
7. **独立测试助手不是天然独立事实。** 只有不可编辑规则、真实 diff、真实运行证据和基线对照才能降低自述失真。
8. **结构化合同会增加少量前置处理。** 微任务通过确定性短路径抵消，不能再为每个小修改增加多层模型审批。
9. **桌面 worker 是发布物的一部分。** 只改 Go 源码不改打包会导致开发环境成功、安装版失败。
10. **旧会话不能伪造新事实。** legacy fallback 必须明确，历史任务不能自动标记 accepted/verified。

---

## 16. 执行 Agent 每阶段交付模板

```markdown
## 工作包
WPx：<名称>

## 基线
- 分支与 commit：
- 修改前相关测试：
- 预存失败/环境限制：

## 实现
- 修改文件：
- 数据/接口变化：
- 默认配置：
- 兼容性：
- 未采用方案及原因：

## 负面场景
- 越界写入：
- 并发/外部修改：
- 重启/恢复：
- 外部副作用边界：

## 实际验证
- 命令：
- 结果：
- 真实 PG/Redis：
- 真实模型：
- 前端/浏览器：
- race：

## 指标
- 新增 tokens/调用：
- checkpoint p50/p95：
- 磁盘增长：
- 与基线差异：

## 未完成与风险
- 未运行：
- 已知限制：
- 下一工作包阻塞项：

## 回退
- 配置回退：
- 数据兼容：
- 文件恢复：
```

---

## 17. 最终验收清单

全部工作包完成后，只有以下项目全部满足才可宣称“根治架构已落地”：

- [ ] 第 15 轮合成回放不会改共享标签体系；
- [ ] 第 20～22 轮回放不会破坏 30px/60px 和订单号位置；
- [ ] 通用 R-01～R-30 矩阵有实现、结果和未验证项说明；
- [ ] 顶层、子 Agent、插件、RunCommand 均进入同一 Action 授权与变更观察；
- [ ] 路径相同但语义目标不同的越界也能被检测；
- [ ] 检查点保留 dirty/untracked，回滚不覆盖外部新修改；
- [ ] 当前合同在压缩、重启和恢复后保持完整；
- [ ] Vue/CSS/TS/Go 的首批语义编辑均有唯一目标和最小编辑测试；
- [ ] 桌面离线包包含可运行的语义 worker；
- [ ] 构建成功但非目标变化时验收失败；
- [ ] 必需真实视觉证据缺失时只返回 unverified；
- [ ] Evolver 不会在未 accepted 或被纠正的任务上生成正面技能；
- [ ] 成本能按 accepted LogicalTask 聚合，包含所有辅助模型；
- [ ] 后端全量、集成编译、按需真实集成和前端构建有实际结果；
- [ ] 灰度与回退开关均验证，旧会话可继续运行；
- [ ] 未自行部署或覆盖用户正在使用的桌面实例。

---

## 18. 参考资料（设计依据，不要求执行 Agent 照搬实现）

- OpenHands Context Condenser：https://docs.openhands.dev/sdk/guides/context-condenser
- OpenHands Security：https://docs.openhands.dev/sdk/guides/security
- Cline Checkpoints：https://docs.cline.bot/core-workflows/checkpoints
- Cline Plan & Act：https://docs.cline.bot/core-workflows/plan-and-act
- OpenCode Permissions：https://opencode.ai/docs/permissions/
- Aider Repo Map：https://aider.chat/docs/repomap.html
- Aider Lint/Test：https://aider.chat/docs/usage/lint-test.html
- Anthropic Advanced Tool Use：https://www.anthropic.com/engineering/advanced-tool-use
- Tree-sitter Advanced Parsing：https://tree-sitter.github.io/tree-sitter/using-parsers/3-advanced-parsing.html
- PostCSS Architecture：https://postcss.org/docs/postcss-architecture
- Vue language-tools：https://github.com/vuejs/language-tools
- LSP 3.17：https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification/

借鉴边界：

- 摘要只处理可压缩叙事，不作为合同事实源；
- shadow Git 用于事务，不自动提交用户 Git；
- 工具权限在执行层生效，不只隐藏 schema；
- repo map 与工具发现用于减少无关上下文，不牺牲当前合同；
- 小任务直接 Act，复杂任务才承担规划和多模型成本；
- AST/编译器只负责语义定位与最小编辑，不能替代真实行为验收。
