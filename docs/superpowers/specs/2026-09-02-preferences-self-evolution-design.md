# 设计：偏好与自进化系统（外部经验大脑）

> 日期：2026-09-02 ｜ 状态：已获用户批准，待实施 ｜ 方案：路线一（外部经验大脑，不改 prompt 骨架/模型权重）

## 1. 背景与目标

BlockMemoryAgent 已有用户画像 v1 闭环（`config/user_profile.md` + `remember_preference` 工具 + 会话结束轻量模型提取 + 仅注入 MetaAgent + `GET/PUT /api/profile`），但**只有追加，没有去重/冲突解决/遗忘**；项目偏好完全空白（仅有自动维护的 `.bma/PROJECT.md` 项目概览，非偏好）；自进化完全空白。

目标（对齐 Hermes Agent / EvoSkill 的"经验大脑"路线）：

1. **用户偏好 v2**：画像从"只追加"升级为"自动整理"——去重、冲突归档、人工行保护。
2. **项目偏好**：每个 workDir 一份偏好文件，人工约定 + 自动沉淀的项目经验（典型例子：渲染动画帧前先统一去白底、保持角色一致性需固定参考图+seed），注入该项目所有会话。
3. **自进化**：会话结束后反思轨迹，把经验固化为结构化技能包（SKILL.md 式），下次遇到相似任务直接召回，"不绕圈、不重复犯错"。

生效机制（用户已确认）：**自动生效 + 可审计可禁用**——沉淀物立即参与召回，全部写 evolution_log，Web UI 可查看/编辑/禁用。

## 2. 范围

**做**：用户画像 Merge 整理；项目偏好存储/注入/工具/API；SessionEvolver（一次轻量模型调用产出三类沉淀）；技能库存储/注册/向量预筛召回/同名更新；evolution_log 审计；Web UI 四块最小可用页面。

**不做**（本期明确排除）：

- Harness 自进化：不改 soul.md / roles.yaml / 角色 prompt / 工具规则（连"提议+审批"也不做）。
- 路线二（RL 训权重）与路线三（零数据自学）。
- 独立 Evolver Agent 离线读全轨迹诊断（方案 C，留作未来升级，技能库存储与召回接口对其兼容）。
- 技能的跨项目移动/合并 UI、技能版本历史 diff。
- 用户主动取消的会话不参与进化。

## 3. 总体数据流

```
会话完成/失败钩子（复用现有 extractProfilePreferences 位置，service_react.go）
        ▼
SessionEvolver：一次轻量模型调用，读 goal+summary+事件摘要+用户消息
        ▼ 产出三类沉淀（结构化 JSON）
  ① 用户偏好增量      ② 项目经验增量        ③ 技能包 0-2 个
        ▼                ▼                     ▼
user_profile.md    .bma/project_       config/skills_learned/*.md
（全局，Merge整理） preferences.md      + PG 元数据 + embedding
        ▼           （per workDir）           ▼
  MetaAgent 注入    该项目所有会话      注册进 skill 池（渐进披露）
                    Meta+Domain 注入    相似任务向量预筛 → 提示可 load

        全部沉淀写 evolution_log 表（审计）→ Web UI 查看/编辑/禁用
```

与既有机制的边界：块记忆（global_knowledge）继续存"事实"（子 Agent 完成时提取，1-5 条短句）；技能库存"工艺/技能"（会话结束时提取，结构化步骤+坑点+验证法）。两者并存、召回通道各自独立，不合并。

## 4. 用户偏好 v2（Merge 整理）

- **人工行 vs 自动行**：无 `(YYYY-MM-DD HH:MM)` 时间戳后缀的行视为人工行，自动整理永不删除/改写；带后缀的自动行可被合并、去重、重写。
- **Merge 流程**：会话结束提取到偏好增量后，轻量模型对目标小节（偏好/技术栈/沟通风格）做合并重写——去重、新偏好与旧自动行冲突时新的生效、旧行移入 `## 反馈记录` 归档（不直接删，可审计）。
- 反馈记录只保留最近 20 条（作为原始审计线索），超出裁最旧。
- 注入不变：仍只注入 MetaAgent system prompt，带 rune 截断。

