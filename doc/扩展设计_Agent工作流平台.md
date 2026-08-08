# BlockMemoryAgent 扩展设计：Agent 工作流平台

> 从"编程助手"扩展为"Agent 工作流平台"，支持编排、测试、研究、团队协作等多场景。核心设计理念不变：上下文恒定、话题隔离、跨会话记忆。
>
> 本文为扩展愿景，未实现；其中架构叙述（Router / Agent Core / SessionBlock 等）基于 v3 历史框架，当前实现以 `doc/项目说明.md` 为准（单一 ReAct 主循环 + `call_sub_agent` 分发，无 Router/状态机）。

---

## 1. 扩展场景总览

BlockMemoryAgent 的核心架构（Router → Agent Core → Memory & Tools）不仅适用于编程场景，可以扩展为支持多种工作流的 Agent 平台：

| 场景 | 核心工作流 | 特有工具 | 记忆重点 |
|------|-----------|----------|---------|
| **编程** | 代码编写、审查、重构、调试 | 文件操作、命令执行、静态分析 | 代码风格、项目约定、历史修改 |
| **编排** | 工作流设计、任务调度、依赖管理 | DAG 定义、定时触发、状态机 | 工作流模板、调度策略、失败模式 |
| **测试** | 测试设计、执行、报告、回归 | 测试框架、覆盖率、性能压测 | 测试用例库、Bug 模式、环境配置 |
| **研究** | 信息搜集、分析、报告生成 | 搜索、爬虫、数据可视化、文献检索 | 研究方向、数据来源、分析结论 |
| **团队协作** | 任务分配、进度跟踪、评审、沟通 | 项目管理、通知、文档协作、会议 | 团队分工、决策记录、项目里程碑 |

**关键洞察**：所有场景的底层问题相同——**上下文膨胀、话题污染、跨会话遗忘**。所以核心架构（记忆外部化、话题隔离、智能路由）可以统一支持，上层只是工作流模板和工具集不同。

---

## 2. 工作流编排（Workflow Orchestration）

### 2.1 设计理念

工作流编排不是简单的 DAG 执行，而是**让 Agent 能够设计、执行、监控、优化工作流**。

**三个层次**：
- **工作流定义**：用 YAML/JSON 定义 DAG（节点、依赖、触发条件）
- **工作流执行**：Agent 按 DAG 调度执行，监控状态，处理失败
- **工作流优化**：根据历史执行数据，Agent 自动优化工作流（并行化、重试策略、资源分配）

### 2.2 工作流定义格式

```yaml
workflow_id: deploy_pipeline
name: 部署流水线
description: 从代码提交到生产部署的完整流程

triggers:
  - type: webhook
    event: git.push
    branch: main
  - type: cron
    schedule: "0 2 * * *"  # 每天凌晨 2 点

nodes:
  - id: build
    name: 构建镜像
    type: task
    agent: code_assistant
    action: |
      运行 docker build，生成镜像并推送到 registry
    inputs:
      - git.commit_sha
    outputs:
      - image.tag
    timeout: 10m
    retry: 2

  - id: test
    name: 运行测试
    type: task
    agent: test_assistant
    action: |
      运行单元测试、集成测试、覆盖率检查
    inputs:
      - build.image.tag
    outputs:
      - test.report
    depends_on: [build]
    timeout: 30m

  - id: security_scan
    name: 安全扫描
    type: task
    agent: security_assistant
    action: |
      运行 Trivy/Snyk 扫描镜像漏洞
    inputs:
      - build.image.tag
    outputs:
      - scan.report
    depends_on: [build]
    timeout: 15m

  - id: deploy_staging
    name: 部署到 staging
    type: task
    agent: devops_assistant
    action: |
      部署到 staging 环境，运行冒烟测试
    inputs:
      - build.image.tag
      - test.report
    outputs:
      - deploy.url
    depends_on: [test, security_scan]
    timeout: 10m

  - id: approval_gate
    name: 人工审批
    type: approval
    approvers: ["team-lead", "tech-lead"]
    timeout: 24h
    depends_on: [deploy_staging]

  - id: deploy_prod
    name: 部署到生产
    type: task
    agent: devops_assistant
    action: |
      滚动部署到生产环境，监控指标
    inputs:
      - build.image.tag
    outputs:
      - deploy.status
    depends_on: [approval_gate]
    timeout: 20m

  - id: notify
    name: 通知团队
    type: notification
    channels: [slack, email]
    message: |
      部署完成：{{ deploy_prod.status }}
    depends_on: [deploy_prod]

on_failure:
  - id: rollback
    name: 自动回滚
    action: |
      如果 deploy_prod 失败，自动回滚到上一个版本

on_success:
  - id: update_docs
    name: 更新文档
    action: |
      更新部署日志和变更记录
```

