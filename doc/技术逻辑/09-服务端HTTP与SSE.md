# 09 服务端 HTTP 与 SSE

> 只从代码还原。代码：`backend/internal/server/`（19 个非测试文件 + `eventkind/kind.go`），路由装配在 `backend/internal/bootstrap/router.go`，静态资源在 `backend/main.go`。

## 目录

- [9.1 职责与三层分工](#91-职责与三层分工)
- [9.2 完整路由表](#92-完整路由表)
- [9.3 SSE 实现](#93-sse-实现)
- [9.4 鉴权](#94-鉴权)
- [9.5 错误映射](#95-错误映射)
- [9.6 关键端点流程](#96-关键端点流程)
- [9.7 工作区文件服务（workspace）](#97-工作区文件服务workspace)
- [9.8 导出](#98-导出)
- [9.9 能力自检 / 模型 / 插件 / 技能库 / 目录选择 / DAG](#99-能力自检--模型--插件--技能库--目录选择--dag)
- [9.10 隐含约定与坑](#910-隐含约定与坑)

---

## 9.1 职责与三层分工

`internal/server` 是 **HTTP 传输适配层**：Gin 请求 → `agent.Agent` 门面调用 → JSON/SSE/zip。不持有业务状态（唯一例外 `APIHandler.paused` 遗留表，无人读）。

1. **`SessionManager`**（session.go:79）：`agent.Agent` 之上薄适配器，全部会话端点。不直接持有存储（`SetPostgresStore`/`SetModelFactory` 是 **no-op 空实现**）。
2. **`APIHandler`**（api.go:44）：系统/配置/管理端点（health/metrics/capabilities/models/plugins/skills/files/profile/export），直接持有 pgStore/redisStore/pluginMgr/learnedSkills/modelFactory。
3. **`DAGHandler`**（dag.go:22）：独立子处理器自带 RegisterRoutes。

`routes.go` 是**唯一会话路由注册入口**——生产（bootstrap/router.go:54）、TUI 本地服务（cmd/tui/main.go:186）、测试三方共用同一清单。

线型 DTO：`Session`（session.go:23-55，含 `State *types.ThreeLayerState` 遗留桥接：只回填 CurrentDomain/ActiveBlocks/PendingClarify，三者全空置 nil）、`SessionEvent`（与 agent.Event 一一对应）、`sessionSummary`（列表轻量线型，防 200 条 ≈14MB）、`agentNode`（session_http.go:258-272）。

## 9.2 完整路由表

生产全部业务路由挂 `router.Group("/api", auth)` ——**除 `/api/health` 外全部需要鉴权**。

### A. 公开

| 方法 | 路径 | handler | 语义 |
|---|---|---|---|
| GET | `/api/health` | api.go:265 | Postgres/Redis/LLM 三态探活（2s 超时） |

### B. 会话端点（routes.go:12-51）

| 方法 | 路径 | handler | 语义 |
|---|---|---|---|
| GET | `/api/sessions` | session_http.go:161 | 摘要列表，`?limit=`（默认 200，上限 1000） |
| POST | `/api/sessions` | :40 | 创建 `{goal, images?, videos?, work_dir?, gear?, thinking?}` |
| POST | `/api/sessions/delete` | :200 | 批量硬删除 `{ids[]}`（≤200，逐条独立结果，恒 200） |
| GET | `/api/sessions/:id/stream` | stream_http.go:18 | SSE 长连接 |
| POST | `/api/sessions/:id/message` | :383 | 发消息 `{content, images?, videos?}` |
| GET | `/api/sessions/:id/board` | :237 | 看板（QueryKindBoard） |
| GET | `/api/sessions/:id/agents` | :276 | Agent 实例列表（block 目标覆盖 goal + 活动证据） |
| GET | `/api/sessions/:id/tree` | :430 | 权威 Agent 树 |
| POST | `/api/sessions/:id/agents/:aid/cancel` | :446 | 取消子 Agent |
| POST | `/api/sessions/:id/agents/:aid/pause` | :563 | 暂停 domain 支路 |
| GET | `/api/sessions/:id/agents/:aid/events` | :602 | 子 Agent 逐轮事件 |
| GET | `/api/sessions/:id/agents/:aid/messages` | :625 | 完整消息历史 + mailbox 留痕（`before_seq/after_seq/limit`；aid=meta 映射主 Agent） |
| POST | `/api/sessions/:id/agents/:aid/message` | :650 | 用户直连（返回 `{ok, queued}`） |
| GET | `/api/sessions/:id/worktrees` / `:aid/diff` | :708/:721 | worktree 清单 / 全量 diff |
| POST | `/api/sessions/:id/worktrees/:aid/:action` | :742 | 合并门 merge/reject（reject 可带 comments） |
| GET | `/api/sessions/:id/metrics` / `efficiency` / `logs` / `token-metrics` / `watchdog` / `mailbox` | session.go | 只读查询族 |
| POST | `/api/sessions/:id/clarify` / `interrupt` / `enqueue` / `cancel` / `stop` / `topic` / `trust-mode` / `gear` / `thinking` / `workdir` | control_http.go / session_http.go | 控制族 |
| GET | `/api/sessions/:id/workspace/*path` | workspace_http.go:35 | 工作区文件流式服务 |
| DELETE | `/api/sessions/:id` | :184 | 硬删单会话 |
| GET | `/api/sessions/:id` | :100 | 详情（全量线型） |
| GET | `/api/sessions/:id/export` | router.go:57（**不在 RegisterSessionRoutes**，TUI 无此端点） | 单会话导出 zip |

### C. 系统/管理端点（bootstrap/router.go:60-105）

metrics、status、metrics/timeline、activity、capabilities、export/memory、DAG 六端点（GET/POST /dag、/dag/running、/dag/:id、DELETE、/dag/:id/trigger）、snapshot/memory/search/memory/levels/memory/eval（**三个 memory 桩**）、skills、files、files/content、fs/browse、fs/pick-dir、profile（GET/PUT）、project/preferences（GET/PUT）、project/tester-config（GET/PUT）、skills/learned（列表/详情/PUT/enable/disable）、evolution/log、plugins（列表/详情/enable/disable/reload）、models（GET/POST /models/switch/POST）。

### D. 非 /api（main.go）

`/assets/*`（immutable 缓存）、`/favicon.svg`（no-cache）、**NoRoute → index.html 200**（SPA 回退）。

## 9.3 SSE 实现

`stream_http.go`：

- 建立（:18-52）：`Get` 探活（失败 404 文本非 SSE 帧）→ 头（`text/event-stream`/no-cache/keep-alive/`Access-Control-Allow-Origin: *`——全仓唯一 CORS 头）→ **`SetWriteDeadline(time.Time{})` 清除 server 级 WriteTimeout**（否则 30s 掐断长任务；依赖 gin ResponseWriter 的 Unwrap）→ Flusher 断言 → **首帧=完整 Session JSON 快照**。
- 帧格式：一律 `data: <json>\n\n` 单行帧，**不用 `event:`/`id:`/`retry:`**。
- 轮询循环（500ms ticker）：
  | 帧 | 触发 | 内容 |
  |---|---|---|
  | 全量快照 | 事件切片被头部裁剪（`lastEventCount > len`） | 完整 Session |
  | 增量事件 | 新事件逐条 | SessionEvent |
  | `live` | StreamingText/ThinkingText 变化 | `{type:"live", streaming_text, thinking_text}` |
  | `session_status` | Status 变化一次 | `{type, status}` |
  | `awaiting_clarify` | **status==awaiting_clarify 时每 tick 都推** | `{type,status,question,question_id,multi_select,options?,detail?,artifacts?,questions?}` |
  | `done` | 非 running 且非 awaiting_clarify → 推完 return（关连接） | `{type:"done", status}` |
- **无独立心跳帧**；**不读 Last-Event-ID、无事件回放**——断线补偿全靠"重连首帧推全量快照"。
- 每连接每秒 2 次全量 `Get`（deep copy），多标签页是服务端主要放大点。

## 9.4 鉴权

`GinAuthMiddleware(token, publicPaths)`（auth.go:28）：

- **token 为空 → 直接放行**（视为未启用鉴权）——`auth_enabled: true` 但 `BMA_API_TOKEN` 为空 = 静默裸奔。
- 否则要求 `Authorization: Bearer <token>` 或 **`?token=` query**（任何 `/api/*` 都可凭 query token 通过；媒体 URL 用它是刻意设计）。
- 比较是 `==`（非常量时间）。
- 前端（web）**不注入任何 token** → 开启鉴权后整个 Web 不可用；当前部署形态是本机 127.0.0.1 + 关闭鉴权。

## 9.5 错误映射

`agentErrorStatus`（session.go:507-531）：

| 哨兵 | HTTP |
|---|---|
| ErrSessionNotFound | 404 |
| ErrQueueFull | 503 |
| **ErrAgentNotDirectable** | 409（必须排在 ErrInvalidSessionState 之前——它是后者的 %w 包装） |
| ErrSessionFinished / ErrInvalidSessionState | 400 |
| ErrPostgresUnavailable | 503 |
| ErrAgentNotFound | 404 |
| ErrAgentBusy | 409 |
| 默认 | 500 |

**不统一**：写操作走 agentErrorStatus；查询端点（metrics/logs/board/watchdog/mailbox/token-metrics/efficiency）统一裸 500；`GET /:id` 与 `GET /:id/agents` 把所有错误折叠成 404「会话不存在」。

## 9.6 关键端点流程

- **创建会话**（session_http.go:40-96）：DecodeBody → goal 非空 → gear/thinking 枚举校验 → `workDirAbs`（必须存在且是目录，**到达 agent 前拦截**）→ `ParseWireImages`（≤4 张/≤4MiB）→ `ParseWireVideos`（≤2 个/白名单扩展名/存在/≤200MB）→ CreateSession。
- **发消息**：DecodeBody → content 非空 → 图片/视频校验 → `Send` → 成功后再 Get 一次返回最新快照。
- **控制族**（control_http.go）：DecodeBody → 业务校验 → `Control(ControlCommand)` → 回 `{session_id, status}`（clarify/interrupt/enqueue 回 running，stop 回 stopping，cancel 回 error）。
- **workdir**：`WorkDir *string` 指针字段——nil=参数错 400；显式 ""=清除回落进程默认。
- **worktree action**：ToLower+TrimSpace 后仅 merge|reject；reject 的 comments 解析失败**静默吞掉**。

## 9.7 工作区文件服务（workspace）

`workspace_http.go:35`：`GET /sessions/:id/workspace/*path` → 路径防护（TrimPrefix("/")→Clean(FromSlash)→拒 ""/"."/IsAbs/../→Join 后 `filepath.Rel` 二次校验）→ **目录一律 404** → `http.ServeContent`（Range/Content-Type/nosniff）+ `Referrer-Policy: no-referrer`。

- path 型路由让 HTML 产物内相对引用（`assets/x.png`）自然可用。
- **符号链接未拦截**：工作目录内软链指向外部仍会被服务（词法级防护）。
- 同类遗留：`/api/files/content`（任意宿主路径读全文）与 `/api/fs/browse`（列目录）**无任何路径限制**，唯一门槛是鉴权。

## 9.8 导出

- **单会话**（export_http.go:34）：`ExportSessionData`（session_history/session_events/agent_events/agent_messages 四 JSON）→ workDir 鸭子类型解析 → zip 流式：`workspace/.bma/**` 产物树。限额 `exportMaxFiles=2000` / 单文件 `64MB`；跳符号链接/读失败/超大文件。
- **记忆库**（:71）：`memory-export-YYYYMMDD-HHMMSS.zip` → `knowledge.jsonl`（archived=false，LIMIT 10000；失败落 `knowledge.error.txt` 不整体 500）+ `user_profile.md` + `skills_learned/*.md`；`sanitizeZipName` 白名单 `[A-Za-z0-9._-]`。

## 9.9 能力自检 / 模型 / 插件 / 技能库 / 目录选择 / DAG

- **capabilities**（capabilities.go:33）：3s ctx，六项 llm/postgres/redis/embed/plugins/workdir；**只做廉价检查不发真实 LLM 调用**：LLM 看 `CurrentModelInfo("meta")`；PG/Redis 双判空+Ping；embed 按 provider 分支（openai/local 要求 api_key 或 base_url，缺给 `Missing` 提示）；plugins 停用不计入、启用有 MissingEnv/LastError 即不合格；workdir 写探针文件即删。返回 `{items, all_ok}`。
- **models**（model.go）：`GET /models`（无 api_key）/`POST /models/switch`（**70s**，含 60s 探活；错误 400 非 500；**未走 DecodeBody 无 UTF-8 校验**）/`POST /models`（`slugModelID` 生成 ID、重名追加 -2）。
- **plugins**（plugins.go）：list/get/enable/disable/reload（30s/60s）；**`GetPluginHandler` 缺 nil 守卫**（其余三个有）。
- **learned_skills**：全部端点 `learnedSkills==nil` → 503；`PUT /:name` 最重：校验→Get→重写文件（CRLF→LF）→重嵌入（失败不阻塞）→UpdateMeta→注册进技能池。
- **fs/pick-dir**（fs_pick.go）：进程级单例（`TryLock`，第二个请求 409）→ 5min ctx → Windows PowerShell -STA WinForms（**结果经临时文件 UTF-8 回传防中文乱码**）/darwin osascript/linux zenity→kdialog→501。
- **tester-config**（tester_config.go）：`.bma/tester.yaml` 读写（off|auto|on + max_rounds 1-5）。
- **DAG**（dag.go）：guard（scheduler nil → 503）；SaveDAG 拒环（400）+ 每次刷新 UpdatedAt；trigger 5s。

## 9.10 隐含约定与坑

1. **`?token=` 例外是全局的**（不只媒体）；Token 比较非常量时间。
2. **`auth_enabled: true` + 空 token = 静默裸奔**。
3. **写超时必须被 SSE 显式清掉**（SetWriteDeadline zero）。
4. **SSE 无回放/无心跳**；断档靠重连首帧全量快照；事件被裁剪也走快照兜底（2026-09-09"前端永远收不到 agent_done"事故的修复点）。
5. **awaiting_clarify 每 500ms 重推**——前端须幂等。
6. **done 判定口径 = 非 running 且非 awaiting_clarify**：awaiting_child/paused_on_child 直接关流。
7. **SSE 每连接每秒 2 次全量 Get** 是放大点。
8. **子 Agent 实例 ID 含 `/` 必须开 RawPath**：`router.UseRawPath=true + UnescapePathValues=true`（bootstrap/router.go:29-30）；**TUI 本地 gin 没开** → TUI 下编排页子 Agent 路由 404。
9. **workspace 符号链接未拦截**；`/api/files/content` 与 `/api/fs/browse` 无路径限制。
10. **NoRoute 全回 index.html 200**：拼错的 API 路径也回 HTML，排查时别被误导。
11. **死代码**：`NewAPIHandler(nil)` 使 broadcaster 恒 nil；`RetrieveHandler`/`EventResolveHandler`/`GraphPause/Resume` 未注册路由（一旦接上会 nil panic）；`TUIBroadcaster.SSEHandler` 无注册点；`APIHandler.paused` 无生产读写。
12. **`SessionManager.SetPostgresStore/SetModelFactory` 空实现**（bootstrap 仍在调）。
13. **列表 limit 内部放大**：`agent.List` 对库侧固定取 ≥200 再合并截断——`?limit=5` 也会查 200 条。
14. **`stats_service` 口径**：Timeline 按小时分桶（calls=token_usage 事件条数；数据源=最近 200 会话的近似口径）；Activity 按"会话列表顺序 × 会话内倒序"分组拼接，**非全局时间序**。
15. **`ToServerSession` 深拷贝**；Messages 只保留 Role/Content/Timestamp——**图片/视频附件刻意不透出**。
16. **`workDirAbs` 三个入口共用**（创建会话/改会话目录/项目偏好/测试助手），保证目录校验一致。
