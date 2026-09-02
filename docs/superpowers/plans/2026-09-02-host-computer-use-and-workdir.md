# 宿主机直控与可选工作目录 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 实现设计文档 `docs/superpowers/specs/2026-09-02-host-computer-use-and-workdir-design.md`:S1 安装目录 BMA_HOME(TUI 任意目录启动)、S3 真机 GUI 插件、S2 Web 每会话工作目录。

**Architecture:** S1 新增 `config.HomeDir()` 三级解析(env > exe 上级 > cwd),两个入口共用;S3 纯 plugins.yaml 配置(stdio 直起 @zavora-ai/computer-use-mcp);S2 经 `tool.WithWorkDir(ctx)` 把每会话工作目录沿 context 传入工具执行器与 `.bma/` 存储,替代进程级单例。

**Tech Stack:** Go(backend,stdlib testing,无 testify)、Vue 3 + Element Plus(web/)、PowerShell(install.ps1)、GNU Make。

## Global Constraints

- 测试一律标准库 `testing` + `t.TempDir()`,**禁止引入 testify**(仓库现状无此依赖)。
- backend 测试命令:`cd backend && GOTOOLCHAIN=local go test ./... -count=1`(即 `make backend-test`);单包:`cd backend && GOTOOLCHAIN=local go test ./internal/config -run TestXxx -v -count=1`。
- 平台:先只支持 Windows;路径处理一律 `filepath`,禁硬编码 `/` 分隔。
- 安全现状保持全信任(`tool_approval_disabled: true`),本计划不新增审批链;命令黑名单与写路径沙箱逻辑本身不动。
- Docker 插件(ui_design/ui_preview/沙箱 computer_use)的 `${WORKDIR}` 挂载仍为进程启动目录,不随会话变化——这是已确认的限制,不在本计划解决。
- 每完成一个 Task 按步骤 commit;commit message 用中文、带前缀(`feat:`/`fix:`/`docs:`/`chore:`),参照仓库历史风格。
- 工作目录语义:TUI = 启动目录(cwd)不变;Web = 每会话 `work_dir`,空值回落进程启动目录(向后兼容)。
- `web_dist` 解析方式变更后,删除 `backend/main.go:203-219` 的 `resolveWebDistPath`(被 home 解析取代)。

---

## Part 1(S1):安装目录 BMA_HOME + TUI 任意目录启动

### Task 1: `config.HomeDir()` 解析器

**Files:**
- Create: `backend/internal/config/home.go`
- Test: `backend/internal/config/home_test.go`

**Interfaces:**
- Produces:
  - `func HomeDir() (string, error)` — 生产入口,内部调 `resolveHome`。
  - `func looksLikeHome(dir string) bool` — 判断 `dir/config/config.yaml` 是否存在。
  - 解析顺序:`BMA_HOME` env(须含 `config/config.yaml`,否则报错)> exe 上级(exe 在 `bin\` 下再上一级;开发态 exe 在 `backend\` 下兼容)> cwd(须含 `config/config.yaml`)。

- [ ] **Step 1: 写失败测试**

```go
package config

import (
	"os"
	"path/filepath"
	"testing"
)

// 造一个"像 home"的目录(含 config/config.yaml)
func mkHome(t *testing.T, base, name string) string {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config", "config.yaml"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestResolveHome_EnvWins(t *testing.T) {
	base := t.TempDir()
	home := mkHome(t, base, "envhome")
	got, err := resolveHome(home, filepath.Join(base, "nowhere", "bin", "tui.exe"), base)
	if err != nil || got != home {
		t.Fatalf("got %q err %v, want %q", got, err, home)
	}
}

func TestResolveHome_EnvInvalid(t *testing.T) {
	base := t.TempDir() // 不含 config/config.yaml
	if _, err := resolveHome(base, "", ""); err == nil {
		t.Fatal("want error for BMA_HOME without config/config.yaml")
	}
}

func TestResolveHome_ExeBinParent(t *testing.T) {
	base := t.TempDir()
	home := mkHome(t, base, "bma") // exe 在 <home>/bin/tui.exe
	got, err := resolveHome("", filepath.Join(home, "bin", "tui.exe"), base)
	if err != nil || got != home {
		t.Fatalf("got %q err %v, want %q", got, err, home)
	}
}

func TestResolveHome_ExeBackendDevLayout(t *testing.T) {
	base := t.TempDir()
	home := mkHome(t, base, "repo") // 开发态:exe 在 <repo>/backend/tui.exe
	got, err := resolveHome("", filepath.Join(home, "backend", "tui.exe"), filepath.Join(base, "other"))
	if err != nil || got != home {
		t.Fatalf("got %q err %v, want %q", got, err, home)
	}
}

func TestResolveHome_CwdFallback(t *testing.T) {
	base := t.TempDir()
	home := mkHome(t, base, "cwdhome")
	got, err := resolveHome("", filepath.Join(base, "random", "x.exe"), home)
	if err != nil || got != home {
		t.Fatalf("got %q err %v, want %q", got, err, home)
	}
}

func TestResolveHome_NotFound(t *testing.T) {
	base := t.TempDir()
	if _, err := resolveHome("", filepath.Join(base, "a", "x.exe"), base); err == nil {
		t.Fatal("want error when nothing looks like home")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd backend && GOTOOLCHAIN=local go test ./internal/config -run TestResolveHome -v -count=1`
Expected: FAIL(编译错误:`resolveHome` 未定义)

- [ ] **Step 3: 实现 `home.go`**

```go
package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// looksLikeHome 判断 dir 是否为 BMA 安装目录(含 config/config.yaml)。
func looksLikeHome(dir string) bool {
	if dir == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, "config", "config.yaml"))
	return err == nil && !info.IsDir()
}

// resolveHome 是 HomeDir 的纯函数核心,便于测试。
// 顺序:BMA_HOME env > exe 上级(bin 再上一级;兼容开发态 backend/)> cwd。
func resolveHome(env, exePath, cwd string) (string, error) {
	if env != "" {
		if looksLikeHome(env) {
			return filepath.Clean(env), nil
		}
		return "", fmt.Errorf("BMA_HOME=%s 无效:缺少 config/config.yaml", env)
	}
	if exePath != "" {
		dir := filepath.Dir(exePath)
		cands := []string{dir, filepath.Dir(dir)} // exe 同级、上级(bin/ 或 backend/ 布局)
		if filepath.Base(dir) == "bin" {
			cands = []string{filepath.Dir(dir), dir}
		}
		for _, c := range cands {
			if looksLikeHome(c) {
				return filepath.Clean(c), nil
			}
		}
	}
	if looksLikeHome(cwd) {
		return filepath.Clean(cwd), nil
	}
	return "", fmt.Errorf("无法定位安装目录:请设置 BMA_HOME 环境变量,或用 -config 等 flag 显式指定")
}

// HomeDir 解析 BMA 安装目录(BMA_HOME)。
func HomeDir() (string, error) {
	exe := ""
	if p, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			p = real
		}
		exe = p
	}
	cwd, _ := os.Getwd()
	return resolveHome(os.Getenv("BMA_HOME"), exe, cwd)
}