## 5. 项目偏好（新）

- **存储**：每个 workDir 一份 `.bma/project_preferences.md`，与 user_profile 同构：
  - `## 项目约定`（人工：如"用 pnpm 不用 npm"、"提交前必须 make lint"）
  - `## 项目经验`（自动沉淀：如"渲染帧先统一去白底再合成"）
  - 同样的人工行/自动行规则与 Merge 整理。
- **实现**：把 `internal/userprofile/store.go` 的 `Store` 泛化为接受文件路径的通用 store（atomic.Pointer RCU 读 + mutex 串行写、Append/Save/Reload 语义不变），userprofile 与 projectprofile 各持一个实例，不复制代码。
- **注入**：MetaAgent + DomainAgent 都注入（项目经验是执行层要遵守的工艺）。DomainAgent 侧在 Dispatcher 前缀拼装处加【项目偏好】段，带 rune 截断。首次写入才创建文件，不强制启动创建。
- **人工通道**：直接编辑文件；`remember_preference` 工具加 `scope: user|project` 参数（默认 user）；HTTP `GET/PUT /api/project/preferences`（作用于当前 workDir）。

## 6. 自进化核心：SessionEvolver + 技能库

### 6.1 触发

- 会话正常完成或失败时执行（失败会话的踩坑经验价值更高，产出标 `outcome`）；用户主动取消不执行。
- 会话过短跳过：事件数 < 5 的琐碎问答不提取，防噪音。
- 复用现有会话完成钩子位置（`service_react.go` `extractProfilePreferences` 附近），扩展为 `EvolveSession`；一次轻量模型调用同时产出三类沉淀，复用 `CallLightweightWithRetry` + JSON 解析模式。
- Evolver 输入：goal + summary + 事件流摘要（复用现有 EventSummarizer）+ 用户消息，输入上限约 4000 runes，输出上限约 2000 runes。

### 6.2 技能包结构（SKILL.md 式 markdown + YAML frontmatter）

```yaml
name: animation-frame-consistency   # 小写连字符，全局唯一
title: 多帧动画图像一致性工艺
when_to_use: 需要生成多张帧图合成动画时
steps: [...]
pitfalls: [白底未去除导致闪烁, 角色形象跨帧漂移]
verify: 合成前逐帧检查背景透明度与角色相似度
outcome: success|failed|mixed
```

字段校验：缺 name/title/when_to_use 即丢弃该条、记日志，其余沉淀照常。

### 6.3 存储分界

- **通用工艺** → 全局技能库 `config/skills_learned/<name>.md`（跨项目复用，人可直接编辑）。
- **项目特有经验** → 该项目 `.bma/project_preferences.md`（不进技能库）。
- 由提取 prompt 指导轻量模型判断 scope；判错了人可在 UI 编辑修正。

### 6.4 同名合并（create-or-update）

产出技能与已有技能 `when_to_use`+title 向量相似度超阈值时走**更新**：合并改进内容、刷新 updated_at、保留 use_count；否则新建。

### 6.5 召回（复用 skill 池渐进披露）

- skill 包启动时从 `skills_learned/` 目录加载注册进现有 skill 池（仅 enabled）；`list_skills` 可见摘要、`load_skill` 取全文。
- MetaAgent 收到新任务 / DomainAgent 派发时，用 goal 向量预筛 top-3 enabled 技能（相似度阈值过滤），命中只注入一行提示：「有相关经验技能 `<name>`——`<title>`，可 load_skill 查看」。**不自动注入全文**。
- 被 load 后 `use_count++`，用于排序与审计。
- embedding 走现有 embed 组件（roles.yaml 的 embed 配置）。

