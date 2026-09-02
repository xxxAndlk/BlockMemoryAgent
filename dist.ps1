# BMA 打包脚本:PowerShell 版 make dist(机器无 make 时使用)。
# 构建 web 前端 + 后端二进制,生成 dist/ 安装布局;完成后运行 install.ps1 安装。
$ErrorActionPreference = "Stop"
$repo = $PSScriptRoot

# web 前端
Push-Location "$repo\web"
npm install; if ($LASTEXITCODE -ne 0) { throw "npm install 失败" }
npm run build; if ($LASTEXITCODE -ne 0) { throw "npm run build 失败" }
Pop-Location

# 后端二进制(GOTOOLCHAIN=local,见 CLAUDE.md 约定)
$env:GOTOOLCHAIN = "local"
New-Item -ItemType Directory -Force -Path "$repo\dist\bin" | Out-Null
Push-Location "$repo\backend"
go build -o ../dist/bin/bma-server.exe .; if ($LASTEXITCODE -ne 0) { throw "go build bma-server 失败" }
go build -o ../dist/bin/tui.exe ./cmd/tui; if ($LASTEXITCODE -ne 0) { throw "go build tui 失败" }
Pop-Location

# dist 布局:config(含 skills_learned)/ web 产物 / plugins.d
New-Item -ItemType Directory -Force -Path "$repo\dist\config", "$repo\dist\web", "$repo\dist\plugins.d" | Out-Null
Copy-Item -Force "$repo\config\*.yaml", "$repo\config\soul.md", "$repo\config\user_profile.md" "$repo\dist\config\"
if (Test-Path "$repo\dist\config\skills_learned") { Remove-Item -Recurse -Force "$repo\dist\config\skills_learned" }
Copy-Item -Recurse "$repo\config\skills_learned" "$repo\dist\config\skills_learned"
if (Test-Path "$repo\dist\web\dist") { Remove-Item -Recurse -Force "$repo\dist\web\dist" }
Copy-Item -Recurse "$repo\web\dist" "$repo\dist\web\dist"

Write-Host "dist/ 布局完成,运行 install.ps1 安装"