### 2.3 工作流引擎

工作流引擎的核心职责：

```go
type WorkflowEngine struct {
    store       WorkflowStore     // 工作流定义持久化
    scheduler   Scheduler         // 任务调度器
    executor    Executor          // 任务执行器（调用 Agent）
    monitor     Monitor           // 监控和告警
    optimizer   Optimizer         // 基于历史数据优化工作流
}

func (e *WorkflowEngine) Run(ctx context.Context, workflowID string, trigger Trigger) error {
    // 1. 加载工作流定义
    wf := e.store.Get(workflowID)
    
    // 2. 创建执行实例（run）
    run := wf.NewRun(trigger)
    
    // 3. 拓扑排序，确定执行顺序
    order := run.TopologicalSort()
    
    // 4. 按顺序执行，处理并行和依赖
    for _, nodeID := range order {
        node := run.GetNode(nodeID)
        
        // 等待依赖完成
        if err := run.WaitForDependencies(nodeID); err != nil {
            run.MarkFailed(nodeID, err)
            if node.OnFailure != nil {
                run.Execute(node.OnFailure)
            }
            continue
        }
        
        // 执行节点
        result := e.executor.Execute(ctx, node, run.Inputs)
        
        // 处理结果
        if result.Success {
            run.MarkSuccess(nodeID, result.Outputs)
        } else {
            run.MarkFailed(nodeID, result.Error)
            if node.Retry > 0 && run.RetryCount(nodeID) < node.Retry {
                run.ScheduleRetry(nodeID)
            } else if node.OnFailure != nil {
                run.Execute(node.OnFailure)
            }
        }
        
        // 5. 同步写入工作流执行记忆
        e.memory.SaveWorkflowTrace(ctx, run.ID, nodeID, result)
    }
    
    // 6. 工作流完成，生成摘要
    summary := e.optimizer.Summarize(run)
    go e.memory.SaveWorkflowSummary(ctx, run.ID, summary)
    
    return nil
}
```

### 2.4 工作流记忆

工作流执行产生大量数据，需要专门的工作流记忆：

```
Workflow Memory
├── 定义层（Workflow Definition）
│   ├── 工作流模板库（可复用的工作流模板）
│   ├── 节点类型库（预定义的节点类型）
│   └── 触发器配置（webhook、cron、event）
│
├── 执行层（Workflow Execution）
│   ├── 执行轨迹（每次运行的完整记录）
│   ├── 节点输出（每个节点的输出数据）
│   ├── 失败模式（失败原因、重试策略效果）
│   └── 性能指标（执行时间、资源消耗）
│
└── 优化层（Workflow Optimization）
    ├── 优化建议（基于历史数据的改进建议）
    ├── 并行化机会（哪些节点可以并行）
    ├── 资源瓶颈（哪些节点耗时最长）
    └── 失败预测（哪些节点最可能失败）
```

**工作流记忆检索**：
- 当用户说"运行部署流水线"时，检索"上一次部署是什么时候？失败了吗？"
- 当工作流失败时，检索"类似的失败模式之前是怎么解决的？"
- 当设计新工作流时，检索"有没有类似的工作流模板可以复用？"

### 2.5 工作流 TUI 展示

