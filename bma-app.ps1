<#
.SYNOPSIS
BlockMemoryAgent 桌面版打包 / 安装 / 更新工具。

.DESCRIPTION
把仓库打成"版本化 + 可回滚"的桌面应用包，并安装或更新到目标目录。

安装目录布局（-Target，默认 %LOCALAPPDATA%\Programs\BlockMemoryAgent）：
  app\versions\<版本>\...    程序（bin + web\dist + 配置模板 + 启动器），不可变
  app\current.json           版本指针（含 previous，用于回滚）
  app\runtime.json           运行态（pid/版本/端口，启动器写入）
  app\launch.ps1             启动器（随包更新）
  BMA.cmd / BMA.vbs          桌面入口（快捷方式指向 BMA.vbs）
  config\  .env  plugins.d\  workspace\  logs\    ← 用户数据：只补缺，永不覆盖

仓库 → 安装目录的数据隔离：
  * 打包只取"程序 + 配置模板"，绝不包含 .env / config\models.json /
    config\user_profile.md / config\skills_learned / workspace / logs
    （这些是仓库本地的密钥与运行时状态）；
  * 安装/更新只对缺失的用户数据文件做种子拷贝，既有文件一律不动；
  * 拒绝安装到源码仓库目录（含 .git / go.work）。

.PARAMETER Command
  pack      构建并打包（默认输出 dist\app\bma-<版本>\ 与 dist\bma-app-<版本>.zip）
  install   安装到目标目录（首次），等同 update
  update    更新到目标目录（保留用户数据，健康门禁，失败自动回滚）
  start     启动当前版本（经 app\launch.ps1）
  stop      停止该安装目录下的后端进程
  restart   先停后起
  status    打印当前版本 / 运行态 / 健康检查（-Json 输出机器可读）
  rollback  切回上一版本并重启

.EXAMPLE
  .\bma-app.ps1 pack
  .\bma-app.ps1 install -Target D:\WebApp\BMA-Desktop
  .\bma-app.ps1 update -Target D:\WebApp\BMA-Desktop
  .\bma-app.ps1 status -Target D:\WebApp\BMA-Desktop -Json
#>
param(
    [Parameter(Position = 0)]
    [ValidateSet('pack', 'install', 'update', 'deploy', 'start', 'stop', 'restart', 'status', 'rollback')]
    [string]$Command = 'status',

    # 安装目录（install/update/start/stop/restart/status/rollback）
    [string]$Target,

    # 包来源（install/update）：包目录或 zip；缺省取 dist\app 下最新版本
    [string]$From,

    # 打包版本号（pack）：缺省 yyyyMMdd-HHmmss-<git短号>
    [string]$Version,

    # 保留的历史版本数（install/update）：默认 3（当前 + 上一版 + 1）
    [int]$Keep = 3,

    # 健康检查超时（秒）
    [int]$TimeoutSec = 180,

    # 首次安装时写入 .env 的 HTTP_ADDR（并行安装/换端口用，例如 127.0.0.1:10011）
    [string]$HttpAddr,

    [switch]$SkipWeb,     # pack：跳过前端构建（复用 web\dist）
    [switch]$Test,        # pack：额外跑 go test ./...
    [switch]$NoZip,       # pack：不生成 zip
    [switch]$NoRestart,   # install/update：装完不重启
    [switch]$NoShortcut,  # install/update/deploy：不建快捷方式
    [switch]$NoShell,     # install/update/deploy：不重启桌面外壳（默认会重启并保留"托盘常驻"体验）
    [switch]$Json         # 只输出 JSON（供外部 Agent 解析）
)

$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.IO.Compression.FileSystem

$script:JsonOutput = [bool]$Json

function Write-Log([string]$Message) {
    if (-not $script:JsonOutput) { Write-Host $Message }
}

function Read-JsonFile([string]$Path) {
    if (-not (Test-Path $Path)) { return $null }
    $raw = [System.IO.File]::ReadAllText($Path)
    if ([string]::IsNullOrWhiteSpace($raw)) { return $null }
    return $raw | ConvertFrom-Json
}

function Write-JsonFile([string]$Path, $Object) {
    $text = $Object | ConvertTo-Json -Depth 8
    [System.IO.File]::WriteAllText($Path, $text, (New-Object System.Text.UTF8Encoding($false)))
}

function Get-EnvValue([string]$Path, [string]$Key) {
    if (-not (Test-Path $Path)) { return '' }
    foreach ($l in [System.IO.File]::ReadAllLines($Path)) {
        if ($l -match "^\s*$([regex]::Escape($Key))\s*=(.*)$") { return $Matches[1].Trim() }
    }
    return ''
}

