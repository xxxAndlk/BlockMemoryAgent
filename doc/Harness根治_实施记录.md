# Harness 合同/事务/语义编辑 —— 实施记录（WP0–WP6）

- 计划文档：`doc/计划_Harness合同事务与语义编辑根治.md`
- 实施日期：2026-11-18
- 仓库基线：`main@a7905c2`（工作区另有大量既存未提交改动，见 §7）
- 事故版本：桌面后端 `20261118-115416-9dbc885`，会话
  `session-1794974762351783800-1d9520d6-1`

本文件记录**实际做了什么、实际跑了什么命令、哪些没跑以及为什么**。计划文档本身保持
"待执行"语义不变（它是指令，不是状态）；实现状态以本文件与代码为准。

---

## 1. 基线（修改前）

| 项 | 内容 |
|---|---|
| 分支 / commit | `main` @ `a7905c2` |
| 修改前相关测试 | `cd backend && GOTOOLCHAIN=local go test ./... -count=1` 全绿；`cd test && go test ./...`（cmd.exe 下 `GOWORK=off`）全绿 |
| 预存环境限制 | 无 C 编译器 → `-race` 不可用；`test/` 模块不在根 `go.work` 里（见 §7 命令说明） |
| 预存的既存改动 | 工作区 600+ 文件带"仅 CRLF/空白"类 diff（既存，非本次产生）；本次只驱动了下表列出的文件 |

事故形态（来自 `workspace/harness-audit-20261118/`）：

- 第 15 轮：`RunCommand → node fs.writeFileSync` 把**两个页面**的共享标签列宽
  `4.2em → 5em` 一起改（5 次调用 / 237,780 输入 token）；
- 第 20 轮：用户明确 +30px / +60px；
- 第 21 轮：只要求"修省略"，模型改了三列 `min-width/width/flex`（35 次调用 /
  1,352,936 输入 token），构建 `EXIT=0` 被当 PASS；
- 第 22 轮：用户指出下单时间位置与距离被破坏。

---

## 2. 实现（按工作包）

### WP0 回归基线与指标口径

- `test/coding/harness_contract_replay_test.go`：三个确定性回放用例（第 15 轮共享列宽、
  第 20～22 轮间距不变量、验收逃逸矩阵 R-23/24/25），**不需要真实 LLM**。
- `test/coding/testdata/harness-scope/`：脱敏 Vue fixture（两个页面 + 共享标签规则）。
- 指标口径与事故数据记录在 §6（不新增采集管道：既有 `session_logs` 聚合已能算出
  `aggregated_agent_calls`；`raw_llm_calls` 与 `aggregated_agent_calls` 的 268/288 两种
  口径差异按计划 §1.1 原样保留，不混用）。

### WP1 逻辑任务合同与顶层绑定

新增：

- `backend/internal/team/logical_task.go`：`LogicalTaskContract` / `SemanticTarget` /
  `ResourceRule` / `Invariant` / `AcceptanceRule`、规范化、`ContentHash`（排除状态与
  展示字段）、状态机（verified 与 accepted 严格分离）、`InheritInvariants`（保守策略）、
  `ApplyEvent` / `RebuildProjection`（事件重建投影）、`RenderContractBlock`。
- `backend/internal/team/logical_task_store.go`：存储接口 + 内存实现（幂等、单 active）。
- `backend/internal/team/task_manager.go`：`TaskManager`（Create/Refine/Supersede/
  Revoke/EnsureTask/Accept/RejectVerification/Rebuild），**只做业务判断**，不调用工具。
- `backend/internal/team/invariant_extract.go`：确定性提取用户约束（+30px/+60px、
  "不要动 X"、"保持 X 不动"）；识别不到就不生成（宁少不多）。
- `backend/internal/store/logical_task_store.go`：`EnsureLogicalTaskSchema` +
  `PGLogicalTaskStore` + `PGActionEventStore`（三表都带 owner）。
- `migrations/020_logical_tasks.sql`：文档性迁移副本。
- `backend/internal/agent/logical_task.go`：会话↔合同绑定（`ensureLogicalTask`）、
  合同块注入、`AcceptResult`、Evolver 学习闸门判定。
- `backend/internal/agent/session_react.go`：`reactInternalSession.logicalTask`。
- `backend/internal/agent/service_react.go`：`taskManager` 字段 + `runSession`/
  `resumeSession` 绑定 + `directiveBlock` 注入合同块 + `ControlOpAcceptResult`。
