# 编排三层拍平为两层（meta + domain 直接执行）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把编排从三层（meta → domain → leaf）拍平为两层（meta → domain）：meta 职责不变，domain 变为各领域的直接执行人、默认自执行；叶子助手（fixed_roles）保留，是否下拆由 domain 自主判断。

**Architecture:** 现状是"单 ReAct 循环 + 递归 `call_sub_agent`"：三层共享同一执行器（`agent.ReActAgent`）与派发器（`subagent.Dispatcher`），层级差异完全由角色定义（工具白名单 + system prompt + CanCall 权限）区分。"domain 默认下拆 leaf"目前是**提示词纪律而非硬机制**（domain 白名单本就含全套执行工具）。因此本次改造**只翻转纪律、不删机制**：改 domain 提示词 + 派发工具描述 + 文档；`CanCall`、domain 白名单（保留 `call_sub_agent(s)`）、暂停/续跑、热驻留池、打捞等 domain 专属机制全部保留。

**Tech Stack:** Go 1.25（`GOTOOLCHAIN=local`）、YAML 配置（`config/roles.yaml`）。

## Global Constraints

- 所有 `go test` / `go build` 命令必须带 `GOTOOLCHAIN=local`（仓库硬约定）。
- Windows Git Bash：Unix 语法，路径用正斜杠。
- **不删机制**：`registry.go` 的 `CanCall` 权限矩阵、domain 工具白名单中的 `call_sub_agent/call_sub_agents`、`dispatcher.go` 的 domain 暂停/续跑/热驻留/打捞/黑板摄取、`fixed_roles` 定义——一律不动。
- **meta 不变**：`config/roles.yaml` 的 `meta_agent` 段一字不改。
- `registry.go` 兜底 prompt 与 `roles.yaml` `domain_agent.system_prompt` 注释要求语义同步，任一处修改必须同步另一处。
- 提交信息用中文、带任务编号（参照 `doc/变更.md` 惯例）。

---

### Task 1: 重写 `config/roles.yaml` domain 提示词（自执行优先）

**Files:**
- Modify: `config/roles.yaml`（头注释 L3-5；`domain_agent.system_prompt` L126-224；`fixed_roles` 头注释 L257-262）
- Test: `test/role_config_sanity_test.go`（既有健全性测试，不改代码）

**Interfaces:**
- Consumes: 无（首个任务）。
- Produces: 新的 `domain_agent.system_prompt` 全文——Task 2 的 `registry.go` 兜底 prompt 必须与其语义对齐。

- [ ] **Step 1: 改头注释（L3-5）**

将：

```
# 架构：MetaAgent 运行单一 ReAct 主循环（LLM -> 工具 -> 结果 -> 循环），
# 通过 call_sub_agent 工具按 role_id 异步派发子 Agent（固定角色或领域负责人），
# 子 Agent 完成后结果摘要经 Mailbox 回灌父 Agent。
```

改为：

```
# 架构：两层编排。MetaAgent 运行单一 ReAct 主循环（LLM -> 工具 -> 结果 -> 循环），
# 通过 call_sub_agent 工具按 role_id 异步派发 DomainAgent（各领域直接执行者，默认自执行）；
# 叶子助手（fixed_roles）保留为可选下拆层，是否启用由 DomainAgent 自主判断。
# 子 Agent 完成后结果摘要经 Mailbox 回灌父 Agent。
```

- [ ] **Step 2: 整体替换 `domain_agent.system_prompt` 块（L126-224）**

将 L126 `system_prompt: |` 起到 L224（【升级处置】段末）的整个块替换为：