```
┌──────────────────────────────────────────────────────────────┐
│  Workflow: deploy_pipeline  Run #42  ●running  3m 12s         │
├──────────────────────────────────────────────────────────────┤
│                                                             │
│  [✓] build          2m 30s   image: myapp:v1.2.3            │
│  [✓] test           5m 15s   245/245 passed, 98% coverage  │
│  [✓] security_scan  3m 40s   0 critical, 2 low (acceptable) │
│  [✓] deploy_staging 1m 20s   https://staging.myapp.com     │
│  [◐] approval_gate  waiting  approvers: team-lead           │
│  [○] deploy_prod      idle   depends: approval_gate         │
│  [○] notify           idle   depends: deploy_prod           │
│                                                             │
│  🧠 recalled: 上次部署 (#41) 在 security_scan 发现 1 个 critical │
│  漏洞，已修复。                                              │
│                                                             │
├──────────────────────────────────────────────────────────────┤
│  [plan] build → test → security_scan → deploy_staging        │
│  → approval_gate → deploy_prod → notify                     │
│  [5/7] 完成  [1/7] 等待审批  [1/7] 空闲                     │
├──────────────────────────────────────────────────────────────┤
│  > _                                                         │
└──────────────────────────────────────────────────────────────┘
```

---

## 3. 测试自动化（Test Automation）

### 3.1 设计理念

测试不是"写测试用例然后运行"，而是**让 Agent 能够设计测试策略、生成测试用例、执行测试、分析结果、修复问题**的闭环。

**测试 Agent 的职责**：
- 测试设计：根据代码变更、需求文档，设计测试策略（单元/集成/E2E/性能）
- 测试生成：自动生成测试用例（基于代码分析、边界值、历史 Bug 模式）
- 测试执行：运行测试，收集结果（通过率、覆盖率、性能指标）
- 结果分析：分析失败原因（代码变更引入、环境变化、 flaky test）
- 修复建议：针对失败测试，给出修复建议或自动修复

### 3.2 测试记忆

```
Test Memory
├── 测试用例库（Test Case Library）
│   ├── 代码路径 → 测试用例映射（哪些代码变了要跑哪些测试）
│   ├── 测试用例模板（常见的测试模式）
│   └── 边界值库（历史发现的边界条件）
│
├── Bug 模式库（Bug Pattern Library）
│   ├── 常见错误模式（null pointer、race condition 等）
│   ├── 修复策略（每种 Bug 的典型修复方法）
│   └── 回归测试（确保修复不引入新 Bug）
│
├── 测试历史（Test History）
│   ├── 每次测试运行的结果（通过/失败/跳过）
│   ├── flaky test 标记（不稳定测试的识别）
│   ├── 覆盖率趋势（覆盖率变化趋势）
│   └── 性能基线（性能测试的历史基线）
│
└── 环境配置（Environment Config）
    ├── 测试环境配置（数据库、缓存、Mock 服务）
    ├── 测试数据（种子数据、 fixtures）
    └── 依赖版本（测试通过的依赖版本组合）
```

### 3.3 测试工作流示例

```yaml
workflow_id: test_pipeline
name: 测试流水线

triggers:
  - type: webhook
    event: git.pull_request

nodes:
  - id: analyze_changes
    name: 分析代码变更
    type: task
    agent: test_assistant
    action: |
      分析 PR 中的代码变更，确定：
      1. 变更了哪些文件/模块
      2. 需要运行哪些测试（单元/集成/E2E）
      3. 历史上有哪些类似变更导致过 Bug
    outputs:
      - changed_files
      - affected_tests
      - risk_assessment

  - id: unit_test
    name: 单元测试
    type: task
    agent: test_assistant
    action: |
      运行单元测试，关注变更相关的测试用例
    inputs:
      - analyze_changes.affected_tests
    outputs:
      - test_report

  - id: coverage_check
    name: 覆盖率检查
    type: task
    agent: test_assistant
    action: |
      检查覆盖率是否达标（>80%），新代码覆盖率是否 >90%
    inputs:
      - unit_test.test_report
    outputs:
      - coverage_report

  - id: flaky_test_check
    name: 不稳定测试检查
    type: task
    agent: test_assistant
    action: |
      检查是否有 flaky test（历史失败率 >10% 的测试）
    inputs:
      - unit_test.test_report
    outputs:
      - flaky_test_report

  - id: generate_test_report
    name: 生成测试报告
    type: task
    agent: test_assistant
    action: |
      综合所有测试结果，生成 PR 测试报告：
      - 通过率、覆盖率、不稳定测试
      - 历史对比（与上次 PR 的对比）
      - 风险评估
    inputs:
      - unit_test.test_report
      - coverage_check.coverage_report
      - flaky_test_check.flaky_test_report
    outputs:
      - pr_test_report
```

