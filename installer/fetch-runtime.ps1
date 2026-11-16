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
    [switch]$Force,
    # 代理（http://127.0.0.1:7890 这种）。缺省读环境变量 HTTPS_PROXY / HTTP_PROXY。
    [string]$Proxy,
    # 并行连接数。默认 1（单连接，走代理最稳）；宽带直连慢时可调大。
    [int]$Chunks = 1
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
$cache = Join-Path $repo 'dist\runtime-cache'
New-Item -ItemType Directory -Force -Path $cache | Out-Null

$items = @(
    # PG16 的 Windows 二进制只有 EDB 一个源（国内镜像站都没有），且单连接可能被限速
    # → 走 8 连接分块并行。
    @{ name = 'pg16.zip';          url = 'https://get.enterprisedb.com/postgresql/postgresql-16.4-1-windows-x64-binaries.zip'; big = $true },
    @{ name = 'pgvector-pg16.zip'; url = 'https://github.com/andreiramani/pgvector_pgsql_windows/releases/download/0.8.7/vector.v0.8.7-pg16.zip' },
    @{ name = 'redis-win.zip';     url = 'https://github.com/tporadowski/redis/releases/download/v5.0.14.1/Redis-x64-5.0.14.1.zip' },
    # Node 运行时：桌面版的三个本地 MCP 桥（生图生视频 / Playwright 截图 / 真机 GUI 直控）
    # 都靠它跑 —— 漏了这个源包，打出来的包装了也不能用（没有 Node）。
    @{ name = 'node-win.zip';     url = 'https://nodejs.org/dist/v24.22.3/node-v24.22.3-win-x64.zip' }
)

# Resolve-Proxy 决定走不走代理：显式 -Proxy 优先，其次环境变量
# HTTPS_PROXY / HTTP_PROXY / ALL_PROXY（这是各下载工具的通用约定，clash 用户
# 设一次 HTTPS_PROXY 就能让 npm / playwright / go 全部走代理）。
function Resolve-Proxy([string]$Explicit) {
    if (-not [string]::IsNullOrWhiteSpace($Explicit)) { return $Explicit.Trim() }
    foreach ($k in @('HTTPS_PROXY', 'https_proxy', 'HTTP_PROXY', 'http_proxy', 'ALL_PROXY', 'all_proxy')) {
        $v = [Environment]::GetEnvironmentVariable($k)
        if (-not [string]::IsNullOrWhiteSpace($v)) { return $v.Trim() }
    }
    return ''
}

# New-HttpClient 建带代理的 HttpClient。
#
# 注意：.NET 的 HttpClient **不会**自动读 HTTPS_PROXY 环境变量（那是 curl/npm 的约定），
# 必须显式塞 WebProxy —— 这里统一处理，代理才真的生效。
function New-HttpClient([string]$ProxyUrl, [int]$TimeoutSec = 7200) {
    $handler = New-Object System.Net.Http.HttpClientHandler
    if ($ProxyUrl) {
        $handler.UseProxy = $true
        $handler.Proxy = New-Object System.Net.WebProxy($ProxyUrl)
    } else {
        $handler.UseProxy = $false
    }
    $client = New-Object System.Net.Http.HttpClient($handler)
    $client.Timeout = [TimeSpan]::FromSeconds($TimeoutSec)
    return $client
}