```yaml
  system_prompt: |
    你是领域负责人（DomainAgent），把父 Agent 交办的目标在你负责的领域内落地。
    你是本领域的直接执行者：读文件/写代码/跑命令默认全部由你一人完成；
    只有当你判断拆分确实更省时，才用 call_sub_agent 把子块派给叶子助手。

    【工作模式】
    1. 第一动作读注入的规格：任务体【任务规范】前缀 + 共享记忆【共享记忆】前缀。
       规格齐全（接口签名/文件清单/验收标准）时直接动笔；仅缺关键信息时
       SearchInFiles 单次定位补充。【项目概览】已附各源码文件行数与顶层符号，
       可直接引用。
    2. 自执行为主：两层编排下你就是执行者，不是中转层——写文件、改代码、
       跑验证命令、看图迭代，默认全由你直接完成，不再默认下拆叶子。
    3. 拆分例外（你自主判断，标准见【拆分决策】）：确需下拆时用 call_sub_agents
       一次性并行派齐，子任务拆到单函数级（单函数/单文件/单个具体改动），
       边界清晰、可独立验收。
    4. 建设期验证 = 每个 js 文件一条 `node --check` 语法自检，不做其他验证。
       任务环境已提供验收脚本时：建设期不读、不跑、不改它，联调验收由下游整品验收负责。
    5. UI/canvas/游戏绘制类改动必须看图迭代：改完经 tool_catalog 挂载 ui_preview，browser_navigate
       打开 file:///workspace/<产物相对工作目录的路径，如 index.html> 页面，browser_take_screenshot
       截图回显（截图不传 filename 才回图），看图迭代绘制效果。
       ui_preview 跑在容器里，一律用 file:///workspace/<相对工作目录> 路径。
       需要图片素材时优先 od_image_generate 出图：正式素材用 save_as 参数直落 spec 钉死的
       静态资源路径（如 assets/img/mon-zombie.png），一图一份；save_as 缺省时落
       .bma/od-artifacts（时间戳名，草稿/临时用途）。
    6. 联网调研由 MetaAgent 负责，调研结论应由 spec/共享记忆提供。HTTPGet 仅限 spec
       明确给出的精确 URL（官方文档/公开 API/内网服务）。调研信息缺失时在结论中列明
       上报，由 MetaAgent 调研补充后返工。

    【侦察纪律（硬约束）】
    - 侦察（读文件/搜代码确认契约）在总预算里是小头，产出（写文件）才是大头。
      侦察上限：默认墙钟 30 分钟（过半会收到系统预警邮件），到点必须停止侦察。
    - 规格里已给的签名/常量/行号视为已验证事实，禁止为它再读文件。
    - 读到关键结论（消费点 API、签名、常量、朝向/布局类数值）立即 WriteSharedMemory
      沉淀——侦察中途失败后重跑靠它跳过重读，不沉淀=侦察白干。
    - 常量级未知（如贴图朝向偏移角度）：spec 钉不了就按最常见约定先实现并在结论标注
      待验证，不写脚本现场测量（实测有 Agent 为一个角度写像素测量脚本烧 15 分钟）。

    【集成验证任务模式】（任务是"验证/联调/修复"一批已产出文件时专用）
    - 第一步（先跑后读）：单条 RunCommand 跑 task 指定的验收脚本（cwd=产物根目录），拿客观失败清单。
      task 给了验收脚本就直接使用它；未给时先 `node --check` 全部 js，
      需运行验证时只写 1 个综合脚本全程复用。
    - 第二步（按需精读）：只 ReadFile 失败点涉及的文件区段，对照共享契约核对集成点
      （DOM id / 全局符号 / 构造签名 / 页面自举暴露实例 / 数据格式 / script 顺序 / 语义单位），
      一次性列出全部不一致后再动笔。
    - 第三步（批量修复）：同一轮修完全部问题；每个修复只重写报错指向的文件。
      级联失败（多个 FAIL 同因）先修根因再复跑；数值/配置类 FAIL 对照契约一次改到位。
      存在失败项时你的响应里必须含修复动作：探索后凭已有信息直接修，复跑会告诉你对错。
    - 第四步（一次复跑）：单条 RunCommand 重跑验收脚本，输出 通过/未通过 + 运行输出证据。
    - 总 LLM 轮次上限 10 轮；墙钟超 ~15 分钟未收敛也必须按当前状态报告未通过项 + 证据。

    【收尾验收（硬约束）】
    - 你对"本领域产出的任务整体性"负责：不只要逐文件语法通过，还要对照
      任务验收标准与共享契约的集成点确认整品可用。
    - 验收动作只允许一条组合命令完成：node --check 每个产出文件（一条命令串行列出）；
      集成点对照靠你已读入的契约（有下拆时再加上叶子回传摘要）。
    - 验收发现问题：一次 WriteFile/EditFile 自修全部问题 + 一条复查命令；
      确有必要才派返工叶子（带失败证据）。修复/返工合计最多 2 轮，验证阶段
      LLM 总轮次上限 5 轮，到限按当前状态上报并在结论中显式声明未通过项。

    【项目结构共享 file_tree】
    接到任务先检查 shared memory 是否有 file_tree slot：
    - 有：直接消费，按树用 SearchInFiles 拿具体行段。
    - 无（你是首个探索者）：ListDir 扫项目结构 + 必要时 ReadFile 抽签名，写入 file_tree 供兄弟复用。
      树格式（紧凑文本，每文件一行：路径 导出符号 大小 关键行段）：
        src/a.js  ExportedClass/factory 19KB L1-400
        src/b.js  HelperFunc 5KB L10-80
    files 字段只列只读参照文件；列出将被改写的文件会在 WriteFile 后整条记忆失效。

    【下拆时的任务准备】（仅当你按【拆分决策】决定下拆时适用）
    - task 只写该文件负责的部分 + 签名引用 + 验收（叶子自动读共享记忆，契约全文不必转贴）。
    - 并行叶子共享同一代码区（公共辅助函数/基类/共享常量）时：先用 WriteSharedMemory 把该区间
      代码原文写进契约再派发，约束写明"共享区代码已在契约提供，只读指派行号范围"。
    - 派固定助手前用 SearchInFiles 抽目标签名 + 行段写入 task：签名齐全叶子才能直接动笔。
    - 规格走 WriteSharedMemory 注入，task 只写目标+验收+路径，接口/签名确认在派发前由你完成。

    【拆分决策（自主判断，默认不拆）】
    - 默认整条任务由你自执行，不论涉及几个文件——下拆有冷启动/回传成本，
      你的预算与上下文就是为完整交付准备的。
    - 同时满足以下条件才可下拆：子块之间无依赖可并行；单块规模足以摊薄派发开销
      （如整文件 >= 300 行的独立生成块）；你判断并行收益明显大于下拆成本。
    - 纯数据/配置文件、小改动、修复/验证类一律自执行，禁止下拆。
    - 一旦决定下拆：call_sub_agents 一次给齐全部叶子任务，不逐个追加；
      其余部分仍由你自执行。

    【写完语法检查】
    - WriteFile 写代码文件（.js/.go/.py/.ts 等）后立即用 RunCommand 跑语法检查：
      JS/TS `node -c <file>`；Go `go build ./<pkg>`；Python `python -m py_compile <file>`。
      发现 syntax error 先修再继续，不带病往下走。
    - 写/改文件只用 WriteFile/EditFile 工具（小改 <20% 文件优先 EditFile 精确替换，输出 token 与耗时远小于整文件重写；新建/大改才 WriteFile 整写），不用 shell 重定向写文件（Windows PowerShell 默认 GBK 编码会损坏中文）。

    【结果汇总】
    - 自执行的部分直接写入结论；有下拆时，子 Agent 完成后你会收到
      [mailbox from <id>] 结果摘要，按拆分顺序整合进最终结论；同一子任务不重复派发。
    - 最终答复 = 回灌父 Agent 的交付物：结论先行、自包含、附关键文件路径与验收证据。

    【升级处置】（仅在有下拆时适用）
    - 收到带 [升级] 前缀的 mailbox 消息 = 子 Agent 无法自治，三选一处置：
      1. 重派：带前序失败原因与已探索成果重派同类子 Agent（打捞摘要自动以【前序探索摘要】注入，勿重复探索）；
      2. 自己接手：评估后直接完成剩余部分；
      3. 回报用户：无法完成时如实说明卡点与建议。
    - 子 Agent 失败消息以 [failure kind=...] 开头为机读标记，其后才是人读文案；整合结论时忽略标记本身。
```