### 3.4 测试 TUI 展示

```
┌──────────────────────────────────────────────────────────────┐
│  Test Pipeline: PR #123  ●running  2m 45s                      │
├──────────────────────────────────────────────────────────────┤
│                                                             │
│  [✓] analyze_changes  15s    5 files changed, 3 modules    │
│  [◐] unit_test         1m 30s  245/245 running...            │
│  [○] coverage_check    idle    depends: unit_test           │
│  [○] flaky_test_check  idle    depends: unit_test           │
│  [○] generate_report   idle    depends: all                 │
│                                                             │
│  测试进度: 180/245 passed, 0 failed, 65 running              │
│  覆盖率: 87% (目标: 80%)  ↑ 3% vs last PR                    │
│                                                             │
│  🧠 recalled: 上次修改 auth.js 时， flaky test #45 失败了  │
│  3 次，原因是 race condition。建议增加同步锁。               │
│                                                             │
│  ⚠️ 警告: 本次变更触及 payment 模块，历史上该模块 Bug 率    │
│  较高（12%），建议增加额外的边界测试。                         │
│                                                             │
├──────────────────────────────────────────────────────────────┤
│  > _                                                         │
└──────────────────────────────────────────────────────────────┘
```

---

## 4. 研究模式（Research Mode）

### 4.1 设计理念

研究不是"搜索然后总结"，而是**让 Agent 能够系统性地搜集信息、交叉验证、分析趋势、生成报告**。

**研究 Agent 的职责**：
- 信息搜集：从多个来源（搜索引擎、数据库、文献、API）搜集信息
- 交叉验证：验证信息的准确性（多个来源对比、事实核查）
- 分析归纳：分析趋势、发现模式、提炼结论
- 报告生成：生成结构化的研究报告（摘要、发现、建议、引用）

### 4.2 研究记忆

```
Research Memory
├── 研究方向（Research Direction）
│   ├── 研究主题（如"AI 编程助手市场趋势"）
│   ├── 研究问题（如"2025 年市场规模是多少？"）
│   └── 研究范围（时间范围、地域范围、数据来源）
│
├── 信息来源（Information Sources）
│   ├── 来源列表（URL、数据库、API、文献）
│   ├── 来源可信度评分（基于历史准确性）
│   └── 来源缓存（已抓取内容的本地缓存）
│
├── 研究结论（Research Findings）
│   ├── 事实列表（已验证的事实）
│   ├── 趋势分析（时间序列、增长趋势）
│   ├── 对比分析（竞品对比、方案对比）
│   └── 不确定性标记（信息不足、来源冲突的结论）
│
└── 研究报告（Research Reports）
    ├── 报告模板（摘要、发现、建议、引用）
    ├── 历史报告（之前的研究报告）
    └── 报告引用（报告中引用的来源和事实）
```

### 4.3 研究工作流示例

```yaml
workflow_id: market_research
name: 市场研究

triggers:
  - type: manual
    user: product_manager

nodes:
  - id: define_scope
    name: 定义研究范围
    type: task
    agent: research_assistant
    action: |
      与用户确认研究主题、问题、范围
    outputs:
      - research_scope

  - id: search_info
    name: 搜集信息
    type: parallel
    agents: [search_assistant, data_assistant]
    actions:
      - search: 搜索引擎、新闻、社交媒体
      - data: 数据库、API、内部数据
    inputs:
      - define_scope.research_scope
    outputs:
      - raw_info

  - id: verify_info
    name: 验证信息
    type: task
    agent: research_assistant
    action: |
      交叉验证信息来源，标记可信度
    inputs:
      - search_info.raw_info
    outputs:
      - verified_info

  - id: analyze_trends
    name: 分析趋势
    type: task
    agent: research_assistant
    action: |
      分析趋势、发现模式、提炼结论
    inputs:
      - verify_info.verified_info
    outputs:
      - analysis

  - id: generate_report
    name: 生成报告
    type: task
    agent: research_assistant
    action: |
      生成结构化研究报告
    inputs:
      - analyze_trends.analysis
    outputs:
      - report
```

### 4.4 研究 TUI 展示

