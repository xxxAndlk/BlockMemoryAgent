# BMA 打包脚本:PowerShell 版 make dist(机器无 make 时使用)。
# 构建 web 前端 + 后端二进制,生成 dist/ 安装布局;完成后运行 install.ps1 安装。
$ErrorActionPreference = "Stop"
$repo = $PSScriptRoot

# web 前端
Push-Location "$repo\web"
# 调 npm.cmd / node+npm-cli.js，**不走 npm.ps1**：npm < 10.9.4 的 npm.ps1 在 `& npm ...`
# 形式下会把 `npm` 切错成 `pm`（npm/cli#8528），报 Unknown command: "pm"。
# --no-audit --no-fund：跳过 registry.npmjs.org 的 audit/fund 网络请求——国内网络到该源
# 时通时断，裸 install 会卡在重试上数分钟~十几分钟（install 本体其实 <1s~15s，2026-10-14 实测）。
$npmCmd = @(Get-Command npm.cmd -CommandType Application -ErrorAction SilentlyContinue) | Select-Object -First 1
if ($npmCmd) {
    $npmExe = $npmCmd.Source
    $npmPrefix = @()
} else {
    $nodeCmd = @(Get-Command node -CommandType Application -ErrorAction SilentlyContinue) | Select-Object -First 1
    if (-not $nodeCmd) { throw "找不到 npm（装 Node.js 18+ 并把 node 加进 PATH）" }
    $cli = Join-Path (Split-Path $nodeCmd.Source -Parent) 'node_modules\npm\bin\npm-cli.js'
    if (-not (Test-Path $cli)) { throw "找不到 npm：既无 npm.cmd 也无 $cli" }
    $npmExe = $nodeCmd.Source
    $npmPrefix = @($cli)
}
& $npmExe @npmPrefix install --no-audit --no-fund; if ($LASTEXITCODE -ne 0) { throw "npm install 失败" }
& $npmExe @npmPrefix run build; if ($LASTEXITCODE -ne 0) { throw "npm run build 失败" }
Pop-Location

# 后端二进制(GOTOOLCHAIN=local,见 CLAUDE.md 约定)
$env:GOTOOLCHAIN = "local"
New-Item -ItemType Directory -Force -Path "$repo\dist\bin" | Out-Null
Push-Location "$repo\backend"
go build -o ../dist/bin/bma-server.exe .; if ($LASTEXITCODE -ne 0) { throw "go build bma-server 失败" }
go build -o ../dist/bin/tui.exe ./cmd/tui; if ($LASTEXITCODE -ne 0) { throw "go build tui 失败" }
Pop-Location

# dist 布局:config / web 产物 / plugins.d
# skills_learned 是运行时产物(自进化技能),不入 dist、install 也不覆盖安装目录现场数据。
New-Item -ItemType Directory -Force -Path "$repo\dist\config", "$repo\dist\web", "$repo\dist\plugins.d" | Out-Null
Copy-Item -Force "$repo\config\*.yaml", "$repo\config\models.json", "$repo\config\soul.md", "$repo\config\user_profile.md" "$repo\dist\config\"
# 清掉旧版打包残留(若曾拷入),防 install 全量覆盖时误伤安装目录的 skills_learned
if (Test-Path "$repo\dist\config\skills_learned") { Remove-Item -Recurse -Force "$repo\dist\config\skills_learned" }
if (Test-Path "$repo\dist\web\dist") { Remove-Item -Recurse -Force "$repo\dist\web\dist" }
Copy-Item -Recurse "$repo\web\dist" "$repo\dist\web\dist"

Write-Host "dist/ 布局完成,运行 install.ps1 安装"
