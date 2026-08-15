# 多 Agent 架构专项评测报告 2026-08-15

> 评测套件：`test/eval/`（bash 驱动 + node 判分器 + 场景目录，2026-08-15 重建；旧 Go 版 harness 已不在仓库）。
> 与 `baseline_2026-08-14.md` 的区别：旧基线测**通用任务完成率**（bugfix/codegen/refactor 等），本套件专测**多 Agent 架构机制**——角色路由、并行派发、依赖链+Mailbox 回灌、共享记忆契约传递、上下文隔离、块记忆落库、长上下文稳定性。
> 原始产物：`test/eval/runs/20260815-135300/`（report.json/report.md + 每场景 tree.json/logs.json/workspace/score.json）。

## 评测配置

- 模型组合：生产 `config/roles.yaml` 现状（meta/domain=glm-5.2，code=kimi-k2.7-code，ui=doubao-seed-2.1-turbo，轻量/test/doc 等=deepseek-v4-flash）
- 环境：docker-compose.test.yml 隔离实例（PG 55432 / Redis 56380），embed=pseudo（768 维，与迁移建库一致）
- 单遍运行；判分全确定性 checkpoint（file/command/tree/logs/pg），无 llm_judge；TheAgentCompany 式全量/部分分
- 单场景墙钟上限 25 分钟，paused 系状态自动续跑

## 总体结果

- **Full pass 率：100%（7/7）**
- **Checkpoint 加权平均分：100%**
- 全部 7 场景终态 completed，无超时/报错/预算耗尽
- 总用时约 72 分钟（13:53–15:05，含每场景独立服务重启）；单场景 151s ~ 1294s

## 单场景明细

| 场景 | 测的机制 | Full pass | 用时(s) | tokens(in/out) | 树节点 |
|---|---|---|---|---|---|
| route-code-test-doc | 角色路由 + 多角色协作 | ✓ | 392 | 158.9K/25.6K | 4 |
| parallel-independent | 并行派发 | ✓ | 241 | 114.4K/13.2K | 3 |
| chain-dependency | 依赖链 + Mailbox 回灌 | ✓ | 903 | 327.8K/41.7K | 3 |
| sharedmem-long-spec | WriteSpec/共享记忆契约传递 | ✓ | 903 | 288.4K/66.9K | 3 |
| isolation-distractor | 子 Agent 上下文隔离/抗污染 | ✓ | 151 | 81.2K/4.8K | 2 |
| memory-facts | 块记忆事实提取 + PG 落库 | ✓ | 362 | 155.9K/19.9K | 4 |
| longctx-multifile | 长上下文多文件项目稳定性 | ✓ | 1294 | 464.7K/81.8K | 8 |

## 机制验证要点（人工抽查）

- **并行派发**：`parallel-independent` 中 MetaAgent 对两个独立 CLI 任务直接并行派 2 个 code_assistant 叶子（小任务跳过 domain 层，符合角色提示词纪律），fib/prime 产物均机检通过。
- **依赖链**：`chain-dependency` 的 LCG(seed=42) 50 值 CSV 与 report.json 统计值经判分器独立重算比对一致——数据生成与消费两段经 Mailbox 摘要正确衔接。
- **共享记忆**：`sharedmem-long-spec` 的 session_logs 命中 WriteSpec/WriteSharedMemory 调用；规格中埋的 3 个易丢细节（snake_case 字段、幂等键 header、422 错误码）全部出现在产出文档与自测脚本中。
- **抗污染**：`isolation-distractor` 两个干扰标记串（ZEBRA-9917/QWERTZ-4820）在 workspace 全部产物中零出现。
- **块记忆**：`memory-facts` 子 Agent 完成后事实提取链（轻量模型提取 → 逐条向量化 → global_knowledge 落库）写入 8 条知识记录，`memory_write_failures` 0 行。
- **长任务**：`longctx-multifile` 8 文件 CLI 项目，派发树 8 节点（3 建设 domain + 4 叶子 + 1 整品验收 domain），端到端 e2e 机检通过，1294s 无压缩塌缩。