// ResolveUnderHome 把相对路径拼到 home 下;绝对路径原样返回。
func ResolveUnderHome(home, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(home, p)
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `cd backend && GOTOOLCHAIN=local go test ./internal/config -run TestResolveHome -v -count=1`
Expected: PASS(6 个测试全过)

- [ ] **Step 5: Commit**

```bash
git add backend/internal/config/home.go backend/internal/config/home_test.go
git commit -m "feat: 新增 config.HomeDir() 安装目录三级解析(BMA_HOME>exe上级>cwd)"
```

---

### Task 2: TUI 入口接入 home 解析

**Files:**
- Modify: `backend/cmd/tui/main.go`(flag 区 48-54、logging 区 94-110)

**Interfaces:**
- Consumes: `config.HomeDir()`、`config.ResolveUnderHome(home, p)`(Task 1)。
- Produces: TUI 从任意目录启动时,5 个配置 flag 未显式指定即落到 `<home>/...`;`logging.dir` 为相对路径时落到 `<home>/<dir>`。行为约定:flag 显式指定优先于 home。

- [ ] **Step 1: 改 flag 默认解析(`backend/cmd/tui/main.go`)**

在 `flag.Parse()`(54 行)之后、文件校验循环(62 行)之前插入:

```go
	// ---- 安装目录解析:未显式指定的配置路径落到 BMA_HOME 下 ----
	home, homeErr := config.HomeDir()
	explicit := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	if homeErr == nil {
		if !explicit["config"] {
			*configPath = filepath.Join(home, "config", "config.yaml")
		}
		if !explicit["roles"] {
			*rolePath = filepath.Join(home, "config", "roles.yaml")
		}
		if !explicit["env"] {
			*envPath = filepath.Join(home, ".env")
		}
		if !explicit["soul"] {
			*soulPath = filepath.Join(home, "config", "soul.md")
		}
		if !explicit["skills"] {
			*skillPath = filepath.Join(home, "config", "skills.yaml")
		}
	}
	// home 解析失败不致命:保留 cwd 相对默认值,由下方文件校验报错提示。
```

import 块(8-29 行)补 `"path/filepath"`。

- [ ] **Step 2: logging 目录落到 home(94-110 行区)**

把:

```go
	logDir := cfg.Logging.Dir
	if !cfg.Logging.Enabled || logDir == "" {
		logDir = "logs"
	}
```

改为:

```go
	logDir := cfg.Logging.Dir
	if !cfg.Logging.Enabled || logDir == "" {
		logDir = "logs"
	}
	if homeErr == nil {
		logDir = config.ResolveUnderHome(home, logDir)
	}
```

- [ ] **Step 3: 编译**

Run: `cd backend && GOTOOLCHAIN=local go build ./cmd/tui`
Expected: 编译通过

- [ ] **Step 4: 手工冒烟**

```bash
cd backend && GOTOOLCHAIN=local go build -o tui.exe ./cmd/tui
mkdir -p /d/tmp/bma-smoke && cd /d/tmp/bma-smoke
/d/data/project/BlockMemoryAgent/backend/tui.exe   # 不带任何 flag
```

Expected: TUI 正常启动(exe 上级=仓库根含 config/,命中开发态分支),不再报 "config file not found"。`Ctrl+C` 退出。

- [ ] **Step 5: Commit**

```bash
git add backend/cmd/tui/main.go
git commit -m "feat: TUI 配置路径经 BMA_HOME 解析,支持任意目录启动"
```

---

### Task 3: server 入口接入 home 解析

**Files:**
- Modify: `backend/main.go`(flag 区 46-53、logging 区 91-109、web-dist 149/203-219)

**Interfaces:**
- Consumes: `config.HomeDir()`、`config.ResolveUnderHome`(Task 1)。
- Produces: server 的 7 个路径 flag 全部支持 home 解析;`resolveWebDistPath` 删除。

- [ ] **Step 1: flag 区插入 home 解析(`flag.Parse()` 之后)**

与 Task 2 同型,覆盖 7 个 flag:

```go
	// ---- 安装目录解析:未显式指定的路径落到 BMA_HOME 下 ----
	home, homeErr := config.HomeDir()
	explicit := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	if homeErr == nil {
		if !explicit["config"] {
			*configPath = filepath.Join(home, "config", "config.yaml")
		}
		if !explicit["roles"] {
			*rolePath = filepath.Join(home, "config", "roles.yaml")
		}
		if !explicit["env"] {
			*envPath = filepath.Join(home, ".env")
		}
		if !explicit["soul"] {
			*soulPath = filepath.Join(home, "config", "soul.md")
		}
		if !explicit["skills"] {
			*skillPath = filepath.Join(home, "config", "skills.yaml")
		}
		if !explicit["profile"] {
			*profilePath = filepath.Join(home, "config", "user_profile.md")
		}
		if !explicit["web-dist"] {
			*webDistPath = filepath.Join(home, "web", "dist")
		}
	}
```

- [ ] **Step 2: logging 目录落到 home(91-109 行区)**

在 `logging.Init(logging.EntryBackend, cfg.Logging.Dir, false)`(95 行)调用前,把传入目录替换为:

```go
	logDir := cfg.Logging.Dir
	if homeErr == nil {
		logDir = config.ResolveUnderHome(home, logDir)
	}
```

(即 `logging.Init(logging.EntryBackend, logDir, false)`,注意保持 `cfg.Logging.Enabled` 的现有判断结构不变。)

- [ ] **Step 3: web-dist 改用 home,删除旧函数**

149 行 `webDist := resolveWebDistPath(*webDistPath)` 改为 `webDist := *webDistPath`(已在 Step 1 完成 home 拼接);删除文件尾部 203-219 的 `resolveWebDistPath` 函数及其 `"os"`/`"path/filepath"` 中仅它使用的 import(`os` 别处仍用则保留,编译器会提示)。

- [ ] **Step 4: 编译 + 回归**

Run: `cd backend && GOTOOLCHAIN=local go build . && GOTOOLCHAIN=local go test ./internal/config ./internal/server -count=1`
Expected: 编译通过,测试 PASS

- [ ] **Step 5: Commit**

```bash
git add backend/main.go
git commit -m "feat: server 入口配置/web-dist/logs 路径经 BMA_HOME 解析"
```

---

### Task 4: `make dist` + install.ps1 + 文档

**Files:**
- Modify: `Makefile`(新增 `dist` target)
- Create: `install.ps1`(仓库根)
- Modify: `doc/项目说明.md`(637-645 启动方式段落,改为安装目录用法)

**Interfaces:**
- Produces: `make dist` 产出 `dist/{bin,config,web/dist,plugins.d}` 安装布局;`install.ps1` 交互式安装并写 `BMA_HOME` 用户环境变量。

- [ ] **Step 1: Makefile 新增 dist target**

```makefile
dist: web-build
	cd backend && GOTOOLCHAIN=local go build -o ../dist/bin/bma-server.exe . && GOTOOLCHAIN=local go build -o ../dist/bin/tui.exe ./cmd/tui
	if not exist dist\\config mkdir dist\\config
	cp config/*.yaml config/soul.md config/user_profile.md dist/config/
	cp -r web/dist dist/web/dist
	mkdir -p dist/plugins.d
	@echo "dist/ 布局完成,运行 install.ps1 安装"
```

(若 `cp config/*.yaml` 对不含 yaml 的文件报错,逐个列:`config/config.yaml config/plugins.yaml config/roles.yaml config/skills.yaml`。)

- [ ] **Step 2: 写 install.ps1**

```powershell
# BMA 安装脚本:复制 dist/ 到安装目录并写入 BMA_HOME 用户环境变量。幂等,可重复运行(升级)。
$ErrorActionPreference = "Stop"
$default = "D:\data\bma"
$target = Read-Host "安装目录 [$default]"
if ([string]::IsNullOrWhiteSpace($target)) { $target = $default }
$src = Join-Path $PSScriptRoot "dist"
if (-not (Test-Path (Join-Path $src "bin\tui.exe"))) { throw "未找到 dist\bin\tui.exe,请先运行 make dist" }

New-Item -ItemType Directory -Force -Path $target, "$target\bin", "$target\config", "$target\plugins.d", "$target\logs" | Out-Null
# 二进制与前端资源:覆盖(升级)
Copy-Item -Force "$src\bin\*" "$target\bin\"
if (Test-Path "$src\web\dist") { New-Item -ItemType Directory -Force -Path "$target\web" | Out-Null; Copy-Item -Force -Recurse "$src\web\dist" "$target\web\" }
# 配置与 .env:已存在则不覆盖
foreach ($f in Get-ChildItem "$src\config") {
    $dst = Join-Path "$target\config" $f.Name
    if (-not (Test-Path $dst)) { Copy-Item $f.FullName $dst }
}
if (-not (Test-Path "$target\.env") -and (Test-Path "$PSScriptRoot\.env.example")) { Copy-Item "$PSScriptRoot\.env.example" "$target\.env" }

[Environment]::SetEnvironmentVariable("BMA_HOME", $target, "User")
# node 检测(host_computer_use 插件前置)
if (-not (Get-Command node -ErrorAction SilentlyContinue)) { Write-Warning "未检测到 node:host_computer_use 插件需要 Node 20+(https://nodejs.org)" }
Write-Host "安装完成: $target  (BMA_HOME 已写入用户环境变量,重开终端生效)"
```

- [ ] **Step 3: 更新 `doc/项目说明.md` 启动段落**

把 637-645 的"全量绝对路径 flag"示例替换为:先 `make dist` + `powershell -File install.ps1`,之后任意目录直接 `tui.exe`;开发态仍可在仓库根 `go run ./backend` 或 `backend/tui.exe`(自动命中 exe 上级探测)。

- [ ] **Step 4: 手工验证**

Run: `make dist` → 确认 `dist/bin/tui.exe`、`dist/bin/bma-server.exe`、`dist/config/`、`dist/web/dist/` 都在。
(可选)在测试目录跑一遍 `install.ps1` 安装到临时目录,设 `BMA_HOME` 后从任意目录启动 tui.exe。

- [ ] **Step 5: Commit**

```bash
git add Makefile install.ps1 doc/项目说明.md
git commit -m "feat: make dist 安装布局 + install.ps1(BMA_HOME 环境变量)"
```

---

## Part 2(S3):真机 GUI 控制插件

### Task 5: plugins.yaml 新增 host_computer_use

**Files:**
- Modify: `config/plugins.yaml`(computer_use 段 81-117 之后新增插件;computer_use 改 `enabled: false`)

**Interfaces:**
- Produces: 新插件 `host_computer_use`(kind: mcp, transport: stdio);沙箱 `computer_use` 默认关闭保留。

- [ ] **Step 1: 改 `config/plugins.yaml`**

`computer_use` 段:`enabled: true` 改为 `enabled: false`,并在其注释尾部追加一行:`# 2026-09-02:沙箱版默认关闭,真机直控改用下方 host_computer_use(stdio 宿主机直起);需要隔离环境时改回 true。`

在 `computer_use` 段之后(`open_design` 段之前)插入:

```yaml
  # Host Computer Use:真机 GUI 直控(2026-09-02,替代沙箱 computer_use 的默认形态)。
  # stdio 在宿主机直起 @zavora-ai/computer-use-mcp(与沙箱镜像内同一包,工具面一致):
  # 截图/鼠标/键盘/剪贴板/窗口管理/UIA 直接作用于真机桌面(Windows,SendInput/UIA)。
  # 前置:宿主机 Node 20+(npx 首次运行自动拉包)。
  # destructive: true 全工具进审批守卫链(当前全信任模式 tool_approval_disabled: true 不拦截)。
  # 与沙箱 computer_use 不要同时启用:工具面重叠,模型易混淆。
  # 已知缺口:沙箱版自加的 screen_record 录屏依赖 X11,宿主机版暂不提供。
  host_computer_use:
    kind: mcp
    enabled: true
    settings:
      transport: stdio
      command: npx
      args: ["-y", "@zavora-ai/computer-use-mcp"]
      destructive: true
      roles: ["meta"]
      # 截图经 image content 回传多模态模型(与 ui_preview 同机制)。
      image_passthrough: true
      # 真机操作一律在会话工作目录语义之外(作用于桌面),工具描述尾部明示风险。
      tool_description_suffix: "注意:本工具直接操作宿主机真实桌面(非沙箱):截图为真机屏幕,鼠标键盘事件作用于真机前台窗口。文件落盘仍用内置 WriteFile/EditFile(会话工作目录),不要用本工具的 filesystem 能力落盘任务产物。"
```

- [ ] **Step 2: 配置健全性测试回归**

Run: `GOTOOLCHAIN=local go test ./test/... -run TestRoleConfig -count=1` 和 `cd backend && GOTOOLCHAIN=local go test ./internal/plugins/... -count=1`
Expected: PASS(plugins.yaml 仍可解析;若仓库有 plugins.yaml 解析测试且断言 enabled 插件清单,按其断言更新)

- [ ] **Step 3: 手工冒烟(需宿主机 Node 20+)**

启动 server,`POST /api/plugins/host_computer_use/enable`,让 meta 角色调 `screenshot` 工具:确认真机截图以 image content 回传;再对记事本做一次 click/type 验证键鼠。完毕 `POST /api/plugins/host_computer_use/disable` 按需保留。
Expected: 截图回传成功;记事本出现输入字符。

- [ ] **Step 4: Commit**

```bash
git add config/plugins.yaml
git commit -m "feat: 新增 host_computer_use 真机 GUI 插件(stdio),沙箱 computer_use 默认关闭"
```

---

## Part 3(S2):Web 每会话工作目录

### Task 6: tool 包 ctx 携带 workDir

**Files:**
- Modify: `backend/internal/domain/tool/sandbox.go`(ctx 键区 272-310;`isPathAllowed` 121-152、`sanitizeWritePath` 175 起、`enforceRoleWritePath` 192-219、`resolvePathWithSandbox` 223-236、`sessionTempDir` 263-270、`resolvePath` 314-321)
- Modify: `backend/internal/domain/tool/executor.go`(`readWorkDir` 161-170)
- Modify: `backend/internal/domain/tool/builtin.go`、`spec.go`(全部调用点改传 ctx;`runCommand` 837 行 `cmd.Dir`)
- Test: `backend/internal/domain/tool/workdir_ctx_test.go`

**Interfaces:**
- Produces(后续 Task 全部依赖):
  - `func WithWorkDir(ctx context.Context, dir string) context.Context`(空 dir 原样返回)
  - `func WorkDirFromContext(ctx context.Context) string`
  - `func (e *Executor) workDirOf(ctx context.Context) string` — ctx 值 > `e.workDir` > `os.Getwd()`
  - Executor 各路径方法的 ctx 变体:`resolvePath(ctx, p)`、`resolvePathWithSandbox(ctx, p)`、`isPathAllowed(ctx, abs)`、`sessionTempDir(ctx, sid)`、`sanitizeWritePath(ctx, ...)`、`enforceRoleWritePath(ctx, role, p)`。

- [ ] **Step 1: 写失败测试 `workdir_ctx_test.go`**

```go
package tool

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestWorkDirOf_PrefersContext(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	e := NewExecutor(dirA)
	if got := e.workDirOf(context.Background()); got != dirA {
		t.Fatalf("no ctx: got %q want %q", got, dirA)
	}
	ctx := WithWorkDir(context.Background(), dirB)
	if got := e.workDirOf(ctx); got != dirB {
		t.Fatalf("ctx: got %q want %q", got, dirB)
	}
}

func TestResolvePathWithSandbox_PerSessionDir(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	e := NewExecutor(dirA)
	ctx := WithWorkDir(context.Background(), dirB)
	// 相对路径落到 ctx 目录
	abs, err := e.resolvePathWithSandbox(ctx, "sub/f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dirB, "sub", "f.txt"); abs != want {
		t.Fatalf("got %q want %q", abs, want)
	}
	// 默认目录(dirA)外的路径在 dirB 语境下同样受限;dirA 内的文件反而越界
	if _, err := e.resolvePathWithSandbox(ctx, filepath.Join(dirA, "x.txt")); err == nil {
		t.Fatal("want escape error for dirA path under dirB ctx")
	}
}

func TestSessionTempDir_PerSession(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	e := NewExecutor(dirA)
	ctx := WithWorkDir(context.Background(), dirB)
	got := e.sessionTempDir(ctx, "sess-1")
	if want := filepath.Join(dirB, ".bma", "tmp", "sess-1"); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestWriteFileLandsInCtxWorkDir(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	r := NewBuiltinRegistry(dirA, nil, nil)
	ctx := WithWorkDir(context.Background(), dirB)
	res, err := r.Dispatch(ctx, "WriteFile", map[string]any{"path": "hello.txt", "content": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if res != nil && !res.OK {
		t.Fatalf("WriteFile failed: %s", res.Error)
	}
	if _, err := os.Stat(filepath.Join(dirB, "hello.txt")); err != nil {
		t.Fatalf("file not in dirB: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dirA, "hello.txt")); err == nil {
		t.Fatal("file unexpectedly in dirA")
	}
}
```

(注:`Dispatch`/`Result.OK`/`Result.Error` 字段名以 `registry.go:430`、`registry.go:46-53` 现状为准,若实为 `res.Err` 之类按现状调整。)

- [ ] **Step 2: 跑测试确认失败**

Run: `cd backend && GOTOOLCHAIN=local go test ./internal/domain/tool -run 'TestWorkDirOf|TestResolvePathWithSandbox_PerSessionDir|TestSessionTempDir_PerSession|TestWriteFileLandsInCtxWorkDir' -v -count=1`
Expected: FAIL(编译错误)

- [ ] **Step 3: 实现 ctx 通道**

`sandbox.go` ctx 键区(272-310,`sessionIDKey`/`roleIDKey` 旁)新增:

```go
// workDirKey 是会话级工作目录的 context 键(每会话独立工作目录,S2)。
type workDirKey struct{}

// WithWorkDir 把会话工作目录注入 ctx;空 dir 原样返回(回落 Executor 默认目录)。
func WithWorkDir(ctx context.Context, dir string) context.Context {
	if dir == "" {
		return ctx
	}
	return context.WithValue(ctx, workDirKey{}, dir)
}

// WorkDirFromContext 取出会话工作目录,未注入返回空串。
func WorkDirFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(workDirKey{}).(string); ok {
		return v
	}
	return ""
}
```

`executor.go` `readWorkDir`(161-170)旁新增:

```go
// workDirOf 按会话解析工作目录:ctx 注入值 > e.workDir > 进程 cwd。
func (e *Executor) workDirOf(ctx context.Context) string {
	if v := WorkDirFromContext(ctx); v != "" {
		return v
	}
	return e.readWorkDir()
}
```

随后把 Executor 各路径方法改为接收 ctx 并以 `e.workDirOf(ctx)` 替代 `e.workDir`/`e.readWorkDir()`:

- `resolvePath(path)` → `resolvePath(ctx, path)`(314-321,`filepath.Join(e.workDirOf(ctx), path)`);
- `resolvePathWithSandbox(path)` → `(ctx, path)`(223-236,错误信息里的 `filepath.Clean(e.workDir)` 同步改 `e.workDirOf(ctx)`);
- `isPathAllowed(absPath)` → `(ctx, absPath)`(121-152,workDirClean 来自 ctx);
- `sessionTempDir(sessionID)` → `(ctx, sessionID)`(263-270);
- `sanitizeWritePath`/`enforceRoleWritePath`(175/192-219)同型加 ctx 首参;
- `builtin.go` 全部调用点(含 `runCommand` 837 行 `cmd.Dir = e.workDirOf(ctx)`、841 行 `e.sessionTempDir(ctx, sessionID)`、450/465 快照、792-805 mkdir)与 `spec.go` 调用点改传 ctx。工具 `Execute(ctx, args)` 本就有 ctx,纯透传。

- [ ] **Step 4: 全量 tool 包测试**

Run: `cd backend && GOTOOLCHAIN=local go test ./internal/domain/tool -count=1`
Expected: 新测试与既有 17 个测试文件全部 PASS(无 ctx 注入时行为与改前一致)

- [ ] **Step 5: Commit**

```bash
git add backend/internal/domain/tool/
git commit -m "feat: 工具执行器支持 ctx 携带每会话工作目录(WithWorkDir/workDirOf)"
```

---

### Task 7: 会话模型与服务层 workDir

**Files:**
- Modify: `backend/internal/agent/types.go`(`CreateRequest` 126-134、`Session` 255-277 各加 `WorkDir`)
- Modify: `backend/internal/agent/session_react.go`(`reactInternalSession` 34-97 加 `workDir`;`createSession` 219-265)
- Modify: `backend/internal/agent/service_react.go`(`CreateSession` 743-762;`runSession` 1784-1811;`resumeSession` 1896-1909;`toReactAgentSession` 2752-2793)
- Test: `backend/internal/agent/session_workdir_test.go`

**Interfaces:**
- Consumes: `tool.WithWorkDir`(Task 6)。
- Produces:
  - `agent.CreateRequest.WorkDir string`、`agent.Session.WorkDir string`(json `work_dir` 由 Task 10 的 server DTO 决定);
  - `reactSessionStore.createSession(goal, workDir string)`(签名变更);
  - 会话结构 `workDir` 字段(小写,内部);`toReactAgentSession` 输出含 `WorkDir`。

- [ ] **Step 1: 写失败测试**

```go
package agent

import (
	"context"
	"path/filepath"
	"testing"
)

func TestCreateSession_PerSessionWorkDir(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	s := newReactServiceForTest(nil, dirA) // provider 参数按 service_react.go:2796 现状签名
	sess, err := s.CreateSession(context.Background(), CreateRequest{Goal: "g", WorkDir: dirB})
	if err != nil {
		t.Fatal(err)
	}
	if sess.WorkDir != dirB {
		t.Fatalf("WorkDir got %q want %q", sess.WorkDir, dirB)
	}
	wantTemp := filepath.Join(dirB, ".bma", "tmp", sess.ID)
	if sess.TempDir != wantTemp {
		t.Fatalf("TempDir got %q want %q", sess.TempDir, wantTemp)
	}
	// 空 WorkDir 回落默认
	sess2, err := s.CreateSession(context.Background(), CreateRequest{Goal: "g2"})
	if err != nil {
		t.Fatal(err)
	}
	if sess2.WorkDir != "" {
		t.Fatalf("empty WorkDir should stay empty, got %q", sess2.WorkDir)
	}
	wantTemp2 := filepath.Join(dirA, ".bma", "tmp", sess2.ID)
	if sess2.TempDir != wantTemp2 {
		t.Fatalf("fallback TempDir got %q want %q", sess2.TempDir, wantTemp2)
	}
	s.Shutdown(context.Background())
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd backend && GOTOOLCHAIN=local go test ./internal/agent -run TestCreateSession_PerSessionWorkDir -v -count=1`
Expected: FAIL(`CreateRequest` 无 `WorkDir` 字段)

- [ ] **Step 3: 实现**

- `types.go`:`CreateRequest` 加 `WorkDir string // 每会话工作目录(绝对路径),空=进程默认`;`Session` 加 `WorkDir string \`json:"work_dir,omitempty"\``。
- `session_react.go` `reactInternalSession` 加 `workDir string` 字段;`createSession(goal string)` 改 `createSession(goal, workDir string)`:233 行 TempDir 改为基于有效目录——

```go
	eff := workDir
	if eff == "" {
		eff = st.workDir
	}
	// TempDir: filepath.Join(eff, ".bma", "tmp", sessionID)
```

同函数内 `EnsureProjectDoc(ctx, st.workDir, ...)`(259)改 `eff`;`st.sharedMemoryReset()`(253-255)改 `st.sharedMemoryReset(eff)`(字段类型改 `func(string)`,接线点 grep `sharedMemoryReset` 全量跟随,默认实现内部拼 `<dir>/.bma/shared`)。
- `service_react.go` `CreateSession`(743):`s.store.createSession(goal)` 改 `s.store.createSession(goal, req.WorkDir)`。
- `runSession`(1797 附近)/`resumeSession`(1909 附近)在 `runCtx := tool.WithSessionID(ctx, session.ID)` 之后各加一行:

```go
	runCtx = tool.WithWorkDir(runCtx, session.workDir)
```

- `toReactAgentSession`(2752-2793)逐字段拷贝区加 `WorkDir: s.workDir,`。
- 子 Agent 派发侧(`subagent/dispatcher.go:2062/3371/3441`、`idle_pool.go:583`):这些点都是在既有 run ctx 上 `WithSessionID`,context 值天然继承 workDirKey,**原则上零改动**;实现时逐处确认其 ctx 派生自 runSession 的 runCtx(若某处用 `context.Background()` 另起,则在该处补 `ctx = tool.WithWorkDir(ctx, <session>.workDir)`,session 经 store 查)。

- [ ] **Step 4: 测试 + 全量 agent 包回归**

Run: `cd backend && GOTOOLCHAIN=local go test ./internal/agent -count=1`
Expected: 新测试 PASS,既有测试无回归(`createSession` 签名变更波及的所有编译点一并修)

- [ ] **Step 5: Commit**

```bash
git add backend/internal/agent/ backend/internal/subagent/
git commit -m "feat: 会话模型携带每会话工作目录,ReAct 运行 ctx 注入 WithWorkDir"
```

---

### Task 8: 持久化 work_dir 列

**Files:**
- Create: `migrations/008_session_work_dir.sql`
- Modify: `backend/internal/store/schema.go`(`EnsureSessionHistorySchema` 40-58)
- Modify: `backend/internal/store/session_store.go`(`SessionHistoryRecord` 13-20;`SaveHistory` 55-80;`RecentHistories` 156-195;`GetHistoryByID` 204-230)
- Modify: `backend/internal/agent/session_react.go`(`persistHistory` 513-556;`restoreSessions` 662-751,重建段 696-709)
- Test: `backend/internal/store/session_store_test.go`(若已存在则加用例)

**Interfaces:**
- Consumes: Task 7 的 `reactInternalSession.workDir`。
- Produces: `SessionHistoryRecord.WorkDir string`;`EnsureSessionHistorySchema` 幂等加列;restore 重建会话带 `workDir`。

- [ ] **Step 1: 迁移文件**

```sql
-- 008: session_history 增加每会话工作目录(S2 Web 每会话工作目录)
ALTER TABLE session_history ADD COLUMN IF NOT EXISTS work_dir TEXT NOT NULL DEFAULT '';
```

- [ ] **Step 2: schema.go 同步**

`EnsureSessionHistorySchema`(40-58)中现有 `ALTER TABLE ... ADD COLUMN IF NOT EXISTS meta_memory` 语句旁,同型追加:

```go
	`ALTER TABLE session_history ADD COLUMN IF NOT EXISTS work_dir TEXT NOT NULL DEFAULT ''`,
```

- [ ] **Step 3: 写失败测试**

在 `session_store_test.go` 加(若该文件不存在,新建并参照同包已有测试的 Postgres 测试的跳过约定;无 DB 的单元环境用 `t.Skip`  Guard,集成验证交给 `test/` 模块):

```go
func TestSessionHistoryRecord_WorkDirRoundTrip(t *testing.T) {
	// 无 DB 环境跳过;schema/列清单正确性由集成测试覆盖。
}
```

注:本 Task 的正确性主要由 ①编译(列清单与 Scan 对齐)②`test/api` 集成测试保障;单测只加编译期可见的列清单一致性检查即可。**Step 4 实现后以 `go build ./...` + 全量单测为闸门。**

- [ ] **Step 4: 实现**

- `SessionHistoryRecord` 加 `WorkDir string`;
- `SaveHistory` INSERT 列清单加 `work_dir`,args 加 `rec.WorkDir`(74-78);
- `RecentHistories`(162-167)/`GetHistoryByID` SELECT 列清单加 `work_dir`,Scan 目标加 `&rec.WorkDir`;
- `persistHistory`(513-556)构造 record 处加 `WorkDir: sess.workDir,`;
- `restoreSessions` 重建 `reactInternalSession{...}`(696-709)加 `workDir: rec.WorkDir,`。

- [ ] **Step 5: 编译 + 测试**

Run: `cd backend && GOTOOLCHAIN=local go build ./... && GOTOOLCHAIN=local go test ./internal/store ./internal/agent -count=1`
Expected: 编译通过,测试 PASS

- [ ] **Step 6: Commit**

```bash
git add migrations/008_session_work_dir.sql backend/internal/store/ backend/internal/agent/session_react.go
git commit -m "feat: session_history 持久化每会话 work_dir(迁移008+恢复带回)"
```

---

### Task 9: `.bma/` 存储跟随会话目录

**Files:**
- Modify: `backend/internal/domain/tool/shared_memory_file_store.go`(root 拼接 31;`WorkDir()` 142-143)
- Modify: `backend/internal/domain/tool/spec.go`(baseline 拼接 448)
- Modify: `backend/internal/userprofile/`(ProjectStore,注入点 `bootstrap.go:518`)
- Test: `backend/internal/domain/tool/shared_memory_workdir_test.go`

**Interfaces:**
- Consumes: `WorkDirFromContext`(Task 6)。
- Produces:
  - `FileSharedMemoryStore.root(ctx context.Context) string`(ctx 目录 > 构造目录);
  - `FileSharedMemoryStore.WorkDirOf(ctx context.Context) string`(供 spec baseline);
  - ProjectStore 按 ctx 解析 `project_preferences.md` 路径。

- [ ] **Step 1: 写失败测试**

```go
package tool

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSharedMemoryRoot_PerSession(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	s := NewFileSharedMemoryStore(dirA)
	ctx := WithWorkDir(context.Background(), dirB)
	if got, want := s.root(ctx), filepath.Join(dirB, ".bma", "shared"); got != want {
		t.Fatalf("ctx root got %q want %q", got, want)
	}
	if got, want := s.root(context.Background()), filepath.Join(dirA, ".bma", "shared"); got != want {
		t.Fatalf("default root got %q want %q", got, want)
	}
	if got := s.WorkDirOf(ctx); got != dirB {
		t.Fatalf("WorkDirOf got %q want %q", got, dirB)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd backend && GOTOOLCHAIN=local go test ./internal/domain/tool -run TestSharedMemoryRoot_PerSession -v -count=1`
Expected: FAIL(`root`/`WorkDirOf` 未定义)

- [ ] **Step 3: 实现**

`shared_memory_file_store.go`:31 行的 root 拼接收进方法:

```go
// root 按会话解析共享记忆根目录:ctx 携带的会话目录优先,空回退构造目录。
func (s *FileSharedMemoryStore) root(ctx context.Context) string {
	if wd := WorkDirFromContext(ctx); wd != "" {
		return filepath.Join(wd, ".bma", "shared")
	}
	return filepath.Join(s.workDir, ".bma", "shared")
}

// WorkDirOf 返回 ctx 生效的工作目录(供 WriteSpec baseline 落盘)。
func (s *FileSharedMemoryStore) WorkDirOf(ctx context.Context) string {
	if wd := WorkDirFromContext(ctx); wd != "" {
		return wd
	}
	return s.workDir
}
```

(结构体字段名以现状为准,若现状是直接拼在构造函数里、没有保存 workDir,则新增 `workDir` 字段保存构造参数。)该类型全部读写方法改为经 `root(ctx)` 取根(方法本就有 ctx 的透传;没有的加 ctx 首参并跟随全部调用点)。`spec.go:448` 的 `filepath.Join(workDir, ".bma", "baseline")` 改用 `sharedMemoryStore.WorkDirOf(ctx)`(workDir 上溯链 `registry.go:418-420` 同步换 `WorkDirOf`)。

`userprofile` ProjectStore:`bootstrap.go:518` 目前把完整文件路径钉死在构造期。改为构造传 workDir 根、读写在方法内经 `WorkDirFromContext` 解析:

```go
// pathFor 解析项目偏好文件路径:ctx 会话目录优先。
func (s *ProjectStore) pathFor(ctx context.Context) string {
	wd := s.workDir
	if v := tool.WorkDirFromContext(ctx); v != "" {
		wd = v
	}
	return filepath.Join(wd, ".bma", "project_preferences.md")
}
```

(`NewProjectStore(filepath.Join(workDir, ".bma", "project_preferences.md"))` 改 `NewProjectStore(workDir)`;方法签名与调用点跟随。)

- [ ] **Step 4: 测试**

Run: `cd backend && GOTOOLCHAIN=local go test ./internal/domain/tool ./internal/userprofile -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add backend/internal/domain/tool/ backend/internal/userprofile/ backend/internal/bootstrap/bootstrap.go
git commit -m "feat: 共享记忆/基线/项目偏好按会话工作目录解析 .bma 根"
```

---

### Task 10: HTTP 层 work_dir + 目录浏览 API

**Files:**
- Modify: `backend/internal/server/session_http.go`(`HandleCreateSession` 15-52)
- Modify: `backend/internal/server/session.go`(`Session` DTO 23-44;`ToServerSession` 326-341)
- Modify: `backend/internal/server/api.go`(新增 `BrowseFSHandler`,参照 `FilesHandler` 431-465)
- Modify: `backend/internal/bootstrap/router.go`(files 路由 64-65 旁注册)
- Test: `backend/internal/server/session_workdir_http_test.go`

**Interfaces:**
- Consumes: `agent.CreateRequest.WorkDir`、`agent.Session.WorkDir`(Task 7)。
- Produces:
  - `POST /api/sessions` 接受 `work_dir`(可选,绝对/相对均转绝对;不存在或非目录 → 400);
  - `GET /api/fs/browse?path=` → `{"path": "...", "parent": "...", "dirs": [{"name","path"}]}`;`path` 为空:Windows 返回盘符列表(`[{"name":"C:\\","path":"C:\\"}]`,`parent` 为空串),其他 OS 返回 `/`;
  - server `Session` DTO 加 `work_dir`(json)。

- [ ] **Step 1: 写失败测试**

```go
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestBrowseFS_ReturnsDirsOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	mkDir := func(p string) {
		if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mkDir("a/b")
	os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644)

	h := &APIHandler{} // 依赖按 api.go 现状构造,若需 agent 等依赖传 nil
	r := gin.New()
	r.GET("/api/fs/browse", h.BrowseFSHandler)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/fs/browse?path="+root, nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Path string `json:"path"`
		Dirs []struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"dirs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Dirs) != 1 || resp.Dirs[0].Name != "a" {
		t.Fatalf("dirs %+v, want only [a]", resp.Dirs)
	}
}

func TestCreateSession_WorkDirValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// SessionManager 构造参照 server 包既有测试(auth_test.go 风格)
	m := &SessionManager{agent: /* 现成 fake/nil 按现状 */} 
	r := gin.New()
	r.POST("/api/sessions", m.HandleCreateSession)

	body := `{"goal":"g","work_dir":"` + strings.ReplaceAll(filepath.Join("Z:", "no-such-dir-xyz"), "\\", "\\\\") + `"}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", w.Code)
	}
}
```

(构造细节以 server 包现有测试为模板;`os` import 别漏。)

- [ ] **Step 2: 跑测试确认失败**

Run: `cd backend && GOTOOLCHAIN=local go test ./internal/server -run 'TestBrowseFS|TestCreateSession_WorkDirValidation' -v -count=1`
Expected: FAIL(`BrowseFSHandler` 未定义)

- [ ] **Step 3: 实现**

`session_http.go` `HandleCreateSession` 请求结构体加字段并校验:

```go
	req, err := DecodeBody[struct {
		Goal    string            `json:"goal"`
		Images  []agent.WireImage `json:"images,omitempty"`
		Videos  []agent.WireVideo `json:"videos,omitempty"`
		WorkDir string            `json:"work_dir,omitempty"`
	}](c.Request)
	// ... 既有 err/goal 校验之后:
	if req.WorkDir != "" {
		abs, err := filepath.Abs(req.WorkDir)
		if err != nil {
			c.String(http.StatusBadRequest, "work_dir 无效")
			return
		}
		info, err := os.Stat(abs)
		if err != nil || !info.IsDir() {
			c.String(http.StatusBadRequest, "work_dir 不存在或不是目录")
			return
		}
		req.WorkDir = abs
	}
	// CreateRequest 处加 WorkDir: req.WorkDir