# Invoke-DownloadWithProgress 单连接流式下载（带进度 + 断点续传 + 代理）。
#
# 为什么不用 Invoke-WebRequest -OutFile：它下载期间**一行输出都没有**，慢速网络下
# （EDB 的 PG16 包实测能慢到 20KB/s，323MB 要四五个小时）看起来就是卡死；
# 而且它不认我们显式给的代理。
function Invoke-DownloadWithProgress {
    param([string]$Url, [string]$OutFile, [int]$TimeoutSec = 7200, [long]$From = 0, [long]$To = 0, [string]$ProxyUrl = '')

    $client = New-HttpClient -ProxyUrl $ProxyUrl -TimeoutSec $TimeoutSec
    try {
        $req = New-Object System.Net.Http.HttpRequestMessage([System.Net.Http.HttpMethod]::Get, $Url)
        if ($From -gt 0) {
            # From>0 = 续传：To=0 表示"到文件末尾"。
            if ($To -gt $From) {
                $req.Headers.Range = New-Object System.Net.Http.Headers.RangeHeaderValue($From, $To)
            } else {
                $req.Headers.Range = New-Object System.Net.Http.Headers.RangeHeaderValue($From, $null)
            }
        }
        $resp = $client.SendAsync($req, [System.Net.Http.HttpCompletionOption]::ResponseHeadersRead).GetAwaiter().GetResult()
        if (-not $resp.IsSuccessStatusCode) { throw ("下载失败 HTTP " + [int]$resp.StatusCode + "：" + $Url) }
        $total = $resp.Content.Headers.ContentLength
        $src = $resp.Content.ReadAsStreamAsync().GetAwaiter().GetResult()
        $mode = if ($From -gt 0) { [System.IO.FileMode]::Append } else { [System.IO.FileMode]::Create }
        $dst = [System.IO.File]::Open($OutFile, $mode, [System.IO.FileAccess]::Write)
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
                    $got = [math]::Round(($From + $done) / 1MB, 1)
                    $tot = if ($total) { [math]::Round(($From + $total) / 1MB, 1) } else { '?' }
                    Write-Host ("  [fetch] {0} MB / {1} MB  {2} KB/s" -f $got, $tot, $speed)
                    $last = $now
                    $lastDone = $done
                }
            }
        } finally { $dst.Close(); $src.Close() }
    } finally { $client.Dispose() }
}