注意：YAML 块标量缩进必须保持 `system_prompt: |` 下每行 4 空格缩进。

- [ ] **Step 3: 改 `fixed_roles` 头注释（L257-262）**

将：

```
# 固定助手角色（配置文件预定义，长期存在，可被 call_sub_agent 派发）
# 这些角色是叶子执行者：拥有 ReadFile/WriteFile/RunCommand 等执行类工具，
# 但不再拥有 call_sub_agent（不能派发子任务，避免越位）。
# 产出质量走分层自检：叶子自检（写完即语法检查）-> 领域 Agent 整体性验收 -> MetaAgent 整品验收+返工，
```

改为：

```
# 固定助手角色（配置文件预定义，长期存在，可被 call_sub_agent 派发）
# 两层编排下的可选下拆层：叶子执行者拥有 ReadFile/WriteFile/RunCommand 等执行类工具，
# 但不再拥有 call_sub_agent（不能派发子任务，避免越位）。
# 是否启用叶子由 DomainAgent 按【拆分决策】自主判断，不再是默认路径。
# 产出质量走分层自检：叶子自检（写完即语法检查）-> 领域 Agent 收尾验收 -> MetaAgent 整品验收+返工，
```

- [ ] **Step 4: 跑 roles.yaml 健全性测试**

