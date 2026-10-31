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

$curPath = Join-Path $Root 'app\current.json'
if (-not (Test-Path $curPath)) {
    Show-Failure "缺少版本指针 $curPath（请先运行 bma-app.ps1 install -Target `"$Root`"）"
    exit 1
}
$cur = Get-Content $curPath -Raw | ConvertFrom-Json
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
    Show-Failure "启动失败：$TimeoutSec 秒内 $base/api/health 未就绪（日志 $errLog）" $errLog
    exit 1
}

if (-not $NoBrowser) { Start-Process $base }
exit 0