```

`session.go` DTO 加 `WorkDir string \`json:"work_dir,omitempty"\``;`ToServerSession` 装配区(326-341)加 `WorkDir: a.WorkDir,`。

`api.go` 新增(与 `FilesHandler` 同型):

```go
// BrowseFSHandler 处理 GET /api/fs/browse?path=,只列目录(前端工作目录选择器)。
// path 为空:Windows 返回盘符列表,其他系统返回 /。
func (h *APIHandler) BrowseFSHandler(c *gin.Context) {
	type dirEntry struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	p := c.Query("path")
	if p == "" {
		dirs := []dirEntry{}
		if runtime.GOOS == "windows" {
			for _, l := range "ABCDEFGHIJKLMNOPQRSTUVWXYZ" {
				d := string(l) + `:\`
				if _, err := os.Stat(d); err == nil {
					dirs = append(dirs, dirEntry{Name: d, Path: d})
				}
			}
		} else {
			dirs = append(dirs, dirEntry{Name: "/", Path: "/"})
		}
		c.JSON(http.StatusOK, gin.H{"path": "", "parent": "", "dirs": dirs})
		return
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		c.String(http.StatusBadRequest, "路径不可读: %s", err.Error())
		return
	}
	dirs := []dirEntry{}
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, dirEntry{Name: e.Name(), Path: filepath.Join(p, e.Name())})
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name) })
	c.JSON(http.StatusOK, gin.H{"path": p, "parent": filepath.Dir(p), "dirs": dirs})
}
```