Run: `cd test && GOTOOLCHAIN=local go test -run TestRoleConfigParsesWithAnchors -v .`
Expected: PASS（YAML 语法/锚点解析正常；该测试不依赖 prompt 内容，主要防 YAML 块标量改坏）

- [ ] **Step 5: Commit**

```bash
git add config/roles.yaml
git commit -m "任务83: roles.yaml domain 提示词改自执行优先，头注释同步两层编排"
```

---

### Task 2: 同步 `registry.go` 兜底 prompt + 锁定"domain 保留下拆能力"测试

**Files:**
- Modify: `backend/internal/domain/role/registry.go:17-59`（注释 + `defaultDomainAgentSystemPrompt` 常量）
- Test: `backend/internal/domain/role/registry_test.go`（追加一个测试）

**Interfaces:**
- Consumes: Task 1 的新 prompt 语义（自执行为主、【拆分决策】例外下拆）。
- Produces: `defaultDomainAgentSystemPrompt` 常量（`registry.go:221` 空配置兜底唯一消费点，签名不变）。

- [ ] **Step 1: 先写失败测试（锁定两层编排的不变量：domain 保留 call_sub_agent 与 domain→leaf 权限）**

在 `backend/internal/domain/role/registry_test.go` 末尾追加：

```go
// TestRegistry_DomainRetainsLeafDispatch 锁定两层编排不变量：
// domain 默认自执行是提示词纪律，机制上仍保留 call_sub_agent 与 domain->fixed 调用权，
// 叶子助手作为可选下拆层存在（2026-08-22 任务 83）。
func TestRegistry_DomainRetainsLeafDispatch(t *testing.T) {
	cfg := &config.RoleConfigFile{
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Name: "代码助手", Type: enums.RoleTypeFixed, CanBeCalled: true},
		},
	}
	r := NewRegistry(cfg)

	d := r.Get("domain")
	if d == nil {
		t.Fatal("domain 角色定义不应为 nil")
	}
	found := false
	for _, name := range d.Tools {
		if name == "call_sub_agent" {
			found = true
		}
	}
	if !found {
		t.Fatal("domain 工具白名单应保留 call_sub_agent（下拆为例外保留能力，不删机制）")
	}
	if !r.CanCall("domain", "code_assistant") {
		t.Fatal("CanCall(domain, code_assistant) 应为 true（叶子保留为可选下拆层）")
	}
}
```

- [ ] **Step 2: 跑测试确认通过（该测试锁定的是现状，应直接绿；若红说明机制被误改，先停下来核对）**

Run: `cd backend && GOTOOLCHAIN=local go test ./internal/domain/role/ -run TestRegistry_DomainRetainsLeafDispatch -v`
Expected: PASS

- [ ] **Step 3: 替换兜底 prompt 常量（registry.go L17-59）**

将 L17-59 的注释块与 `defaultDomainAgentSystemPrompt` 常量整体替换为：

```go
// defaultDomainAgentSystemPrompt 是 DomainAgent 系统提示词的兜底默认值。
// roles.yaml 未配置 domain_agent.system_prompt 时使用，保证空配置可启动。
// 内容与 config/roles.yaml 中 domain_agent.system_prompt 保持语义一致（兜底为精简版）；
// 任一处修改需同步另一处，避免行为漂移。
// 2026-08-22 任务 83：两层编排——domain 默认自执行，下拆叶子为例外自主决策。
const defaultDomainAgentSystemPrompt = `你是领域负责人（DomainAgent），把父 Agent 交办的目标在你负责的领域内落地。你是本领域的直接执行者：读文件/写代码/跑命令默认全部由你一人完成；只有当你判断拆分确实更省时，才用 call_sub_agent 下拆叶子助手。

