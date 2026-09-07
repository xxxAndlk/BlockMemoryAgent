# 对擂评测计划：BlockMemoryAgent vs Claude Code

> 状态：进行中（cron 驱动迭代，每 30 分钟一轮观察）
> 评测装置：`test/duel/`；产物：`test/duel/runs/`；报告：`test/duel/reports/`
> 本文档是权威计划与评分体系定义；迭代记录见 `doc/eval/duel_log.md`。

## 1. 目标

用两个标准化任务对比 **Claude Code（对照组，视为信任基准）** 与 **当前 BMA 系统（实验组）** 的完成质量与完成时间，并通过持续迭代修改 BMA（代码/配置/提示词均可改），使 BMA 在两个任务上都**追上、赶平或超越** Claude Code。

### 终止条件（全部满足才停）

1. 任务 A：BMA 完成度 ≥ Claude 完成度 − 0.02，且 BMA 用时 ≤ 1.10 × Claude 用时
2. 任务 B：同上口径
3. 上述两条在**连续 2 轮**中同时成立
4. 硬性熔断：第 12 轮仍不达标则停止并输出差距分析报告

## 2. 任务定义

### 任务 A：一次性中小任务（6 个完成点）

**`csvstat`：CSV 统计命令行工具**（Python，仅标准库）。空工作区 + 预置 `data/sample.csv` 夹具，一次prompt 交付。

| 编号 | 完成点 | 权重 |
|---|---|---|
| A1 | `csvstat.py` 存在，`python csvstat.py data/sample.csv` 退出码 0 且 stdout 为合法 JSON | 1 |
| A2 | 顶层字段 `row_count`/`column_count`/`columns` 与夹具一致 | 2 |
| A3 | 每个数值列的 `mean/min/max` 统计正确（相对误差 ≤1e-6） | 2 |
| A4 | 每列缺失计数 `missing` 正确 | 1 |
| A5 | `--columns a,b` 过滤、`--output FILE` 写文件、`--pretty` 缩进输出均可用 | 2 |
| A6 | `python -m unittest discover -s tests` 通过且 `README.md` 存在 | 2 |

总权重 10。判分器 `test/duel/grade.js task_a` 自行解析夹具 CSV 计算期望值，不硬编码。

### 任务 B：大型多次迭代任务（1 + 5 轮迭代，共 40 个完成点）

**`task-api`：纯 Node.js 标准库的任务管理 API + 简单前端**。同一工作区内分 6 次迭代交付；BMA 侧同会话续投 prompt，Claude 侧 `claude -p`（iter≥1 用 `--continue` 保持对话）。每轮迭代判分独立进行，且都带回归检查（`npm test` 必须持续绿）。

- **iter0 地基（10 点）**：package.json+start 脚本 / /api/health / GET /api/tasks / POST 建任务 / 列表可见 / 无 title 400 / 未知路由 404 / 静态首页 / README / npm test ≥3 例通过
- **iter1 CRUD 完备（6 点）**：GET 单条 200/404 / PATCH / DELETE / data/tasks.json 持久化（重启不丢） / ?completed= 过滤 / npm test 绿
- **iter2 前端（6 点）**：首页引用 app.js / app.js 可访问且 fetch 任务列表 / style.css / 新建+删除逻辑 / 完成切换逻辑 / ?q= 搜索
- **iter3 健壮性（6 点）**：写操作 X-API-Key 401 校验 / 正确 key 放行 / title 校验（>200、全空白 400） / /api/stats 一致 / 畸形 JSON 400 / access.log 落盘 + npm test 绿
- **iter4 特性（6 点）**：dueDate+?overdue / priority+?sort=priority / tags+?tag= / bulk-complete / CSV 导出 / 分页+X-Total-Count + npm test 绿
- **iter5 工程质量（6 点）**：npm test ≥15 例 / node --check 全 src / Dockerfile / src/config.js 配置模块 / docs/api.md ≥8 端点 / 组合过滤（completed+tag+q）

约束（双侧同）：仅 Node 标准库、禁外部 npm 依赖、工作区内交付、全程自主不等用户输入。

## 3. 评分体系（详细）

### 3.1 完成度 C（0~1，主指标）

每个完成点二元判定（pass/fail）× 权重。判分全部确定性（命令执行/文件检查/HTTP 断言），无 LLM 裁判。

- 迭代 i 完成度：`C_i = Σ通过权重 / Σ该迭代总权重`
- 任务 A：`C = C_A`
- 任务 B：`C = mean(C_0..C_5)`，未执行到的迭代计 0