`router.go` 64-65 旁注册:`api.GET("/fs/browse", apiHandler.BrowseFSHandler)`。

- [ ] **Step 4: 测试 + server 包回归**

Run: `cd backend && GOTOOLCHAIN=local go test ./internal/server -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add backend/internal/server/ backend/internal/bootstrap/router.go
git commit -m "feat: 会话创建支持 work_dir,新增 /api/fs/browse 目录浏览"
```

---

### Task 11: 前端目录选择器

**Files:**
- Modify: `web/src/types/index.ts`(`Session` 28-39 加 `work_dir?: string`)
- Modify: `web/src/api/session.ts`(`createSession` 9-14)
- Create: `web/src/components/WorkDirPicker.vue`
- Create: `web/src/composables/useWorkDir.ts`
- Modify: `web/src/views/dashboard/index.vue`(goal 输入区 182-189、`runSession` 55-68、列表 236-299)
- Modify: `web/src/views/chat/index.vue`(`handleSubmit` 191-194)

**Interfaces:**
- Consumes: `POST /api/sessions {work_dir}`、`GET /api/fs/browse`(Task 10)。
- Produces:
  - `createSession(goal: string, images?: WireImage[], workDir?: string)`;
  - `useWorkDir()` → `{ workDir: Ref<string> }`(localStorage 键 `bma:last-workdir`,空串=默认);
  - `WorkDirPicker.vue`:props `modelValue: string`,emit `update:modelValue`;内含 el-input + "浏览"按钮 + el-dialog(经 `/fs/browse` 上下钻取目录,点选确定)。