```
┌──────────────────────────────────────────────────────────────┐
│  Research: AI 编程助手市场趋势  ●running  15m 30s              │
├──────────────────────────────────────────────────────────────┤
│                                                             │
│  [✓] define_scope      2m    研究范围已确认                 │
│  [✓] search_info       8m    搜集到 45 条信息                │
│  [✓] verify_info       3m    验证完成，38 条可信，7 条存疑  │
│  [◐] analyze_trends    2m    正在分析趋势...                 │
│  [○] generate_report   idle   depends: analyze_trends        │
│                                                             │
│  信息来源分布:                                               │
│  - 新闻网站: 20 条  可信度: 75%                              │
│  - 行业报告: 8 条   可信度: 90%                              │
│  - 社交媒体: 12 条  可信度: 60%                              │
│  - 学术文献: 5 条   可信度: 95%                              │
│                                                             │
│  🧠 recalled: 上次研究"AI 编程助手"时，Cursor 的市占率是    │
│  35%（2024 Q3）。本次研究需要更新数据。                       │
│                                                             │
│  初步发现:                                                   │
│  - 市场规模: 预计 2025 年达到 $X 亿（来源: Gartner, McKinsey）│
│  - 增长率: 45% YoY（来源: 多源交叉验证）                     │
│  - 不确定性: 中国市场的数据不足（仅 3 条来源）                 │
│                                                             │
├──────────────────────────────────────────────────────────────┤
│  > _                                                         │
└──────────────────────────────────────────────────────────────┘
```

---

## 5. 团队协作（Team Collaboration）

### 5.1 设计理念

团队协作不是"多人同时用一个 Agent"，而是**让 Agent 成为团队成员，参与任务分配、进度跟踪、评审、沟通**。

**协作 Agent 的职责**：
- 任务分配：根据团队成员的技能和负载，分配任务
- 进度跟踪：跟踪项目进度，识别瓶颈，提醒风险
- 评审参与：参与代码评审、文档评审，给出建议
- 沟通协调：在团队频道同步信息，协调跨团队依赖
- 知识共享：自动提取项目知识，分享给团队成员

### 5.2 团队记忆

```
Team Memory
├── 团队成员（Team Members）
│   ├── 成员档案（技能、偏好、负载、历史贡献）
│   ├── 角色定义（前端、后端、测试、产品、设计）
│   └── 负载跟踪（当前任务数、预计完成时间）
│
├── 项目知识（Project Knowledge）
│   ├── 项目架构（技术栈、模块划分、依赖关系）
│   ├── 编码规范（风格指南、命名约定、最佳实践）
│   ├── 决策记录（ADR、技术选型、方案对比）
│   └── 会议纪要（会议讨论、决策、行动项）
│
├── 任务管理（Task Management）
│   ├── 任务列表（待办、进行中、已完成）
│   ├── 任务依赖（哪些任务阻塞了哪些任务）
│   └── 任务分配（谁负责什么任务，预计何时完成）
│
└── 沟通记录（Communication Log）
    ├── 频道消息（Slack/Discord/Teams 的消息历史）
    ├── 评论线程（PR 评论、Issue 评论、文档评论）
    └── 决策过程（讨论 → 决策 → 执行的完整记录）
```

### 5.3 协作模式

#### 模式 1：任务分配

```
用户: 把"支付模块重构"任务分配给合适的成员

Agent: 分析团队成员负载和技能...

🧠 recalled: 
- 张三: 负载 3 个任务，擅长后端，熟悉支付系统
- 李四: 负载 1 个任务，擅长前端，不熟悉支付系统
- 王五: 负载 5 个任务，已超负荷

Agent: 建议分配给张三。理由：
1. 熟悉支付系统（上次重构过相关模块）
2. 负载适中（3 个任务，预计 2 周完成）
3. 后端技能匹配

用户: 确认分配

Agent: 已分配给张三，并通知其加入相关上下文。
```

#### 模式 2：进度跟踪