## 7. 数据模型（migration 007 + bootstrap Ensure 幂等建表，保持现有风格）

```sql
CREATE TABLE learned_skills (
  name TEXT PRIMARY KEY,
  title TEXT NOT NULL,
  when_to_use TEXT NOT NULL,
  content_path TEXT NOT NULL,
  embedding VECTOR(...),          -- 维度对齐现有 embed 配置
  enabled BOOLEAN NOT NULL DEFAULT TRUE,
  use_count INT NOT NULL DEFAULT 0,
  source_session TEXT,
  outcome TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- ivfflat 向量索引

CREATE TABLE evolution_log (
  id BIGSERIAL PRIMARY KEY,
  kind TEXT NOT NULL,             -- user_pref/project_lesson/skill_create/skill_update
  target TEXT NOT NULL,           -- 小节名或技能名
  summary TEXT NOT NULL,
  source_session TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

## 8. 审计与管理面

**HTTP API**：

- `GET/PUT /api/profile`（已有，不变）
- `GET/PUT /api/project/preferences`（当前 workDir 项目偏好）
- `GET /api/skills/learned`、`GET/PUT /api/skills/learned/{name}`
- `POST /api/skills/learned/{name}/enable`、`POST /api/skills/learned/{name}/disable`
- `GET /api/evolution/log`

**Web UI 最小可用四块**（Vue3，复用现有页框架）：

- 画像页：接通已有 `/api/profile`（当前是 FeaturePlaceholder）
- 项目偏好页：查看/编辑
- 技能库页：列表 + 详情 + 编辑 + 启用/禁用
- 进化日志：审计列表（可作技能库页 tab）

## 9. 错误处理

- 轻量模型不可用 / JSON 解析失败：静默降级、记 session_logs，不影响会话主流程（与事实提取回退一致）。
- 技能字段校验失败：丢弃该条、记日志，其余沉淀照常。
- 文件与 PG 一致性：先写文件成功后写 PG；启动时重扫目录注册，容忍并修复孤儿（文件在 PG 无记录则补注册，PG 有文件无则标记禁用）。
- 禁用技能：向量预筛与 list_skills 均过滤。

## 10. 测试

- 单测：Merge 逻辑（人工行保护 / 冲突归档 / 去重 / 反馈记录裁剪）；store 泛化后双实例（user/project）；EvolveSession mock 轻量模型验证三类产出落库/落文件与降级路径；技能注册/召回/禁启用过滤/同名更新。
- 集成：test/api 补新端点 e2e（含 profile 现有覆盖不动）。
- 回归：`make backend-test`、`make test-test`、`make lint` 全绿。

## 11. 分期落地

1. **期 1**：store 泛化 + 用户画像 Merge 整理 + 项目偏好存储/注入 + `remember_preference` scope 参数。
2. **期 2**：SessionEvolver（三类产出）+ evolution_log。
3. **期 3**：技能库注册/向量预筛召回/use_count/同名更新。
4. **期 4**：HTTP API + Web UI 四块。

每期独立可验；期 1-3 纯后端可测。

## 12. 关键设计决策记录

| 决策 | 结论 | 理由 |
|---|---|---|
| 进化范围 | 只做路线一（经验大脑），不动 prompt 骨架 | 用户两例（技能总结、动画帧经验）全落路线一；Harness 进化风险/收益不成比例 |
| 生效机制 | 自动生效 + 审计可禁用 | 用户确认；审批队列易卡住进化闭环 |
| 召回通道 | skill 池渐进披露（方案 A），非块记忆 | 块记忆装不下结构化技能；渐进披露 token 友好 |
| 项目偏好注入范围 | MetaAgent + DomainAgent 都注入 | 工艺经验是执行层需要的（用户可推翻） |
| 失败会话 | 参与进化，标 outcome | 踩坑经验价值高（用户可推翻） |
| 项目偏好内容 | 人工约定 + 自动经验同文件分小节 | 用户例子里两者兼有 |