- [ ] **Step 1: 类型与 API**

`types/index.ts` `Session` 接口加 `work_dir?: string`。

`session.ts` `createSession` 改:

```ts
export function createSession(goal: string, images?: WireImage[], workDir?: string): Promise<Session> {
  const body: Record<string, unknown> = { goal }
  if (images?.length) body.images = images
  if (workDir) body.work_dir = workDir
  return fetchJson('/sessions', { method: 'POST', body: JSON.stringify(body) })
}
```

新增 `web/src/api/fs.ts`:

```ts
import { fetchJson } from './client'

export interface BrowseResult {
  path: string
  parent: string
  dirs: { name: string; path: string }[]
}

export function browseFS(path: string): Promise<BrowseResult> {
  return fetchJson(`/fs/browse?path=${encodeURIComponent(path)}`)
}
```

- [ ] **Step 2: composable + 选择器组件**

`useWorkDir.ts`:

```ts
import { ref } from 'vue'

const KEY = 'bma:last-workdir'
const workDir = ref(localStorage.getItem(KEY) ?? '')

export function useWorkDir() {
  const set = (v: string) => {
    workDir.value = v
    localStorage.setItem(KEY, v)
  }
  return { workDir, setWorkDir: set }
}
```

`WorkDirPicker.vue`(Element Plus,与现有组件同风格):