### 3.2 稳健性 R（0~1）

- 任务 A：终态 completed=1.0；timeout=0.5；error/boot_failed=0
- 任务 B：`R = (干净完成的迭代数 / 6) × 终态系数`，终态系数同上（最后一次运行的终态）

### 3.3 效率 E（0~1）

`E = min(1, T_ref / T_actual)`。T_ref = 同轮 Claude 用时（round-1 之后轮次的 Claude 基线冻结为 round-1 值，除非判分器/任务变更触发重测基线）。用时口径：任务 A 为会话创建到终态的墙钟；任务 B 为 6 次迭代墙钟之和（不含判分时间）。

### 3.4 质量 Q（0~1，任务 A 生效；2026-09-06 应要求新增，与 C/E/R 并列报告）

完成度 C 只验"功能点有没有"，Q 验"做得好不好"。确定性探针 `quality_task_a.js`（11 权重）：BOM 容忍、空白行语义、短行缺列、科学计数法/前导零、nan/inf 降级、未知列报错、--columns 保序、测试用例数（≥20 满/≥10 半）、README 三节完整度、控制台编码防护。对双侧产物同口径执行；claude 基线取 round-1 产物实测值（Q=0.909），BMA 每轮测当轮产物。
任务 B 质量量规（API 健壮性/代码结构/文档）后续按需补充，暂以 C/E/R 评定。

### 3.5 综合分 S（0~100，趋势跟踪用）

`S = 100 × (0.70×C + 0.20×E + 0.10×R)`

### 3.6 胜负判定（每任务每轮）

| 判定 | 条件 |
|---|---|
| BMA 胜 | C_bma > C_claude + 0.02 且 T_bma ≤ T_claude（任务 A 另需 Q_bma > Q_claude + 0.02） |
| 追平（达标） | C_bma ≥ C_claude − 0.02 且 T_bma ≤ 1.10 × T_claude（任务 A 另需 Q_bma ≥ Q_claude − 0.05） |
| 未达标 | 其余 |

注：Q 门槛 2026-09-06 起生效，此前的连续达标计数清零重计（历史轮次未测 Q，无法追认）。

### 3.7 辅助观测指标（不入分，仅归因）

token 输入/输出总量、LLM 调用次数、子 Agent 派发数（tree.json）、每迭代用时分布。

### 3.8 任务 B 逐迭代门控（2026-09-06 应要求启用）

任务 B 不再一口气跑完 6 迭代再总算，改为**逐迭代门控**：

- 每个迭代 i 独立尝试（`task_b_gate.sh <round> <i> <try>`）：工作区先恢复到 iter(i−1) 的合格快照（iter0 为空工作区），起新 server + 新会话，逐字投递 `prompt_iter<i>.md`（与 claude 侧指令输入保持逐字一致；claude 侧用 `--continue` 带会话历史，BMA 侧每次新会话——上下文差异属系统属性，记录不补偿）。
- **合格线（三维同时满足）**：完成率 score_i ≥ ref_i − 0.02；速度 elapsed_i ≤ 1.10 × ref_i；人工质检通过（编排 Agent 精读本迭代产物：实现正确性/测试增量/边缘处理/夹具只读纪律/交付物干净度，无阻断问题）。
  - ref_i = claude 同轮基线的该迭代 score/elapsed（`task_b_gate.sh ref <round>`）。
- **合格** → `task_b_gate.sh snapshot` 保存进度（快照排除 .bma/logs/node_modules/运行时数据），进入下一迭代。
- **不合格** → 记录 attempt，做一个焦点改动（修 BMA 系统），重试同一迭代，直到合格。重试次数全程留档并在报告中披露。
- **官方成绩口径**：6 迭代全部合格后，用最终配置跑一次**完整无重试全程**（`run_task_b.sh bma`，单会话 6 迭代），以该次的 C/T 进入 report.js 对比；门控过程只作改进回路。
- 计时口径与旧协议一致：会话创建→终态，不含 server 启动与判分。

## 4. 评测装置（test/duel/）