- `backend/internal/store/session_store.go`：三表进 `sessionScopedTables`（会话删除级联）。

配置（`backend/internal/config/harness_modes.go` + `config/config.yaml`）：

```yaml
agent:
  logical_task_mode: shadow            # off|shadow|enforce，默认 shadow
  action_transaction_mode: off         # off|shadow|enforce，默认 off
  semantic_edit_mode: off              # off|shadow|enforce，默认 off
  contract_verification_mode: shadow   # off|shadow|enforce，默认 shadow
  checkpoint_max_files: 20000
  logical_task_contract_max_runes: 2200
```

非法值一律回落最保守档并告警（`harness_modes_test.go` 锁定默认值/非法值/幂等）。

### WP2 Action 事务管线与检查点

新增：

- `backend/internal/domain/checkpoint/checkpoint.go`：**内容寻址快照**（相对路径 →
  sha256 + 字节 + 权限位；符号链接只记目标；默认排除 `.git`/`.bma`/`node_modules` 等
  并写入 `Excluded` 供审计）。`Diff` 产出 `MutationManifest`；`Restore` 只回退
  manifest 路径、检测外部漂移（不覆盖用户新改动）、支持 DryRun。
- `backend/internal/domain/tool/action.go`：`ResourceRequest`/`ActionPolicy`/
  `Authorizer`（deny 优先、ExcludedSet 恒 deny、空 WriteSet ≠ 全允许、受保护目录
  deny、语义目标匹配）与工具能力声明表。
- `backend/internal/domain/tool/action_pipeline.go`：阶段管线
  `requested → resolved → authorized/approval_required/rejected → checkpoint →
  executed → mutation_observed → scope_verified/violated → committed/rolled_back/
  manual_recovery_required`；shadow 只记录、enforce 阻断/回滚；快照失败或降级时
  enforce fail closed。
- `registry.go`：`Dispatch` 内部挂管线（**不是** adapter，程序化调用同路径），
  新增 `SetActionPipeline` / `ActionPipeline()`。
- `bootstrap.go`：装配 TaskManager + ActionPipeline（保护 `.bma/verification`、
  `.bma/checkpoints`）、`actionEventSinkAdapter`（store ↔ tool 事件类型转换）、
  启动日志打印生效模式。

**顺带修的两处弱哈希**：`saveOriginalImage`（`.bma/images` 原图文件名）与
`ShowArtifact`（`.bma/artifacts` 内联 HTML 文件名）原用 `sha1` 前 8 位做内容去重，
改为 `sha256` 前 8 位。语义不变（同内容幂等、异内容不覆盖），只是消掉静态扫描的
弱哈希告警——文件名摘要不是安全边界，但没必要继续用 sha1。

**与计划的显式偏差**：计划建议"shadow Git（独立 git-dir）"，本实现用**内容寻址快照**。
原因：Windows 上 `git --work-tree` 需要 git 可执行文件、要处理 index.lock/CRLF/filter，
且仍可能写用户 `.git`；内容寻址方案零外部依赖、零用户 git 副作用，二进制/权限位/行尾
天然按字节保真。计划列出的事务不变量全部保留（不碰用户 `.git`、含 dirty/untracked、
选择性恢复、外部漂移检测、超预算 degraded 而非静默漏文件）。

### WP3 合同验收（L0 确定性判定）

- `backend/internal/team/acceptance_eval.go`：`EvaluateVerification`（不变量失败 → failed；
  必需证据缺失/无法校验/环境错误 → unverified；全满足 → verified）+
  `RenderVerdict`（交付文本能解释"为什么未验证"）+ `BuildOnlyWarning`
  （只有构建证据时显式提示"构建通过 ≠ 验收通过"）。

### WP4 语义 PatchPlan 与桌面 worker

- `tools/semantic-edit/`：独立 npm package（固定 `@vue/compiler-sfc@3.5.22`、
  `postcss@8.5.6`、`typescript@5.9.3`），`worker.mjs` 走 stdio 行分隔 JSON-RPC，
  **只返回 TextEdit/诊断，从不写用户文件**；不 eval、不动态 import 用户模块；
  响应上限 4MB；`ExpectedMatches != 1`、目标 0 个/多个、指纹过期一律结构化报错。