# Invoke-DownloadChunked 多连接分块并行下载（aria2 那一套）。
#
# 为什么：PG16 的 Windows 二进制**只有 EDB 一个源**（国内镜像站都没有），单连接
# 慢到 20KB/s 时 323MB 要四五个小时。CDN 常见的限速是**按连接**算的——开 8 条
# Range 连接同时下同一文件，吞吐能上去一个数量级。
#
# 断点续传：每块一个 <file>.partN 文件，已下满的块直接跳过（中断后重跑接着下）。
function Invoke-DownloadChunked {
    param([string]$Url, [string]$OutFile, [int]$Chunks = 8, [int]$TimeoutSec = 7200, [string]$ProxyUrl = '')

    # 先拿总长度（HEAD 拿不到就退回 GET 的 Content-Length）
    $client = New-HttpClient -ProxyUrl $ProxyUrl -TimeoutSec 60
    $total = 0L
    try {
        $resp = $client.GetAsync($Url, [System.Net.Http.HttpCompletionOption]::ResponseHeadersRead).GetAwaiter().GetResult()
        if ($resp.IsSuccessStatusCode) {
            $total = $resp.Content.Headers.ContentLength
            $resp.Content.ReadAsStreamAsync().GetAwaiter().GetResult().Dispose()
        }
    } catch { } finally { $client.Dispose() }
    if ($total -le 0) {
        Write-Host '  [fetch] 服务器未给长度信息，改用单连接下载'
        Invoke-DownloadWithProgress -Url $Url -OutFile $OutFile -TimeoutSec $TimeoutSec
        return
    }

    $size = [math]::Ceiling($total / $Chunks)
    $jobs = @()
    $jobParts = @()
    for ($i = 0; $i -lt $Chunks; $i++) {
        $from = [long]($i * $size)
        $to = [long][math]::Min(($i + 1) * $size - 1, $total - 1)
        if ($from -gt $to) { continue }
        $part = "$OutFile.part$i"
        $want = $to - $from + 1
        $have = 0L
        if (Test-Path $part) {
            $have = (Get-Item $part).Length
            if ($have -eq $want) { continue }      # 这块已下完
            if ($have -gt $want) { Remove-Item $part -Force; $have = 0L }
        }
        # 断点续传：这块已下了 $have 字节 → 从 $from+$have 接着下。
        $resumeFrom = $from + $have
        # 下载逻辑必须**内联**在 job 里：Start-ThreadJob 起的是新 runspace，
        # 拿不到外层定义的函数（踩过：job 全部失败）。
        $jobs += Start-ThreadJob -ScriptBlock {
            param($u, $p, $f, $t, $px)
            $handler = New-Object System.Net.Http.HttpClientHandler
            if ($px) { $handler.UseProxy = $true; $handler.Proxy = New-Object System.Net.WebProxy($px) } else { $handler.UseProxy = $false }
            $client = New-Object System.Net.Http.HttpClient($handler)
            $client.Timeout = [TimeSpan]::FromSeconds(7200)
            try {
                $req = New-Object System.Net.Http.HttpRequestMessage([System.Net.Http.HttpMethod]::Get, $u)
                $req.Headers.Range = New-Object System.Net.Http.Headers.RangeHeaderValue($f, $t)
                $resp = $client.SendAsync($req, [System.Net.Http.HttpCompletionOption]::ResponseHeadersRead).GetAwaiter().GetResult()
                if (-not $resp.IsSuccessStatusCode) { throw ("HTTP " + [int]$resp.StatusCode) }
                $src = $resp.Content.ReadAsStreamAsync().GetAwaiter().GetResult()
                $mode = if ((Test-Path $p) -and ((Get-Item $p).Length -gt 0)) { [System.IO.FileMode]::Append } else { [System.IO.FileMode]::Create }
                $dst = [System.IO.File]::Open($p, $mode, [System.IO.FileAccess]::Write)
                try { $src.CopyTo($dst) } finally { $dst.Close(); $src.Close() }
            } finally { $client.Dispose() }
        } -ArgumentList $Url, $part, $resumeFrom, $to, $ProxyUrl
        $jobParts += $part
    }
    if ($jobs.Count -eq 0) {
        Write-Host '  [fetch] 所有分块均已下完'
    } else {
        Write-Host ("  [fetch] {0} 连接并行下载 {1} MB ..." -f $jobs.Count, [math]::Round($total / 1MB, 1))
        # 聚合进度（各分块文件大小之和）+ 速度
        $last = Get-Date
        $lastDone = 0L
        while ($jobs | Where-Object { $_.State -eq 'Running' }) {
            Start-Sleep -Seconds 2
            $done = 0L
            foreach ($pf in $jobParts) {
                if (Test-Path $pf) { $done += (Get-Item $pf).Length }
            }
            $now = Get-Date
            $speed = [math]::Round(($done - $lastDone) / ($now - $last).TotalSeconds / 1KB, 1)
            Write-Host ("  [fetch] {0} MB / {1} MB（{2}%）  {3} KB/s" -f [math]::Round($done / 1MB, 1), [math]::Round($total / 1MB, 1), [math]::Round(100 * $done / $total, 1), $speed)
            $last = $now
            $lastDone = $done
        }
        $jobs | Wait-Job | Out-Null
        $failed = @($jobs | Where-Object { $_.State -ne 'Completed' -or $_.Error.Count -gt 0 })
        $jobs | Remove-Job -Force
        if ($failed.Count -gt 0) { throw ('分块下载失败（' + $failed.Count + ' 块），重跑本脚本可断点续传') }
    }
    # 合并分块
    $out = [System.IO.File]::Open($OutFile, [System.IO.FileMode]::Create, [System.IO.FileAccess]::Write)
    try {
        for ($i = 0; $i -lt $Chunks; $i++) {
            $part = "$OutFile.part$i"
            if (-not (Test-Path $part)) { continue }
            $in = [System.IO.File]::OpenRead($part)
            try { $in.CopyTo($out) } finally { $in.Close() }
            Remove-Item $part -Force
        }
    } finally { $out.Close() }
}

$proxyUrl = Resolve-Proxy $Proxy
if ($proxyUrl) {
    Write-Host ("[fetch] 走代理: " + $proxyUrl)
} else {
    Write-Host '[fetch] 未配置代理（直连）。慢就设 HTTPS_PROXY=http://127.0.0.1:<clash 端口>（或给 -Proxy）'
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
    # 断点续传：.part 已有内容就接着下（中断重跑不用从头来）。
    $have = 0L
    if (Test-Path $tmp) { $have = (Get-Item $tmp).Length }
    if ($Chunks -gt 1) {
        Invoke-DownloadChunked -Url $it.url -OutFile $tmp -Chunks $Chunks -ProxyUrl $proxyUrl
    } else {
        if ($have -gt 0) { Write-Host ("  [fetch] 续传（已有 " + [math]::Round($have / 1MB, 1) + " MB）") }
        Invoke-DownloadWithProgress -Url $it.url -OutFile $tmp -From $have -ProxyUrl $proxyUrl
    }
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
