# make-icon.ps1 — 生成 BlockMemoryAgent 应用图标（assets\icon.ico + icon.png）。
#
# 设计：深蓝渐变圆角底 + 3×3 记忆方块阵列（青绿），中心块用琥珀色高亮，块间细连线
# 表示"记忆总线"——与项目名 BlockMemory 对应，缩小到 16px 仍能看出"方块阵列"。
#
# 用法：powershell -ExecutionPolicy Bypass -File tools\make-icon.ps1
param(
    [string]$OutDir = (Join-Path $PSScriptRoot '..\assets')
)
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Drawing

$sizes = @(16, 24, 32, 48, 64, 128, 256)
$master = 256

function New-RoundedPath([single]$x, [single]$y, [single]$w, [single]$h, [single]$r) {
    $p = New-Object System.Drawing.Drawing2D.GraphicsPath
    $d = $r * 2
    $p.AddArc($x, $y, $d, $d, 180, 90)
    $p.AddArc($x + $w - $d, $y, $d, $d, 270, 90)
    $p.AddArc($x + $w - $d, $y + $h - $d, $d, $d, 0, 90)
    $p.AddArc($x, $y + $h - $d, $d, $d, 90, 90)
    $p.CloseFigure()
    return $p
}

$bmp = New-Object System.Drawing.Bitmap($master, $master)
$g = [System.Drawing.Graphics]::FromImage($bmp)
$g.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::AntiAlias
$g.Clear([System.Drawing.Color]::Transparent)

# 底：深蓝渐变圆角方形
$bgPath = New-RoundedPath 6 6 244 244 54
$bgBrush = New-Object System.Drawing.Drawing2D.LinearGradientBrush(
    (New-Object System.Drawing.PointF(0, 0)),
    (New-Object System.Drawing.PointF(256, 256)),
    [System.Drawing.Color]::FromArgb(255, 13, 25, 41),
    [System.Drawing.Color]::FromArgb(255, 18, 74, 104))
$g.FillPath($bgBrush, $bgPath)

# 内描边：让边缘更利落
$edgePen = New-Object System.Drawing.Pen([System.Drawing.Color]::FromArgb(90, 94, 234, 212), 3)
$g.DrawPath($edgePen, $bgPath)

# 3×3 方块：先画连线（位于块下方），再画块
$cell = 50
$gap = 14
$originX = [single]((256 - (3 * $cell + 2 * $gap)) / 2)
$originY = [single]((256 - (3 * $cell + 2 * $gap)) / 2)
$centers = @()
for ($r = 0; $r -lt 3; $r++) {
    $row = @()
    for ($c = 0; $c -lt 3; $c++) {
        $row += , @(($originX + $c * ($cell + $gap) + $cell / 2), ($originY + $r * ($cell + $gap) + $cell / 2))
    }
    $centers += , $row
}

$wirePen = New-Object System.Drawing.Pen([System.Drawing.Color]::FromArgb(70, 94, 234, 212), 5)
for ($r = 0; $r -lt 3; $r++) {
    for ($c = 0; $c -lt 3; $c++) {
        $x, $y = $centers[$r][$c]
        if ($c -lt 2) {
            $nx, $ny = $centers[$r][$c + 1]
            $g.DrawLine($wirePen, [single]$x, [single]$y, [single]$nx, [single]$ny)
        }
        if ($r -lt 2) {
            $nx, $ny = $centers[$r + 1][$c]
            $g.DrawLine($wirePen, [single]$x, [single]$y, [single]$nx, [single]$ny)
        }
    }
}

# 方块：外围青绿，中心琥珀高亮
$teal = @(
    [System.Drawing.Color]::FromArgb(255, 45, 212, 191),
    [System.Drawing.Color]::FromArgb(255, 56, 189, 248),
    [System.Drawing.Color]::FromArgb(255, 34, 197, 181)
)
$accent = [System.Drawing.Color]::FromArgb(255, 251, 191, 36)
for ($r = 0; $r -lt 3; $r++) {
    for ($c = 0; $c -lt 3; $c++) {
        $x = $originX + $c * ($cell + $gap)
        $y = $originY + $r * ($cell + $gap)
        $blockPath = New-RoundedPath $x $y $cell $cell 13
        if ($r -eq 1 -and $c -eq 1) {
            $fill = New-Object System.Drawing.SolidBrush($accent)
        } else {
            $color = $teal[($r + $c) % 3]
            $fill = New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb(235, $color))
        }
        $g.FillPath($fill, $blockPath)
        $fill.Dispose()
        $blockPath.Dispose()
    }
}

# 256 主图
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
$pngPath = Join-Path $OutDir 'icon.png'
$bmp.Save($pngPath, [System.Drawing.Imaging.ImageFormat]::Png)

# 多尺寸 PNG → ICO（Vista+ 支持 PNG 压缩条目）
# 用 ArrayList 存 (尺寸, PNG 字节)：5.1 里对 @() 做 `+=` 混类型偶发 op_Addition 报错。
$entrySizes = New-Object System.Collections.ArrayList
$entryBytes = New-Object System.Collections.ArrayList
foreach ($s in $sizes) {
    $sb = New-Object System.Drawing.Bitmap($s, $s)
    $sg = [System.Drawing.Graphics]::FromImage($sb)
    $sg.InterpolationMode = [System.Drawing.Drawing2D.InterpolationMode]::HighQualityBicubic
    $sg.PixelOffsetMode = [System.Drawing.Drawing2D.PixelOffsetMode]::HighQuality
    $sg.DrawImage($bmp, 0, 0, $s, $s)
    $ms = New-Object System.IO.MemoryStream
    $sb.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png)
    [void]$entrySizes.Add([int]$s)
    [void]$entryBytes.Add($ms.ToArray())
    $sg.Dispose(); $sb.Dispose(); $ms.Dispose()
}

$icoPath = Join-Path $OutDir 'icon.ico'
$fs = [System.IO.File]::Create($icoPath)
$bw = New-Object System.IO.BinaryWriter($fs)
$bw.Write([UInt16]0)                      # reserved
$bw.Write([UInt16]1)                      # type = icon
$bw.Write([UInt16]$entrySizes.Count)
$offset = 6 + 16 * $entrySizes.Count
for ($i = 0; $i -lt $entrySizes.Count; $i++) {
    $s = $entrySizes[$i]
    $bytes = $entryBytes[$i]
    $dim = 0
    if ($s -lt 256) { $dim = $s }
    $bw.Write([byte]$dim)                 # width
    $bw.Write([byte]$dim)                 # height
    $bw.Write([byte]0)                    # color count
    $bw.Write([byte]0)                    # reserved
    $bw.Write([UInt16]1)                  # planes
    $bw.Write([UInt16]32)                 # bit count
    $bw.Write([UInt32]$bytes.Length)
    $bw.Write([UInt32]$offset)
    $offset = $offset + $bytes.Length
}
for ($i = 0; $i -lt $entryBytes.Count; $i++) { $bw.Write($entryBytes[$i]) }
$bw.Flush(); $bw.Dispose(); $fs.Dispose()

$g.Dispose(); $bmp.Dispose()
"icon generated: $icoPath ($([int]((Get-Item $icoPath).Length)) bytes), $pngPath"
