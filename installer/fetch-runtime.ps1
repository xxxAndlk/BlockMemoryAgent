<#
.SYNOPSIS
下载自带运行时源包（PostgreSQL 免安装版 + pgvector 扩展 + Redis for Windows）。

.DESCRIPTION
桌面版要在目标机器上免装 Docker 运行，就得以子进程方式跑 PostgreSQL(pgvector) 与 Redis。
这三个二进制不进 git（体积大、且是第三方再分发物），首次打包前拉一次到
dist\runtime-cache\ 即可；bma-app.ps1 pack 会自动解压裁剪并打进包。

注意：随包的 Node / Playwright(+Chromium headless shell) / computer-use-mcp 是
npm 包，由 bma-app.ps1 pack 在构建期现装（缓存于 dist\runtime-cache\prepared\），
不在本脚本的下载范围内。

来源与许可：
  * PostgreSQL 16 免安装版 —— EnterpriseDB 官方 binaries zip（PostgreSQL License）
  * pgvector 0.8.7 —— 社区 Windows 预编译包（PostgreSQL License）
  * Redis 5.0.14.1 for Windows —— tporadowski 移植版（BSD-3）

.EXAMPLE
  pwsh -File .\installer\fetch-runtime.ps1
#>
param(
    [switch]$Force
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
$cache = Join-Path $repo 'dist\runtime-cache'
New-Item -ItemType Directory -Force -Path $cache | Out-Null

$items = @(
    @{ name = 'pg16.zip';          url = 'https://get.enterprisedb.com/postgresql/postgresql-16.4-1-windows-x64-binaries.zip' },
    @{ name = 'pgvector-pg16.zip'; url = 'https://github.com/andreiramani/pgvector_pgsql_windows/releases/download/0.8.7/vector.v0.8.7-pg16.zip' },
    @{ name = 'redis-win.zip';     url = 'https://github.com/tporadowski/redis/releases/download/v5.0.14.1/Redis-x64-5.0.14.1.zip' }
)

foreach ($it in $items) {
    $dst = Join-Path $cache $it.name
    if ((Test-Path $dst) -and (-not $Force)) {
        $mb = [math]::Round((Get-Item $dst).Length / 1MB, 1)
        Write-Host "[fetch] 已存在 $($it.name)（$mb MB），跳过"
        continue
    }
    Write-Host "[fetch] 下载 $($it.name) ..."
    $tmp = "$dst.part"
    if (Test-Path $tmp) { Remove-Item $tmp -Force }
    Invoke-WebRequest -Uri $it.url -OutFile $tmp -UseBasicParsing
    Move-Item -Force $tmp $dst
    $mb = [math]::Round((Get-Item $dst).Length / 1MB, 1)
    Write-Host "[fetch] 完成 $($it.name)（$mb MB）"
}

Write-Host '[fetch] 运行时源包就绪，下一步： pwsh -File .\bma-app.ps1 pack'

# WebView2 引导安装程序（目标机缺运行时时由安装器静默补装；exe 不进 git）
$wvDir = Join-Path $PSScriptRoot 'webview2'
New-Item -ItemType Directory -Force -Path $wvDir | Out-Null
$wvExe = Join-Path $wvDir 'MicrosoftEdgeWebview2Setup.exe'
if ((Test-Path $wvExe) -and (-not $Force)) {
    Write-Host '[fetch] WebView2 引导程序已存在，跳过'
} else {
    Write-Host '[fetch] 下载 WebView2 引导程序 ...'
    Invoke-WebRequest -Uri 'https://go.microsoft.com/fwlink/p/?LinkId=2124703' -OutFile $wvExe -UseBasicParsing
    Write-Host '[fetch] 完成'
}