- `tasks/task_a/{prompt.md, workspace_seed/data/sample.csv}`、`tasks/task_b/prompt_iter{0..5}.md` — 双侧同一 prompt，逐字相同
- `grade.js <task_a|task_b> <iter> <workspace> --out score.json` — 确定性判分器，任务 B 判分时自行起/杀被测服务（随机端口）
- `quality_task_a.js <workspace>` — 任务 A 质量探针（Q），run_task_a.sh 判分后自动执行 → quality.json
- `lib.sh` — 复用 SWE 对照组模式：测试库 TRUNCATE 隔离、per-run 配置副本（PG 55432/Redis 56380/随机端口/关鉴权/ui_model 重指）、健康等待、会话轮询（paused 自动续跑）、PG 转储 tree/logs/token
- `run_task_a.sh <side> <round>` / `run_task_b.sh <side> <round>` — 单侧单任务；`run_round.sh <round> [both|bma|claude]` — 串行矩阵（测试库为全局资源，禁并行）
- `report.js <round>` — 聚合本轮回合 → `reports/round-N.md` + `reports/duel-status.json`（含胜负判定与达标计数）
- 墙钟上限：任务 A 1200s/侧；任务 B 1800s/迭代。超时记 timeout 并强杀

公平性：同 prompt、同初始工作区、同判分器、同模型端点生态（沿用 SWE 对照组口径）。

### 4.1 指令对齐管控（2026-09-06 应要求加严）

- **任务指令正文双侧逐字相同**：claude 侧 `claude -p "$(cat prompt)"`，BMA 侧同一文件内容作 goal（iter0）/content（iter>0）原文投递，无任何包装/增删；md5 留档于 duel_log。
- **阻断 claude 侧隐式指令泄漏**：claude CLI 默认从 cwd 向上继承仓库根 `CLAUDE.md` 与 `.claude/settings*.json`（实测探针确认会加载 `BlockMemoryAgent/CLAUDE.md`，其中含 Windows bash 等可获益约定），BMA 侧无此输入。自 round-7 起 claude 调用统一加 `--setting-sources user`（只保留 user 级设置，探针验证：记忆加载 → NONE，auth/model 不受影响）。`--bare` 不可用：会绕过 keychain/用户端点配置导致鉴权失败。
- 因此 **claude 基线自 round-7 重测**（report.js 的 claudeRef 自动取最近一轮 claude 结果）；round-1 基线仅作历史对照。重测顺序：round-7 先重跑任务 A 双侧验证指令对齐，无异常再 round-8 重测任务 B claude 基线。

## 5. 迭代循环协议（cron 每 30 分钟）

每轮触发执行：

1. **观察**：检查进行中 run 的状态（runs/round-N/**/run-meta.json、driver.log）；卡死（>上限 1.5×）则杀并记 timeout
2. **回合收尾**：本轮双侧 run 全部有 run-meta.json 后，跑 `report.js` 出报告与 duel-status.json
2.5 **人工质检**：双侧产物完赛后，由编排 Agent 亲自精读两侧交付物（实现/测试/README/工作区状态），产出 `reports/qc-round-N-<task>.md`：逐项裁断、探针外过程性问题、对下一轮提示词/系统改动的可执行输入。质检结论与探针 Q 并列作为质量证据；冲突时以人工质检为准并校正探针
3. **判定**：达成终止条件 → 写最终报告、删 cron、通知用户
4. **归因**：落后时读 BMA 侧 logs.json/tree.json/失分 checkpoint，定位主因（提示词/角色权限/工具缺陷/模型配置/耗时结构）
5. **改进**：每轮只做**一个焦点改动**（可改 BMA 任意代码/配置），改前确认无 run 在进行（Windows 下运行中二进制不可覆盖），改后 `make build` 类命令重建 `dist/bin/bma-server.exe`
6. **重测**：启动下一轮 `run_round.sh <N+1> bma`（Claude 基线冻结，除非判分器变更）
7. **记录**：追加 `doc/eval/duel_log.md`（轮次/改动/分数/差距/下一步）

约束：改动最小化、不 git commit、不动 test/duel 判分口径（判分器修复性变更需同时重测 Claude 基线并在日志注明）。

## 6. 轮次规划

| 轮次 | 内容 |
|---|---|
| round-1 | 双侧基线（4 个 run：A-claude / A-bma / B-claude / B-bma） |
| round-2..6 | BMA 迭代追赶（每轮 2 个 run），Claude 基线冻结于 round-1 |
| round-7 | 指令对齐管控生效：任务 A 双侧重跑（claude 基线重测，§4.1） |
| round-8 | 任务 B claude 基线重测；随后 BMA 进入逐迭代门控回路（§3.8） |
| round-9..N | 任务 B 门控推进 + 最终全程官方成绩；任务 A 维持双任务达标计数 |