```
用户: 项目进度怎么样？有什么风险？

Agent: 分析项目状态...

🧠 recalled: 
- 项目里程碑: 3 月 15 日上线
- 当前日期: 2 月 20 日
- 剩余任务: 15 个，已完成: 20 个

Agent: 
进度: 57% 完成，按当前速度预计 3 月 10 日完成，提前 5 天。

风险:
1. ⚠️ 支付模块重构（分配给张三）进度滞后 20%，可能阻塞集成测试
   - 建议: 让李四协助，或拆分任务
2. ⚠️ 前端适配（分配给李四）依赖后端 API，API 预计 2 月 25 日完成
   - 建议: 提前沟通 API 变更，让李四准备 Mock 数据

建议:
- 本周五召开进度对齐会，重点讨论支付模块风险
```

#### 模式 3：评审参与

```
用户: 帮我评审这个 PR

Agent: 分析 PR 内容...

🧠 recalled: 
- 项目编码规范: 优先使用 TypeScript，不用 any
- 上次类似 PR: #45，引入了性能问题，原因是循环嵌套

Agent: 
评审意见:
1. ✅ 代码风格符合规范（TypeScript，无 any）
2. ⚠️ 第 45 行: 循环嵌套（O(n²)），可能影响性能
   - 建议: 使用 Map 优化，参考 PR #45 的修复方案
3. ❌ 缺少单元测试（新增代码覆盖率 0%）
   - 建议: 补充边界测试（null、空数组、大数据量）
4. 💡 建议: 增加日志记录，便于排查问题

是否自动修复问题 2？（需要修改 3 行代码）
```

### 5.4 协作 TUI 展示

```
┌──────────────────────────────────────────────────────────────┐
│  Team: 商城项目  ●active  5 members  23 tasks                 │
├──────────────────────────────────────────────────────────────┤
│                                                             │
│  项目进度: 57%  ████████████████░░░░░░░░░░░░░░░              │
│  预计完成: 3 月 10 日（提前 5 天）                            │
│                                                             │
│  成员负载:                                                   │
│  张三  ████████░░  3/5  tasks  [支付模块重构]  ⚠️ 滞后 20%   │
│  李四  ███░░░░░░░  1/5  tasks  [前端适配]      ○ 正常       │
│  王五  ██████████  5/5  tasks  [测试、文档]    ⚠️ 超负荷    │
│                                                             │
│  风险预警:                                                   │
│  ⚠️ 支付模块重构可能阻塞集成测试（3 月 1 日）                  │
│  ⚠️ 王五 超负荷，建议分配 1 个任务给李四                      │
│                                                             │
│  待办事项:                                                   │
│  1. [ ] 周五进度对齐会（重点: 支付模块风险）                   │
│  2. [ ] 支付模块 API 文档更新（阻塞李四）                     │
│  3. [ ] PR #123 评审（分配给 Agent）                        │
│                                                             │
│  🧠 recalled: 上周会议决定：支付模块重构优先于前端适配         │
│                                                             │
├──────────────────────────────────────────────────────────────┤
│  > _                                                         │
└──────────────────────────────────────────────────────────────┘
```

---

## 6. 统一记忆架构

所有场景共享统一的记忆架构，只是记忆内容不同：

```
Unified Memory Architecture
├── 短期记忆（Short-term Memory）
│   ├── 当前会话上下文（Redis，TTL: 1h）
│   ├── 当前工作流状态（Redis，TTL: 工作流运行期间）
│   └── 当前任务列表（Redis，TTL: 会话期间）
│
├── 中期记忆（Medium-term Memory）
│   ├── 执行轨迹（PostgreSQL，保留 7 天）
│   ├── 工作流运行记录（PostgreSQL，保留 7 天）
│   ├── 测试运行记录（PostgreSQL，保留 7 天）
│   └── 研究信息来源（PostgreSQL，保留 7 天）
│
└── 长期记忆（Long-term Memory）
    ├── 项目知识（pgvector，永久）
    │   ├── 编码规范、架构决策、技术选型
    ├── 团队知识（pgvector，永久）
    │   ├── 成员技能、分工、偏好、历史贡献
    ├── 工作流模板（pgvector，永久）
    │   ├── 可复用的工作流定义、节点类型
    ├── 测试知识（pgvector，永久）
    │   ├── 测试用例、Bug 模式、修复策略
    ├── 研究知识（pgvector，永久）
    │   ├── 研究结论、来源可信度、报告模板
    └── 通用知识（pgvector，永久）
        ├── 用户偏好、系统配置、全局约定
```

