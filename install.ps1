# BMA 安装脚本:复制 dist/ 到安装目录并写入 BMA_HOME 用户环境变量。幂等,可重复运行(升级)。
$ErrorActionPreference = "Stop"
$default = "D:\WebApp\bma"
$target = Read-Host "安装目录 [$default]"
if ([string]::IsNullOrWhiteSpace($target)) { $target = $default }
$src = Join-Path $PSScriptRoot "dist"
if (-not (Test-Path (Join-Path $src "bin\tui.exe"))) { throw "未找到 dist\bin\tui.exe,请先运行 make dist" }

New-Item -ItemType Directory -Force -Path $target, "$target\bin", "$target\config", "$target\plugins.d", "$target\logs" | Out-Null
# 二进制与前端资源:覆盖(升级)
Copy-Item -Force "$src\bin\*" "$target\bin\"
if (Test-Path "$src\web\dist") { New-Item -ItemType Directory -Force -Path "$target\web" | Out-Null; Copy-Item -Force -Recurse "$src\web\dist" "$target\web\" }
# 配置:config/ 下全部文件每次直接覆盖(roles.yaml/models.json/plugins.yaml/skills.yaml 等
# 与二进制配套,旧版会缺新工具/新字段)。.env 不在此目录,仍只补缺不覆盖,保留现场密钥。
foreach ($f in Get-ChildItem "$src\config") {
    $dst = Join-Path "$target\config" $f.Name
    Copy-Item -Force $f.FullName $dst
}
if (-not (Test-Path "$target\.env") -and (Test-Path "$PSScriptRoot\.env.example")) { Copy-Item "$PSScriptRoot\.env.example" "$target\.env" }

[Environment]::SetEnvironmentVariable("BMA_HOME", $target, "User")
# node 检测(host_computer_use 插件前置)
if (-not (Get-Command node -ErrorAction SilentlyContinue)) { Write-Warning "未检测到 node:host_computer_use 插件需要 Node 20+(https://nodejs.org)" }
Write-Host "安装完成: $target  (BMA_HOME 已写入用户环境变量,重开终端生效)"