【可用工具】
- WriteFile / EditFile / RunCommand：你的主力工具——默认由你直接完成改动与语法检查。
- ReadFile / ListDir / SearchInFiles：读文件、列目录、符号检索，用于采集上下文、定位关键代码。
- WriteSharedMemory(content)：把采集到的关键上下文写入共享记忆，被派发的子 Agent 自动读取。
- call_sub_agent(role_id, task)：例外通道——仅当你判断子块可并行且规模值得时才下拆。
- HTTPGet：联网获取文档/API/参考资料（仅限 spec 明确给出的精确 URL）。

【工作模式】
1. 第一动作读注入的规格与共享记忆；规格齐全直接动笔，仅缺关键信息时 SearchInFiles 单次定位补充。
2. 自执行为主：两层编排下你就是执行者，不是中转层——写文件/跑命令/看图迭代默认全由你完成。
3. 拆分例外（自主判断，默认不拆）：仅当多个互不依赖的大块可并行生成、且并行收益明显大于
   下拆成本时，才 call_sub_agents 一次派齐；能自己写完的不要派。纯数据/配置文件、小改动、
   修复/验证类一律自执行。子任务拆到单函数级、自包含、带验收标准。
4. 写完代码文件立即 RunCommand 语法检查（node -c / go build / py_compile），不带病往下走。

【结果汇总】
- 自执行部分直接写入结论；有下拆时按 [mailbox from <id>] 摘要整合。
- 你的最终答复就是回灌给父 Agent 的交付物：结论先行、自包含、附关键文件路径与验收证据；不要写过程流水账。`
```

- [ ] **Step 4: 跑 role 包全部测试**

Run: `cd backend && GOTOOLCHAIN=local go test ./internal/domain/role/ -v -count=1`
Expected: PASS（含既有 `TestRegistry_CanCall_PeerPermissions` 等全部保持绿）

- [ ] **Step 5: Commit**

```bash
git add backend/internal/domain/role/registry.go backend/internal/domain/role/registry_test.go
git commit -m "任务83: registry 兜底 prompt 同步自执行优先，新增两层编排不变量测试"
```

---

### Task 3: 更新 `call_sub_agent` 工具描述与 role_id 字段描述

**Files:**
- Modify: `backend/internal/domain/subagent/dispatcher.go:1128`（entries 首行）、`:1140-1143`（【路由规则】段）
- Modify: `backend/internal/domain/tool/builtin.go:146`（`role_id` 字段 description）
- Test: `backend/internal/domain/subagent/`、`backend/internal/domain/tool/` 既有测试

**Interfaces:**
- Consumes: Task 1 的语义（domain=直接执行者）。
- Produces: 无新符号；只改 LLM 可见文案，Go 接口不变。

- [ ] **Step 1: 改 dispatcher.go L1128 entries 首行**

将：

```go
	entries := []string{"domain（默认派发入口：复杂任务/不确定范围走这里，由 DomainAgent 读文件/联网/拆到单函数级再派助手或自执行）"}
```

改为：

```go
	entries := []string{"domain（默认派发入口：复杂任务/不确定范围走这里，DomainAgent 是该领域的直接执行者，收到后默认自执行，仅其自主判断需要时才下拆叶子助手）"}
```

- [ ] **Step 2: 改 dispatcher.go L1140-1143 【路由规则】段**

将：

```go
		"【路由规则】\n" +
		"1. 默认走 domain：多文件/多函数/多步骤/不确定范围 -> role_id=\"domain\"，由 DomainAgent 拆分后再派助手。\n" +
		"2. 直派固定助手：仅当任务已单函数级、单文件、领域明确（如\"修改 X 函数签名\"、\"补一个测试\"）时直派对应助手。\n" +
		"3. 不确定走哪条？走 domain。domain 可自执行单点改动，不会无谓下拆。\n\n" +
```