**记忆召回策略**：
- 编程场景：召回编码规范、项目架构、历史修改
- 工作流场景：召回工作流模板、失败模式、优化建议
- 测试场景：召回测试用例、Bug 模式、 flaky test 标记
- 研究场景：召回研究方向、来源可信度、历史结论
- 协作场景：召回成员技能、任务分配、决策记录

---

## 7. 扩展后的架构图

```
┌─────────────────────────────────────────────────────────────┐
│  User Input                                                  │
│  （编程/工作流/测试/研究/协作）                               │
└────────┬────────────────────────────────────────────────────┘
         │
         ▼
┌─────────────────────────────────────────────────────────────┐
│  Router（智能路由）                                           │
│  - 场景识别（编程/工作流/测试/研究/协作）                     │
│  - 任务分类（简单/工具/复杂）                                 │
│  - 场景路由（加载对应的记忆空间和工具集）                     │
└────────┬────────────────────────────────────────────────────┘
         │
         ▼
┌─────────────────────────────────────────────────────────────┐
│  Agent Core（Plan-Execute-Reflect）                          │
│  - Plan: 生成结构化执行计划（1-5 步）                         │
│  - Execute: 执行当前步骤（LLM 调用或工具调用）                  │
│  - Observe: 收集执行结果                                      │
│  - Reflect: 评估是否偏离，是否调整计划                         │
│  - 最大 3-5 轮后强制终止                                      │
└────────┬────────────────────────────────────────────────────┘
         │
         ▼
┌─────────────────────────────────────────────────────────────┐
│  Memory & Tools（记忆与工具层）                              │
│  ┌──────────────────┐  ┌──────────────────┐                  │
│  │  Unified Memory  │  │  Tool Sets       │                  │
│  │  ├── 短期记忆     │  │  ├── 编程工具    │                  │
│  │  │   （Redis）    │  │  │   （文件操作）  │                  │
│  │  ├── 中期记忆     │  │  ├── 工作流工具  │                  │
│  │  │   （PostgreSQL）│  │  │   （DAG 执行）│                  │
│  │  └── 长期记忆     │  │  ├── 测试工具    │                  │
│  │      （pgvector） │  │  │   （测试框架）  │                  │
│  │                   │  │  ├── 研究工具    │                  │
│  │                   │  │  │   （搜索/API）  │                  │
│  │                   │  │  ├── 协作工具    │                  │
│  │                   │  │  │   （通知/协作） │                  │
│  │                   │  │  └── MCP 客户端  │                  │
│  │                   │  │      （外部生态） │                  │
│  └──────────────────┘  └──────────────────┘                  │
└─────────────────────────────────────────────────────────────┘
```

---

## 8. 场景切换与记忆共享

### 8.1 场景切换

用户在同一个会话中切换场景：

```
用户: 帮我运行部署流水线（工作流场景）
Agent: 工作流执行中...

用户: 等等，先帮我检查一下这个测试为什么失败（测试场景）
Agent: 切换场景...

🧠 recalled: 当前工作流 deploy_pipeline 运行到 deploy_staging 步骤，等待中。

Agent: 正在分析测试失败...

用户: 好的，继续部署（回到工作流场景）
Agent: 恢复工作流执行...

🧠 recalled: 测试分析结论：失败原因是环境变量配置错误，已修复。
```

**场景切换机制**：
- 每个场景有独立的 SessionBlock（话题隔离）
- 场景切换时，当前场景归档，新场景加载
- 跨场景知识共享（如"部署流水线"和"测试"都涉及环境配置）

### 8.2 记忆共享

不同场景之间共享记忆：

```
编程场景 → 工作流场景
- 共享: 项目架构、代码规范、环境配置
- 不共享: 具体的代码修改细节

工作流场景 → 测试场景
- 共享: 部署环境、服务依赖、配置参数
- 不共享: 工作流的调度策略

测试场景 → 研究场景
- 共享: 性能指标、Bug 模式、技术选型
- 不共享: 测试用例的具体实现

研究场景 → 协作场景
- 共享: 研究结论、决策建议、项目规划
- 不共享: 研究过程中的原始数据
```