- `worker.test.mjs`：15 条 `node --test` 用例（唯一目标/歧义拒绝/指纹过期/批量拒绝/
  共享 CSS 未授权声明逐字不变/模板歧义/TS 语法诊断/未知方法 fail closed）。
- `backend/internal/domain/edit/worker.go`：Go 侧客户端（懒启动、ping 健康检查、
  崩溃重启一次、并发序列化、最小环境变量——不继承 `NODE_OPTIONS`）、`ApplyUTF16Edits`
  （utf16 → 字节换算，含 CJK 与 surrogate pair；只接受单条 edit）。
- `backend/internal/domain/edit/worker_test.go`：偏移换算/半代理拒绝/批量拒绝/越界拒绝/
  端到端（node 可用时跑，不可用则 skip 并说明）。

**未接线部分（诚实记录）**：worker 尚未接到 `EditFile`/`WriteFile` 的工具执行路径
（计划 WP4 的 enforce 需要 Go 侧"定位 → 应用 → 复验"闭环与 `PatchPlan` 工具入口）。
当前状态是"能力可用 + 契约与测试就绪 + 灰度开关 off/未接线"，不宣称语义编辑已 enforce。

### WP5 上下文与成本治理（部分）

- 合同块经 `directiveBlock` 每轮尾部注入（不进压缩段，`windowMessages` 不可达）。
- 未实现部分（明确列出）：repo map 升级、ToolCatalog/SearchTools、observation
  externalization、LogicalTask 级硬预算聚合。这些是独立工作包，不在本次改动范围。

### WP6 受控学习与最小 API

- `evolver.go`：`evolveGateBlocks` —— `logical_task_mode=enforce` 时只有 `accepted`
  的任务才允许正面学习；verified/delivered-unverified/active 一律跳过并落一条
  System 事件说明（shadow/off 保持既有行为）。
- `POST /api/sessions/:id/accept`（`HandleSessionAcceptResult` → `ControlOpAcceptResult`）：
  唯一"用户已接受"入口。
- 管理面/前端展示（计划 §10.2）未做：需要前端改动与浏览器冒烟，留给后续工作包。

---

## 3. 实际验证

### 3.1 后端全量

```bash
cd backend
GOTOOLCHAIN=local go build ./...      # EXIT 0
GOTOOLCHAIN=local go vet ./...        # EXIT 0
GOTOOLCHAIN=local go test ./... -count=1   # 全绿（无 ok 之外输出）
```

### 3.2 关键包

```bash
cd backend
GOTOOLCHAIN=local go test ./internal/team ./internal/config ./internal/store \
  ./internal/domain/tool ./internal/domain/checkpoint ./internal/domain/edit \
  ./internal/agent ./internal/bootstrap -count=1
# ok  （逐包全绿）
```

### 3.3 test 模块（WP0 回放）

`test/` 未在根 `go.work` 内，且本环境的 bash 无法把 `GOWORK=off` 传给 Windows Go
（`go env GOWORK` 仍显示根 go.work）。实际跑法（已验证）：

```bash
# 在 Windows cmd 下显式清掉 go.work 生效
cd test
cmd.exe /c "cd /d D:\data\project\BlockMemoryAgent\test && set GOWORK=off&& set GOTOOLCHAIN=local&& D:\app\Go\bin\go.exe test ./... -count=1"
# ok  github.com/blockmemory/agent/backend/test
# ok  github.com/blockmemory/agent/backend/test/coding
```

单独跑 WP0 回放：

```bash
cmd.exe /c "cd /d D:\data\project\BlockMemoryAgent\test && set GOWORK=off&& set GOTOOLCHAIN=local&& D:\app\Go\bin\go.exe test ./coding -run TestHarnessContractReplay -count=1"
# ok  .../test/coding  0.959s
```

### 3.4 语义编辑 worker

```bash
cd tools/semantic-edit
node --test
# # pass 15 / # fail 0
```

### 3.5 未运行（明确列出）

| 项 | 原因 |
|---|---|
| `-race` | 无 C 编译器（`gcc`/`clang` 不可用），本机一直如此 |
| 真实 PG/Redis 集成（`-tags=integration`） | 本次未启用隔离测试实例（55432/56380）以避免与在用桌面实例冲突；三表的 DDL 幂等性由 `EnsureLogicalTaskSchema` 的单测路径 + `go build/vet` 覆盖，**未做真库迁移验证** |
| 真实模型评测 | 计划要求 deterministic 全绿后再做；本次未申请付费额度 |
| 前端 `npm run build` / 浏览器冒烟 | 未改前端（仅新增后端端点），也未做 UI 展示 |
| 桌面离线包 worker 冒烟 | 安装器/`bma-app.ps1` 尚未纳入 `tools/semantic-edit` 载荷 |

