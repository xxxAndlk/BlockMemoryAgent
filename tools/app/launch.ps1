# launch.ps1 — BlockMemoryAgent 桌面启动器（安装后位于 <安装目录>\app\launch.ps1）。
#
# 职责（单实例语义）：
#   1. 读 app\current.json 版本指针，确定要跑哪个版本；
#   2. 已在跑（/api/health 200）→ 直接开浏览器，不重复启动；
#   3. 未在跑 → 以 BMA_HOME=<安装目录> 启动该版本的 bin\bma-server.exe，等健康后开浏览器；
#   4. 写 app\runtime.json（pid/版本/端口），供 bma-app.ps1 status/stop 与外部调用方发现。
#
# 参数：
#   -NoBrowser   只启动不打开浏览器（自动化/脚本调用）
#   -Quiet       失败时不弹窗、不打开日志（交给调用方处理退出码）
#   -TimeoutSec  等待健康检查的超时（默认 120 秒）
param(
    [switch]$NoBrowser,
    [switch]$Quiet,
    [int]$TimeoutSec = 120
)
$ErrorActionPreference = 'Stop'

# <安装目录>\app\launch.ps1 → <安装目录>
$Root = Split-Path -Parent $PSScriptRoot

function Get-EnvValue([string]$Path, [string]$Key) {
    if (-not (Test-Path $Path)) { return '' }
    foreach ($l in [System.IO.File]::ReadAllLines($Path)) {
        if ($l -match "^\s*$([regex]::Escape($Key))\s*=(.*)$") { return $Matches[1].Trim() }
    }
    return ''
}

function Get-BaseUrl {
    $addr = Get-EnvValue (Join-Path $Root '.env') 'HTTP_ADDR'
    if ([string]::IsNullOrWhiteSpace($addr)) { $addr = '127.0.0.1:10010' }
    if ($addr.StartsWith(':')) { $addr = '127.0.0.1' + $addr }
    if ($addr -notmatch '^https?://') { $addr = 'http://' + $addr }
    return $addr
}

function Test-Health([string]$Base) {
    try { return Invoke-RestMethod -Uri "$Base/api/health" -TimeoutSec 2 } catch { return $null }
}

function Test-PortOpen([string]$Base) {
    try {
        $u = [Uri]$Base
        $c = New-Object System.Net.Sockets.TcpClient
        $c.Connect($u.Host, $u.Port)
        $c.Close()
        return $true
    } catch { return $false }
}

function Show-Failure([string]$Message, [string]$LogPath) {
    Write-Error $Message
    if ($Quiet) { return }
    # 桌面场景没有控制台：能定位日志就用记事本打开，否则弹消息框
    try {
        if ($LogPath -and (Test-Path $LogPath)) { Start-Process notepad.exe $LogPath; return }
        Add-Type -AssemblyName PresentationFramework -ErrorAction Stop
        [System.Windows.MessageBox]::Show($Message, 'BlockMemoryAgent 启动失败') | Out-Null
    } catch { }
}

$base = Get-BaseUrl

$existing = Test-Health $base
if ($existing) {
    if (-not $NoBrowser) { Start-Process $base }
    exit 0
}

# 端口被占但健康检查不通：可能是正在启动的实例，也可能是卡死的旧进程。先等 10 秒。
if (Test-PortOpen $base) {
    $deadline = (Get-Date).AddSeconds(10)
    while ((Get-Date) -lt $deadline) {
        Start-Sleep -Milliseconds 500
        $existing = Test-Health $base
        if ($existing) { if (-not $NoBrowser) { Start-Process $base }; exit 0 }
    }
    Show-Failure "端口 $base 已被占用但 /api/health 无响应（可能是卡死的旧进程，请用 bma-app.ps1 stop）"
    exit 1
}

# ---- 依赖自举（PostgreSQL / Redis）----
# 后端是严格启动：PG/Redis 连不上直接退出（装配依赖失败即退出）。桌面版要做到
# "双击即用"，起后端之前必须先把依赖拉起来。依赖实现只有一份（Go，桌面外壳），
# 这里通过 --ensure-deps 复用；外壳不在时按旧路直接起后端（失败信息会提示依赖）。
$depsExe = Join-Path $Root 'BMA-Desktop.exe'
$depsOut = Join-Path $Root 'app\deps.json'
if (Test-Path $depsExe) {
    Write-Host '[launch] 自举依赖（PostgreSQL / Redis）...'
    # 不用 Start-Process -Wait：它会等整棵进程树，而自举可能拉起 Docker Desktop
    # 这类常驻 GUI 进程，会把这里永久堵死。改成只等本进程退出。
    if (Test-Path $depsOut) { Remove-Item $depsOut -Force -ErrorAction SilentlyContinue }
    $dp = Start-Process -FilePath $depsExe -ArgumentList @('--ensure-deps', '--deps-out', $depsOut) -WindowStyle Hidden -PassThru
    $dp.WaitForExit()
    # 以 deps.json 为准（自举进程无论成败都会写它），退出码只作辅助。
    $depsOk = $false
    $msg = '依赖自举失败（PostgreSQL / Redis 未就绪）'
    $hint = ''
    if (Test-Path $depsOut) {
        try {
            # 必须显式 -Encoding UTF8：Windows PowerShell 5.1 默认按 ANSI(GBK) 读文件，
            # 会把报告里的中文读坏，JSON 直接解析失败（报"依赖未就绪"的假象）。
            $dr = Get-Content $depsOut -Raw -Encoding UTF8 | ConvertFrom-Json
            $depsOk = [bool]$dr.ok
            if ($dr.error) { $msg = $dr.error }
            if ($dr.hint) { $hint = $dr.hint }
        } catch { }
    }
    if (-not $depsOk) {
        if ($hint) { $msg = $msg + "`n`n建议：" + $hint }
        Show-Failure $msg $null
        exit 1
    }
}

