<#
.SYNOPSIS
BlockMemoryAgent 桌面版：**装 / 更新 / 回退** 一条脚本搞定。

.DESCRIPTION
把仓库打成"版本化 + 可回滚"的桌面应用包，并安装或更新到目标目录。

日常只有三个动作（都在这一个脚本里）：
  pwsh -File .\bma-app.ps1 install  -Target <安装目录>   # 装 / 更新（不带 -From 就地构建）
  pwsh -File .\bma-app.ps1 rollback -Target <安装目录>   # 回退到上一版
  pwsh -File .\bma-app.ps1 status   -Target <安装目录>   # 看版本 / 运行态 / 健康

分发给别人装（产出自包含安装包，目标机零外部依赖）：
  pwsh -File .\bma-app.ps1 dist                          # = 打包 + 编译 Inno 安装包
  pwsh -File .\bma-app.ps1 publish -BaseUrl <地址>        # 自动更新清单（latest.json）

安装目录布局（-Target，默认 %LOCALAPPDATA%\Programs\BlockMemoryAgent）：
  app\versions\<版本>\...    程序（bin + web\dist + 配置模板 + 启动器），不可变
  app\current.json           版本指针（含 previous，用于回滚）
  app\runtime.json           运行态（pid/版本/端口，启动器写入）
  app\launch.ps1             启动器（随包更新）
  runtime\                   自带运行时（PG+pgvector / Redis / Node / 媒体桥 / Chromium）
  docker\                    依赖编排（compose + 媒体构建上下文），程序载荷，随版本覆盖
  BMA.cmd / BMA.vbs          桌面入口（快捷方式指向 BMA.vbs）
  config\  .env  plugins.d\  workspace\  logs\    ← 用户数据：只补缺，永不覆盖

仓库 → 安装目录的数据隔离：
  * 打包只取"程序 + 配置模板 + 自带运行时"，绝不包含 .env / config\models.json /
    config\user_profile.md / config\skills_learned / workspace / logs
    （这些是仓库本地的密钥与运行时状态）；
  * 安装/更新只对缺失的用户数据文件做种子拷贝，既有文件一律不动；
  * 拒绝安装到源码仓库目录（含 .git / go.work）。

.PARAMETER Command
  install   装 / 更新到目标目录（不给 -From 就地构建；给 -From 装现成的包/目录/安装包 exe）
  update    install 的别名
  deploy    install 的别名（旧名保留）
  rollback  切回上一版本并重启
  status    打印当前版本 / 运行态 / 健康检查（-Json 输出机器可读）
  shortcuts 重建桌面 + 开始菜单快捷方式（装完发现没图标，或快捷方式被误删时用）
  dist      打包 + 编译 Inno 安装包（dist\installer\BlockMemoryAgent-Setup-<版本>.exe）
  pack      只打包（不进安装包）
  start / stop / restart   起停当前版本
  media     应用媒体生成配置（重建桥镜像 + open_design/minimax-media 容器，换 keeper）
  migrate-data  把别处的数据搬进自带实例
  publish   生成自动更新清单（latest.json）

.EXAMPLE
  pwsh -File .\bma-app.ps1 install  -Target D:\WebApp\BMA-Install
  pwsh -File .\bma-app.ps1 rollback -Target D:\WebApp\BMA-Install
  pwsh -File .\bma-app.ps1 dist
  pwsh -File .\bma-app.ps1 install -From .\dist\installer\BlockMemoryAgent-Setup-xxx.exe -Target D:\WebApp\BMA-Install
#>
param(
    [Parameter(Position = 0)]
    [ValidateSet('pack', 'install', 'update', 'deploy', 'dist', 'start', 'stop', 'restart', 'status', 'rollback', 'media', 'migrate-data', 'publish', 'shortcuts')]
    [string]$Command = 'status',

    # 安装目录（install/update/start/stop/restart/status/rollback）
    [string]$Target,

    # 包来源（install/update）：包目录或 zip；缺省取 dist\app 下最新版本
    [string]$From,

    # migrate-data：源库 DSN（把该实例的数据搬进桌面版自带实例）
    [string]$FromDsn,

    # 打包版本号（pack）：缺省 yyyyMMdd-HHmmss-<git短号>
    [string]$Version,

    # publish：安装包的公开下载地址前缀（拼出 latest.json 里的 url）
    [string]$BaseUrl,

    # publish：更新说明（写进 latest.json，桌面端弹窗里显示）
    [string]$Notes,

    # 保留的历史版本数（install/update）：默认 3（当前 + 上一版 + 1）
    [int]$Keep = 3,

    # 健康检查超时（秒）。首次装机可能要拉 pgvector/redis 镜像，故默认给到 10 分钟。
    [int]$TimeoutSec = 600,

    # 首次安装时写入 .env 的 HTTP_ADDR（并行安装/换端口用，例如 127.0.0.1:10011）
    [string]$HttpAddr,

    [switch]$SkipWeb,     # pack：跳过前端构建（复用 web\dist）
    [switch]$Test,        # pack：额外跑 go test ./...
    [switch]$NoZip,       # pack：不生成 zip
    [switch]$NoRuntime,   # pack：不带自带运行时（PG/Redis），只出程序包
    [switch]$NoFetch,     # pack：不自动下载运行时源包（缺了就报错）
    # 代理（http://127.0.0.1:7890 这类 clash 端口）。设置后 npm / playwright / go /
    # 运行时源包下载**全部**走它（子进程继承环境变量）。
    [string]$Proxy,
    # 强制指定桌面目录（shortcuts / install）：桌面被重定向到奇怪位置、或需要装到
    # 另一个账号的桌面时用。不传则自动探测所有**可写**候选。
    [string]$DesktopDir,
    [switch]$NoRestart,   # install/update：装完不重启
    [switch]$NoShortcut,  # install/update/deploy：不建快捷方式
    [switch]$NoShell,     # install/update/deploy：不重启桌面外壳（默认会重启并保留"托盘常驻"体验）
    [switch]$Json         # 只输出 JSON（供外部 Agent 解析）
)

$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.IO.Compression.FileSystem

# 随包本地 MCP 桥的 npm 版本（钉版本：升级要同时改这里的版本号与运行时配方号）。
$playwrightMcpVersion = '0.0.85'
$computerUseMcpVersion = '7.8.0'

$script:JsonOutput = [bool]$Json

function Write-Log([string]$Message) {
    if (-not $script:JsonOutput) { Write-Host $Message }
}

function Read-JsonFile([string]$Path) {
    if (-not (Test-Path $Path)) { return $null }
    $raw = [System.IO.File]::ReadAllText($Path)
    if ([string]::IsNullOrWhiteSpace($raw)) { return $null }
    return $raw | ConvertFrom-Json
}

function Write-JsonFile([string]$Path, $Object) {
    $text = $Object | ConvertTo-Json -Depth 8
    [System.IO.File]::WriteAllText($Path, $text, (New-Object System.Text.UTF8Encoding($false)))
}

function Get-EnvValue([string]$Path, [string]$Key) {
    if (-not (Test-Path $Path)) { return '' }
    foreach ($l in [System.IO.File]::ReadAllLines($Path)) {
        if ($l -match "^\s*$([regex]::Escape($Key))\s*=(.*)$") { return $Matches[1].Trim() }
    }
    return ''
}

function Set-EnvValue([string]$Path, [string]$Key, [string]$Value) {
    $utf8NoBom = New-Object System.Text.UTF8Encoding($false)
    $lines = @()
    if (Test-Path $Path) { $lines = @([System.IO.File]::ReadAllLines($Path)) }
    $hit = $false
    for ($i = 0; $i -lt $lines.Count; $i++) {
        if ($lines[$i] -match "^\s*$([regex]::Escape($Key))\s*=") { $lines[$i] = "$Key=$Value"; $hit = $true; break }
    }
    if (-not $hit) { $lines += "$Key=$Value" }
    [System.IO.File]::WriteAllLines($Path, $lines, $utf8NoBom)
}

# API Token 只补缺：既有 .env 里的 token 永远不换（升级不换锁）。
function Ensure-ApiToken([string]$EnvPath) {
    if (-not (Test-Path $EnvPath)) { return $false }
    $cur = Get-EnvValue $EnvPath 'BMA_API_TOKEN'
    # 空值或 .env.example 的占位符都视为"没设"，生成随机 token；真实 token 永不更换。
    if (-not [string]::IsNullOrWhiteSpace($cur) -and ($cur -notlike 'change-me*') -and ($cur -notlike 'your-*')) { return $false }
    $token = [guid]::NewGuid().ToString('N') + [guid]::NewGuid().ToString('N').Substring(0, 16)
    Set-EnvValue $EnvPath 'BMA_API_TOKEN' $token
    Write-Log "[install] 已生成 BMA_API_TOKEN 写入 $EnvPath"
    return $true
}

function Get-GitCommit([string]$Repo) {
    try {
        $out = & git -C $Repo rev-parse --short HEAD 2>$null
        if ($LASTEXITCODE -eq 0 -and $out) { return (($out | Out-String).Trim()) }
    } catch { }
    return 'unknown'
}

function Get-PromptVersion([string]$Repo) {
    $p = Join-Path $Repo 'backend\pkg\prompts\prompts.go'
    if (Test-Path $p) {
        $m = Select-String -Path $p -Pattern 'const Version = "([^"]+)"' | Select-Object -First 1
        if ($m) { return $m.Matches[0].Groups[1].Value }
    }
    return 'unknown'
}

function Get-BaseUrl([string]$Root) {
    $addr = Get-EnvValue (Join-Path $Root '.env') 'HTTP_ADDR'
    if ([string]::IsNullOrWhiteSpace($addr)) { $addr = '127.0.0.1:10010' }
    if ($addr.StartsWith(':')) { $addr = '127.0.0.1' + $addr }
    if ($addr -notmatch '^https?://') { $addr = 'http://' + $addr }
    return $addr
}

function Get-Health([string]$Root) {
    try { return Invoke-RestMethod -Uri ((Get-BaseUrl $Root) + '/api/health') -TimeoutSec 3 } catch { return $null }
}

# ---------------- pack ----------------

# ---------------- runtime（自带运行时载荷）----------------

# 自带运行时：PostgreSQL 免安装版 + pgvector 扩展 + Redis for Windows。
# 目标机器因此不需要装 Docker——商业桌面应用的常规做法（服务端二进制随包、app 当子进程跑）。
#
# 源文件放在 dist\runtime-cache\（不进 git）：
#   pg16.zip            EDB PostgreSQL 16 免安装包
#   pgvector-pg16.zip   pgvector 的 Windows 预编译扩展（社区包）
#   redis-win.zip       Redis for Windows（5.0.14）
# 首次需要时用 installer\fetch-runtime.ps1 下载；解压裁剪结果缓存在
# dist\runtime-cache\prepared\，重复打包直接复用。

function Get-RuntimeCache([string]$Repo) { return (Join-Path $Repo 'dist\runtime-cache') }

# Ensure-RuntimeSources 确保运行时源包（PG16 / pgvector / Redis）已就位；缺则自动下载一次。
#
# 为什么自动下载：这些 zip 是第三方再分发物，不进 git，全新机器上必然没有。
# 用户的目标是"一条命令装好"——缺了就补，而不是让人先去跑另一个脚本再回来。
# 需要离线/离线包时用 -NoFetch 跳过（此时缺源包会明确报错）。
function Ensure-RuntimeSources([string]$Repo, [switch]$NoFetch) {
    if (Test-RuntimeAvailable $Repo) { return $true }
    if ($NoFetch) { return $false }
    $fetch = Join-Path $Repo 'installer\fetch-runtime.ps1'
    if (-not (Test-Path $fetch)) { return $false }
    Write-Log '[runtime] 缺少运行时源包，自动下载（一次性，约 300MB）...'
    try {
        & pwsh -NoProfile -ExecutionPolicy Bypass -File $fetch
        if ($LASTEXITCODE -ne 0) { throw "fetch-runtime.ps1 退出码 $LASTEXITCODE" }
    } catch {
        Write-Log ('[runtime] warn: 自动下载失败：' + $_.Exception.Message)
        return $false
    }
    return (Test-RuntimeAvailable $Repo)
}

function Test-RuntimeAvailable([string]$Repo) {
    $cache = Get-RuntimeCache $Repo
    return ((Test-Path (Join-Path $cache 'pg16.zip')) -and
            (Test-Path (Join-Path $cache 'pgvector-pg16.zip')) -and
            (Test-Path (Join-Path $cache 'redis-win.zip')) -and
            # Node 也要：三个本地 MCP 桥全靠它跑，缺了等于装了个不能用的包。
            (Test-Path (Join-Path $cache 'node-win.zip')))
}