function Set-EnvValue([string]$Path, [string]$Key, [string]$Value) {
    $utf8NoBom = New-Object System.Text.UTF8Encoding($false)
    $lines = @()
    if (Test-Path $Path) { $lines = @([System.IO.File]::ReadAllLines($Path)) }
    $hit = $false
    for ($i = 0; $i -lt $lines.Count; $i++) {
        if ($lines[$i] -match "^\s*$([regex]::Escape($Key))\s*=") { $lines[$i] = "$Key=$Value"; $hit = $true; break }
    }
    if (-not $hit) { $lines += "$Key=$Value" }
    [System.IO.File]::WriteAllLines($Path, $lines, $utf8NoBom)
}

# API Token 只补缺：既有 .env 里的 token 永远不换（升级不换锁）。
function Ensure-ApiToken([string]$EnvPath) {
    if (-not (Test-Path $EnvPath)) { return $false }
    $cur = Get-EnvValue $EnvPath 'BMA_API_TOKEN'
    # 空值或 .env.example 的占位符都视为"没设"，生成随机 token；真实 token 永不更换。
    if (-not [string]::IsNullOrWhiteSpace($cur) -and ($cur -notlike 'change-me*') -and ($cur -notlike 'your-*')) { return $false }
    $token = [guid]::NewGuid().ToString('N') + [guid]::NewGuid().ToString('N').Substring(0, 16)
    Set-EnvValue $EnvPath 'BMA_API_TOKEN' $token
    Write-Log "[install] 已生成 BMA_API_TOKEN 写入 $EnvPath"
    return $true
}

function Get-GitCommit([string]$Repo) {
    try {
        $out = & git -C $Repo rev-parse --short HEAD 2>$null
        if ($LASTEXITCODE -eq 0 -and $out) { return (($out | Out-String).Trim()) }
    } catch { }
    return 'unknown'
}

function Get-PromptVersion([string]$Repo) {
    $p = Join-Path $Repo 'backend\pkg\prompts\prompts.go'
    if (Test-Path $p) {
        $m = Select-String -Path $p -Pattern 'const Version = "([^"]+)"' | Select-Object -First 1
        if ($m) { return $m.Matches[0].Groups[1].Value }
    }
    return 'unknown'
}

function Get-BaseUrl([string]$Root) {
    $addr = Get-EnvValue (Join-Path $Root '.env') 'HTTP_ADDR'
    if ([string]::IsNullOrWhiteSpace($addr)) { $addr = '127.0.0.1:10010' }
    if ($addr.StartsWith(':')) { $addr = '127.0.0.1' + $addr }
    if ($addr -notmatch '^https?://') { $addr = 'http://' + $addr }
    return $addr
}

function Get-Health([string]$Root) {
    try { return Invoke-RestMethod -Uri ((Get-BaseUrl $Root) + '/api/health') -TimeoutSec 3 } catch { return $null }
}

# ---------------- pack ----------------