改为：

```go
		"【路由规则】\n" +
		"1. 默认走 domain：多文件/多函数/多步骤/不确定范围 -> role_id=\"domain\"。DomainAgent 是该领域的直接执行者，收到后默认自执行，仅其自主判断需要时才下拆叶子。\n" +
		"2. 直派固定助手：仅当任务已单函数级、单文件、领域明确（如\"修改 X 函数签名\"、\"补一个测试\"）时直派对应助手。\n" +
		"3. 不确定走哪条？走 domain。\n" +
		"4. 若你本身就是 DomainAgent：你不能派 domain（会被拒绝）。默认自执行，仅按你提示词中的【拆分决策】必要时直派固定助手。\n\n" +
```

- [ ] **Step 3: 改 builtin.go L146 role_id 字段描述**

将：

```go
		RoleID string `json:"role_id" description:"被调用子 Agent 的角色标识，从工具描述的角色清单中选。默认走 domain，仅单函数级、单文件、领域明确的任务直派固定助手。"`
```

改为：

```go
		RoleID string `json:"role_id" description:"被调用子 Agent 的角色标识，从工具描述的角色清单中选。默认走 domain（其收到后默认自执行），仅单函数级、单文件、领域明确的任务直派固定助手。"`
```

- [ ] **Step 4: 跑两个包的测试**

Run: `cd backend && GOTOOLCHAIN=local go build ./... && GOTOOLCHAIN=local go test ./internal/domain/subagent/ ./internal/domain/tool/ -count=1`
Expected: build 通过；两包测试 PASS（无测试断言上述文案，全绿即无回归）

- [ ] **Step 5: Commit**

```bash
git add backend/internal/domain/subagent/dispatcher.go backend/internal/domain/tool/builtin.go
git commit -m "任务83: call_sub_agent 工具描述改两层语义（domain=直接执行者）"
```

---

### Task 4: 文档同步（编排对比 / 项目说明 / 变更记录）

**Files:**
- Modify: `doc/编排对比_阿里AICP军团.md`（§1.1 标题与表格 L13-19；§1.2 L33；§后段 L135）
- Modify: `doc/项目说明.md`（L122、L175、L243、L467）
- Modify: `doc/变更.md`（文件顶部 L6 `---` 之后插入新日期段）

**Interfaces:**
- Consumes: Task 1-3 已完成的代码/配置改动。
- Produces: `doc/变更.md` 任务 83 条目（执行结果行留待 Task 5 验证后回填）。

- [ ] **Step 1: 编排对比 §1.1（L13-19）表格改两层**

将 L13-19：

```
### 1.1 三层角色（由 LLM 决定深度，非固定阶段）

| 层 | 角色 | 职责 | Token 预算 |
|---|---|---|---|
| 顶层 | MetaAgent | session 级，接用户目标，派 DomainAgent，整合终答 | 0（不限） |
| 中层 | DomainAgent | 领域隔离，读文件/联网采上下文，拆到单函数级再派叶子或自执行 | 50K（触限暂停可恢复） |
| 叶子 | 固定助手（code/ui/test/doc/prompt_reviewer） | 终端执行者，单函数/单文件改动 | 20K（触限部分回灌） |
```

改为（顺带修正预算漂移——当前 `config/config.yaml` `token_budget_per_role` 实际全为 150000）：

```
### 1.1 两层角色 + 可选叶子（2026-08-22 任务 83 拍平：domain 默认自执行）

| 层 | 角色 | 职责 | Token 预算 |
|---|---|---|---|
| 顶层 | MetaAgent | session 级，接用户目标，派 DomainAgent，整合终答 | 150K |
| 执行层 | DomainAgent | 领域直接执行者：读文件/写代码/跑验证默认自执行，仅自主判断需要时才下拆叶子 | 150K（触限暂停可恢复） |
| 可选叶子 | 固定助手（code/ui/test/doc/prompt_reviewer） | 可选下拆执行者，单函数/单文件改动；不再是默认路径 | 150K（触限部分回灌） |
```

- [ ] **Step 2: 编排对比 §1.2 L33 修正白名单描述**

将：

```
- **工具白名单按角色过滤**：domain 只见 `call_sub_agent`，叶子只见执行类工具（dispatcher.go:895）。
```

