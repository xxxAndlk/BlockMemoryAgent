<#
.SYNOPSIS
编译 BlockMemoryAgent 桌面版安装包（Inno Setup）。

.DESCRIPTION
前置：
  1. pwsh -File .\bma-app.ps1 pack            （生成 dist\app\bma-<版本> 与 dist\setup\current）
  2. pwsh -File .\installer\fetch-runtime.ps1 （下载自带运行时源包）
  3. 已安装 Inno Setup 6（winget install JRSoftware.InnoSetup）

产出：dist\installer\BlockMemoryAgent-Setup-<版本>.exe

.EXAMPLE
  pwsh -File .\installer\build-installer.ps1
#>
param(
    [string]$Version,
    [switch]$Install
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo

# 找 Inno Setup 编译器
$iscc = @(
    'C:\Program Files (x86)\Inno Setup 6\ISCC.exe',
    'C:\Program Files\Inno Setup 6\ISCC.exe',
    "$env:LOCALAPPDATA\Programs\Inno Setup 6\ISCC.exe"
) | Where-Object { Test-Path $_ } | Select-Object -First 1

if (-not $iscc) {
    throw '未找到 Inno Setup 6（ISCC.exe）。安装： winget install JRSoftware.InnoSetup'
}

# 版本：优先取参数，否则取最新包的 VERSION.json
if (-not $Version) {
    # 排除 current（它是安装器的源副本，不是版本目录；否则会挑到自己，自我镜像等于没拷）
    $latest = Get-ChildItem (Join-Path $repo 'dist\setup') -Directory -ErrorAction SilentlyContinue |
        Where-Object { $_.Name -ne 'current' } |
        Sort-Object LastWriteTime -Descending | Select-Object -First 1
    if (-not $latest) { throw '没有可安装的暂存目录，先跑： pwsh -File .\bma-app.ps1 pack' }
    $Version = $latest.Name
}

$stage = Join-Path $repo ('dist\setup\' + $Version)
if (-not (Test-Path $stage)) { throw "安装器暂存目录不存在: $stage" }

# Inno Setup 的源目录固定为 dist\setup\current（iss 里写死的路径）
$current = Join-Path $repo 'dist\setup\current'
# 用 robocopy 镜像拷贝：Copy-Item 遇到"目录已存在且被占用"会直接失败
# （报"无法将容器复制到现有叶项上"），robocopy 能稳过去。
robocopy $stage $current /MIR /NFL /NDL /NJH /NJS /NP /R:1 /W:1 | Out-Null
if ($LASTEXITCODE -ge 8) { throw "robocopy 失败（退出码 $LASTEXITCODE）" }
$fileCount = (Get-ChildItem $current -Recurse -File | Measure-Object).Count
if ($fileCount -lt 10) { throw "安装器暂存目录内容异常（$current，只有 $fileCount 个文件）" }
Write-Host "[installer] 暂存目录就绪：$current（$fileCount 个文件）"

Write-Host "[installer] 编译版本 $Version ..."
& $iscc "/DMyAppVersion=$Version" 'installer\blockmemoryagent.iss'
if ($LASTEXITCODE -ne 0) { throw "Inno Setup 编译失败（退出码 $LASTEXITCODE）" }

$exe = Join-Path $repo ('dist\installer\BlockMemoryAgent-Setup-' + $Version + '.exe')
if (-not (Test-Path $exe)) { throw "未产出安装包: $exe" }
$mb = [math]::Round((Get-Item $exe).Length / 1MB, 1)
Write-Host "[installer] 产出: $exe（$mb MB）"

if ($Install) {
    Write-Host '[installer] 启动安装程序 ...'
    Start-Process -FilePath $exe -Wait
}