function Invoke-Pack {
    param([string]$Version, [switch]$SkipWeb, [switch]$Test, [switch]$NoZip)

    $repo = $PSScriptRoot
    $commit = Get-GitCommit $repo
    if ([string]::IsNullOrWhiteSpace($Version)) { $Version = (Get-Date -Format 'yyyyMMdd-HHmmss') + '-' + $commit }
    $promptVersion = Get-PromptVersion $repo

    $stage = Join-Path $repo ('dist\app\bma-' + $Version)
    if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
    New-Item -ItemType Directory -Force -Path (Join-Path $stage 'bin'), (Join-Path $stage 'web\dist'), (Join-Path $stage 'config-templates'), (Join-Path $stage 'launcher') | Out-Null

    # 前端
    $webDist = Join-Path $repo 'web\dist'
    if (-not $SkipWeb) {
        Write-Log '[pack] 构建前端 ...'
        Push-Location (Join-Path $repo 'web')
        try {
            if (-not (Test-Path 'node_modules')) {
                & npm install --no-audit --no-fund
                if ($LASTEXITCODE -ne 0) { throw 'npm install 失败' }
            }
            & npm run build
            if ($LASTEXITCODE -ne 0) { throw 'npm run build 失败' }
        } finally { Pop-Location }
    }
    if (-not (Test-Path (Join-Path $webDist 'index.html'))) {
        throw '缺少 web/dist/index.html（先构建前端，或去掉 -SkipWeb）'
    }
    Copy-Item -Recurse -Force (Join-Path $webDist '*') (Join-Path $stage 'web\dist')

    # 应用图标 + Windows 资源（desktop exe 的图标来自 assets\icon.ico）
    $iconPath = Join-Path $repo 'assets\icon.ico'
    if (-not (Test-Path $iconPath)) {
        Write-Log '[pack] 生成应用图标 ...'
        & powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $repo 'tools\make-icon.ps1') | Out-Null
    }
    if (-not (Test-Path $iconPath)) { throw '缺少 assets\icon.ico（tools\make-icon.ps1 生成失败）' }
    $syso = Join-Path $repo 'backend\cmd\desktop\rsrc.syso'
    $needSyso = (-not (Test-Path $syso)) -or ((Get-Item $iconPath).LastWriteTime -gt (Get-Item $syso).LastWriteTime)
    if ($needSyso) {
        Write-Log '[pack] 生成 exe 图标资源 ...'
        Push-Location (Join-Path $repo 'backend')
        try { & go run github.com/akavel/rsrc@v0.10.2 -ico $iconPath -arch amd64 -o $syso } finally { Pop-Location }
        if ($LASTEXITCODE -ne 0) { throw 'rsrc 生成图标资源失败' }
    }

    # 后端（ldflags 注入构建标识，/api/health 的 build.version 据此校验更新是否生效）
    $env:GOTOOLCHAIN = 'local'
    $builtAt = (Get-Date).ToString('yyyy-MM-ddTHH:mm:sszzz')
    $ldflags = '-X github.com/blockmemory/agent/backend/internal/buildinfo.Version=' + $Version +
        ' -X github.com/blockmemory/agent/backend/internal/buildinfo.Commit=' + $commit +
        ' -X github.com/blockmemory/agent/backend/internal/buildinfo.BuiltAt=' + $builtAt
    Write-Log "[pack] 构建后端 $Version ..."
    Push-Location (Join-Path $repo 'backend')
    try {
        & go build ./...
        if ($LASTEXITCODE -ne 0) { throw 'go build ./... 失败' }
        if ($Test) {
            & go test ./...
            if ($LASTEXITCODE -ne 0) { throw 'go test ./... 失败' }
        }
        & go build -trimpath -ldflags $ldflags -o (Join-Path $stage 'bin\bma-server.exe') .
        if ($LASTEXITCODE -ne 0) { throw 'go build bma-server 失败' }
        & go build -trimpath -ldflags $ldflags -o (Join-Path $stage 'bin\tui.exe') ./cmd/tui
        if ($LASTEXITCODE -ne 0) { throw 'go build tui 失败' }

        # 桌面外壳（WebView2 原生窗口，自带图标）：GUI 子系统，避免黑框控制台
        $env:CGO_ENABLED = '0'
        & go build -trimpath -ldflags ('-H=windowsgui ' + $ldflags) -o (Join-Path $stage 'bin\bma-desktop.exe') ./cmd/desktop
        if ($LASTEXITCODE -ne 0) { throw 'go build bma-desktop 失败' }
    } finally { Pop-Location }

    # 配置模板（只放模板，不放仓库运行时数据）
    foreach ($t in @('config.yaml', 'roles.yaml', 'skills.yaml', 'soul.md', 'plugins.yaml', 'external_agents.yaml')) {
        $src = Join-Path $repo ('config\' + $t)
        if (Test-Path $src) { Copy-Item -Force $src (Join-Path $stage 'config-templates') }
        else { Write-Log "[pack] warn: 缺少配置模板 config\$t" }
    }
    Copy-Item -Force (Join-Path $repo '.env.example') (Join-Path $stage 'config-templates\env.example')

    # 启动器 + 图标
    foreach ($l in @('launch.ps1', 'BMA.cmd', 'BMA.vbs')) {
        Copy-Item -Force (Join-Path $repo ('tools\app\' + $l)) (Join-Path $stage 'launcher')
    }
    Copy-Item -Force $iconPath (Join-Path $stage 'launcher\icon.ico')

    # 禁运清单：仓库运行时数据与密钥，任何情况下都不许进包
    foreach ($f in @('models.json', 'user_profile.md', 'plugins.installed.yaml', 'skills_learned', '.env')) {
        $p = Join-Path $stage ('config-templates\' + $f)
        if (Test-Path $p) { Remove-Item -Recurse -Force $p; Write-Log "[pack] 已剔除禁运文件 $f" }
    }

    # 密钥扫描：模板里只允许 ${VAR} 插值，发现疑似真实 key 一律拒包
    $hits = @(Get-ChildItem $stage -Recurse -File |
        Where-Object { $_.Name -notlike '*example*' } |
        Select-String -Pattern 'sk-[A-Za-z0-9]{16,}', 'api_key\s*:\s*["'']?[A-Za-z0-9_\-]{16,}' -List -ErrorAction SilentlyContinue)
    if ($hits.Count -gt 0) {
        throw ('包内发现疑似真实密钥，拒绝打包: ' + (($hits | ForEach-Object { $_.Path }) -join ', '))
    }

    # 清单与版本元数据
    $files = @(Get-ChildItem $stage -Recurse -File | ForEach-Object {
        $rel = $_.FullName.Substring($stage.Length + 1).Replace('\', '/')
        [pscustomobject]@{
            path   = $rel
            sha256 = (Get-FileHash -Algorithm SHA256 -Path $_.FullName).Hash.ToLower()
            size   = $_.Length
        }
    } | Sort-Object path)
    $manifestPath = Join-Path $stage 'MANIFEST.sha256'
    $manifestLines = @($files | ForEach-Object { $_.sha256 + ' *' + $_.path })
    [System.IO.File]::WriteAllLines($manifestPath, $manifestLines, (New-Object System.Text.UTF8Encoding($false)))
    $manifestHash = (Get-FileHash -Algorithm SHA256 -Path $manifestPath).Hash.ToLower()
    Write-JsonFile (Join-Path $stage 'VERSION.json') ([ordered]@{
        name            = 'BlockMemoryAgent'
        version         = $Version
        commit          = $commit
        built_at        = $builtAt
        prompt_version  = $promptVersion
        file_count      = $files.Count
        manifest_sha256 = $manifestHash
    })

    $zip = $null
    if (-not $NoZip) {
        $zip = Join-Path $repo ('dist\bma-app-' + $Version + '.zip')
        if (Test-Path $zip) { Remove-Item -Force $zip }
        [System.IO.Compression.ZipFile]::CreateFromDirectory($stage, $zip, [System.IO.Compression.CompressionLevel]::Optimal, $false)
        Write-Log "[pack] zip: $zip"
    }

    Write-Log "[pack] 包目录: $stage"
    Write-Log "[pack] 版本 $Version（commit $commit，prompt $promptVersion，$($files.Count) 个文件）"
    return [ordered]@{
        ok             = $true
        command        = 'pack'
        version        = $Version
        commit         = $commit
        prompt_version = $promptVersion
        package        = $stage
        zip            = $zip
        file_count     = $files.Count
    }
}

# ---------------- install / update ----------------

function Resolve-Package([string]$From) {
    if (-not [string]::IsNullOrWhiteSpace($From)) {
        $src = [System.IO.Path]::GetFullPath($From)
        if (-not (Test-Path $src)) { throw "包不存在: $src" }
        if (Test-Path $src -PathType Container) { return @{ Path = $src; Temp = $null } }
        $tmp = Join-Path ([System.IO.Path]::GetTempPath()) ('bma-pkg-' + [guid]::NewGuid().ToString('N'))
        [System.IO.Compression.ZipFile]::ExtractToDirectory($src, $tmp)
        $dir = Get-ChildItem $tmp -Directory | Select-Object -First 1
        if (-not $dir) { throw "压缩包为空: $src" }
        return @{ Path = $dir.FullName; Temp = $tmp }
    }
    $root = Join-Path $PSScriptRoot 'dist\app'
    if (-not (Test-Path $root)) { throw '未找到 dist\app，请先运行: .\bma-app.ps1 pack' }
    $cand = Get-ChildItem $root -Directory -Filter 'bma-*' | Sort-Object LastWriteTime -Descending | Select-Object -First 1
    if (-not $cand) { throw '未找到已打包版本，请先运行: .\bma-app.ps1 pack' }
    return @{ Path = $cand.FullName; Temp = $null }
}

# 关掉该安装目录下的桌面外壳（BMA-Desktop.exe）；不动其他安装的实例。
function Stop-DesktopShell([string]$Root) {
    $stopped = @()
    try {
        Get-CimInstance Win32_Process -Filter "name='bma-desktop.exe'" -ErrorAction SilentlyContinue | ForEach-Object {
            if ($_.ExecutablePath -and $_.ExecutablePath.StartsWith($Root, [System.StringComparison]::OrdinalIgnoreCase)) {
                try {
                    Stop-Process -Id $_.ProcessId -Force -ErrorAction Stop
                    $stopped += [int]$_.ProcessId
                } catch { }
            }
        }
    } catch { }
    return $stopped
}

function Stop-BmaServer([string]$Root) {
    $pids = @()
    $rt = Read-JsonFile (Join-Path $Root 'app\runtime.json')
    if ($rt -and $rt.pid) { $pids += [int]$rt.pid }
    try {
        Get-CimInstance Win32_Process -Filter "name='bma-server.exe'" -ErrorAction SilentlyContinue | ForEach-Object {
            if ($_.ExecutablePath -and $_.ExecutablePath.StartsWith($Root, [System.StringComparison]::OrdinalIgnoreCase)) {
                $pids += [int]$_.ProcessId
            }
        }
    } catch { }
    $stopped = @()
    foreach ($procId in ($pids | Sort-Object -Unique)) {
        if ($procId -eq $PID) { continue }
        try {
            Stop-Process -Id $procId -Force -ErrorAction Stop
            $stopped += $procId
        } catch { }
    }
    if ($stopped.Count -gt 0) {
        $deadline = (Get-Date).AddSeconds(20)
        while ((Get-Date) -lt $deadline) {
            $alive = @($stopped | Where-Object { Get-Process -Id $_ -ErrorAction SilentlyContinue })
            if ($alive.Count -eq 0) { break }
            Start-Sleep -Milliseconds 300
        }
    }
    $runtimePath = Join-Path $Root 'app\runtime.json'
    if (Test-Path $runtimePath) { Remove-Item -Force $runtimePath -ErrorAction SilentlyContinue }
    return $stopped
}

function Start-And-Wait {
    param([string]$Root, [int]$TimeoutSec, [string]$ExpectVersion)
    $launch = Join-Path $Root 'app\launch.ps1'
    if (-not (Test-Path $launch)) { Write-Log '[start] 缺少 app\launch.ps1'; return $null }
    # 异步拉起启动器（ShellExecute，不继承本进程的 stdio），由本函数按健康检查判定成败：
    # 判定只看**健康检查 + build.version**——启动器自身可能因为收尾错误返回非 0，或者
    # 某些旧版本启动器会挂住不退，都不该影响"服务到底起没起"这个事实。
    $argList = @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $launch, '-NoBrowser', '-Quiet', '-TimeoutSec', "$TimeoutSec")
    $lp = Start-Process -FilePath 'powershell.exe' -ArgumentList $argList -WindowStyle Hidden -PassThru
    $deadline = (Get-Date).AddSeconds($TimeoutSec + 15)
    $h = $null
    while ((Get-Date) -lt $deadline) {
        $h = Get-Health $Root
        if ($h -and $h.build -and $h.build.version -and ($h.build.version -eq $ExpectVersion)) {
            return $h
        }
        if ($lp.HasExited -and -not $h) { break }
        Start-Sleep -Milliseconds 500
    }
    if ($h -and $h.build -and $h.build.version) {
        Write-Log ("[start] 运行版本 " + $h.build.version + " ≠ 期望 " + $ExpectVersion + '（旧进程可能仍在跑）')
    } elseif ($lp.HasExited) {
        Write-Log ("[start] 启动器已退出（退出码 " + $lp.ExitCode + "），服务未就绪")
    } else {
        Write-Log '[start] 启动器仍未返回，且服务未就绪'
    }
    return $null
}

function Remove-OldVersions {
    param([string]$Root, [int]$Keep, [string]$Current, [string]$Previous)
    $dir = Join-Path $Root 'app\versions'
    if (-not (Test-Path $dir)) { return @() }
    $all = @(Get-ChildItem $dir -Directory | Sort-Object Name -Descending)
    $kept = @()
    if ($Current) { $kept += $Current }
    if ($Previous -and ($kept -notcontains $Previous)) { $kept += $Previous }
    $extra = [Math]::Max(0, $Keep - $kept.Count)
    $removed = @()
    foreach ($d in $all) {
        if ($kept -contains $d.Name) { continue }
        if ($extra -gt 0) { $kept += $d.Name; $extra--; continue }
        try { Remove-Item -Recurse -Force $d.FullName; $removed += $d.Name } catch { }
    }
    return $removed
}

function New-AppShortcuts {
    param([string]$Root, [string]$Version)
    # 优先指向桌面外壳（原生窗口 + 自带图标）；外壳缺失时退回 VBS 脚本启动器。
    $desktopExe = Join-Path $Root 'BMA-Desktop.exe'
    $vbs = Join-Path $Root 'BMA.vbs'
    $target = $null
    if (Test-Path $desktopExe) { $target = $desktopExe }
    elseif (Test-Path $vbs) { $target = $vbs }
    if (-not $target) { return }

    $icon = Join-Path $Root 'app\icon.ico'
    if (-not (Test-Path $icon)) { $icon = Join-Path $Root ('app\versions\' + $Version + '\bin\bma-server.exe') }
    try {
        $ws = New-Object -ComObject WScript.Shell
        $targets = @($Root)
        $desktop = [Environment]::GetFolderPath('Desktop')
        if ($desktop) { $targets += $desktop }
        foreach ($dir in $targets) {
            $lnkPath = Join-Path $dir 'BlockMemoryAgent.lnk'
            $lnk = $ws.CreateShortcut($lnkPath)
            $lnk.TargetPath = $target
            $lnk.WorkingDirectory = $Root
            $lnk.IconLocation = ($icon + ',0')
            $lnk.Description = 'BlockMemoryAgent'
            $lnk.Save()
        }
        Write-Log "[install] 已创建快捷方式（$($targets -join ', ')）→ $target"
    } catch {
        Write-Log ("[install] warn: 创建快捷方式失败: " + $_.Exception.Message)
    }
}

function Invoke-Install {
    param([string]$Target, [string]$From, [int]$Keep, [int]$TimeoutSec, [string]$HttpAddr, [switch]$NoRestart, [switch]$NoShortcut, [switch]$NoShell)

    if ([string]::IsNullOrWhiteSpace($Target)) { $Target = Join-Path $env:LOCALAPPDATA 'Programs\BlockMemoryAgent' }
    $Target = [System.IO.Path]::GetFullPath($Target)
    if ((Test-Path (Join-Path $Target '.git')) -or (Test-Path (Join-Path $Target 'go.work'))) {
        throw "目标目录 $Target 看起来是源码仓库，拒绝安装（避免污染仓库数据）"
    }

    $pkgInfo = Resolve-Package $From
    $pkg = $pkgInfo.Path
    try {
        $meta = Read-JsonFile (Join-Path $pkg 'VERSION.json')
        if (-not $meta) { throw "包缺少 VERSION.json: $pkg" }
        $ver = $meta.version
        Write-Log "[install] 包 $ver（commit $($meta.commit)）→ $Target"

        foreach ($d in @('app', 'app\versions', 'config', 'plugins.d', 'logs', 'workspace')) {
            New-Item -ItemType Directory -Force -Path (Join-Path $Target $d) | Out-Null
        }

        $curPath = Join-Path $Target 'app\current.json'
        $prev = $null
        if (Test-Path $curPath) {
            $old = Read-JsonFile $curPath
            if ($old) {
                if ($old.version -eq $ver) { $prev = $old.previous } else { $prev = $old.version }
            }
        }

        # 桌面外壳先关（窗口连着旧后端，不关会在切换后白屏/报错）；记下它原本是否在跑
        $shellPids = Stop-DesktopShell $Target
        $hadShell = $shellPids.Count -gt 0
        if ($hadShell) { Write-Log ('[install] 已关闭桌面外壳: pid ' + ($shellPids -join ', ')) }

        $stopped = Stop-BmaServer $Target
        if ($stopped.Count -gt 0) { Write-Log ('[install] 已停止运行中的实例: pid ' + ($stopped -join ', ')) }

        # 1) 程序载荷 → 版本目录（不可变，重复安装同版本则整体替换该版本目录）
        $dst = Join-Path $Target ('app\versions\' + $ver)
        if (Test-Path $dst) { Remove-Item -Recurse -Force $dst }
        Copy-Item -Recurse -Force $pkg $dst
        Write-Log "[install] 版本目录: $dst"

        # 2) 用户数据：只补缺，绝不覆盖
        $seeded = @()
        $tplDir = Join-Path $dst 'config-templates'
        if (Test-Path $tplDir) {
            foreach ($f in Get-ChildItem $tplDir -File) {
                if ($f.Name -eq 'env.example') { continue }
                $out = Join-Path (Join-Path $Target 'config') $f.Name
                if (-not (Test-Path $out)) {
                    Copy-Item -Force $f.FullName $out
                    $seeded += ('config\' + $f.Name)
                }
            }
            $envDst = Join-Path $Target '.env'
            if (-not (Test-Path $envDst)) {
                Copy-Item -Force (Join-Path $tplDir 'env.example') $envDst
                if (-not [string]::IsNullOrWhiteSpace($HttpAddr)) { Set-EnvValue $envDst 'HTTP_ADDR' $HttpAddr }
                $seeded += '.env'
            }
        }
        $tokenGenerated = Ensure-ApiToken (Join-Path $Target '.env')

        # 3) 启动器与入口（随包更新）：优先桌面外壳，脚本启动器作为兜底
        Copy-Item -Force (Join-Path $dst 'launcher\launch.ps1') (Join-Path $Target 'app\launch.ps1')
        foreach ($l in @('BMA.cmd', 'BMA.vbs')) {
            Copy-Item -Force (Join-Path $dst ('launcher\' + $l)) (Join-Path $Target $l)
        }
        $desktopSrc = Join-Path $dst 'bin\bma-desktop.exe'
        if (Test-Path $desktopSrc) { Copy-Item -Force $desktopSrc (Join-Path $Target 'BMA-Desktop.exe') }
        $iconDst = Join-Path $Target 'app\icon.ico'
        if (Test-Path (Join-Path $dst 'launcher\icon.ico')) { Copy-Item -Force (Join-Path $dst 'launcher\icon.ico') $iconDst }

        # 4) 版本指针
        Write-JsonFile $curPath ([ordered]@{
                version      = $ver
                previous     = $prev
                installed_at = (Get-Date).ToString('o')
                commit       = $meta.commit
                package      = $pkg
            })

        # 5) 清理旧版本（保留当前 + 上一版 + 额外 Keep-2 个）
        $pruned = Remove-OldVersions -Root $Target -Keep $Keep -Current $ver -Previous $prev

        # 6) 桌面入口
        if (-not $NoShortcut) { New-AppShortcuts -Root $Target -Version $ver }

        # 7) 重启 + 健康门禁 + 失败回滚
        $health = $null
        $restarted = $false
        if (-not $NoRestart) {
            $health = Start-And-Wait -Root $Target -TimeoutSec $TimeoutSec -ExpectVersion $ver
            if (-not $health) {
                if ($prev -and (Test-Path (Join-Path $Target ('app\versions\' + $prev)))) {
                    Write-Log "[install] 新版本健康检查失败，回滚到 $prev"
                    Write-JsonFile $curPath ([ordered]@{
                            version      = $prev
                            previous     = $ver
                            installed_at = (Get-Date).ToString('o')
                            package      = 'rollback'
                        })
                    $health = Start-And-Wait -Root $Target -TimeoutSec $TimeoutSec -ExpectVersion $prev
                    throw "更新 $ver 失败，已回滚到 $prev（参见 $Target\logs 下的启动日志）"
                }
                throw "启动 $ver 失败，且没有可回滚的旧版本（参见 $Target\logs 下的启动日志）"
            }
            $restarted = $true
        }

        # 更新后把桌面外壳拉回来（原来是开着的才拉，保持"托盘常驻"体验）；-NoShell 可跳过
        $shellRestarted = $false
        if ($restarted -and $hadShell -and (-not $NoShell)) {
            $desktopExe = Join-Path $Target 'BMA-Desktop.exe'
            if (Test-Path $desktopExe) {
                Start-Process -FilePath $desktopExe -WorkingDirectory $Target
                $shellRestarted = $true
                Write-Log '[install] 桌面外壳已重启'
            }
        }

        if ($seeded.Count -gt 0) { Write-Log ('[install] 已种子拷贝用户数据: ' + ($seeded -join ', ')) }
        if ($pruned.Count -gt 0) { Write-Log ('[install] 已清理旧版本: ' + ($pruned -join ', ')) }
        Write-Log "[install] 完成：$ver @ $Target"

        return [ordered]@{
            ok              = $true
            command         = 'install'
            target          = $Target
            version         = $ver
            previous        = $prev
            restarted       = $restarted
            shell_restarted = $shellRestarted
            health          = $health
            seeded          = $seeded
            pruned_versions = $pruned
            api_token_new   = $tokenGenerated
        }
    } finally {
        if ($pkgInfo.Temp -and (Test-Path $pkgInfo.Temp)) { Remove-Item -Recurse -Force $pkgInfo.Temp -ErrorAction SilentlyContinue }
    }
}

# deploy = pack + update：一条命令完成"构建 → 切版本 → 重启后端 → 重启桌面外壳"。
function Invoke-Deploy {
    param([string]$Target, [string]$Version, [int]$Keep, [int]$TimeoutSec, [string]$HttpAddr, [switch]$SkipWeb, [switch]$Test, [switch]$NoShortcut, [switch]$NoShell)
    Write-Log '[deploy] 1/2 打包 ...'
    $packed = Invoke-Pack -Version $Version -SkipWeb:$SkipWeb -Test:$Test
    Write-Log ('[deploy] 2/2 安装 ' + $packed.version + ' ...')
    $res = Invoke-Install -Target $Target -From $packed.package -Keep $Keep -TimeoutSec $TimeoutSec -HttpAddr $HttpAddr -NoShortcut:$NoShortcut -NoShell:$NoShell
    $res.command = 'deploy'
    $res.build = [ordered]@{ version = $packed.version; commit = $packed.commit; prompt_version = $packed.prompt_version }
    return $res
}

# ---------------- start / stop / status / rollback ----------------

function Resolve-Target([string]$Target) {
    if ([string]::IsNullOrWhiteSpace($Target)) { return (Join-Path $env:LOCALAPPDATA 'Programs\BlockMemoryAgent') }
    return [System.IO.Path]::GetFullPath($Target)
}

function Invoke-Start {
    param([string]$Target, [int]$TimeoutSec)
    $Target = Resolve-Target $Target
    $cur = Read-JsonFile (Join-Path $Target 'app\current.json')
    if (-not $cur) { throw "未安装（缺少 $Target\app\current.json）" }
    $h = Start-And-Wait -Root $Target -TimeoutSec $TimeoutSec -ExpectVersion $cur.version
    if (-not $h) { throw "启动失败（版本 $($cur.version)）" }
    return [ordered]@{ ok = $true; command = 'start'; target = $Target; version = $cur.version; health = $h }
}

function Invoke-Stop {
    param([string]$Target)
    $Target = Resolve-Target $Target
    $stopped = Stop-BmaServer $Target
    return [ordered]@{ ok = $true; command = 'stop'; target = $Target; stopped_pids = $stopped }
}

function Invoke-Status {
    param([string]$Target)
    $Target = Resolve-Target $Target
    $cur = Read-JsonFile (Join-Path $Target 'app\current.json')
    $rt = Read-JsonFile (Join-Path $Target 'app\runtime.json')
    $versions = @()
    $vd = Join-Path $Target 'app\versions'
    if (Test-Path $vd) { $versions = @(Get-ChildItem $vd -Directory | Select-Object -ExpandProperty Name | Sort-Object -Descending) }
    $running = $false
    if ($rt -and $rt.pid) { $running = ($null -ne (Get-Process -Id $rt.pid -ErrorAction SilentlyContinue)) }
    $h = Get-Health $Target
    return [ordered]@{
        ok        = $true
        command   = 'status'
        target    = $Target
        installed = $cur
        runtime   = $rt
        running   = $running
        http      = (Get-BaseUrl $Target)
        health    = $h
        versions  = $versions
    }
}

function Invoke-Rollback {
    param([string]$Target, [int]$TimeoutSec)
    $Target = Resolve-Target $Target
    $curPath = Join-Path $Target 'app\current.json'
    $cur = Read-JsonFile $curPath
    if (-not $cur) { throw "未安装（缺少 $curPath）" }
    if ([string]::IsNullOrWhiteSpace($cur.previous)) { throw '没有可回滚的上一版本' }
    $prev = $cur.previous
    if (-not (Test-Path (Join-Path $Target ('app\versions\' + $prev)))) { throw "上一版本 $prev 已不存在（可能已被清理）" }

    Stop-BmaServer $Target | Out-Null
    Write-JsonFile $curPath ([ordered]@{
            version      = $prev
            previous     = $cur.version
            installed_at = (Get-Date).ToString('o')
            package      = 'rollback'
        })
    $h = Start-And-Wait -Root $Target -TimeoutSec $TimeoutSec -ExpectVersion $prev
    if (-not $h) { throw "回滚到 $prev 后健康检查失败" }
    return [ordered]@{ ok = $true; command = 'rollback'; target = $Target; version = $prev; previous = $cur.version; health = $h }
}

# ---------------- dispatch ----------------

$result = $null
try {
    switch ($Command) {
        'pack' { $result = Invoke-Pack -Version $Version -SkipWeb:$SkipWeb -Test:$Test -NoZip:$NoZip }
        'install' { $result = Invoke-Install -Target $Target -From $From -Keep $Keep -TimeoutSec $TimeoutSec -HttpAddr $HttpAddr -NoRestart:$NoRestart -NoShortcut:$NoShortcut -NoShell:$NoShell }
        'update' { $result = Invoke-Install -Target $Target -From $From -Keep $Keep -TimeoutSec $TimeoutSec -HttpAddr $HttpAddr -NoRestart:$NoRestart -NoShortcut:$NoShortcut -NoShell:$NoShell }
        'deploy' { $result = Invoke-Deploy -Target $Target -Version $Version -Keep $Keep -TimeoutSec $TimeoutSec -HttpAddr $HttpAddr -SkipWeb:$SkipWeb -Test:$Test -NoShortcut:$NoShortcut -NoShell:$NoShell }
        'start' { $result = Invoke-Start -Target $Target -TimeoutSec $TimeoutSec }
        'stop' { $result = Invoke-Stop -Target $Target }
        'restart' {
            Invoke-Stop -Target $Target | Out-Null
            $result = Invoke-Start -Target $Target -TimeoutSec $TimeoutSec
            $result.command = 'restart'
        }
        'status' { $result = Invoke-Status -Target $Target }
        'rollback' { $result = Invoke-Rollback -Target $Target -TimeoutSec $TimeoutSec }
    }
} catch {
    $message = $_.Exception.Message
    if ($script:JsonOutput) {
        Write-Output (([ordered]@{ ok = $false; command = $Command; error = $message }) | ConvertTo-Json -Depth 8)
    } else {
        Write-Error $message
    }
    exit 1
}

if ($script:JsonOutput) {
    Write-Output ($result | ConvertTo-Json -Depth 8)
} else {
    switch ($Command) {
        'pack' { Write-Host ("打包完成: " + $result.package) }
        'status' {
            Write-Host ("安装目录: " + $result.target)
            if ($result.installed) { Write-Host ("当前版本: " + $result.installed.version + "（commit " + $result.installed.commit + "，安装于 " + $result.installed.installed_at + "）") }
            else { Write-Host '当前版本: 未安装' }
            Write-Host ("运行中: " + $result.running)
            if ($result.health) { Write-Host ("健康: " + $result.http + " build=" + $result.health.build.version + " pid=" + $result.health.pid) }
            else { Write-Host ("健康: 未响应（" + $result.http + "）") }
            if ($result.versions) { Write-Host ("本地版本: " + ($result.versions -join ', ')) }
        }
        'stop' { Write-Host ("已停止: " + ($(if ($result.stopped_pids) { $result.stopped_pids -join ', ' } else { '（无运行中进程）' }))) }
        default { Write-Host ("完成: " + $result.command) }
    }
}
exit 0
