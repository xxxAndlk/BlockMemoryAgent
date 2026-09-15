# BMA 安装脚本:复制 dist/ 到安装目录并写入 BMA_HOME 用户环境变量。幂等,可重复运行(升级)。
# TODO #18-1 T28 装机向导段:交互补填 LLM Key、自动生成 BMA_API_TOKEN、安装后能力探活。
$ErrorActionPreference = "Stop"
$default = "D:\WebApp\bma"
$target = Read-Host "安装目录 [$default]"
if ([string]::IsNullOrWhiteSpace($target)) { $target = $default }
$src = Join-Path $PSScriptRoot "dist"
if (-not (Test-Path (Join-Path $src "bin\tui.exe"))) { throw "未找到 dist\bin\tui.exe,请先运行 make dist" }

New-Item -ItemType Directory -Force -Path $target, "$target\bin", "$target\config", "$target\plugins.d", "$target\logs" | Out-Null
# 二进制与前端资源:覆盖(升级)
Copy-Item -Force "$src\bin\*" "$target\bin\"
if (Test-Path "$src\web\dist") { New-Item -ItemType Directory -Force -Path "$target\web" | Out-Null; if (Test-Path "$target\web\dist") { Remove-Item -Recurse -Force "$target\web\dist" }; Copy-Item -Force -Recurse "$src\web\dist" "$target\web\" }
# 配置:config/ 下全部文件每次直接覆盖(roles.yaml/models.json/plugins.yaml/skills.yaml 等
# 与二进制配套,旧版会缺新工具/新字段)。.env 不在此目录,仍只补缺不覆盖,保留现场密钥。
# skills_learned 例外:运行时产物(自进化技能),保留安装目录现场数据,不覆盖。
foreach ($f in Get-ChildItem "$src\config") {
    if ($f.Name -eq "skills_learned") { continue }
    $dst = Join-Path "$target\config" $f.Name
    Copy-Item -Force $f.FullName $dst
}
if (-not (Test-Path "$target\.env") -and (Test-Path "$PSScriptRoot\.env.example")) { Copy-Item "$PSScriptRoot\.env.example" "$target\.env" }

# ---- 装机向导:.env 按 key 就地替换/追加(幂等,回车=跳过保留现值) ----
function Set-EnvValue {
    param([string]$Path, [string]$Key, [string]$Value)
    $utf8NoBom = New-Object System.Text.UTF8Encoding($false)
    $lines = @(if (Test-Path $Path) { [System.IO.File]::ReadAllLines($Path) } else { @() })
    $hit = $false
    for ($i = 0; $i -lt $lines.Count; $i++) {
        if ($lines[$i] -match "^\s*$([regex]::Escape($Key))\s*=") { $lines[$i] = "$Key=$Value"; $hit = $true; break }
    }
    if (-not $hit) { $lines += "$Key=$Value" }
    [System.IO.File]::WriteAllLines($Path, $lines, $utf8NoBom)
}
function Get-EnvValue {
    param([string]$Path, [string]$Key)
    if (-not (Test-Path $Path)) { return "" }
    foreach ($l in [System.IO.File]::ReadAllLines($Path)) {
        if ($l -match "^\s*$([regex]::Escape($Key))\s*=(.*)$") { return $Matches[1].Trim() }
    }
    return ""
}

$envFile = Join-Path $target ".env"
# LLM Key(OpenAI 兼容后端,示例值视为未填):回车跳过,不覆盖现场密钥
$curKey = Get-EnvValue $envFile "OPENAI_API_KEY"
if ([string]::IsNullOrWhiteSpace($curKey) -or $curKey -like "*your-*") {
    $llmKey = Read-Host "LLM API Key (OPENAI_API_KEY,回车=稍后手动填 $envFile)"
    if (-not [string]::IsNullOrWhiteSpace($llmKey)) { Set-EnvValue $envFile "OPENAI_API_KEY" $llmKey }
}
# BMA_API_TOKEN:缺才生成(升级不换锁)
if ([string]::IsNullOrWhiteSpace((Get-EnvValue $envFile "BMA_API_TOKEN"))) {
    $token = [guid]::NewGuid().ToString("N") + [guid]::NewGuid().ToString("N").Substring(0, 16)
    Set-EnvValue $envFile "BMA_API_TOKEN" $token
    Write-Host "已生成 API Token(BMA_API_TOKEN)写入 $envFile —— 前端登录/客户端调用用它作 Bearer Token"
}

[Environment]::SetEnvironmentVariable("BMA_HOME", $target, "User")
# node 检测(host_computer_use 插件前置)
if (-not (Get-Command node -ErrorAction SilentlyContinue)) { Write-Warning "未检测到 node:host_computer_use 插件需要 Node 20+(https://nodejs.org)" }
Write-Host "安装完成: $target  (BMA_HOME 已写入用户环境变量,重开终端生效)"

# ---- 装机探活:服务已在跑就顺带核对六类能力(TODO #18-1),没跑只提示 ----
$httpAddr = Get-EnvValue $envFile "HTTP_ADDR"
if ([string]::IsNullOrWhiteSpace($httpAddr)) { $httpAddr = "127.0.0.1:10010" }
$base = "http://$httpAddr"
try {
    $health = Invoke-RestMethod -Uri "$base/api/health" -TimeoutSec 3
    Write-Host "检测到运行中的服务($base),开始能力自检..."
    $headers = @{}
    $token = Get-EnvValue $envFile "BMA_API_TOKEN"
    if (-not [string]::IsNullOrWhiteSpace($token)) { $headers["Authorization"] = "Bearer $token" }
    $caps = Invoke-RestMethod -Uri "$base/api/capabilities" -Headers $headers -TimeoutSec 5
    foreach ($it in $caps.items) {
        $mark = if ($it.ok) { "[OK]  " } else { "[FAIL]" }
        Write-Host "  $mark $($it.Name)  $($it.detail)"
        if (-not $it.ok -and $it.hint) { Write-Host "        -> $($it.hint)" -ForegroundColor Yellow }
    }
    if (-not $caps.all_ok) { Write-Warning "存在未就绪能力,按上方提示修复后重启服务即可(配置在 $target)" }
} catch {
    Write-Host "未检测到运行中的服务($base)。启动后访问 $base/api/capabilities 可核对六类能力(LLM/PG/Redis/嵌入/插件/工作目录)。"
}