$curPath = Join-Path $Root 'app\current.json'
if (-not (Test-Path $curPath)) {
    Show-Failure "缺少版本指针 $curPath（请先运行 bma-app.ps1 install -Target `"$Root`"）"
    exit 1
}
$cur = Get-Content $curPath -Raw -Encoding UTF8 | ConvertFrom-Json
$verDir = Join-Path $Root ('app\versions\' + $cur.version)
$exe = Join-Path $verDir 'bin\bma-server.exe'
if (-not (Test-Path $exe)) {
    Show-Failure "版本 $($cur.version) 缺少 bin\bma-server.exe（$verDir）"
    exit 1
}

$logDir = Join-Path $Root 'logs'
New-Item -ItemType Directory -Force -Path $logDir | Out-Null
$ts = Get-Date -Format 'yyyyMMdd_HHmmss'
$outLog = Join-Path $logDir "app_$ts.out.log"
$errLog = Join-Path $logDir "app_$ts.err.log"

# 启动方式：生成 app\run.cmd（设 BMA_HOME + 标准输出/错误重定向到日志文件），再用
# ShellExecute（Start-Process -FilePath <cmd>，不带任何 -Redirect*）拉起——新进程组、
# **不继承调用方句柄**。若直接在 PowerShell 里 Start-Process 服务本体，子进程会继承
# 调用方的 stdout 管道，`& powershell -File launch.ps1` 这类调用会一直等 EOF，表现为
# "启动永远不返回"并被误判失败（2026-10-30 实测）。
$runCmdPath = Join-Path $Root 'app\run.cmd'
$webDist = Join-Path $verDir 'web\dist'
$runLines = @(
    '@echo off',
    ('set "BMA_HOME=' + $Root + '"'),
    # 插件配置里的路径占位符（整串形式）：随包 Node + 三个本地 MCP 桥
    # （生图生视频 / Playwright 截图 / 真机 GUI 直控）。漏任何一个 → 插件命令变成
    # `node ""`，node 会把 stdin 当 JS 源码解析 → 握手超时（2026-11-16 实测）。
    ('set "BMA_NODE_EXE=' + (Join-Path $Root 'runtime\node\node.exe') + '"'),
    ('set "BMA_MEDIA_BRIDGE=' + (Join-Path $Root 'runtime\media\open-design-mcp\server.js') + '"'),
    ('set "BMA_UI_PREVIEW_MCP=' + (Join-Path $Root 'runtime\media\ui-preview-mcp\node_modules\@playwright\mcp\cli.js') + '"'),
    ('set "BMA_COMPUTER_USE_MCP=' + (Join-Path $Root 'runtime\media\computer-use-mcp\node_modules\@zavora-ai\computer-use-mcp\dist\launch.js') + '"'),
    ('set "BMA_PLAYWRIGHT_BROWSERS=' + (Join-Path $Root 'runtime\media\playwright-browsers') + '"'),
    ('cd /d "' + $Root + '"'),
    ('"' + $exe + '" -web-dist "' + $webDist + '" 1>>"' + $outLog + '" 2>>"' + $errLog + '"')
)
[System.IO.File]::WriteAllLines($runCmdPath, $runLines, [System.Text.Encoding]::Default)
Start-Process -FilePath $runCmdPath -WindowStyle Hidden

$deadline = (Get-Date).AddSeconds($TimeoutSec)
$health = $null
$serverPid = 0
while ((Get-Date) -lt $deadline) {
    Start-Sleep -Milliseconds 700
    $health = Test-Health $base
    if ($health) { break }
    # 进程已退出且日志有内容 → 不必等满超时
    $proc = Get-CimInstance Win32_Process -Filter "name='bma-server.exe'" -ErrorAction SilentlyContinue |
        Where-Object { $_.ExecutablePath -eq $exe } | Select-Object -First 1
    if (-not $proc -and (Test-Path $errLog) -and ((Get-Item $errLog).Length -gt 0)) { break }
}

if ($health) { $serverPid = [int]$health.pid }

[ordered]@{
    pid        = $serverPid
    version    = $cur.version
    http       = $base
    started_at = (Get-Date).ToString('o')
    exe        = $exe
    log        = $errLog
} | ConvertTo-Json | Set-Content (Join-Path $Root 'app\runtime.json') -Encoding UTF8

if (-not $health) {
    $runtime = Join-Path $Root 'app\runtime.json'
    if (Test-Path $runtime) { Remove-Item $runtime -Force -ErrorAction SilentlyContinue }
    $why = "启动失败：$TimeoutSec 秒内 $base/api/health 未就绪（日志 $errLog）"
    # 后端是严格启动：最常见原因是 PostgreSQL/Redis 没起（Docker 未运行）。
    $compose = Join-Path $Root 'docker\docker-compose.yml'
    if (Test-Path $compose) {
        $why = $why + "`n`n若日志提示 PostgreSQL/Redis 连接失败，请先启动 Docker Desktop 并执行：" +
            "`n  docker compose -f `"$compose`" up -d postgres redis"
    }
    Show-Failure $why $errLog
    exit 1
}

if (-not $NoBrowser) { Start-Process $base }
exit 0