```vue
<template>
  <div class="workdir-picker">
    <el-input :model-value="modelValue" placeholder="工作目录(留空=默认)" clearable
      @update:model-value="$emit('update:modelValue', $event)">
      <template #append><el-button @click="open = true">浏览</el-button></template>
    </el-input>
    <el-dialog v-model="open" title="选择工作目录" width="520px">
      <div class="browse-head">
        <el-button size="small" :disabled="!parent" @click="load(parent)">上级</el-button>
        <span class="cur">{{ current || '选择盘符' }}</span>
      </div>
      <el-scrollbar max-height="320px">
        <div v-for="d in dirs" :key="d.path" class="dir-item" @dblclick="load(d.path)" @click="pick = d.path"
             :class="{ active: pick === d.path }">{{ d.name }}</div>
        <el-empty v-if="!dirs.length" description="无子目录" :image-size="40" />
      </el-scrollbar>
      <template #footer>
        <el-button @click="open = false">取消</el-button>
        <el-button type="primary" :disabled="!pick && !current" @click="confirm">选定 {{ pick || current }}</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { browseFS, type BrowseResult } from '../api/fs'

const props = defineProps<{ modelValue: string }>()
const emit = defineEmits<{ 'update:modelValue': [string] }>()
const open = ref(false)
const current = ref('')
const parent = ref('')
const dirs = ref<BrowseResult['dirs']>([])
const pick = ref('')

async function load(p: string) {
  const r = await browseFS(p)
  current.value = r.path
  parent.value = r.parent
  dirs.value = r.dirs
  pick.value = ''
}
watch(open, (v) => { if (v) load(current.value || props.modelValue || '') })
function confirm() {
  emit('update:modelValue', pick.value || current.value)
  open.value = false
}
</script>

<style scoped>
.browse-head { display: flex; gap: 8px; align-items: center; margin-bottom: 8px; }
.dir-item { padding: 4px 8px; cursor: pointer; border-radius: 4px; }
.dir-item:hover { background: var(--el-fill-color-light); }
.dir-item.active { background: var(--el-color-primary-light-8); }
</style>
```