改为：

```
- **工具白名单按角色过滤**：meta 无执行类工具，domain 见全套执行+派发工具（`call_sub_agent` 保留为例外下拆通道），叶子只见执行类工具（dispatcher.go:895）。
```

- [ ] **Step 3: 编排对比 L135 修正路由描述**

将（L135 行内片段）：

```
DomainAgent 可自执行单点改动而不无谓下拆（dispatcher.go:628 路由规则），避免固定层的空转。
```

改为：

```
DomainAgent 默认自执行、仅自主判断需要时才下拆叶子（2026-08-22 任务 83 起两层编排，路由规则见 dispatcher.go Description），避免固定层的空转。
```

- [ ] **Step 4: 项目说明.md 四处描述**

L122 将：

```
- **层级即调用栈**：MetaAgent 调 `call_sub_agent(domain_agent, ...)`，DomainAgent 再调 `call_sub_agent(code_assistant, ...)`；Go 调用栈本身就是层级关系，无需 `CallStack`/`SessionBlock`/`NextAction` 状态机。
```

改为：

```
- **层级即调用栈**：两层编排——MetaAgent 调 `call_sub_agent(domain, ...)`，DomainAgent 默认自执行落地，仅自主判断需要时才再调 `call_sub_agent(code_assistant, ...)` 下拆叶子；Go 调用栈本身就是层级关系，无需 `CallStack`/`SessionBlock`/`NextAction` 状态机。
```

L175 将行内片段：

```
DomainAgent 开放完整读/写/执行/HTTP/共享内存权限（承担上下文采集 + 任务拆分 + 派发执行）；
```

改为：

```
DomainAgent 开放完整读/写/执行/HTTP/共享内存权限（领域内直接执行者：默认自执行，下拆叶子为例外自主决策）；
```

L243 将行内片段：

```
当前 DomainAgent 走原生工具权限方案（直接在 ReAct 循环内做上下文采集 + 拆分 + 派发）。
```

改为：

```
当前 DomainAgent 走原生工具权限方案（直接在 ReAct 循环内做上下文采集 + 自执行，下拆叶子为例外自主决策）。
```

L467 将行内片段：

```
DomainAgent 开放完整读/写/执行/HTTP/共享内存权限，承担上下文采集 + 任务拆分 + 派发执行。
```

改为：

```
DomainAgent 开放完整读/写/执行/HTTP/共享内存权限，是领域内直接执行者：默认自执行，下拆叶子为例外自主决策。
```

- [ ] **Step 5: 变更.md 顶部插入任务 83 条目**

在 L6 `---` 之后、`## 2026-08-21` 之前插入：

```markdown

## 2026-08-22

### 任务 83：编排拍平为两层（meta + domain 直接执行，叶子降为可选下拆）

- **人物**：Kimi（罗坤）
- **背景/决策**：用户决策——三层编排（meta→domain→leaf）拍平为两层。meta 职责不变（派发+汇总+整品验收）；domain 从"派发优先的中间编排者"变为各领域直接执行人，默认自执行；叶子助手（fixed_roles）保留，是否下拆由 domain 按【拆分决策】自主判断。原"domain 默认下拆"本就是提示词纪律而非硬机制，故本次只翻转纪律、不删机制。
- **改动**：
  1. `config/roles.yaml`：`domain_agent.system_prompt` 重写（自执行为主；【拆分决策（自主判断，默认不拆）】取代【自执行阈值】的强制下拆；【收尾验收】取代【整体性验收与返工】；【派发任务准备】改【下拆时的任务准备】）；头注释与 fixed_roles 头注释同步两层语义。
  2. `backend/internal/domain/role/registry.go`：`defaultDomainAgentSystemPrompt` 兜底同步；`registry_test.go` 新增 `TestRegistry_DomainRetainsLeafDispatch` 锁定不变量（domain 白名单保留 call_sub_agent、CanCall(domain→fixed)=true）。
  3. `backend/internal/domain/subagent/dispatcher.go` + `backend/internal/domain/tool/builtin.go`：`call_sub_agent` 工具描述【路由规则】改为"domain=直接执行者"，补 DomainAgent 视角规则（不能派 domain）。
  4. 文档：`doc/编排对比_阿里AICP军团.md` §1.1 改两层表格（顺带修正预算漂移：原写 50K/20K，实际 config.yaml 全部 150K）、§1.2 白名单描述；`doc/项目说明.md` 四处 domain 职责描述。
- **不动的机制**：`CanCall` 权限矩阵、domain 工具白名单（保留 `call_sub_agent(s)`）、domain 暂停/续跑（errPaused/ResumePaused）、热驻留池（idle_pool）、失败打捞、黑板摄取、fixed_roles 六个角色定义——全部保留。下拆能力不删，只是不再是默认。
- **执行结果**：（Task 5 验证后回填）
```

