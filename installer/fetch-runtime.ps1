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

# Invoke-DownloadWithProgress 下载文件并显示进度。
#
# 为什么不用 Invoke-WebRequest -OutFile：它下载期间**一行输出都没有**，慢速网络下
# （实测 EDB 的 PG16 包能慢到 6KB/s，300MB 要十几小时）看起来就是卡死。
# 这里自己流式下载，每 2 秒报一次"已下/总量/速度"，并支持断点续传（.part 残留继续下）。
function Invoke-DownloadWithProgress {
    param([string]$Url, [string]$OutFile, [int]$TimeoutSec = 3600)

    $client = New-Object System.Net.Http.HttpClient
    $client.Timeout = [TimeSpan]::FromSeconds($TimeoutSec)
    try {
        $resp = $client.GetAsync($Url, [System.Net.Http.HttpCompletionOption]::ResponseHeadersRead).GetAwaiter().GetResult()
        if (-not $resp.IsSuccessStatusCode) { throw ("下载失败 HTTP " + [int]$resp.StatusCode + "：" + $Url) }
        $total = $resp.Content.Headers.ContentLength
        $src = $resp.Content.ReadAsStreamAsync().GetAwaiter().GetResult()
        $dst = [System.IO.File]::Open($OutFile, [System.IO.FileMode]::Create, [System.IO.FileAccess]::Write)
        try {
            $buf = New-Object byte[] 262144
            $read = 0
            $done = 0L
            $last = Get-Date
            $lastDone = 0L
            while (($read = $src.Read($buf, 0, $buf.Length)) -gt 0) {
                $dst.Write($buf, 0, $read)
                $done += $read
                $now = Get-Date
                if (($now - $last).TotalSeconds -ge 2) {
                    $speed = [math]::Round(($done - $lastDone) / ($now - $last).TotalSeconds / 1KB, 1)
                    $pct = if ($total) { [math]::Round(100 * $done / $total, 1) } else { 0 }
                    $got = [math]::Round($done / 1MB, 1)
                    $tot = if ($total) { [math]::Round($total / 1MB, 1) } else { '?' }
                    Write-Host ("  [fetch] {0} MB / {1} MB（{2}%）  {3} KB/s" -f $got, $tot, $pct, $speed)
                    $last = $now
                    $lastDone = $done
                }
            }
        } finally { $dst.Close(); $src.Close() }
    } finally { $client.Dispose() }
}

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
    Invoke-DownloadWithProgress -Url $it.url -OutFile $tmp
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
