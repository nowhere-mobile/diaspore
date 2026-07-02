# Generates a Nowhere edition boot animation (PNG frames + desc.txt, dark wordmark with a dispersing-spore
# motif that loops). Offline; uses .NET only. Parameterized per edition (defaults = the Diaspore/FP3 one):
#   -Wordmark endospore -Height 2400 -DotR 154 -DotG 160 -DotB 166 -OutDir <editions/endospore/vendor/media>
# NOTE: Android's bootanimation mmaps each frame, so the zip entries MUST be STORED (method 0). This
# .NET's CompressionLevel.NoCompression DOES emit STORED entries (verified: `unzip -v` shows 0%), so the
# output plays as-is -- no `zip -0` re-zip needed.
param(
  [string]$Wordmark = 'diaspore',
  [int]$Width  = 1080,
  [int]$Height = 2160,
  [int]$DotR = 150, [int]$DotG = 200, [int]$DotB = 255,   # dispersing-dot tint (Diaspore = light blue)
  [string]$OutDir = (Join-Path $PSScriptRoot 'media')
)
Add-Type -AssemblyName System.Drawing
Add-Type -AssemblyName System.IO.Compression
Add-Type -AssemblyName System.IO.Compression.FileSystem

$W = $Width; $H = $Height; $FPS = 12; $N = 36; $nDots = 14
$outRoot = $OutDir
$work = Join-Path $env:TEMP ('bootanim_' + [Guid]::NewGuid().ToString('N'))
$part0 = Join-Path $work 'part0'
[IO.Directory]::CreateDirectory($part0) | Out-Null
[IO.Directory]::CreateDirectory($outRoot) | Out-Null

$bg = [System.Drawing.Color]::FromArgb(255, 11, 11, 16)
$cx = [single]($W / 2); $cy = [single]($H / 2)
$font    = New-Object System.Drawing.Font('Segoe UI', 170, [System.Drawing.FontStyle]::Regular, [System.Drawing.GraphicsUnit]::Pixel)
$subFont = New-Object System.Drawing.Font('Segoe UI', 44,  [System.Drawing.FontStyle]::Regular, [System.Drawing.GraphicsUnit]::Pixel)
$sf = New-Object System.Drawing.StringFormat
$sf.Alignment = [System.Drawing.StringAlignment]::Center
$sf.LineAlignment = [System.Drawing.StringAlignment]::Center

for ($i = 0; $i -lt $N; $i++) {
  $bmp = New-Object System.Drawing.Bitmap($W, $H)
  $g = [System.Drawing.Graphics]::FromImage($bmp)
  $g.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::AntiAlias
  $g.TextRenderingHint = [System.Drawing.Text.TextRenderingHint]::AntiAlias
  $g.Clear($bg)
  $p = $i / [double]$N

  for ($k = 0; $k -lt $nDots; $k++) {
    $prog = (($p + $k / [double]$nDots) % 1.0)
    $ang = ($k * 2 * [Math]::PI / $nDots) - [Math]::PI / 2
    $rad = 30 + $prog * 300
    $dx = [Math]::Cos($ang) * $rad
    $dy = [Math]::Sin($ang) * $rad - $prog * 120
    $a = [int]([Math]::Max(0.0, (1 - $prog)) * 190)
    $sz = [single][Math]::Max(2.0, 9 * (1 - $prog * 0.6))
    $br = New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb($a, $DotR, $DotG, $DotB))
    $g.FillEllipse($br, [single]($cx + $dx - $sz / 2), [single]($cy - 250 + $dy - $sz / 2), $sz, $sz)
    $br.Dispose()
  }

  $pulse = 248 + [int](7 * [Math]::Sin(2 * [Math]::PI * $p))
  $tb = New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb($pulse, 250, 252, 255))
  $g.DrawString($Wordmark, $font, $tb, $cx, $cy, $sf); $tb.Dispose()
  $sb = New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb(150, 165, 178, 192))
  $g.DrawString('your phone, nowhere', $subFont, $sb, $cx, [single]($cy + 150), $sf); $sb.Dispose()

  $g.Dispose()
  $bmp.Save((Join-Path $part0 ('frame{0:0000}.png' -f $i)), [System.Drawing.Imaging.ImageFormat]::Png)
  $bmp.Dispose()
}

[IO.File]::WriteAllText((Join-Path $work 'desc.txt'), "$W $H $FPS`np 0 0 part0`n")

$zipPath = Join-Path $outRoot 'bootanimation.zip'
if (Test-Path $zipPath) { [IO.File]::Delete($zipPath) }
$zip = [System.IO.Compression.ZipFile]::Open($zipPath, [System.IO.Compression.ZipArchiveMode]::Create)
function Add-Stored($zip, $file, $name) {
  $e = $zip.CreateEntry($name, [System.IO.Compression.CompressionLevel]::NoCompression)
  $s = $e.Open(); $bytes = [IO.File]::ReadAllBytes($file); $s.Write($bytes, 0, $bytes.Length); $s.Dispose()
}
Add-Stored $zip (Join-Path $work 'desc.txt') 'desc.txt'
[IO.Directory]::GetFiles($part0, '*.png') | Sort-Object | ForEach-Object {
  Add-Stored $zip $_ ('part0/' + [IO.Path]::GetFileName($_))
}
$zip.Dispose()
[IO.Directory]::Delete($work, $true)
Write-Output ("bootanimation.zip: " + (Get-Item $zipPath).Length + " bytes; $N frames @ ${FPS}fps; ${W}x${H}")