- [ ] **Step 6: Commit**

```bash
git add doc/编排对比_阿里AICP军团.md doc/项目说明.md doc/变更.md
git commit -m "任务83: 文档同步两层编排（编排对比/项目说明/变更记录）"
```

---

### Task 5: 全量回归验证 + 回填变更记录

**Files:**
- Modify: `doc/变更.md`（任务 83 条目的"执行结果"行）

**Interfaces:**
- Consumes: Task 1-4 全部改动。
- Produces: 无新符号。

- [ ] **Step 1: backend 全量构建 + 全量测试**

Run: `cd backend && GOTOOLCHAIN=local go build ./... && GOTOOLCHAIN=local go test ./... -count=1`
Expected: build 通过；全部 PASS。重点包：`internal/domain/role`、`internal/domain/subagent`、`internal/domain/tool`、`internal/agent`。

- [ ] **Step 2: test 模块回归**

Run: `cd test && GOTOOLCHAIN=local go test -run TestRoleConfigParsesWithAnchors -v .`
Expected: PASS

注：`test/api`、`test/tui` 等 e2e 需要 docker fixtures（PG 55432/Redis 56380），如本机 fixtures 在跑可执行 `cd test && GOTOOLCHAIN=local go test ./api/ ./tui/`，不在跑则跳过并在变更记录注明未跑。

- [ ] **Step 3: eval dry-run（不烧 API 额度）**

Run: `cd test && EVAL_DRY_RUN=1 GOTOOLCHAIN=local go test -tags=eval ./eval/ -run TestEval -v`
Expected: PASS（dry-run 只校验场景 YAML 与检查点装配）

- [ ] **Step 4: 回填变更.md 执行结果**

将任务 83 条目的：

```
- **执行结果**：（Task 5 验证后回填）
```

改为（按实际结果填写，以下为预期模板）：

```
- **执行结果**：✅ 完成。backend 全量 `go test ./...` 绿；`TestRoleConfigParsesWithAnchors` 绿；eval dry-run 绿。（e2e 是否跑过如实注明）
  - **运维动作（需用户执行）**：重启 TUI/服务生效（prompt 与工具描述为启动时加载）。
  - 观察点：domain 是否不再默认下拆叶子（看 session 日志 call_sub_agent 由 domain 发起的频率应显著下降）；domain 单 Agent 时长与 token 消耗上升属预期（自执行吸收了原叶子工作量）；【拆分决策】例外下拆是否偶发有效。
```

- [ ] **Step 5: Commit**

```bash
git add doc/变更.md
git commit -m "任务83: 回填执行结果（全量回归绿）"
```

---

## Self-Review 记录

- **Spec 覆盖**：meta 不变（无任务触碰 meta_agent 段/白名单）；domain 变直接执行人（Task 1/2/3）；叶子保留+domain 自主判断（Task 1【拆分决策】+ Task 2 不变量测试锁机制）；机制不删（Global Constraints + Task 2 测试）。
- **Placeholder 扫描**：所有改动均给出完整替换文本；无 TBD/TODO。
- **类型一致性**：无新增/改名符号；`defaultDomainAgentSystemPrompt`、`Description()`、`role_id` description 均为原处内容替换。
- **已知遗留（有意不做）**：`token_budget_per_role` domain 档维持 150K 不调高——domain 自执行增多后若触限，既有 errPaused 暂停/续跑机制兜底，先观察再调；`config/skills.yaml` 头注释"按 domain 筛 Skill 子集下发"属弱关联遗产注释，不影响行为，不在本次范围。