- [ ] **Step 3: 接线 dashboard 与 chat**

`dashboard/index.vue`:goal 输入区(182-189)下方加 `<WorkDirPicker v-model="workDir" />`;`runSession`(55-68)改 `createSession(goal.value.trim(), undefined, workDir.value || undefined)`;script 里 `const { workDir, setWorkDir } = useWorkDir()` 并在创建成功后 `setWorkDir(workDir.value)`;会话列表项(236-299)加一行显示 `{{ s.work_dir || '默认目录' }}`。

`chat/index.vue` `handleSubmit` 191-194 的 `createSession(content, images)` 改 `createSession(content, images, workDir.value || undefined)`,script 引入 `useWorkDir`。

- [ ] **Step 4: 构建验证**

Run: `cd web && npm run build`
Expected: 构建通过无 TS 错误

- [ ] **Step 5: 手工冒烟**

起 server + 前端:新建会话选 `D:\data` 下某目录 → 会话详情 `work_dir` 正确;让 Agent `WriteFile` 一个相对路径文件 → 文件落在所选目录;再并行建一个默认目录会话互不干扰。

- [ ] **Step 6: Commit**

```bash
git add web/src/
git commit -m "feat: 前端工作目录选择器(新建会话可选目录,记住上次选择)"
```

---

### Task 12: 全量回归与收尾

**Files:**
- Modify: `docs/superpowers/specs/2026-09-02-host-computer-use-and-workdir-design.md`(若实现与文档有偏差,回写)

- [ ] **Step 1: 全量测试**

Run: `make backend-test && make test-compile && cd web && npm run build`
Expected: backend 139+ 测试全 PASS,test 模块编译通过,前端构建通过

- [ ] **Step 2: 端到端手工清单**

- [ ] 任意目录启动 `tui.exe`(不带 flag)正常进 TUI;
- [ ] `install.ps1` 装到临时目录后,设 `BMA_HOME` 从任意目录启动;
- [ ] Web 建两个不同 `work_dir` 会话并行,各自 `WriteFile` 落盘到各自目录、互不串;
- [ ] `host_computer_use` screenshot 回传真机截图。

- [ ] **Step 3: Commit**

```bash
git add -A
git commit -m "chore: S2 收尾回归(设计文档回写如有偏差)"
```