# Build-RuntimePayload 解压裁剪出 runtime\ 载荷（prepared 缓存命中则直接复制）。
function Build-RuntimePayload {
    param([string]$Repo, [string]$OutDir)

    $cache = Get-RuntimeCache $Repo
    if (-not (Test-RuntimeAvailable $Repo)) { throw "缺少运行时源包（$cache 下需要 pg16.zip / pgvector-pg16.zip / redis-win.zip），先跑 installer\fetch-runtime.ps1" }
    $prepared = Join-Path $cache 'prepared'
    # 配方版本：载荷**组成**变了就重建（否则会复用旧缓存，缺新加的 node/媒体桥）。
    $recipe = 'v6-node-media-playwright-cu-headless'
    $recipeFile = Join-Path $prepared '.recipe'
    $recipeMatch = ((Test-Path $recipeFile) -and ((Get-Content $recipeFile -Raw).Trim() -eq $recipe))
    # 配方**明确**不匹配：组成可能整体不同，清空重建（全本地源包/缓存，代价可控）。
    # 配方文件缺失（上次准备到一半就断了）：按“只补缺”处理，不白扔已下好的几百 MB。
    if ((Test-Path $recipeFile) -and -not $recipeMatch) {
        Write-Log ('[runtime] 载荷配方变了（' + ((Get-Content $recipeFile -Raw).Trim()) + ' → ' + $recipe + '），重建')
        if (Test-Path $prepared) { Remove-Item -Recurse -Force $prepared }
    }
    New-Item -ItemType Directory -Force -Path $prepared | Out-Null

    # **逐部件判定、只补缺不全铺**（2026-11-16 实战）。此前是"任一部件缺 → 删整个
    # prepared 重做"，于是"只剩桥依赖没装好"也会把已下好的 290MB Chromium/PostgreSQL
    # 全部重下——失败一次就要多等十几分钟。现在：配方一致时，哪个部件缺就只重建哪个，
    # 已经完整的部件（PG/Redis/Node/浏览器）原样保留。
    $pgOut = Join-Path $prepared 'postgres'
    $pgDone = ((Test-Path (Join-Path $pgOut 'bin\postgres.exe')) -and
               (Test-Path (Join-Path $pgOut 'share\extension\vector.control')) -and
               (Test-Path (Join-Path $pgOut 'lib\vector.dll')))
    if ($pgDone) {
        Write-Log '[runtime] 复用 PostgreSQL + pgvector'
    } else {
        # PG 与 pgvector 是一体的（扩展文件装进 PG 目录），缺任一就整体重解（本地 zip，很快）。
        if (Test-Path $pgOut) { Remove-Item -Recurse -Force $pgOut }

        Write-Log '[runtime] 解压 PostgreSQL ...'
        $pgTmp = Join-Path $cache '_pg'
        if (Test-Path $pgTmp) { Remove-Item -Recurse -Force $pgTmp }
        Expand-Archive -Path (Join-Path $cache 'pg16.zip') -DestinationPath $pgTmp -Force
        $pgRoot = Join-Path $pgTmp 'pgsql'
        if (-not (Test-Path $pgRoot)) { $pgRoot = $pgTmp }
        New-Item -ItemType Directory -Force -Path $pgOut | Out-Null
        foreach ($d in @('bin', 'lib', 'share')) {
            Copy-Item -Recurse -Force (Join-Path $pgRoot $d) (Join-Path $pgOut $d)
        }
        # 裁剪：头文件、文档、静态库、上位版本升级工具——都不影响运行，能省几百 MB。
        foreach ($junk in @('include', 'doc')) {
            $p = Join-Path $pgOut $junk
            if (Test-Path $p) { Remove-Item -Recurse -Force $p }
        }
        Get-ChildItem -Path (Join-Path $pgOut 'lib') -Filter '*.a' -File -ErrorAction SilentlyContinue | Remove-Item -Force
        foreach ($junk in @('pg_upgrade.exe', 'pgbench.exe')) {
            $p = Join-Path $pgOut ('bin\' + $junk)
            if (Test-Path $p) { Remove-Item -Force $p }
        }
        Remove-Item -Recurse -Force $pgTmp -ErrorAction SilentlyContinue

        Write-Log '[runtime] 安装 pgvector 扩展 ...'
        $vecTmp = Join-Path $cache '_vec'
        if (Test-Path $vecTmp) { Remove-Item -Recurse -Force $vecTmp }
        Expand-Archive -Path (Join-Path $cache 'pgvector-pg16.zip') -DestinationPath $vecTmp -Force
        # PostgreSQL 的约定：控制文件与 SQL 脚本在 share\extension\，动态库在 pkglibdir。
        # EDB 的 Windows 免安装版把 pkglibdir 定在 lib\（不是 Linux 的 lib/postgresql）。
        $extDir = Join-Path $pgOut 'share\extension'
        $libDir = Join-Path $pgOut 'lib'
        New-Item -ItemType Directory -Force -Path $extDir, $libDir | Out-Null
        $vecShare = Join-Path $vecTmp 'share\extension'
        if (Test-Path $vecShare) {
            Get-ChildItem -Path $vecShare -File | ForEach-Object { Copy-Item -Force $_.FullName $extDir }
        }
        $vecLib = Join-Path $vecTmp 'lib'
        if (Test-Path $vecLib) {
            Get-ChildItem -Path $vecLib -File -Filter '*.dll' | ForEach-Object { Copy-Item -Force $_.FullName $libDir }
        }
        Remove-Item -Recurse -Force $vecTmp -ErrorAction SilentlyContinue
        if (-not (Test-Path (Join-Path $extDir 'vector.control'))) {
            throw 'pgvector 扩展安装失败：payload 里没有 vector.control'
        }
        if (-not (Test-Path (Join-Path $libDir 'vector.dll'))) {
            throw 'pgvector 扩展安装失败：payload 里没有 vector.dll'
        }
    }

    $redisOut = Join-Path $prepared 'redis'
    if (Test-Path (Join-Path $redisOut 'redis-server.exe')) {
        Write-Log '[runtime] 复用 Redis'
    } else {
        Write-Log '[runtime] 解压 Redis ...'
        $redisTmp = Join-Path $cache '_redis'
        if (Test-Path $redisTmp) { Remove-Item -Recurse -Force $redisTmp }
        Expand-Archive -Path (Join-Path $cache 'redis-win.zip') -DestinationPath $redisTmp -Force
        New-Item -ItemType Directory -Force -Path $redisOut | Out-Null
        foreach ($f in @('redis-server.exe', 'redis-cli.exe')) {
            $src = Get-ChildItem -Path $redisTmp -Filter $f -Recurse -File | Select-Object -First 1
            if ($src) { Copy-Item -Force $src.FullName $redisOut }
        }
        Remove-Item -Recurse -Force $redisTmp -ErrorAction SilentlyContinue
        if (-not (Test-Path (Join-Path $redisOut 'redis-server.exe'))) { throw 'Redis 解压失败：缺 redis-server.exe' }
    }

    # Node：随包，用于以 stdio 方式跑 ui_design 桥（免 Docker 的媒体链路）
    $nodeOut = Join-Path $prepared 'node'
    $nodeZip = Join-Path $cache 'node-win.zip'
    if (Test-Path (Join-Path $nodeOut 'node.exe')) {
        Write-Log '[runtime] 复用 Node'
    } elseif (Test-Path $nodeZip) {
        Write-Log '[runtime] 解压 Node ...'
        $nodeTmp = Join-Path $cache '_node'
        if (Test-Path $nodeTmp) { Remove-Item -Recurse -Force $nodeTmp }
        Expand-Archive -Path $nodeZip -DestinationPath $nodeTmp -Force
        $nodeRoot = (Get-ChildItem $nodeTmp -Directory | Select-Object -First 1).FullName
        New-Item -ItemType Directory -Force -Path $nodeOut | Out-Null
        Copy-Item -Force (Join-Path $nodeRoot 'node.exe') $nodeOut
        Remove-Item -Recurse -Force $nodeTmp -ErrorAction SilentlyContinue
        if (-not (Test-Path (Join-Path $nodeOut 'node.exe'))) { throw 'Node 解压失败：缺 node.exe' }
    } else {
        Write-Log '[runtime] warn: 未找到 node-win.zip（桌面版将没有免 Docker 的媒体链路）'
    }

    # ui_design 桥（stdio）：源码 + 依赖（npm install 一次，之后走缓存）
    $bridgeSrc = Join-Path $Repo 'docker\open-design-mcp'
    if (Test-Path (Join-Path $bridgeSrc 'server.js')) {
        $mediaOut = Join-Path $prepared 'media\open-design-mcp'
        New-Item -ItemType Directory -Force -Path $mediaOut | Out-Null
        foreach ($f in @('server.js', 'media-core.js', 'package.json')) {
            Copy-Item -Force (Join-Path $bridgeSrc $f) $mediaOut
        }
        # 判定靠**入口依赖**而不是 server.js（见上方 preparedOk 注释）。
        if (-not (Test-Path (Join-Path $mediaOut 'node_modules\@modelcontextprotocol\sdk'))) {
            Write-Log '[runtime] 安装桥依赖（npm install，一次）...'
            Invoke-NpmInstall -Dir $mediaOut -What 'ui_design 桥依赖' -Repo $Repo
        }
    }

    # MiniMax 适配桥（零依赖单文件 Node 服务）：桌面版没有 Docker 网络，
    # .env 里指向容器服务名（minimax-media:8787）的通道靠这个本地进程顶替。
    $mmSrc = Join-Path $Repo 'docker\minimax-media'
    if (Test-Path (Join-Path $mmSrc 'server.js')) {
        $mmOut = Join-Path $prepared 'media\minimax-media'
        New-Item -ItemType Directory -Force -Path $mmOut | Out-Null
        Copy-Item -Force (Join-Path $mmSrc 'server.js') $mmOut
    }

    # 注意：运行时载荷这三处**必须用 npm**（不能用 pnpm）——pnpm 的 node_modules 是
    # 符号链接指向全局 store，载荷要整个拷进安装包、到别的机器上用，链接会断。
    # ui_preview（Playwright MCP）：Agent 的"眼睛"（浏览器截图 / UI 回显）。
    # 容器版要 Docker；桌面版改为随包 Node 直接跑本地 Chromium —— 目标机免 Docker、
    # 免系统 Node。浏览器二进制也随包（PLAYWRIGHT_BROWSERS_PATH 指过去），离线可用。
    $uiPrevOut = Join-Path $prepared 'media\ui-preview-mcp'
    if (-not (Test-Path (Join-Path $uiPrevOut 'node_modules\@playwright\mcp\cli.js'))) {
        Write-Log '[runtime] 安装 Playwright MCP ...'
        New-Item -ItemType Directory -Force -Path $uiPrevOut | Out-Null
        Write-JsonFile (Join-Path $uiPrevOut 'package.json') ([ordered]@{
                name    = 'bma-ui-preview-mcp'
                private = $true
                version = '1.0.0'
                dependencies = [ordered]@{ '@playwright/mcp' = $playwrightMcpVersion }
            })
        Invoke-NpmInstall -Dir $uiPrevOut -What '@playwright/mcp' -Repo $Repo
    }
    $browsers = Join-Path $prepared 'media\playwright-browsers'
    if (-not (Test-Path $browsers)) {
        Write-Log '[runtime] 下载 Chromium headless shell（一次性，约 290MB，随包分发）...'
        New-Item -ItemType Directory -Force -Path $browsers | Out-Null
        $prevBrowsersPath = $env:PLAYWRIGHT_BROWSERS_PATH
        $env:PLAYWRIGHT_BROWSERS_PATH = $browsers
        Push-Location $uiPrevOut
        try {
            # 只装 headless shell：桌面版的 ui_preview 恒为 headless（配置固定 --headless），
            # 完整 Chromium（451MB）用不上 —— 实测 headless shell 单独即可完成导航与截图。
            & node (Join-Path $uiPrevOut 'node_modules\playwright\cli.js') install chromium-headless-shell
            if ($LASTEXITCODE -ne 0) { throw 'playwright install chromium-headless-shell 失败' }
        } finally {
            Pop-Location
            $env:PLAYWRIGHT_BROWSERS_PATH = $prevBrowsersPath
        }
    }

    # host_computer_use（真机 GUI 直控）：原先走 npx（要系统 Node + npm 源），
    # 桌面版改为随包 Node 跑本地安装好的包 —— 同样免系统 Node。
    $cuOut = Join-Path $prepared 'media\computer-use-mcp'
    if (-not (Test-Path (Join-Path $cuOut 'node_modules\@zavora-ai\computer-use-mcp\dist\launch.js'))) {
        Write-Log '[runtime] 安装 computer-use-mcp ...'
        New-Item -ItemType Directory -Force -Path $cuOut | Out-Null
        Write-JsonFile (Join-Path $cuOut 'package.json') ([ordered]@{
                name    = 'bma-computer-use-mcp'
                private = $true
                version = '1.0.0'
                dependencies = [ordered]@{ '@zavora-ai/computer-use-mcp' = $computerUseMcpVersion }
            })
        Invoke-NpmInstall -Dir $cuOut -What '@zavora-ai/computer-use-mcp' -Repo $Repo
    }

    # 铺到包目录（prepared 保留作缓存，下次打包直接复制）
    if (Test-Path $OutDir) { Remove-Item -Recurse -Force $OutDir }
    New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
    Copy-Item -Recurse -Force (Join-Path $prepared '*') $OutDir
    # 记下配方版本（下次打包据此判断缓存是否可用）
    [System.IO.File]::WriteAllText((Join-Path $prepared '.recipe'), $recipe)
    # 载荷指纹标记：更新时比对"运行时有没有变"，没变就不重铺 300MB+。
    $fp = Get-PayloadFingerprint $OutDir
    [System.IO.File]::WriteAllText((Join-Path $OutDir '.payload-id'), ($recipe + ':' + $fp))

    $size = [math]::Round(((Get-ChildItem -Recurse -File $OutDir | Measure-Object -Property Length -Sum).Sum / 1MB), 0)
    Write-Log "[runtime] 载荷就绪：$OutDir（$size MB）"
}

# Resolve-PkgExe 把包管理器名（npm / pnpm）解析成**可直接 & 调用**的 Application 路径。
#
# **为什么必须绕开 PowerShell 的名字解析**（2026-11-16 新机器事故根因）：
# PS 在 PATH 里会优先选 `npm.ps1`（ExternalScript）而不是 `npm.cmd`，而 Node 22.18/22.20
# 自带的 npm 10.9.3 的 npm.ps1 有参数切分 bug（npm/cli#8528，10.9.4 才修）：
# 它用 `$NPM_NO_REDIRECTS_COMMAND.Substring($MyInvocation.InvocationName.Length)` 剥掉命令名，
# 而脚本里写 `& npm install ...`（带调用运算符）时 InvocationName 只有 `&`（长度 1），
# 于是参数变成 `pm install ...` → npm 报 `Unknown command: "pm"`。
# 不带 `&` 的 `npm install` 恰好 InvocationName=`npm` 才正常——所以本仓库自带的
# dist.ps1 一直能用，只有打包脚本里显式 `& npm` 的这几处会翻车。
# 解法：一律调 `.cmd`/`.exe`（cmd shim 不经过 PS 的参数解析），不走 `.ps1`。
function Resolve-PkgExe {
    param([string]$Name)
    foreach ($ext in @('.cmd', '.exe')) {
        $hit = @(Get-Command ($Name + $ext) -CommandType Application -ErrorAction SilentlyContinue) |
            Select-Object -First 1
        if ($hit -and $hit.Source) { return $hit.Source }
    }
    return $null
}

# Get-NpmInvoker 返回 npm 调用器：exe + 前置参数（npm.cmd 优先，退化到 node + npm-cli.js）。
function Get-NpmInvoker {
    $exe = Resolve-PkgExe 'npm'
    if ($exe) { return [ordered]@{ exe = $exe; prefix = @() } }
    $node = Resolve-PkgExe 'node'
    if ($node) {
        $cli = Join-Path (Split-Path $node -Parent) 'node_modules\npm\bin\npm-cli.js'
        if (Test-Path $cli) { return [ordered]@{ exe = $node; prefix = @($cli) } }
    }
    Write-Log '[pack] warn: 找不到 npm.cmd / npm-cli.js，将退回 PowerShell 名字解析（npm < 10.9.4 会报 Unknown command: "pm"）'
    return [ordered]@{ exe = 'npm'; prefix = @() }
}

# Invoke-Npm 跑 npm（参数逐个透传，不经 Invoke-Expression），返回退出码。
# -Log 给了就把全部输出（含 stderr）落盘，供失败时回显尾部；没给就直写控制台（**不进管道**，
# 否则调用方的 `$code = Invoke-Npm ...` 会收成"输出行 + 退出码"的数组）。
function Invoke-Npm {
    param([string[]]$Arguments, [string]$Log)
    $inv = Get-NpmInvoker
    $all = @($inv.prefix) + @($Arguments)
    if ($Log) { & $inv.exe @all *> $Log } else { & $inv.exe @all 2>&1 | ForEach-Object { Write-Host $_ } }
    return $LASTEXITCODE
}

# Get-NpmVersion 取 npm 版本（诊断用；同样绕开 .ps1 shim）。
function Get-NpmVersion {
    $inv = Get-NpmInvoker
    $all = @($inv.prefix) + @('-v')
    try { return ((@(& $inv.exe @all 2>$null) | Select-Object -First 1) + '') } catch { return '' }
}

# Get-PackageManager 探测本机的包管理器：优先 pnpm，没有退回 npm。
#
# 只用于**前端依赖**（web/ 的 node_modules 是本机开发用，不进包）。
# 运行时载荷（runtime\media\*）必须用 npm —— pnpm 的 node_modules 是符号链接指向
# 全局 store，拷进安装包到别的机器上链接就断了。
function Get-PackageManager {
    foreach ($name in @('pnpm', 'npm')) {
        $exe = Resolve-PkgExe $name
        if (-not $exe) { continue }
        $ver = ''
        try { $ver = ((@(& $exe -v 2>$null) | Select-Object -First 1) + '') } catch { $ver = '' }
        return [ordered]@{ kind = $name; cmd = $exe; version = $ver }
    }
    return $null
}

# Install-FrontendDeps 装前端依赖：优先用本机已有的 pnpm，失败再退回 npm。
#
# pnpm 常见的坑：pnpm 10 对"未批准的构建脚本"（esbuild / @parcel/watcher 等）
# 直接以 ERR_PNPM_IGNORED_BUILDS 退出 1 —— 装到一半的 node_modules 不能用，
# 所以失败时**清掉重来**用 npm 装。
function Install-FrontendDeps([string]$WebDir) {
    $pm = Get-PackageManager
    if (-not $pm) { throw '找不到 npm 或 pnpm（装 Node.js 18+ 并把 node 加进 PATH）' }
    Write-Log ('[pack] 用 ' + $pm.kind + ' ' + $pm.version + ' 安装前端依赖（首次较慢）...')
    if ($pm.kind -eq 'pnpm') {
        & $pm.cmd install 2>&1 | ForEach-Object { Write-Host $_ }
        if ($LASTEXITCODE -eq 0) { return 'pnpm' }
        Write-Log '[pack] warn: pnpm install 失败，清理 node_modules 后退回 npm ...'
        Remove-Item (Join-Path $WebDir 'node_modules') -Recurse -Force -ErrorAction SilentlyContinue
    }
    $code = Invoke-Npm @('install', '--no-audit', '--no-fund')
    if ($code -ne 0) {
        throw ('前端依赖安装失败（npm ' + (Get-NpmVersion) + ' 也失败；检查网络/registry，或用 -SkipWeb 复用已有 web\dist）')
    }
    return 'npm'
}

# Invoke-NpmInstall 在载荷目录里装 npm 依赖（失败时报真实原因，不吞输出）。
#
# 两个要点：
#  1. **PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1**：@playwright/mcp 与 computer-use-mcp 都
#     依赖 `playwright` 包，它的 postinstall 会把**全部浏览器**（chromium + firefox +
#     webkit + headless shell，约 500MB）下载到用户目录——我们只随包带一个 headless
#     shell（见下），这些下载纯属浪费，慢到超时还会让 npm install 直接失败。
#  2. 输出落盘再回显尾部：原来 `| Out-Null` 把报错吞了，只剩一句"npm install 失败"。
function Invoke-NpmInstall {
    param([string]$Dir, [string]$What, [string]$Repo)

    $logDir = Join-Path $Repo 'dist'
    New-Item -ItemType Directory -Force -Path $logDir | Out-Null
    $log = Join-Path $logDir ('npm-install-' + ($What -replace '[^A-Za-z0-9_-]', '_') + '.log')

    $skip = $env:PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD
    $env:PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD = '1'
    Push-Location $Dir
    try {
        $code = Invoke-Npm @('install', '--omit=dev', '--no-audit', '--no-fund') -Log $log
    } finally {
        Pop-Location
        $env:PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD = $skip
    }
    if ($code -ne 0) {
        Write-Host ('--- npm install ' + $What + ' 失败（npm ' + (Get-NpmVersion) + '，末尾 20 行）---')
        Get-Content $log -Tail 20 -ErrorAction SilentlyContinue | ForEach-Object { Write-Host $_ }
        Write-Host ('--- 完整输出见 ' + $log + ' ---')
        throw ('npm install ' + $What + ' 失败')
    }
}

# Get-PayloadFingerprint 计算目录载荷指纹（相对路径 + 大小的 SHA256）。
#
# 用途：更新时判断"运行时载荷有没有变"——没变就不必重铺 300MB+（PostgreSQL/Redis/
# Chromium/Node 全在里面）。指纹只覆盖"路径 + 大小"，不逐字节哈希 400MB（太慢）。
function Get-PayloadFingerprint([string]$Dir) {
    if (-not (Test-Path $Dir)) { return '' }
    $sb = New-Object System.Text.StringBuilder
    Get-ChildItem $Dir -Recurse -File | Sort-Object FullName | ForEach-Object {
        $rel = $_.FullName.Substring($Dir.Length + 1).Replace('\', '/')
        [void]$sb.Append($rel).Append('|').Append($_.Length).Append("`n")
    }
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        $bytes = [System.Text.Encoding]::UTF8.GetBytes($sb.ToString())
        return ([BitConverter]::ToString($sha.ComputeHash($bytes))).Replace('-', '').ToLower()
    } finally { $sha.Dispose() }
}

# Set-PluginBlock 把 plugins.yaml 里某个插件块整体替换为给定内容。
#
# 插件条目都在 plugins: 下的两空格缩进层，块边界按"下一个同缩进的 key"界定。
# 只替换指定块，其余配置原样保留（开发实例仍用仓库里的容器方案）。
function Set-PluginBlock([string]$Path, [string]$PluginId, [string[]]$Block) {
    if (-not (Test-Path $Path)) { return $false }
    $lines = [System.Collections.ArrayList]@([System.IO.File]::ReadAllLines($Path))
    $start = -1
    for ($i = 0; $i -lt $lines.Count; $i++) {
        if ($lines[$i] -match ('^  ' + [regex]::Escape($PluginId) + ':\s*$')) { $start = $i; break }
    }
    if ($start -lt 0) {
        Write-Log "[pack] warn: plugins.yaml 里没有 $PluginId 块，跳过改写"
        return $false
    }
    $end = $lines.Count
    for ($i = $start + 1; $i -lt $lines.Count; $i++) {
        if ($lines[$i] -match '^  [A-Za-z_][A-Za-z0-9_]*:\s*$') { $end = $i; break }
    }
    $lines.RemoveRange($start, $end - $start)
    $lines.InsertRange($start, $Block)
    [System.IO.File]::WriteAllLines($Path, $lines, (New-Object System.Text.UTF8Encoding($false)))
    return $true
}

# Convert-PluginsToNative 把包内 plugins.yaml 的三个"依赖外部运行时"的插件改成本地 stdio。
#
# 桌面版要免装 Docker、免装系统 Node：
#   * ui_design  → 随包 Node 起 ui_design 桥，上游是原生媒体服务（127.0.0.1:7456）；
#   * ui_preview → 随包 Node 起 Playwright MCP，Chromium 也在包内（离线可用）；
#   * host_computer_use → 随包 Node 起本地 computer-use-mcp（原先走 npx，要系统 Node + npm 源）。
# 开发实例仍用仓库里的容器方案（不改仓库配置）。
function Convert-PluginsToNative([string]$Path) {
    if (-not (Test-Path $Path)) { return }

    # ui_design：生图/生视频桥（路径用正斜杠：YAML 双引号里反斜杠算转义符）。
    $uiDesign = [string[]]@(
        '  ui_design:',
        '    kind: mcp',
        '    enabled: true',
        '    settings:',
        '      # 桌面版走 stdio：随包 Node 直接起桥进程，上游是原生媒体服务（免 Docker）。',
        '      transport: stdio',
        '      command: ${BMA_NODE_EXE}',
        '      args: ["${BMA_MEDIA_BRIDGE}"]',
        '      shared: false',
        '      env:',
        '        OD_API_URL: http://127.0.0.1:7456',
        '        OD_API_TOKEN: ${OD_API_TOKEN:}',
        '        # 工作目录占位符由插件管理器展开（config 层刻意不解析它）。',
        '        OD_WORKSPACE_DIR: ${WORKDIR}',
        '        OD_OUTPUT_REL_PREFIX: .bma/od-artifacts',
        '        OD_IMAGE_CAPABILITIES: ${IMAGE_CAPABILITIES:}',
        '        OD_VIDEO_CAPABILITIES: ${VIDEO_CAPABILITIES:}',
        '      roles: ["meta","domain","ui_assistant"]',
        '      exec_timeout_sec: 1200'
    )
    if (Set-PluginBlock $Path 'ui_design' $uiDesign) {
        Write-Log '[pack] plugins.yaml：ui_design 改为 stdio（随包 Node，免 Docker）'
    }

    # ui_preview：Playwright MCP 本地跑（Chromium 随包）。
    # ${WORKDIR} 由插件管理器在每次会话建连时展开（截图落进会话自己的工作目录）。
    $uiPreview = [string[]]@(
        '  ui_preview:',
        '    kind: mcp',
        '    enabled: true',
        '    settings:',
        '      # 桌面版走 stdio：随包 Node 起 Playwright MCP，Chromium 也在安装包内（免 Docker、免系统 Node）。',
        '      transport: stdio',
        '      command: ${BMA_NODE_EXE}',
        '      args: ["${BMA_UI_PREVIEW_MCP}", "--headless", "--output-dir", "${WORKDIR}/.bma/ui-artifacts", "--allow-unrestricted-file-access", "--image-responses", "allow"]',
        '      env:',
        '        # 浏览器二进制随包（离线可用）：Playwright 按此变量找浏览器。',
        '        PLAYWRIGHT_BROWSERS_PATH: ${BMA_PLAYWRIGHT_BROWSERS}',
        '      image_passthrough: true',
        '      roles: ["meta","domain","ui_assistant"]',
        '      exec_timeout_sec: 180'
    )
    if (Set-PluginBlock $Path 'ui_preview' $uiPreview) {
        Write-Log '[pack] plugins.yaml：ui_preview 改为 stdio（随包 Playwright，免 Docker）'
    }

    # host_computer_use：真机 GUI 直控，改随包 Node 跑本地包（原先 npx：需系统 Node + npm 源）。
    $hostCu = [string[]]@(
        '  host_computer_use:',
        '    kind: mcp',
        '    enabled: true',
        '    settings:',
        '      # 桌面版走 stdio：随包 Node 跑已装好的包（免系统 Node / npm 源）。',
        '      transport: stdio',
        '      command: ${BMA_NODE_EXE}',
        '      args: ["${BMA_COMPUTER_USE_MCP}"]',
        '      destructive: true',
        '      roles: ["meta", "test_assistant"]',
        '      image_passthrough: true',
        '      tool_description_suffix: "注意:本工具直接操作宿主机真实桌面(非沙箱):截图为真机屏幕,鼠标键盘事件作用于真机前台窗口。文件落盘仍用内置 WriteFile/EditFile(会话工作目录),不要用本工具的 filesystem 能力落盘任务产物。"'
    )
    if (Set-PluginBlock $Path 'host_computer_use' $hostCu) {
        Write-Log '[pack] plugins.yaml：host_computer_use 改为 stdio（随包 Node，免系统 Node）'
    }
}

function Invoke-Pack {
    param([string]$Version, [switch]$SkipWeb, [switch]$Test, [switch]$NoZip, [switch]$NoRuntime, [switch]$NoFetch)

    $repo = $PSScriptRoot
    $commit = Get-GitCommit $repo
    if ([string]::IsNullOrWhiteSpace($Version)) { $Version = (Get-Date -Format 'yyyyMMdd-HHmmss') + '-' + $commit }
    $promptVersion = Get-PromptVersion $repo

    $stage = Join-Path $repo ('dist\app\bma-' + $Version)
    if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
    New-Item -ItemType Directory -Force -Path (Join-Path $stage 'bin'), (Join-Path $stage 'web\dist'), (Join-Path $stage 'config-templates'), (Join-Path $stage 'launcher') | Out-Null

    # 前端
    $webDist = Join-Path $repo 'web\dist'
    if (-not $SkipWeb) {
        Write-Log '[pack] 构建前端 ...'
        Push-Location (Join-Path $repo 'web')
        try {
            if (-not (Test-Path 'node_modules')) {
                Install-FrontendDeps -WebDir (Get-Location).Path | Out-Null
            }
            # 直接调**本地二进制**，不经过 npm run <script>：
            #   * 不依赖 package.json 里的脚本名（换机器/拉旧代码时脚本可能还没合进来）；
            #   * 少一层 npm 参数转发，出错信息直指真正执行的命令。
            $binDir = Join-Path (Get-Location).Path 'node_modules\.bin'
            $tscExe = Join-Path $binDir 'vue-tsc.cmd'
            $viteExe = Join-Path $binDir 'vite.cmd'
            if (-not (Test-Path $viteExe)) {
                throw ('找不到 ' + $viteExe + '（node_modules 不完整？先在 web 目录跑 npm install，或用 -SkipWeb 复用已有 web\dist）')
            }
            # 环境版本也打进日志：前端构建出问题时报错信息自解释。
            Write-Log ('[pack] node ' + (& node -v 2>$null) + ' / npm ' + (Get-NpmVersion))
            # 类型检查单独跑：**不阻断打包**。类型问题该在开发期修，不是拦住出包的闸门
            # （新机器/新 Node 版本的类型差异不该让整个安装流程失败）。
            if (Test-Path $tscExe) {
                & $tscExe -b
                if ($LASTEXITCODE -ne 0) {
                    Write-Log '[pack] warn: vue-tsc 类型检查未通过（不阻断打包）——请在开发环境修掉'
                }
            } else {
                Write-Log '[pack] warn: 找不到 vue-tsc，跳过类型检查'
            }
            # vite 构建：失败时把真实报错打回控制台（原来只有一句"npm run build 失败"，
            # 看不到原因）。输出同时落文件备查。
            $webLog = Join-Path $repo 'dist\web-build.log'
            New-Item -ItemType Directory -Force -Path (Split-Path $webLog -Parent) | Out-Null
            # vite 构建吃内存，机器内存小时默认堆上限会 OOM——给足堆。
            $env:NODE_OPTIONS = '--max-old-space-size=4096'
            Write-Log ('[pack] 执行: ' + $viteExe + ' build')
            & $viteExe build *> $webLog
            $webExit = $LASTEXITCODE
            $env:NODE_OPTIONS = ''
            if ($webExit -ne 0) {
                Write-Host '--- 前端构建输出（开头 10 行 / 末尾 30 行）---'
                Get-Content $webLog -TotalCount 10 -ErrorAction SilentlyContinue | ForEach-Object { Write-Host $_ }
                Write-Host '   ...'
                Get-Content $webLog -Tail 30 -ErrorAction SilentlyContinue | ForEach-Object { Write-Host $_ }
                Write-Host ('--- 完整输出见 ' + $webLog + ' ---')
                throw '前端构建失败（vite build）'
            }
        } finally { Pop-Location }
    }
    if (-not (Test-Path (Join-Path $webDist 'index.html'))) {
        throw '缺少 web/dist/index.html（先构建前端，或去掉 -SkipWeb）'
    }
    Copy-Item -Recurse -Force (Join-Path $webDist '*') (Join-Path $stage 'web\dist')

    # 应用图标 + Windows 资源（desktop exe 的图标来自 assets\icon.ico）
    $iconPath = Join-Path $repo 'assets\icon.ico'
    if (-not (Test-Path $iconPath)) {
        Write-Log '[pack] 生成应用图标 ...'
        & powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $repo 'tools\make-icon.ps1') | Out-Null
    }
    if (-not (Test-Path $iconPath)) { throw '缺少 assets\icon.ico（tools\make-icon.ps1 生成失败）' }
    $syso = Join-Path $repo 'backend\cmd\desktop\rsrc.syso'
    $needSyso = (-not (Test-Path $syso)) -or ((Get-Item $iconPath).LastWriteTime -gt (Get-Item $syso).LastWriteTime)
    if ($needSyso) {
        Write-Log '[pack] 生成 exe 图标资源 ...'
        Push-Location (Join-Path $repo 'backend')
        try { & go run github.com/akavel/rsrc@v0.10.2 -ico $iconPath -arch amd64 -o $syso } finally { Pop-Location }
        if ($LASTEXITCODE -ne 0) { throw 'rsrc 生成图标资源失败' }
    }

    # 后端（ldflags 注入构建标识，/api/health 的 build.version 据此校验更新是否生效）
    $env:GOTOOLCHAIN = 'local'
    $builtAt = (Get-Date).ToString('yyyy-MM-ddTHH:mm:sszzz')
    $ldflags = '-X github.com/blockmemory/agent/backend/internal/buildinfo.Version=' + $Version +
        ' -X github.com/blockmemory/agent/backend/internal/buildinfo.Commit=' + $commit +
        ' -X github.com/blockmemory/agent/backend/internal/buildinfo.BuiltAt=' + $builtAt
    Write-Log "[pack] 构建后端 $Version ..."
    Push-Location (Join-Path $repo 'backend')
    try {
        & go build ./...
        if ($LASTEXITCODE -ne 0) { throw 'go build ./... 失败' }
        if ($Test) {
            & go test ./...
            if ($LASTEXITCODE -ne 0) { throw 'go test ./... 失败' }
        }
        & go build -trimpath -ldflags $ldflags -o (Join-Path $stage 'bin\bma-server.exe') .
        if ($LASTEXITCODE -ne 0) { throw 'go build bma-server 失败' }
        & go build -trimpath -ldflags $ldflags -o (Join-Path $stage 'bin\tui.exe') ./cmd/tui
        if ($LASTEXITCODE -ne 0) { throw 'go build tui 失败' }
        # 原生媒体服务（免 Docker 的生图/生视频上游适配）
        & go build -trimpath -ldflags $ldflags -o (Join-Path $stage 'bin\mediaservice.exe') ./cmd/mediaservice
        if ($LASTEXITCODE -ne 0) { throw 'go build mediaservice 失败' }

        # 桌面外壳（WebView2 原生窗口，自带图标）：GUI 子系统，避免黑框控制台
        $env:CGO_ENABLED = '0'
        & go build -trimpath -ldflags ('-H=windowsgui ' + $ldflags) -o (Join-Path $stage 'bin\bma-desktop.exe') ./cmd/desktop
        if ($LASTEXITCODE -ne 0) { throw 'go build bma-desktop 失败' }
    } finally { Pop-Location }

    # 配置模板（只放模板，不放仓库运行时数据）
    foreach ($t in @('config.yaml', 'roles.yaml', 'skills.yaml', 'soul.md', 'plugins.yaml', 'external_agents.yaml')) {
        $src = Join-Path $repo ('config\' + $t)
        if (Test-Path $src) { Copy-Item -Force $src (Join-Path $stage 'config-templates') }
        else { Write-Log "[pack] warn: 缺少配置模板 config\$t" }
    }
    Copy-Item -Force (Join-Path $repo '.env.example') (Join-Path $stage 'config-templates\env.example')
    # 桌面版的 ui_design 走 stdio（随包 Node + 原生媒体服务），免 Docker
    Convert-PluginsToNative (Join-Path $stage 'config-templates\plugins.yaml')

    # 启动器 + 图标
    foreach ($l in @('launch.ps1', 'BMA.cmd', 'BMA.vbs')) {
        Copy-Item -Force (Join-Path $repo ('tools\app\' + $l)) (Join-Path $stage 'launcher')
    }
    Copy-Item -Force $iconPath (Join-Path $stage 'launcher\icon.ico')

    # 依赖编排 + 媒体栈构建上下文（桌面版自举 PostgreSQL/Redis 与媒体栈用）。
    # 只取 compose 文件与 4 个构建上下文；bma-plugin.sh / media-smoke.mjs /
    # docker-compose.test.yml / install_docker.sh 是 Linux/bash 专用，不进包。
    $dockerSrc = Join-Path $repo 'docker'
    $dockerDst = Join-Path $stage 'docker'
    New-Item -ItemType Directory -Force -Path $dockerDst | Out-Null
    $composeSrc = Join-Path $dockerSrc 'docker-compose.yml'
    if (Test-Path $composeSrc) {
        Copy-Item -Force $composeSrc $dockerDst
    } else {
        throw '缺少 docker\docker-compose.yml（桌面版依赖自举需要它）'
    }
    foreach ($ctx in @('open-design-daemon', 'minimax-media', 'open-design-mcp', 'ui-preview-mcp')) {
        $ctxSrc = Join-Path $dockerSrc $ctx
        if (Test-Path $ctxSrc) {
            Copy-Item -Recurse -Force $ctxSrc (Join-Path $dockerDst $ctx)
        } else {
            Write-Log "[pack] warn: 缺少 docker\$ctx（媒体栈构建上下文）"
        }
    }

    # 自带运行时（PostgreSQL+pgvector / Redis）：目标机器免装 Docker。
    # 源包不在仓库里（dist\runtime-cache），需先跑 installer\fetch-runtime.ps1 下载。
    #
    # 缺源包时**直接报错**（除非显式 -NoRuntime）：不带运行时的包装出来跑不起来
    # （后端严格启动，连不上 PG/Redis 就退出）——早失败好过装完才发现。
    if (-not $NoRuntime) {
        # 源包缺失时自动下载一次（全新机器的常规路径），失败再报错。
        if (-not (Ensure-RuntimeSources -Repo $repo -NoFetch:$NoFetch)) {
            throw ("缺少运行时源包（" + (Get-RuntimeCache $repo) + " 下需要 pg16.zip / pgvector-pg16.zip / redis-win.zip）。" +
                   "先跑： pwsh -File .\installer\fetch-runtime.ps1（下载 PG16 + pgvector + Redis + WebView2 引导程序）；" +
                   "若确实只要纯程序包（开发机用），显式加 -NoRuntime。")
        }
        Write-Log '[pack] 准备自带运行时载荷 ...'
        Build-RuntimePayload -Repo $repo -OutDir (Join-Path $stage 'runtime')
    }

    # 禁运清单：仓库运行时数据与密钥，任何情况下都不许进包
    foreach ($f in @('models.json', 'user_profile.md', 'plugins.installed.yaml', 'skills_learned', '.env')) {
        $p = Join-Path $stage ('config-templates\' + $f)
        if (Test-Path $p) { Remove-Item -Recurse -Force $p; Write-Log "[pack] 已剔除禁运文件 $f" }
    }

    # 模型注册表模板：models.json 在禁运清单里（用户注册表可能含明文 Key），但新装
    # 机器没有它就没有任何可用模型 —— 生成一份"只含 ${VAR} 引用"的模板随包走。
    $modelsSrc = Join-Path $repo 'config\models.json'
    if (Test-Path $modelsSrc) {
        try {
            $mj = Get-Content $modelsSrc -Raw -Encoding UTF8 | ConvertFrom-Json
            foreach ($m in $mj.models) {
                if ($m.api_key -and -not ($m.api_key -match '\$\{')) { $m.api_key = '${ANTHROPIC_API_KEY}' }
                if ($m.base_url -and -not ($m.base_url -match '\$\{')) { $m.base_url = '${ANTHROPIC_BASE_URL}' }
            }
            $jsonText = $mj | ConvertTo-Json -Depth 8
            [System.IO.File]::WriteAllText((Join-Path $stage 'config-templates\models.json'), $jsonText, (New-Object System.Text.UTF8Encoding($false)))
            Write-Log '[pack] 已生成模型注册表模板（去密钥，仅 ${VAR} 引用）'
        } catch {
            Write-Log '[pack] warn: 生成模型注册表模板失败（新装机器将没有可用模型）'
        }
    }

    # 密钥扫描：模板里只允许 ${VAR} 插值，发现疑似真实 key 一律拒包。
    # 扫描面 = 我们自己的配置/脚本（config-templates、启动器等），跳过 runtime\
    # （第三方二进制 + node_modules：Chromium 的 .pak 会随机命中正则，误报）。
    $hits = @(Get-ChildItem $stage -Recurse -File |
        Where-Object {
            $_.FullName -notlike '*\runtime\*' -and
            $_.Name -notlike '*example*' -and
            $_.Extension -notin @('.pak', '.dll', '.exe', '.node', '.zip', '.7z', '.bin', '.dat')
        } |
        Select-String -Pattern 'sk-[A-Za-z0-9]{16,}', 'api_key\s*:\s*["'']?[A-Za-z0-9_\-]{16,}' -List -ErrorAction SilentlyContinue)
    if ($hits.Count -gt 0) {
        throw ('包内发现疑似真实密钥，拒绝打包: ' + (($hits | ForEach-Object { $_.Path }) -join ', '))
    }

    # 安装器暂存目录（Inno Setup 的源）：按"最终安装后的样子"铺好，
    # 让安装脚本只做一次整目录拷贝即可。
    $setupDir = Join-Path $repo ('dist\setup\' + $Version)
    if (Test-Path $setupDir) { Remove-Item -Recurse -Force $setupDir }
    $verDir = Join-Path $setupDir ('app\versions\' + $Version)
    New-Item -ItemType Directory -Force -Path $verDir, (Join-Path $setupDir 'app') | Out-Null
    Copy-Item -Recurse -Force (Join-Path $stage 'bin') (Join-Path $verDir 'bin')
    Copy-Item -Recurse -Force (Join-Path $stage 'web') (Join-Path $verDir 'web')
    Copy-Item -Recurse -Force (Join-Path $stage 'config-templates') (Join-Path $verDir 'config-templates')
    Copy-Item -Force (Join-Path $stage 'launcher\launch.ps1') (Join-Path $setupDir 'app\launch.ps1')
    Copy-Item -Force (Join-Path $stage 'launcher\icon.ico') (Join-Path $setupDir 'app\icon.ico')
    Copy-Item -Force (Join-Path $stage 'bin\bma-desktop.exe') (Join-Path $setupDir 'BMA-Desktop.exe')
    if (Test-Path (Join-Path $stage 'docker')) {
        Copy-Item -Recurse -Force (Join-Path $stage 'docker') (Join-Path $setupDir 'docker')
    }
    if (Test-Path (Join-Path $stage 'runtime')) {
        Copy-Item -Recurse -Force (Join-Path $stage 'runtime') (Join-Path $setupDir 'runtime')
    }
    # 用户数据种子：安装时"缺失才铺"（Inno Setup 的 onlyifdoesntexist），已存在绝不覆盖
    Copy-Item -Recurse -Force (Join-Path $stage 'config-templates') (Join-Path $setupDir 'config-templates')
    Copy-Item -Force (Join-Path $stage 'config-templates\env.example') (Join-Path $setupDir '.env.example')
    # 版本指针（安装器不再改写它）
    Write-JsonFile (Join-Path $setupDir 'app\current.json') ([ordered]@{
            version      = $Version
            previous     = ''
            installed_at = (Get-Date).ToString('o')
            commit       = $commit
            package      = 'installer'
        })
    # 版本号纯文本：安装器的 [Code] 段用它写版本指针（Pascal 侧读文件，
    # 避开预处理器常量在 [Code] 里的引号问题——/D 传的是裸值）。
    [System.IO.File]::WriteAllText((Join-Path $setupDir 'app\version.txt'), $Version, (New-Object System.Text.UTF8Encoding($false)))
    Write-Log "[pack] 安装器暂存: $setupDir"

    # 清单与版本元数据
    $files = @(Get-ChildItem $stage -Recurse -File | ForEach-Object {
        $rel = $_.FullName.Substring($stage.Length + 1).Replace('\', '/')
        [pscustomobject]@{
            path   = $rel
            sha256 = (Get-FileHash -Algorithm SHA256 -Path $_.FullName).Hash.ToLower()
            size   = $_.Length
        }
    } | Sort-Object path)
    $manifestPath = Join-Path $stage 'MANIFEST.sha256'
    $manifestLines = @($files | ForEach-Object { $_.sha256 + ' *' + $_.path })
    [System.IO.File]::WriteAllLines($manifestPath, $manifestLines, (New-Object System.Text.UTF8Encoding($false)))
    $manifestHash = (Get-FileHash -Algorithm SHA256 -Path $manifestPath).Hash.ToLower()
    Write-JsonFile (Join-Path $stage 'VERSION.json') ([ordered]@{
        name            = 'BlockMemoryAgent'
        version         = $Version
        commit          = $commit
        built_at        = $builtAt
        prompt_version  = $promptVersion
        file_count      = $files.Count
        manifest_sha256 = $manifestHash
    })

    $zip = $null
    if (-not $NoZip) {
        $zip = Join-Path $repo ('dist\bma-app-' + $Version + '.zip')
        if (Test-Path $zip) { Remove-Item -Force $zip }
        [System.IO.Compression.ZipFile]::CreateFromDirectory($stage, $zip, [System.IO.Compression.CompressionLevel]::Optimal, $false)
        Write-Log "[pack] zip: $zip"
    }

    Write-Log "[pack] 包目录: $stage"
    Write-Log "[pack] 版本 $Version（commit $commit，prompt $promptVersion，$($files.Count) 个文件）"
    return [ordered]@{
        ok             = $true
        command        = 'pack'
        version        = $Version
        commit         = $commit
        prompt_version = $promptVersion
        package        = $stage
        zip            = $zip
        file_count     = $files.Count
    }
}

# ---------------- install / update ----------------

function Resolve-Package([string]$From) {
    if (-not [string]::IsNullOrWhiteSpace($From)) {
        $src = [System.IO.Path]::GetFullPath($From)
        if (-not (Test-Path $src)) { throw "包不存在: $src" }
        if (Test-Path $src -PathType Container) { return @{ Path = $src; Temp = $null } }
        $tmp = Join-Path ([System.IO.Path]::GetTempPath()) ('bma-pkg-' + [guid]::NewGuid().ToString('N'))
        [System.IO.Compression.ZipFile]::ExtractToDirectory($src, $tmp)
        $dir = Get-ChildItem $tmp -Directory | Select-Object -First 1
        if (-not $dir) { throw "压缩包为空: $src" }
        return @{ Path = $dir.FullName; Temp = $tmp }
    }
    $root = Join-Path $PSScriptRoot 'dist\app'
    if (-not (Test-Path $root)) { throw '未找到 dist\app，请先运行: .\bma-app.ps1 pack' }
    $cand = Get-ChildItem $root -Directory -Filter 'bma-*' | Sort-Object LastWriteTime -Descending | Select-Object -First 1
    if (-not $cand) { throw '未找到已打包版本，请先运行: .\bma-app.ps1 pack' }
    return @{ Path = $cand.FullName; Temp = $null }
}

# 关掉该安装目录下的桌面外壳（BMA-Desktop.exe）；不动其他安装的实例。
function Stop-DesktopShell([string]$Root) {
    $stopped = @()
    try {
        Get-CimInstance Win32_Process -Filter "name='bma-desktop.exe'" -ErrorAction SilentlyContinue | ForEach-Object {
            if ($_.ExecutablePath -and $_.ExecutablePath.StartsWith($Root, [System.StringComparison]::OrdinalIgnoreCase)) {
                try {
                    Stop-Process -Id $_.ProcessId -Force -ErrorAction Stop
                    $stopped += [int]$_.ProcessId
                } catch { }
            }
        }
    } catch { }
    # 等进程真正退出：exe 文件在进程结束前一直被锁着，
    # 不等就会在覆盖 BMA-Desktop.exe 时撞 "being used by another process"。
    if ($stopped.Count -gt 0) {
        $deadline = (Get-Date).AddSeconds(20)
        while ((Get-Date) -lt $deadline) {
            $alive = @($stopped | Where-Object { Get-Process -Id $_ -ErrorAction SilentlyContinue })
            if ($alive.Count -eq 0) { break }
            Start-Sleep -Milliseconds 300
        }
    }
    return $stopped
}

# Copy-WithRetry 复制文件，撞文件占用时重试几次（安装/更新期间进程退出有延迟）。
#
# 注意：不要返回值——PowerShell 函数会把未捕获的输出混进调用方的输出流，
# 污染 Invoke-Install 的返回值（会变成数组，调用方再赋值属性就报错）。
function Copy-WithRetry([string]$Src, [string]$Dst, [int]$Attempts = 5) {
    for ($i = 1; $i -le $Attempts; $i++) {
        try {
            Copy-Item -Force $Src $Dst
            return
        } catch {
            if ($i -ge $Attempts) { throw }
            Write-Log ("[install] 文件被占用，等候重试（{0}/{1}）: {2}" -f $i, $Attempts, $Dst)
            Start-Sleep -Seconds 2
        }
    }
}

function Stop-BmaServer([string]$Root) {
    $pids = @()
    $rt = Read-JsonFile (Join-Path $Root 'app\runtime.json')
    if ($rt -and $rt.pid) { $pids += [int]$rt.pid }
    try {
        Get-CimInstance Win32_Process -Filter "name='bma-server.exe'" -ErrorAction SilentlyContinue | ForEach-Object {
            if ($_.ExecutablePath -and $_.ExecutablePath.StartsWith($Root, [System.StringComparison]::OrdinalIgnoreCase)) {
                $pids += [int]$_.ProcessId
            }
        }
    } catch { }
    $stopped = @()
    foreach ($procId in ($pids | Sort-Object -Unique)) {
        if ($procId -eq $PID) { continue }
        try {
            Stop-Process -Id $procId -Force -ErrorAction Stop
            $stopped += $procId
        } catch { }
    }
    if ($stopped.Count -gt 0) {
        $deadline = (Get-Date).AddSeconds(20)
        while ((Get-Date) -lt $deadline) {
            $alive = @($stopped | Where-Object { Get-Process -Id $_ -ErrorAction SilentlyContinue })
            if ($alive.Count -eq 0) { break }
            Start-Sleep -Milliseconds 300
        }
    }
    $runtimePath = Join-Path $Root 'app\runtime.json'
    if (Test-Path $runtimePath) { Remove-Item -Force $runtimePath -ErrorAction SilentlyContinue }
    return $stopped
}

function Start-And-Wait {
    param([string]$Root, [int]$TimeoutSec, [string]$ExpectVersion)
    $launch = Join-Path $Root 'app\launch.ps1'
    if (-not (Test-Path $launch)) { Write-Log '[start] 缺少 app\launch.ps1'; return $null }
    # 异步拉起启动器（ShellExecute，不继承本进程的 stdio），由本函数按健康检查判定成败：
    # 判定只看**健康检查 + build.version**——启动器自身可能因为收尾错误返回非 0，或者
    # 某些旧版本启动器会挂住不退，都不该影响"服务到底起没起"这个事实。
    $argList = @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $launch, '-NoBrowser', '-Quiet', '-TimeoutSec', "$TimeoutSec")
    $lp = Start-Process -FilePath 'powershell.exe' -ArgumentList $argList -WindowStyle Hidden -PassThru
    $deadline = (Get-Date).AddSeconds($TimeoutSec + 15)
    $h = $null
    $started = Get-Date
    $lastTick = 0
    while ((Get-Date) -lt $deadline) {
        $h = Get-Health $Root
        if ($h -and $h.build -and $h.build.version -and ($h.build.version -eq $ExpectVersion)) {
            return $h
        }
        if ($lp.HasExited -and -not $h) { break }
        # 等待期给进度（每 10 秒一行）：否则长时间无输出像是卡死。
        $elapsed = [int]((Get-Date) - $started).TotalSeconds
        if ($elapsed -ge ($lastTick + 10)) {
            $lastTick = $elapsed
            Write-Log ("[start] 等待后端就绪 ... {0}s（依赖自举 / 首次 initdb 可能耗时）" -f $elapsed)
        }
        Start-Sleep -Milliseconds 500
    }
    if ($h -and $h.build -and $h.build.version) {
        Write-Log ("[start] 运行版本 " + $h.build.version + " ≠ 期望 " + $ExpectVersion + '（旧进程可能仍在跑）')
    } elseif ($lp.HasExited) {
        Write-Log ("[start] 启动器已退出（退出码 " + $lp.ExitCode + "），服务未就绪")
        # 启动器把依赖自举的失败原因写在 app\deps.json —— 报出来，别只说一句"未就绪"。
        $depsFile = Join-Path $Root 'app\deps.json'
        if (Test-Path $depsFile) {
            try {
                $dr = Get-Content $depsFile -Raw -Encoding UTF8 | ConvertFrom-Json
                if ($dr.error) {
                    Write-Log ('[start] 依赖自举失败原因：' + $dr.error)
                    if ($dr.hint) { Write-Log ('[start] 建议：' + $dr.hint) }
                }
            } catch { }
        }
    } else {
        Write-Log '[start] 启动器仍未返回，且服务未就绪'
    }
    return $null
}

function Remove-OldVersions {
    param([string]$Root, [int]$Keep, [string]$Current, [string]$Previous)
    $dir = Join-Path $Root 'app\versions'
    if (-not (Test-Path $dir)) { return @() }
    $all = @(Get-ChildItem $dir -Directory | Sort-Object Name -Descending)
    $kept = @()
    if ($Current) { $kept += $Current }
    if ($Previous -and ($kept -notcontains $Previous)) { $kept += $Previous }
    $extra = [Math]::Max(0, $Keep - $kept.Count)
    $removed = @()
    foreach ($d in $all) {
        if ($kept -contains $d.Name) { continue }
        if ($extra -gt 0) { $kept += $d.Name; $extra--; continue }
        try { Remove-Item -Recurse -Force $d.FullName; $removed += $d.Name } catch { }
    }
    return $removed
}

# Test-DirWritable 判断当前账号能不能往这个目录写文件（**真正动手试**，不信 ACL 推断）。
#
# 只 `Test-Path` 是不够的：目录存在不等于可写（只读、被 ACL 拒、被重定向到不可写位置）。
# 而“可探测”也不等于“可写”：别人的用户配置目录（`C:\Users\<别人>\Desktop`）
# 连 stat 都可能是 Access denied。所以：能建/删临时文件才算数，任何异常一律当“不可写”。
function Test-DirWritable([string]$Dir) {
    if ([string]::IsNullOrWhiteSpace($Dir)) { return $false }
    try {
        # Test-Path 自己就可能抛（父目录无 traverse 权限）——单独包一层。
        if (-not (Test-Path -LiteralPath $Dir -PathType Container -ErrorAction SilentlyContinue)) { return $false }
    } catch { return $false }
    $probe = Join-Path $Dir ('.bma-w-' + [Guid]::NewGuid().ToString('N').Substring(0, 8) + '.tmp')
    try {
        [System.IO.File]::WriteAllText($probe, '')
        Remove-Item -LiteralPath $probe -Force -ErrorAction SilentlyContinue
        return $true
    } catch { return $false }
}

# Get-DesktopDirs 返回**候选**桌面目录（按可信度排序，去重、只留存在且可写的）。
#
# 为什么不能只信 `[Environment]::GetFolderPath('Desktop')`（2026-11-16 实战：用户装了
# 却"桌面没图标"）：那玩意返回的是当前**进程账号**的桌面。典型翻车场景：
#   * 桌面被 OneDrive 接走 → 现桌面是 `%USERPROFILE%\OneDrive\Desktop`，而旧的
#     `%USERPROFILE%\Desktop` 目录仍然存在 → 快捷方式落进没人看的空壳目录；
#   * 用管理员/服务账号跑安装，跟自己平时登录的账号不是同一个。
# 所以：注册表 `User Shell Folders` 的 Desktop 是第一权威（explorer 实际用的），
# 其次 .NET API，再次 USERPROFILE / OneDrive 探测，最后公共桌面。
#
# **两道防线**（都是 2026-11-16 实战教训，一犯就整个函数静默失效）：
#   1. 每次探测/入库都 try/catch（`Test-Path` 在无 traverse 权限的父目录上会抛，
#      而 `$ErrorActionPreference='Stop'` 会把它变成终止性异常，连带整个建快捷方式中断）；
#   2. 只收**可写**目录（`Test-DirWritable`）。别人的用户配置目录连 stat 都可能被拒——
#      上一版把"其它用户的桌面"也当候选，就在 `C:\Users\WsiAccount\Desktop` 上
#      抛 Access denied，导致自己的桌面也没建成。
function Get-DesktopDirs {
    param([string]$Override)
    if (-not [string]::IsNullOrWhiteSpace($Override)) {
        $p = $Override
        try { $p = [Environment]::ExpandEnvironmentVariables($p) } catch { }
        if (Test-DirWritable $p) { return @($p) }
        Write-Log ("[install] warn: -DesktopDir 指定的目录不可写，忽略: " + $Override)
        return @()
    }

    $cands = New-Object System.Collections.Generic.List[string]
    $add = {
        param($p, [switch]$RequireWritable)
        if ([string]::IsNullOrWhiteSpace($p)) { return }
        # 展开 REG_EXPAND_SZ 里可能残留的 %USERPROFILE%
        try { $p = [Environment]::ExpandEnvironmentVariables($p) } catch { return }
        if (-not [System.IO.Path]::IsPathRooted($p)) { return }
        if ($cands.Contains($p)) { return }
        # 全部分支都要过可写门：不可写的一律不收，宁可少一处也不让上游抛异常。
        try {
            if (Test-DirWritable $p) { $cands.Add($p) }
        } catch { }
    }

    foreach ($regKey in @(
            'HKCU:\Software\Microsoft\Windows\CurrentVersion\Explorer\User Shell Folders',
            'HKCU:\Software\Microsoft\Windows\CurrentVersion\Explorer\Shell Folders')) {
        try {
            $v = (Get-ItemProperty -Path $regKey -Name 'Desktop' -ErrorAction SilentlyContinue).Desktop
            if ($v) { & $add $v }
        } catch { }
    }
    try { & $add ([Environment]::GetFolderPath('Desktop')) } catch { }
    try { & $add ([Environment]::GetFolderPath('DesktopDirectory')) } catch { }
    if ($env:USERPROFILE) { & $add (Join-Path $env:USERPROFILE 'Desktop') }
    if ($env:OneDrive) { & $add (Join-Path $env:OneDrive 'Desktop') }
    # 公共桌面：该机上**所有用户**的桌面都看得见（多账号机器最可靠的一处）。
    if ($env:PUBLIC) { & $add (Join-Path $env:PUBLIC 'Desktop') }
    return $cands
}

# Get-StartMenuDirs 返回开始菜单目录（始终有入口，不依赖桌面重定向是否猜对）。
function Get-StartMenuDirs {
    $cands = New-Object System.Collections.Generic.List[string]
    $add = {
        param($p)
        if ([string]::IsNullOrWhiteSpace($p)) { return }
        try { $p = [Environment]::ExpandEnvironmentVariables($p) } catch { return }
        if (-not [System.IO.Path]::IsPathRooted($p)) { return }
        if ($cands.Contains($p)) { return }
        # 开始菜单目录可能尚不存在（首次登录的新账号）→ 能建就建，建不了就跳过。
        try {
            if (-not (Test-Path -LiteralPath $p -PathType Container -ErrorAction SilentlyContinue)) {
                New-Item -ItemType Directory -Force -Path $p -ErrorAction Stop | Out-Null
            }
            $cands.Add($p)
        } catch { }
    }
    try { & $add ([Environment]::GetFolderPath('Programs')) } catch { }
    if ($env:APPDATA) { & $add (Join-Path $env:APPDATA 'Microsoft\Windows\Start Menu\Programs') }
    if ($env:ProgramData) { & $add (Join-Path $env:ProgramData 'Microsoft\Windows\Start Menu\Programs') }
    return $cands
}

# New-AppShortcuts 建桌面 + 开始菜单入口，并**校验落盘**后才报成功。
#
# 原来的坑：只往 `GetFolderPath('Desktop')` 写一个，写完不检查就打印"已创建快捷方式"，
# 桌面被 OneDrive 重定向时用户根本看不到（Powershell 也不报错）。现改为：
#   * 桌面 → 所有候选目录都建（顶级 "BlockMemoryAgent.lnk"）；
#   * 开始菜单 → 建 "BlockMemoryAgent" 文件夹 + 卸载项，搜索/开始菜单里总能找到；
#   * 每个 .lnk 落盘后 Test-Path 复核，日志里分开报"已建/失败"。
function New-AppShortcuts {
    param([string]$Root, [string]$Version, [string]$DesktopDir)
    # 优先指向桌面外壳（原生窗口 + 自带图标）；外壳缺失时退回 VBS 脚本启动器。
    $desktopExe = Join-Path $Root 'BMA-Desktop.exe'
    $vbs = Join-Path $Root 'BMA.vbs'
    $target = $null
    if (Test-Path $desktopExe) { $target = $desktopExe }
    elseif (Test-Path $vbs) { $target = $vbs }
    if (-not $target) {
        Write-Log ("[install] warn: 找不到 BMA-Desktop.exe / BMA.vbs，跳过快捷方式（$Root）")
        return
    }

    $icon = Join-Path $Root 'app\icon.ico'
    if (-not (Test-Path $icon)) { $icon = Join-Path $Root ('app\versions\' + $Version + '\bin\bma-server.exe') }

    $made = New-Object System.Collections.Generic.List[string]
    $failed = New-Object System.Collections.Generic.List[string]

    # 公共属性抽成一个脚本块；每处调用都**单独 try/catch**——
    # 绝不让一环失败拖垮全流程（上一版就是外层一个大 catch，别人的桌面抛
    # Access denied 后直接退出，连自己桌面都没建，还只报一句 warn）。
    $ws = $null
    try { $ws = New-Object -ComObject WScript.Shell } catch {
        Write-Log ("[install] warn: 无法创建 WScript.Shell COM（" + $_.Exception.Message + "），跳过快捷方式；直接双击 " + $target)
        return
    }
    $makeLnk = {
        param($lnkPath)
        $lnk = $ws.CreateShortcut($lnkPath)
        $lnk.TargetPath = $target
        $lnk.WorkingDirectory = $Root
        if (Test-Path $icon) { $lnk.IconLocation = ($icon + ',0') }
        $lnk.Description = 'BlockMemoryAgent'
        $lnk.Save()
        return [bool](Test-Path -LiteralPath $lnkPath -ErrorAction SilentlyContinue)
    }

    # 1) 桌面：所有**可写**候选目录都放一份（多账号/OneDrive 重定向时总有一个是对的）
    foreach ($dir in (Get-DesktopDirs -Override $DesktopDir)) {
        $lnkPath = Join-Path $dir 'BlockMemoryAgent.lnk'
        try {
            if (& $makeLnk $lnkPath) { $made.Add($lnkPath) } else { $failed.Add($lnkPath + ' (保存后文件不存在)') }
        } catch { $failed.Add($lnkPath + ' (' + $_.Exception.Message + ')') }
    }

    # 2) 开始菜单：文件夹里放程序 + 卸载（开始菜单搜索/所有应用里必能找到）
    foreach ($dir in (Get-StartMenuDirs)) {
        $folder = Join-Path $dir 'BlockMemoryAgent'
        $lnkPath = Join-Path $folder 'BlockMemoryAgent.lnk'
        try {
            New-Item -ItemType Directory -Force -Path $folder -ErrorAction Stop | Out-Null
            if (& $makeLnk $lnkPath) { $made.Add($lnkPath) } else { $failed.Add($lnkPath + ' (保存后文件不存在)') }
        } catch { $failed.Add($lnkPath + ' (' + $_.Exception.Message + ')') }
        # 卸载项独立 try：没有 unins000.exe（脚本安装）或写失败都不影响主入口。
        try {
            $uninst = Join-Path $Root 'unins000.exe'
            if (Test-Path $uninst) {
                $ulnk = $ws.CreateShortcut((Join-Path $folder '卸载 BlockMemoryAgent.lnk'))
                $ulnk.TargetPath = $uninst
                $ulnk.WorkingDirectory = $Root
                $ulnk.Save()
            }
        } catch { }
    }

    # 3) 安装目录里也放一份（从安装目录找入口的老习惯）
    $rootLnk = Join-Path $Root 'BlockMemoryAgent.lnk'
    try {
        if (& $makeLnk $rootLnk) { $made.Add($rootLnk) } else { $failed.Add($rootLnk + ' (保存后文件不存在)') }
    } catch { $failed.Add($rootLnk + ' (' + $_.Exception.Message + ')') }

    if ($made.Count -gt 0) {
        Write-Log ("[install] 已创建快捷方式（" + $made.Count + " 处）→ " + $target)
        foreach ($p in $made) { Write-Log ('         · ' + $p) }
    }
    if ($failed.Count -gt 0) {
        Write-Log "[install] warn: 以下位置上未能落盘（不影响其它入口）:"
        foreach ($p in $failed) { Write-Log ('         × ' + $p) }
    }
    if ($made.Count -eq 0) {
        Write-Log "[install] warn: 没建成任何快捷方式。请手动：右键 " + $target + " → 发送到 → 桌面快捷方式"
        Write-Log "[install]        或重跑：pwsh -File .\bma-app.ps1 shortcuts -Target " + $Root + " -DesktopDir <你想要的桌面路径>"
    }
}

function Invoke-Install {
    param([string]$Target, [string]$From, [int]$Keep, [int]$TimeoutSec, [string]$HttpAddr, [switch]$NoRestart, [switch]$NoShortcut, [switch]$NoShell)

    if ([string]::IsNullOrWhiteSpace($Target)) { $Target = Join-Path $env:LOCALAPPDATA 'Programs\BlockMemoryAgent' }
    $Target = [System.IO.Path]::GetFullPath($Target)
    if ((Test-Path (Join-Path $Target '.git')) -or (Test-Path (Join-Path $Target 'go.work'))) {
        throw "目标目录 $Target 看起来是源码仓库，拒绝安装（避免污染仓库数据）"
    }

    $pkgInfo = Resolve-Package $From
    $pkg = $pkgInfo.Path
    try {
        $meta = Read-JsonFile (Join-Path $pkg 'VERSION.json')
        if (-not $meta) { throw "包缺少 VERSION.json: $pkg" }
        $ver = $meta.version
        Write-Log "[install] 包 $ver（commit $($meta.commit)）→ $Target"

        foreach ($d in @('app', 'app\versions', 'config', 'plugins.d', 'logs', 'workspace')) {
            New-Item -ItemType Directory -Force -Path (Join-Path $Target $d) | Out-Null
        }

        $curPath = Join-Path $Target 'app\current.json'
        $prev = $null
        if (Test-Path $curPath) {
            $old = Read-JsonFile $curPath
            if ($old) {
                if ($old.version -eq $ver) { $prev = $old.previous } else { $prev = $old.version }
            }
        }

        # 桌面外壳先关（窗口连着旧后端，不关会在切换后白屏/报错）；记下它原本是否在跑
        $shellPids = Stop-DesktopShell $Target
        $hadShell = $shellPids.Count -gt 0
        if ($hadShell) { Write-Log ('[install] 已关闭桌面外壳: pid ' + ($shellPids -join ', ')) }

        $stopped = Stop-BmaServer $Target
        if ($stopped.Count -gt 0) { Write-Log ('[install] 已停止运行中的实例: pid ' + ($stopped -join ', ')) }

        # 换版本目录前先停掉该目录下的后台进程（自带 PG/Redis、媒体服务、媒体桥）：
        # 它们的可执行文件正被占用，目录删不掉（Access denied）。
        $stopExe = Join-Path $Target 'BMA-Desktop.exe'
        if (Test-Path $stopExe) {
            $sp = Start-Process -FilePath $stopExe -ArgumentList '--stop' -WindowStyle Hidden -PassThru
            [void]$sp.WaitForExit(60000)
        }

        # 1) 程序载荷 → 版本目录（不可变，重复安装同版本则整体替换该版本目录）
        $dst = Join-Path $Target ('app\versions\' + $ver)
        if (Test-Path $dst) { Remove-Item -Recurse -Force $dst }
        Copy-Item -Recurse -Force $pkg $dst
        Write-Log "[install] 版本目录: $dst"

        # 1b) 依赖编排（程序载荷，随版本覆盖）：桌面版自举 PostgreSQL/Redis 与媒体栈要用
        $dockerPkg = Join-Path $dst 'docker'
        $dockerOut = Join-Path $Target 'docker'
        if (Test-Path $dockerPkg) {
            if (Test-Path $dockerOut) { Remove-Item -Recurse -Force $dockerOut }
            New-Item -ItemType Directory -Force -Path $dockerOut | Out-Null
            Copy-Item -Recurse -Force (Join-Path $dockerPkg '*') $dockerOut
            Write-Log "[install] 依赖编排: $dockerOut"
        }

        # 1c) 自带运行时（PostgreSQL+pgvector / Redis / Node / 媒体桥 / Chromium）：铺到安装目录根部。
        # 运行时体积大（300MB+）且**与程序版本无关**——只在"载荷指纹变化"时重铺。
        # 判据 = .payload-id（配方号 + 载荷指纹）：程序升级但运行时没变 → 跳过重铺
        # （此前按"程序版本"比对，每次更新都重拷 300MB，白白多花几十秒）。
        $rtPkg = Join-Path $dst 'runtime'
        $rtOut = Join-Path $Target 'runtime'
        if (Test-Path $rtPkg) {
            $pkgMarker = Join-Path $rtPkg '.payload-id'
            $pkgId = ''
            if (Test-Path $pkgMarker) { $pkgId = (Get-Content $pkgMarker -Raw).Trim() }
            $installedId = ''
            $installedMarker = Join-Path $rtOut '.payload-id'
            if (Test-Path $installedMarker) { $installedId = (Get-Content $installedMarker -Raw).Trim() }
            if ($pkgId -and ($installedId -eq $pkgId)) {
                Write-Log '[install] 自带运行时未变化（载荷指纹一致），跳过重铺'
            } else {
                Write-Log "[install] 铺设自带运行时（较慢，约 300MB）..."
                if (Test-Path $rtOut) { Remove-Item -Recurse -Force $rtOut -ErrorAction SilentlyContinue }
                Copy-Item -Recurse -Force $rtPkg $rtOut
                Write-Log "[install] 自带运行时: $rtOut"
            }
        }

        # 2) 用户数据：只补缺，绝不覆盖
        $seeded = @()
        $tplDir = Join-Path $dst 'config-templates'
        if (Test-Path $tplDir) {
            foreach ($f in Get-ChildItem $tplDir -File) {
                if ($f.Name -eq 'env.example') { continue }
                $out = Join-Path (Join-Path $Target 'config') $f.Name
                if (-not (Test-Path $out)) {
                    Copy-Item -Force $f.FullName $out
                    $seeded += ('config\' + $f.Name)
                }
            }
            $envDst = Join-Path $Target '.env'
            if (-not (Test-Path $envDst)) {
                Copy-Item -Force (Join-Path $tplDir 'env.example') $envDst
                if (-not [string]::IsNullOrWhiteSpace($HttpAddr)) { Set-EnvValue $envDst 'HTTP_ADDR' $HttpAddr }
                $seeded += '.env'
            }
        }
        $tokenGenerated = Ensure-ApiToken (Join-Path $Target '.env')

        # plugins.yaml 是"程序侧模板 + 用户可改"的混合体：桌面版随包把容器类插件改成
        # 本地 stdio，所以升级时要能更新它——但**只在用户没改过时**更新。
        # 判据：与上次随包的那份（config\plugins.yaml.packaged）逐字节一致。
        # 没有参照副本时（首次安装）：与随包模板一致即视为"用户没改"（刚种子化的）。
        $pkgPlugins = Join-Path (Join-Path $Target 'config') 'plugins.yaml'
        $prevPlugins = Join-Path (Join-Path $Target 'config') 'plugins.yaml.packaged'
        $tplPlugins = Join-Path $tplDir 'plugins.yaml'
        if (Test-Path $tplPlugins) {
            $update = $false
            if (-not (Test-Path $pkgPlugins)) {
                $update = $true
            } else {
                $ref = $null
                if (Test-Path $prevPlugins) { $ref = $prevPlugins }
                elseif (Test-Path $tplPlugins) { $ref = $tplPlugins }
                if ($ref) {
                    $update = [System.Linq.Enumerable]::SequenceEqual(
                        [System.IO.File]::ReadAllBytes($pkgPlugins),
                        [System.IO.File]::ReadAllBytes($ref))
                }
            }
            if ($update) {
                Copy-Item -Force $tplPlugins $pkgPlugins
                Write-Log '[install] config\plugins.yaml 已随包更新（用户未修改过）'
            } else {
                Write-Log '[install] warn: config\plugins.yaml 已被修改，未随包更新（媒体插件可能仍是容器方式）'
            }
            # 参照副本始终刷新为随包版本（下次升级比对用）——不刷新的话，
            # 首装之后 .packaged 一直缺失，此后每次升级都会误判"用户改过"。
            Copy-Item -Force $tplPlugins $prevPlugins
        }

        # 3) 启动器与入口（随包更新）：优先桌面外壳，脚本启动器作为兜底
        Copy-WithRetry (Join-Path $dst 'launcher\launch.ps1') (Join-Path $Target 'app\launch.ps1')
        foreach ($l in @('BMA.cmd', 'BMA.vbs')) {
            Copy-WithRetry (Join-Path $dst ('launcher\' + $l)) (Join-Path $Target $l)
        }
        $desktopSrc = Join-Path $dst 'bin\bma-desktop.exe'
        if (Test-Path $desktopSrc) { Copy-WithRetry $desktopSrc (Join-Path $Target 'BMA-Desktop.exe') }
        $iconDst = Join-Path $Target 'app\icon.ico'
        if (Test-Path (Join-Path $dst 'launcher\icon.ico')) { Copy-WithRetry (Join-Path $dst 'launcher\icon.ico') $iconDst }

        # 4) 版本指针
        Write-JsonFile $curPath ([ordered]@{
                version      = $ver
                previous     = $prev
                installed_at = (Get-Date).ToString('o')
                commit       = $meta.commit
                package      = $pkg
            })

        # 5) 清理旧版本（保留当前 + 上一版 + 额外 Keep-2 个）
        $pruned = Remove-OldVersions -Root $Target -Keep $Keep -Current $ver -Previous $prev

        # 6) 桌面入口
        if (-not $NoShortcut) { New-AppShortcuts -Root $Target -Version $ver }

        # 7) 重启 + 健康门禁 + 失败回滚
        $health = $null
        $restarted = $false
        if (-not $NoRestart) {
            $health = Start-And-Wait -Root $Target -TimeoutSec $TimeoutSec -ExpectVersion $ver
            if (-not $health) {
                if ($prev -and (Test-Path (Join-Path $Target ('app\versions\' + $prev)))) {
                    Write-Log "[install] 新版本健康检查失败，回滚到 $prev"
                    Write-JsonFile $curPath ([ordered]@{
                            version      = $prev
                            previous     = $ver
                            installed_at = (Get-Date).ToString('o')
                            package      = 'rollback'
                        })
                    $health = Start-And-Wait -Root $Target -TimeoutSec $TimeoutSec -ExpectVersion $prev
                    throw "更新 $ver 失败，已回滚到 $prev（参见 $Target\logs 下的启动日志）"
                }
                throw "启动 $ver 失败，且没有可回滚的旧版本（参见 $Target\logs 下的启动日志）"
            }
            $restarted = $true
        }

        # 起桌面外壳。语义：**之前开着就拉回来**（保持"托盘常驻"，不要在更新时把它弄丢），
        # 但“之前没开”时也要拉起一个——安装/更新的终点应该是一个能用的窗口加托盘，
        # 而不是“后端在跑但用户面前什么都没有”（2026-11-16 用户实战：装完桌面没图标、
        # 也没开窗口，以为装失败）。安装机是交互桌面（非无人值守）就直接拉起。
        $shellRestarted = $false
        if ($restarted -and (-not $NoShell)) {
            $desktopExe = Join-Path $Target 'BMA-Desktop.exe'
            if (Test-Path $desktopExe) {
                $skipShell = $false
                # 无人值守/服务会话里起 GUI 会挂在一个看不见的会话里，且可能阻住安装旁
                # 一直等 —— N/A 的空闲时长 / 无交互会话时只提示不拉起。
                try {
                    if ([Environment]::UserInteractive -eq $false) { $skipShell = $true }
                    $sess = (Get-Process -Id $PID).SessionId
                    if ($sess -eq 0) { $skipShell = $true }
                } catch { }
                if ($skipShell) {
                    Write-Log ('[install] 当前是非交互会话，不自动开窗口；双击 ' + $desktopExe + '（或桌面快捷方式）即可启动')
                } else {
                    Start-Process -FilePath $desktopExe -WorkingDirectory $Target
                    $shellRestarted = $true
                    if ($hadShell) { Write-Log '[install] 桌面外壳已重启' }
                    else { Write-Log '[install] 桌面外壳已启动（窗口 + 托盘图标）' }
                }
            } else {
                Write-Log ('[install] warn: 缺少 ' + $desktopExe + '（桌面外壳未随包？）')
            }
        }

        if ($seeded.Count -gt 0) { Write-Log ('[install] 已种子拷贝用户数据: ' + ($seeded -join ', ')) }
        if ($pruned.Count -gt 0) { Write-Log ('[install] 已清理旧版本: ' + ($pruned -join ', ')) }
        Write-Log "[install] 完成：$ver @ $Target"

        return [ordered]@{
            ok              = $true
            command         = 'install'
            target          = $Target
            version         = $ver
            previous        = $prev
            restarted       = $restarted
            shell_restarted = $shellRestarted
            health          = $health
            seeded          = $seeded
            pruned_versions = $pruned
            api_token_new   = $tokenGenerated
        }
    } finally {
        if ($pkgInfo.Temp -and (Test-Path $pkgInfo.Temp)) { Remove-Item -Recurse -Force $pkgInfo.Temp -ErrorAction SilentlyContinue }
    }
}

# deploy = pack + update：一条命令完成"构建 → 切版本 → 重启后端 → 重启桌面外壳"。
function Invoke-Deploy {
    param([string]$Target, [string]$Version, [int]$Keep, [int]$TimeoutSec, [string]$HttpAddr, [switch]$SkipWeb, [switch]$Test, [switch]$NoShortcut, [switch]$NoShell, [switch]$NoRuntime, [switch]$NoFetch)
    Write-Log '[deploy] 1/2 打包 ...'
    $packed = Invoke-Pack -Version $Version -SkipWeb:$SkipWeb -Test:$Test -NoRuntime:$NoRuntime -NoFetch:$NoFetch
    Write-Log ('[deploy] 2/2 安装 ' + $packed.version + ' ...')
    $res = Invoke-Install -Target $Target -From $packed.package -Keep $Keep -TimeoutSec $TimeoutSec -HttpAddr $HttpAddr -NoShortcut:$NoShortcut -NoShell:$NoShell
    $res.command = 'deploy'
    $res.build = [ordered]@{ version = $packed.version; commit = $packed.commit; prompt_version = $packed.prompt_version }
    return $res
}

# Invoke-InstallOrBuild 统一的"装 / 更新"入口——用户只需要记住这一个动词：
#   * 不给 -From：就地构建（pack）再装（= deploy）。全新机器与开发机日常更新都走这条；
#     缺运行时源包时自动下载一次（Ensure-RuntimeSources）。
#   * -From 目录/zip：装现成的包（dist\app\bma-<版本> 或 dist\bma-app-<版本>.zip）。
#   * -From *.exe：跑现成的 Inno 安装包（静默），装完起服务并校验健康。
function Invoke-InstallOrBuild {
    param([string]$Target, [string]$From, [string]$Version, [int]$Keep, [int]$TimeoutSec, [string]$HttpAddr,
        [switch]$SkipWeb, [switch]$Test, [switch]$NoRestart, [switch]$NoShortcut, [switch]$NoShell,
        [switch]$NoRuntime, [switch]$NoFetch)

    if ([string]::IsNullOrWhiteSpace($From)) {
        return Invoke-Deploy -Target $Target -Version $Version -Keep $Keep -TimeoutSec $TimeoutSec -HttpAddr $HttpAddr `
            -SkipWeb:$SkipWeb -Test:$Test -NoShortcut:$NoShortcut -NoShell:$NoShell -NoRuntime:$NoRuntime -NoFetch:$NoFetch
    }
    if ($From.ToLower().EndsWith('.exe')) {
        return Invoke-InstallFromSetup -SetupExe $From -Target $Target -TimeoutSec $TimeoutSec -NoRestart:$NoRestart -NoShortcut:$NoShortcut
    }
    $res = Invoke-Install -Target $Target -From $From -Keep $Keep -TimeoutSec $TimeoutSec -HttpAddr $HttpAddr `
        -NoRestart:$NoRestart -NoShortcut:$NoShortcut -NoShell:$NoShell
    $res.command = 'install'
    return $res
}

# Invoke-InstallFromSetup 跑现成的 Inno 安装包（-From *.exe）。
#
# 注意：/VERYSILENT 等开关必须原样传给安装器——在 Git Bash 里直接写会被 MSYS
# 当成路径转换（变成 C:/Program Files/Git/VERYSILENT），安装器于是弹向导卡住。
# 用 Start-Process 传参就没有这个问题。
function Invoke-InstallFromSetup {
    param([string]$SetupExe, [string]$Target, [int]$TimeoutSec, [switch]$NoRestart, [switch]$NoShortcut)

    $exe = [System.IO.Path]::GetFullPath($SetupExe)
    if (-not (Test-Path $exe)) { throw "找不到安装包：$exe" }
    if ([string]::IsNullOrWhiteSpace($Target)) { $Target = Join-Path $env:LOCALAPPDATA 'Programs\BlockMemoryAgent' }
    $Target = [System.IO.Path]::GetFullPath($Target)
    Write-Log ("[install] 运行安装包 → " + $Target)
    $args = @('/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART', "/DIR=$Target")
    $p = Start-Process -FilePath $exe -ArgumentList $args -PassThru -Wait
    if ($p.ExitCode -ne 0) { throw ("安装包执行失败（退出码 " + $p.ExitCode + "）") }

    $cur = Read-JsonFile (Join-Path $Target 'app\current.json')
    $expect = ''
    if ($cur) { $expect = $cur.version }
    $health = $null
    if (-not $NoRestart) {
        # 静默安装不会自动启动应用（安装包的 [Run] 在静默模式下跳过）→ 这里起并校验。
        $health = Start-And-Wait -Root $Target -TimeoutSec $TimeoutSec -ExpectVersion $expect
        if (-not $health) { throw ("安装完成但后端未就绪（见 " + (Join-Path $Target 'logs') + "）") }
    }
    return [ordered]@{ ok = $true; command = 'install'; target = $Target; version = $expect; health = $health; setup = $exe }
}

# Invoke-Dist 打包 + 编译 Inno 安装包（一条命令产出可分发的自包含安装包）。
function Invoke-Dist {
    param([string]$Version, [switch]$SkipWeb, [switch]$Test, [switch]$NoRuntime, [switch]$NoFetch)

    $packed = Invoke-Pack -Version $Version -SkipWeb:$SkipWeb -Test:$Test -NoRuntime:$NoRuntime -NoFetch:$NoFetch
    $builder = Join-Path $PSScriptRoot 'installer\build-installer.ps1'
    if (-not (Test-Path $builder)) { throw '缺少 installer\build-installer.ps1' }
    Write-Log '[dist] 编译安装包（要几分钟，压缩 600MB 运行时）...'
    & pwsh -NoProfile -ExecutionPolicy Bypass -File $builder -Version $packed.version
    if ($LASTEXITCODE -ne 0) { throw ("安装包编译失败（退出码 " + $LASTEXITCODE + "）") }
    $setup = Join-Path $PSScriptRoot ('dist\installer\BlockMemoryAgent-Setup-' + $packed.version + '.exe')
    if (-not (Test-Path $setup)) { throw "未产出安装包：$setup" }
    $mb = [math]::Round((Get-Item $setup).Length / 1MB, 1)
    Write-Host ("安装包: " + $setup + "（" + $mb + " MB）")
    return [ordered]@{ ok = $true; command = 'dist'; version = $packed.version; setup = $setup; size_mb = $mb }
}

# ---------------- migrate-data ----------------

# Invoke-MigrateData 把某个 PostgreSQL 实例的数据搬进桌面版自带实例。
#
# 场景：桌面版切到"自带运行时"后是独立库（%LOCALAPPDATA%\BlockMemoryAgent\data\pg），
# 开发实例那套（Docker PG）里的会话与记忆不会自动跟过来 —— 用本命令一次性搬过去。
# 走 pg_dump/pg_restore（包内自带，版本与目标实例一致）。
function Invoke-MigrateData {
    param([string]$Target, [string]$FromDsn)

    $Target = Resolve-Target $Target
    $pgBin = Join-Path $Target 'runtime\postgres\bin'
    if (-not (Test-Path (Join-Path $pgBin 'pg_dump.exe'))) {
        throw "缺少 $pgBin\pg_dump.exe（安装包未带自带运行时）"
    }
    $envPath = Join-Path $Target '.env'
    $toDsn = Get-EnvValue $envPath 'POSTGRES_DSN'
    if ([string]::IsNullOrWhiteSpace($toDsn)) { throw "目标 .env 里没有 POSTGRES_DSN（先跑一次桌面版，让它初始化自带实例）" }
    if ([string]::IsNullOrWhiteSpace($FromDsn)) { throw '必须给 -FromDsn（源库的完整 DSN，含口令）' }

    $dump = Join-Path $env:TEMP ("bma_migrate_" + (Get-Date -Format 'yyyyMMdd_HHmmss') + '.dump')
    Write-Log "[migrate] 导出源库 → $dump"
    $env:PGPASSWORD = ([Uri]$FromDsn).UserInfo.Split(':') | Select-Object -Last 1
    & (Join-Path $pgBin 'pg_dump.exe') -h ([Uri]$FromDsn).Host -p ([Uri]$FromDsn).Port -U ([Uri]$FromDsn).UserInfo.Split(':')[0] -d ([Uri]$FromDsn).AbsolutePath.TrimStart('/') -Fc -f $dump
    if ($LASTEXITCODE -ne 0) { throw 'pg_dump 失败' }

    Write-Log '[migrate] 导入目标库（先清空同名表）...'
    $to = [Uri]$toDsn
    $env:PGPASSWORD = $to.UserInfo.Split(':') | Select-Object -Last 1
    & (Join-Path $pgBin 'pg_restore.exe') -h $to.Host -p $to.Port -U $to.UserInfo.Split(':')[0] -d $to.AbsolutePath.TrimStart('/') --clean --if-exists --no-owner $dump
    if ($LASTEXITCODE -ne 0) { throw 'pg_restore 失败（目标库非空时可能需要先停掉桌面版）' }
    Remove-Item $dump -Force -ErrorAction SilentlyContinue
    Write-Log '[migrate] 完成'
    return [ordered]@{ ok = $true; command = 'migrate-data'; target = $Target; from = $FromDsn }
}

# ---------------- media ----------------

# Invoke-Media 应用媒体生成配置（对齐 docker/bma-plugin.sh media，但作用于安装目录）。
# 唯一手改点 = <安装目录>\.env 的媒体段（IMAGE_PROVIDER/IMAGE_URL/IMAGE_KEY 等）：
#   daemon 侧 → 重建容器让 entrypoint 重写 media-config.json（env_file 仅创建时读取）；
#   桥侧     → 重建镜像（server.js 改动才需要，层缓存幂等）+ 换 keeper（共享容器按镜像
#               tag 复用，不换新会继续跑旧代码/旧 env）。
function Invoke-Media {
    param([string]$Target)

    $Target = Resolve-Target $Target
    $compose = Join-Path $Target 'docker\docker-compose.yml'
    if (-not (Test-Path $compose)) { throw "缺少 $compose（安装包不完整，请重新运行 install）" }
    $dockerCtx = Join-Path $Target 'docker'

    $bridge = Join-Path $dockerCtx 'open-design-mcp'
    if (Test-Path $bridge) {
        Write-Log '[media] 构建 bma/open-design-mcp:local ...'
        & docker build -t bma/open-design-mcp:local $bridge | Out-Null
        if ($LASTEXITCODE -ne 0) { throw '构建 bma/open-design-mcp:local 失败' }
    }

    Write-Log '[media] 拉起 minimax-media 适配桥 ...'
    & docker compose -f $compose up -d --build minimax-media
    if ($LASTEXITCODE -ne 0) { throw '启动 minimax-media 失败' }

    Write-Log '[media] 重建 open_design 容器（entrypoint 重写 media-config.json）...'
    & docker compose -f $compose up -d --build --force-recreate open_design
    if ($LASTEXITCODE -ne 0) { throw '重建 open_design 失败' }

    # 换 ui_design keeper：共享容器按镜像 tag 复用，不换新会继续跑旧代码/旧 env。
    $keepers = @(& docker ps -aq --filter name=bma-plugin-ui_design)
    if ($keepers.Count -gt 0) {
        & docker rm -f $keepers | Out-Null
        Write-Log '[media] 已换 ui_design keeper'
    }

    Write-Log '[media] 完成'
    return [ordered]@{ ok = $true; command = 'media'; target = $Target; compose = $compose }
}

# ---------------- publish ----------------

# Invoke-Publish 生成自动更新清单（latest.json）。
#
# 桌面外壳的自动更新读的就是这个 JSON（.env 的 BMA_UPDATE_URL 指过来）：
#   { "version": "...", "url": "...", "sha256": "...", "notes": "..." }
# 版本号用时间戳前缀（yyyyMMdd-HHmmss-<commit>），字符串比较即时间比较。
#
# 用法：
#   pwsh -File .\bma-app.ps1 publish -BaseUrl https://example.com/bma
#   → dist\installer\latest.json（连同安装包一起传到该地址即可）
function Invoke-Publish {
    param([string]$BaseUrl, [string]$Version, [string]$Notes)

    $installerDir = Join-Path $PSScriptRoot 'dist\installer'
    if (-not (Test-Path $installerDir)) { throw "缺少 $installerDir（先跑 pack + installer\build-installer.ps1）" }
    $exe = $null
    if ($Version) {
        $exe = Join-Path $installerDir ('BlockMemoryAgent-Setup-' + $Version + '.exe')
        if (-not (Test-Path $exe)) { throw "找不到安装包: $exe" }
    } else {
        $exe = (Get-ChildItem $installerDir -Filter 'BlockMemoryAgent-Setup-*.exe' |
                Sort-Object LastWriteTime -Descending | Select-Object -First 1).FullName
        if (-not $exe) { throw "找不到安装包（先跑 pack + installer\build-installer.ps1）" }
    }
    $file = Split-Path $exe -Leaf
    $ver = $file -replace '^BlockMemoryAgent-Setup-', '' -replace '\.exe$', ''
    $sha = (Get-FileHash -Algorithm SHA256 -Path $exe).Hash.ToLower()
    $url = ''
    if ($BaseUrl) {
        $url = $BaseUrl.TrimEnd('/') + '/' + $file
    }
    $manifest = [ordered]@{
        version = $ver
        url     = $url
        sha256  = $sha
        notes   = $Notes
    }
    $out = Join-Path $installerDir 'latest.json'
    Write-JsonFile $out $manifest
    Write-Log "[publish] $out"
    if (-not $url) { Write-Log '[publish] warn: 未给 -BaseUrl，清单里的 url 为空（桌面端拿到会拒绝安装）' }
    Write-Host ("更新清单: " + $out)
    return [ordered]@{ ok = $true; command = 'publish'; version = $ver; url = $url; sha256 = $sha; manifest = $out }
}

# ---------------- start / stop / status / rollback ----------------

function Resolve-Target([string]$Target) {
    if ([string]::IsNullOrWhiteSpace($Target)) { return (Join-Path $env:LOCALAPPDATA 'Programs\BlockMemoryAgent') }
    return [System.IO.Path]::GetFullPath($Target)
}

function Invoke-Start {
    param([string]$Target, [int]$TimeoutSec)
    $Target = Resolve-Target $Target
    $cur = Read-JsonFile (Join-Path $Target 'app\current.json')
    if (-not $cur) { throw "未安装（缺少 $Target\app\current.json）" }
    $h = Start-And-Wait -Root $Target -TimeoutSec $TimeoutSec -ExpectVersion $cur.version
    if (-not $h) { throw "启动失败（版本 $($cur.version)）" }
    return [ordered]@{ ok = $true; command = 'start'; target = $Target; version = $cur.version; health = $h }
}

function Invoke-Stop {
    param([string]$Target)
    $Target = Resolve-Target $Target
    $stopped = Stop-BmaServer $Target
    return [ordered]@{ ok = $true; command = 'stop'; target = $Target; stopped_pids = $stopped }
}

function Invoke-Status {
    param([string]$Target)
    $Target = Resolve-Target $Target
    $cur = Read-JsonFile (Join-Path $Target 'app\current.json')
    $rt = Read-JsonFile (Join-Path $Target 'app\runtime.json')
    $versions = @()
    $vd = Join-Path $Target 'app\versions'
    if (Test-Path $vd) { $versions = @(Get-ChildItem $vd -Directory | Select-Object -ExpandProperty Name | Sort-Object -Descending) }
    $running = $false
    if ($rt -and $rt.pid) { $running = ($null -ne (Get-Process -Id $rt.pid -ErrorAction SilentlyContinue)) }
    $h = Get-Health $Target
    return [ordered]@{
        ok        = $true
        command   = 'status'
        target    = $Target
        installed = $cur
        runtime   = $rt
        running   = $running
        http      = (Get-BaseUrl $Target)
        health    = $h
        versions  = $versions
    }
}

function Invoke-Rollback {
    param([string]$Target, [int]$TimeoutSec)
    $Target = Resolve-Target $Target
    $curPath = Join-Path $Target 'app\current.json'
    $cur = Read-JsonFile $curPath
    if (-not $cur) { throw "未安装（缺少 $curPath）" }
    if ([string]::IsNullOrWhiteSpace($cur.previous)) {
        throw ('没有可回滚的上一版本（指针 app\current.json 的 previous 为空）。' +
               '若是安装包装的，回退办法是：用旧版安装包覆盖安装 —— bma-app.ps1 install -From <旧 setup.exe> -Target ' + $Target)
    }
    $prev = $cur.previous
    if (-not (Test-Path (Join-Path $Target ('app\versions\' + $prev)))) {
        throw ("上一版本 $prev 的程序目录已不存在（可能已被清理）——" +
               "用旧版安装包覆盖安装即可回退： bma-app.ps1 install -From <旧 setup.exe> -Target " + $Target)
    }

    Stop-DesktopShell $Target | Out-Null
    Stop-BmaServer $Target | Out-Null
    Write-JsonFile $curPath ([ordered]@{
            version      = $prev
            previous     = $cur.version
            installed_at = (Get-Date).ToString('o')
            package      = 'rollback'
        })
    $h = Start-And-Wait -Root $Target -TimeoutSec $TimeoutSec -ExpectVersion $prev
    if (-not $h) { throw "回滚到 $prev 后健康检查失败（见 $Target\logs）" }
    Write-Host ("已回退到: " + $prev)
    return [ordered]@{ ok = $true; command = 'rollback'; target = $Target; version = $prev; previous = $cur.version; health = $h }
}

# New-Shortcuts 命令实现：给已装好的实例重建桌面/开始菜单入口。
#
# 存在的理由：快捷方式属"外部系统状态"（桌面重定向、权限、被误删），可能是装完才坏的，
# 而为了补一个 .lnk 跑一遍完整 install（重新打包 + 重铺运行时）代价太大。
function Invoke-Shortcuts {
    param([string]$Target, [string]$DesktopDir, [switch]$NoShortcut)
    $Target = Resolve-Target $Target
    $cur = Read-JsonFile (Join-Path $Target 'app\current.json')
    if (-not $cur) { throw "未安装（缺少 $Target\app\current.json）" }
    if ($NoShortcut) { throw '-NoShortcut 与 shortcuts 命令冲突' }
    New-AppShortcuts -Root $Target -Version $cur.version -DesktopDir $DesktopDir
    return [ordered]@{
        ok = $true; command = 'shortcuts'; target = $Target; version = $cur.version
        desktop_dirs = @(Get-DesktopDirs -Override $DesktopDir)
    }
}

# ---------------- dispatch ----------------

# 代理：设进环境变量，子进程（npm / playwright / go / fetch-runtime）全部继承。
# .NET 的 HttpClient 不认 HTTPS_PROXY（那是 curl/npm 的约定），fetch-runtime.ps1
# 里显式把它转成 WebProxy——两处都要有才都能走代理。
if (-not [string]::IsNullOrWhiteSpace($Proxy)) {
    $env:HTTPS_PROXY = $Proxy
    $env:HTTP_PROXY = $Proxy
    $env:https_proxy = $Proxy
    $env:http_proxy = $Proxy
    Write-Log ('[proxy] 使用代理 ' + $Proxy + '（npm / playwright / go / 运行时源包下载都会走它）')
}

$result = $null
try {
    switch ($Command) {
        'pack' { $result = Invoke-Pack -Version $Version -SkipWeb:$SkipWeb -Test:$Test -NoZip:$NoZip -NoRuntime:$NoRuntime -NoFetch:$NoFetch }
        'install' { $result = Invoke-InstallOrBuild -Target $Target -From $From -Version $Version -Keep $Keep -TimeoutSec $TimeoutSec -HttpAddr $HttpAddr -SkipWeb:$SkipWeb -Test:$Test -NoRestart:$NoRestart -NoShortcut:$NoShortcut -NoShell:$NoShell -NoRuntime:$NoRuntime -NoFetch:$NoFetch }
        'update' { $result = Invoke-InstallOrBuild -Target $Target -From $From -Version $Version -Keep $Keep -TimeoutSec $TimeoutSec -HttpAddr $HttpAddr -SkipWeb:$SkipWeb -Test:$Test -NoRestart:$NoRestart -NoShortcut:$NoShortcut -NoShell:$NoShell -NoRuntime:$NoRuntime -NoFetch:$NoFetch }
        'deploy' { $result = Invoke-InstallOrBuild -Target $Target -From $From -Version $Version -Keep $Keep -TimeoutSec $TimeoutSec -HttpAddr $HttpAddr -SkipWeb:$SkipWeb -Test:$Test -NoRestart:$NoRestart -NoShortcut:$NoShortcut -NoShell:$NoShell -NoRuntime:$NoRuntime -NoFetch:$NoFetch }
        'dist' { $result = Invoke-Dist -Version $Version -SkipWeb:$SkipWeb -Test:$Test -NoRuntime:$NoRuntime -NoFetch:$NoFetch }
        'start' { $result = Invoke-Start -Target $Target -TimeoutSec $TimeoutSec }
        'stop' { $result = Invoke-Stop -Target $Target }
        'restart' {
            Invoke-Stop -Target $Target | Out-Null
            $result = Invoke-Start -Target $Target -TimeoutSec $TimeoutSec
            $result.command = 'restart'
        }
        'status' { $result = Invoke-Status -Target $Target }
        'rollback' { $result = Invoke-Rollback -Target $Target -TimeoutSec $TimeoutSec }
        'media' { $result = Invoke-Media -Target $Target }
        'migrate-data' { $result = Invoke-MigrateData -Target $Target -FromDsn $FromDsn }
        'publish' { $result = Invoke-Publish -BaseUrl $BaseUrl -Version $Version -Notes $Notes }
        'shortcuts' { $result = Invoke-Shortcuts -Target $Target -DesktopDir $DesktopDir -NoShortcut:$NoShortcut }
    }
} catch {
    $message = $_.Exception.Message
    if ($script:JsonOutput) {
        Write-Output (([ordered]@{ ok = $false; command = $Command; error = $message }) | ConvertTo-Json -Depth 8)
    } else {
        Write-Error $message
    }
    exit 1
}

if ($script:JsonOutput) {
    Write-Output ($result | ConvertTo-Json -Depth 8)
} else {
    switch ($Command) {
        'pack' { Write-Host ("打包完成: " + $result.package) }
        'status' {
            Write-Host ("安装目录: " + $result.target)
            if ($result.installed) {
                # installed_at 只有 bma-app.ps1 安装路径会写；Inno 安装器写的指针没有它。
                $meta = "commit " + $result.installed.commit
                if ($result.installed.installed_at) { $meta += "，安装于 " + $result.installed.installed_at }
                Write-Host ("当前版本: " + $result.installed.version + "（" + $meta + "）")
            }
            else { Write-Host '当前版本: 未安装' }
            Write-Host ("运行中: " + $result.running)
            if ($result.health) { Write-Host ("健康: " + $result.http + " build=" + $result.health.build.version + " pid=" + $result.health.pid) }
            else { Write-Host ("健康: 未响应（" + $result.http + "）") }
            if ($result.versions) { Write-Host ("本地版本: " + ($result.versions -join ', ')) }
        }
        'stop' { Write-Host ("已停止: " + ($(if ($result.stopped_pids) { $result.stopped_pids -join ', ' } else { '（无运行中进程）' }))) }
        default { Write-Host ("完成: " + $result.command) }
    }
}
exit 0