## 过程中发现与修复（均为评测侧问题，非系统缺陷）

1. **判分口径误报 ×2**（首轮 91.8% 的两个失分项）：`tree_no_dup_sibling_roles` 原按"同父下同 role"判重，把「4 个 commands/*.js 各派一个 code_assistant」「fib/prime 各派一个叶子」的合法并行误判为去重失效。系统的去重机制粒度是"同父下同领域"（dispatcher 树快照去重），判分已对齐为只对 domain 节点判重，离线重判后两场转 PASS。
2. **harness 配置静默失配 ×1**（memory-facts 首轮 62.5%）：生产 roles.yaml 的 `embed.provider` 从 local 漂移为 openai，run_suite.sh 中按字面值匹配的 sed 静默失配 → 嵌入请求 404 → 块记忆零落库。对照证据：同日上午降级轮（embed=pseudo）落库 113 条正常。已改为 embed 块内值无关替换 + 改写失败即中止的硬校验，重跑该场景满分（8 条落库）。

## 观察与后续

- token 消耗与任务复杂度正相关（hard 场景 465K in / 82K out），可作后续 token 优化参照；长场景 8 节点树形符合"3 建设 domain 并行 + 整品验收收口"的提示词纪律。
- 本轮为单遍结果（pass@1）；建议下一轮 `EVAL_RUNS=3` 式重跑看稳定性，并把首日上午的 deepseek 全降级轮整理为模型组合 A/B 对照（降级轮因配额中断未跑完，数据在 `runs/20260815-131242/`）。
- 已知遗留：`/api/sessions/{id}/token-metrics` 当前构建返回 null（react 服务未实现该 Query 分支），报告 tokens 列从 session_logs 汇总回退；测试库 session_history 缺 meta_memory 列（schema 漂移，仅一条 ERRO 日志，不影响终态）。
- 候选扩展场景：子 Agent 运行时取消（/agents/{aid}/cancel）恢复力、depth-3 深层派发、跨会话知识召回（当前块记忆只写不读，召回侧仍待接线）。

---

## 第二轮：多轮记忆一致性 + 派发复杂度 + 耗时归因（2026-08-15 下午）

### 新增场景与驱动

- `run_multiturn.sh`：多轮驱动（场景用 `turns.json` 代替 prompt.md；第 1 轮建会话，后续轮 POST /message 续聊；每轮抓增量答复落 `turns-result.json`）。
- `score.js` 新增 7 个 check 类型：`turn_reply_regex` / `turn_reply_absent_regex` / `logs_absent_regex` / `tree_max_nodes` / `pg_fact_count` / `pg_no_dup_facts` / `turn_latency_max`。
- 两个新场景：`multiturn-memory-12`（12 轮：事实注入 → 4 轮干扰 → 召回 → 矛盾更新 → 2 轮派发型代码任务 → 2 轮干扰 → 最终召回 ×2）、`simple-chat-dispatch`（"1+1=?" 单轮）。

### 结果（均为端到端真实 LLM 运行）

| 场景 | 测的机制 | Full pass | 用时 | run 目录 |
|---|---|---|---|---|
| multiturn-memory-12 | 12 轮记忆一致性 + 块记忆 + 按需派发 | ✓ 12/12 | 487s | `runs/20260815-161634/` |
| simple-chat-dispatch | 简单问答零派发 | ✓ 5/5 | 31s* | `runs/20260815-162831/`（新二进制冒烟轮） |

\* 多轮驱动当时 POLL_INTERVAL=30s，每轮 elapsed 含至多 ~29s 完成检测虚高；该轮真实 LLM 仅 ~3.4s（1.45s 轻量画像提取 + 1.96s 主调用）。两个驱动的轮询均已改 5s。

### 记忆一致性实证（核心结论：无记忆混乱、无块记忆混乱）

- T1 注入 4 条用户事实（张伟/Phoenix/PostgreSQL 14/杭州），根 Agent 主动走 **WriteSharedMemory** 持久化。
- 跨 3 轮干扰后 T5 召回：Phoenix + PostgreSQL 14 ✓（当时的正确值）。
- T6 矛盾更新（迁移 MySQL 8）被正确接受，答复明示"旧记录已作废"。
- 再跨 4 轮干扰（含 2 轮真实派发代码任务）后 T11 最终召回：**仅 "MySQL 8"，无旧值残留**；T12：张伟 + 杭州 ✓。
- 块记忆：2 个派发任务共落 8 条事实，`pg_no_dup_facts` 零逐字重复、`memory_write_failures` 0 行。
- 派发复杂度：简单问答 0 子 Agent、树 0 节点、直接作答 ✓；同会话中代码任务轮正常触发派发（dispatch_happened ✓）——派发按需发生，不多不少。
- 质量观察（不影响判分）：整品验收子 Agent 提取的事实含低价值噪音（如"验收过程中未修改任何文件"），实现+验收两 Agent 的事实存在语义级冗余；块记忆写侧无去重/价值分级，召回侧未接线前无实际危害。

### 耗时归因（基于首轮 7 场景运行数据）

- **墙钟 62–89%（典型 ~80%）在等 LLM 响应**，按既定口径不优化。最大单项是 glm-5.2 思考型长尾（单调用 max 169.5s，MetaAgent/sharedmem）；deepseek-v4-flash 叶子调用全部 <30s。
- 非 LLM 间隙约 60–70% = 正常工具执行 + 未记账的轻量 LLM（事实提取/事件摘要）+ 驱动轮询虚高（每场景 11–28s）。
- 唯一真实异常：**embedding 端点挂起阻塞派发主路径**（`injectScopedRecall` → `pg.Embed`，60s 超时×重试，用派发 ctx），实证 chain-dependency 158s、longctx-multifile 61s 沉默延迟；pseudo embed 下为 0s。

### 本轮修复清单

- 评测侧 ×5：多轮答复抓取改"按轮增量切片 + 整条排除含 [tool_call] 记录"（logs.json 实为 ID 倒序，先升序再切片）；`pg_fact` 计数上限误校准改 `pg_no_dup_facts` 逐字重复检查；派发检查裸词 `call_sub_agent` 会误命中系统提示词，改匹配真实派发事件；两个驱动轮询 30s→5s。
- 系统侧 ×2（小修，已重建二进制并冒烟验证）：`retryProvider` 补 `ModelName()`（session_logs/server.log 的 model= 全空的根因——`llmModelName()` 类型断言不到重试包装层）；轻量 LLM 调用（事实提取/事件摘要/画像提取/意图仲裁）经 ctx 挂 session logger 写 session_logs，带 `Meta.layer=lightweight` 与实测 LatencyMs。
- 记 TODO ×2（`doc/TODO.md`）：**#45** 子 Agent 收尾事实提取同步阻塞 DONE 通知（实证最长 69s，异步化有黑板可见性竞态需专门设计）；**#46** embedding 调用挂起阻塞派发主路径（需独立短超时 + 熔断，涉及召回降级策略权衡）。

## 第三轮：TUI 真实链路 + 高相似度记忆混淆（2026-08-15 傍晚）

### 驱动方式（与上两轮的区别）

本轮不经 HTTP 驱动脚本，而是驱动**真实 TUI Model**：`backend/internal/tui/live_multiturn_test.go`（`BMA_LIVE_MULTITURN=1` 手动开启）复刻 cmd/tui 全链路——bootstrap.Build 真实装配 + 本地 HTTP 路由 + `NewModel`，逐轮模拟按键（KeyRunes 打字 → KeyEnter 提交 → tick 等待终态 → 快照 View），剧本来自 `scenarios/similar-memory-12/turns.json`。真实终端 tee 驱动（winpty）经实测不可行：无控制台窗口时 winpty 断言失败（`wp != nullptr && cols > 0`），bubbletea 也必须持有 TTY。run 目录：`test/eval/runs/tui-similar-memory-12/`。

### 场景设计（相似度对抗）

双生项目 Phoenix / Falcon：句式逐字相同，仅槽位值不同（PG14/PG15、Redis 6/7、张伟 B1023/李四 B1024，同部署杭州机房）——嵌入与字面都高度相似，专攻"块/共享记忆串槽"。12 轮：T1-T4 注入 8 个槽位事实 → T5/T6 交叉召回 → T7 无关干扰 → T8 矛盾更新（Falcon PG15→16）→ T9 防污染召回（Phoenix 应仍为 14）→ T10 更新值召回 → T11 人物槽位召回 → T12 八槽位对比表。

### 结果：12/12 PASS，零混淆（全程 150s）

| 轮 | 检查点 | 结果 |
|---|---|---|
| T5 | Phoenix=PG14/张伟（不得混 15/李四） | ✓ |
| T6 | Falcon 缓存=Redis 7（不得混 6） | ✓ |
| T8 | 更新落 version 3，确认回复含全景表两行均正确 | ✓ |
| T9 | **防污染关键轮**：Phoenix 仍=PostgreSQL 14 | ✓ |
| T10 | Falcon=PostgreSQL 16（不残留 15） | ✓ |
| T11 | 李四=B1024/Falcon（不得混 B1023/Phoenix） | ✓ |
| T12 | 对比表 8 槽位全对 | ✓ |

- **共享记忆落盘逐字验证**（`.bma/shared/`）：phoenix v2 与 falcon v3 两份文件各自槽位完全正确、零串行——Falcon 的 PG16 更新未触碰 Phoenix 文件；falcon 文件保留"已从 PostgreSQL 15 迁移"审计痕迹。
- **压缩层无失真**：16 条事件摘要（【近期事件】）准确压缩全部事实含版本变迁，无串槽。
- **派发复杂度**：12 轮纯记忆/问答，零子 Agent 派发、零工具滥用（仅 3 次必要的 WriteSharedMemory），T5/T6/T9-T12 均为直接作答。
- 块记忆（global_knowledge）本轮零写入——纯聊天无子 Agent 完成，不触发事实提取落库，符合既定设计边界；混淆验证落在共享记忆 KV 与召回链路。

### 过程中发现的问题（1 真产品缺陷 + 1 产品小修 + 2 评测基建）

- **真缺陷（TODO #47，当日已修复）**：TUI 新会话自动选中竞态——`createSession` 后台 goroutine 写 `pendingSelectID`，但 bubbletea 值语义下 `agent.CreateSession` 阻塞 ~15s 期间 Model 已被 tick 拷贝多轮，写入落在废弃副本上，新会话永不自动选中。与 live_harness 注释的"首条问题不可见"同源。修复：`flash`/`flashUntil`/`pendingSelectID` 收进指针共享的 `sharedState`（model.go），后台写入对所有拷贝可见；同根因的后台 flash 丢失一并修复。评测驱动仍保留手动 selectSession 兜底。
- **产品小修（已改）**：`store/schema.go` `EnsureSessionHistorySchema` 注释承诺确保 `meta_memory` 列存在，实际对存量表是空操作（CREATE TABLE IF NOT EXISTS 不补列），测试库报 `column "meta_memory" does not exist`；已补 `ALTER TABLE ... ADD COLUMN IF NOT EXISTS`。
- 评测基建 ×2：live_multiturn_test.go 多轮驱动（含受理/终态判定防"上一轮残留态误判"、答复三级回退提取 Messages→Result→Events）；`scenarios/similar-memory-12/`。
- 测试库运维：global_knowledge 旧表 vector(768) 与现行配置 1024 维不匹配，rename 备份为 `global_knowledge_768_bak` 后自动重建（未丢数据）。