**记忆共享规则**：
- 全局记忆（编码规范、项目架构、团队分工）跨场景共享
- 场景特定记忆（代码修改、工作流定义、测试用例）只在本场景内共享
- 场景切换时，自动召回全局记忆 + 相关场景记忆

---

## 9. 扩展后的 TUI 设计

### 9.1 场景切换栏

```
┌──────────────────────────────────────────────────────────────┐
│  BlockMemoryAgent > session-001  ●running                     │
│  [编程] [工作流] [测试] [研究] [协作] ← 场景切换栏            │
│  当前: [编程]  ◄────────────────────────────────────────────│
├──────────────────────────────────────────────────────────────┤
│                                                             │
│  主对话区（根据场景显示不同内容）                             │
│                                                             │
│  > 帮我修复这个 CSS 文件                                     │
│  ...                                                        │
│                                                             │
├──────────────────────────────────────────────────────────────┤
│  [plan] 1.ReadFile → 2.WriteFile → 3.RunTest   [2/3] ✓       │
├──────────────────────────────────────────────────────────────┤
│  > _                                                         │
└──────────────────────────────────────────────────────────────┘
```

场景切换栏：
- 显示当前可用的场景（编程、工作流、测试、研究、协作）
- 当前场景高亮显示
- 点击/按数字键切换场景

### 9.2 场景特定面板

按场景切换右侧 Agent 面板的内容：

**编程场景**：
- 文件树（当前工作目录）
- 代码审查状态
- 工具调用历史

**工作流场景**：
- 工作流 DAG 图
- 节点执行状态
- 执行日志

**测试场景**：
- 测试用例列表
- 覆盖率报告
- 失败测试详情

**研究场景**：
- 信息来源列表
- 研究进度
- 可信度分析

**协作场景**：
- 成员列表
- 任务分配
- 进度图表

---

## 10. 实现路径

### Phase 1：核心骨架（3-4 周）
- [ ] Session Manager：多场景会话管理
- [ ] Router：场景识别 + 任务分类
- [ ] Agent Core：Plan-Execute-Reflect 循环
- [ ] 编程工具：文件操作、命令执行
- [ ] 中期记忆：PostgreSQL 执行轨迹表
- [ ] TUI 骨架：场景切换栏 + 主对话 + 底部状态栏

### Phase 2：工作流与测试（3-4 周）
- [ ] 工作流引擎：DAG 定义、调度、执行、监控
- [ ] 工作流 TUI：DAG 图、节点状态、执行日志
- [ ] 测试工具：测试框架集成、覆盖率、性能压测
- [ ] 测试 TUI：测试用例列表、覆盖率报告、失败详情
- [ ] 长期记忆：工作流模板、测试用例库、Bug 模式

### Phase 3：研究与协作（3-4 周）
- [ ] 研究工具：搜索引擎、API、数据可视化
- [ ] 研究 TUI：信息来源、研究进度、可信度分析
- [ ] 协作工具：任务管理、通知、评审
- [ ] 协作 TUI：成员列表、任务分配、进度图表
- [ ] 团队记忆：成员档案、项目知识、决策记录

### Phase 4：优化与扩展（2-3 周）
- [ ] MCP 集成：对接外部工具生态
- [ ] 场景切换：场景间记忆共享、上下文恢复
- [ ] 性能优化：长时运行测试、记忆压缩、向量检索
- [ ] 文档与示例：各场景的使用示例、最佳实践

---

## 11. 总结

BlockMemoryAgent 的扩展设计遵循一个核心原则：**底层架构统一（记忆、路由、Agent Core），上层场景扩展（编程、工作流、测试、研究、协作）**。

每个场景都受益于底层的核心能力：
- **上下文恒定**：无论场景多复杂，LLM 输入始终控制在预算内
- **话题隔离**：场景内子任务不互相污染
- **跨会话记忆**：场景切换后，关键知识自动召回
- **智能路由**：简单任务直接响应，复杂任务进入 Plan-Execute-Reflect

扩展后的 BlockMemoryAgent 不是一个"编程助手"，而是一个**Agent 工作流平台**——你可以用它编程、编排工作流、自动化测试、进行研究、协调团队，而所有场景都享有同样的记忆和上下文管理能力。

**一句话：一个不会遗忘、不会变笨、不会污染的 Agent 平台，支持你从编程到协作的所有工作流。**