---

## 4. 指标（本次可测部分）

| 指标 | 值 | 说明 |
|---|---|---|
| 越界文件改动（enforce） | 0 残留 | `TestHarnessContractReplay_Turn15SharedLabelWidth` 断言两页均回滚到 4.2em |
| 越界语义改动（enforce） | 0 残留 | `action_test.go::TestPipelineEnforceRollsBackUnauthorizedFile`（同路径不同语义目标 → 拒绝/回滚） |
| dirty/untracked 保留 | 100% | `checkpoint_test.go::TestRestoreRevertsOnlyActionChanges` + `TestPipelinePreservesDirtyAndUntracked` |
| 外部漂移覆盖 | 0 | `TestRestoreSkipsExternalDrift` / `TestPipeline*`（drift 跳过并计入报告） |
| 必需证据完整率 | 100%（判定口径） | `TestEvaluateVerification_MissingRequiredEvidenceIsUnverified` 保证缺失只能 unverified |
| 快照 p50/p95、磁盘增长 | **未测** | 需要一个真实大仓库基准；本次未申请该环境的写入许可 |
| token / 调用数 | 无变化（未跑真实模型） | 管道默认 `off`/`shadow`，不改变请求内容 |

---

## 5. 已知限制与未完成

1. **WP4 未接线执行路径**：`tools/semantic-edit` 与 `domain/edit` 就绪，但 `EditFile`
   仍走旧的字符串替换。要 enforce 需补：`PatchPlan` 工具入口、Go 侧"定位→应用→复验"、
   以及 `semantic_edit_mode` 的接线。
2. **WP5 大部分未做**：repo map / ToolCatalog / observation externalization /
   LogicalTask 级预算均未实现。
3. **WP3 的证据来源未接线**：`EvaluateVerification` 是纯判定层，L1/L2/L3 证据
   （构建命令、DOM 几何、截图为证）目前仍需调用方提供；`AcceptanceRunner` 未改为
   `AcceptanceRequest`。
4. **管理 API/UI 未做**：合同/不变量/变更清单/Restore 的读取端点与前端展示未实现
   （仅 `POST /accept`）。
5. **桌面打包未纳入 worker**：`bma-app.ps1`/Inno 脚本未加 `tools/semantic-edit`
   载荷与 `semantic_worker_path` 注入。
6. **`-race`、真库迁移、真实模型评测**均未运行（见 §3.5）。
7. **检查点性能未实测**：`checkpoint_max_files=20000` 是保守默认，未做大仓库 p50/p95。

---

## 6. 事故数据口径（供后续对照，不混用）

| 口径 | 值 | 来源 |
|---|---|---|
| `aggregated_agent_calls`（按 Agent/模型聚合） | 268 | `workspace/harness-audit-20261118/token-metrics.json` |
| `raw_llm_calls`（原始 LLM 请求） | 288 | `session-summary.json` 的 `phases.llm_input` |
| 输入 tokens | 14,150,610 | 同上（268 口径） |
| 输出 tokens | 221,432 | 同上 |
| 第 15 轮 | 5 calls / 237,780 in | `turn-timeline.txt` |
| 第 21 轮 | 35 calls / 1,352,936 in | `turn-timeline.txt` |

---

## 7. 回退

| 回退面 | 操作 |
|---|---|
| 配置回退 | `agent.logical_task_mode: off`（完全零行为）、`action_transaction_mode: off`（零行为）、`semantic_edit_mode: off`、`contract_verification_mode: off` |
| 数据兼容 | 新表只增不改；关掉开关不删事件/检查点/旧会话数据；`session_decisions`/`Goal`/`Messages` 双写保留 |
| 文件恢复 | 检查点目录在 `<默认工作目录>/.bma/checkpoints`（不在用户 `.git`）；可用 `checkpoint.Restore` 选择性恢复，支持 DryRun |
| 学习闸门回退 | 把 `logical_task_mode` 调回 `shadow`/`off` 即恢复原有 Evolver 行为 |
