# Creates videos/p1 ... videos/p<producers> (producers is read from config.json)
# and copies every video in samples/ into each folder, renamed p<i>_<name>
# so you can tell which producer uploaded which video.
#
# The consumer rejects an upload whose contents it already has, so by default
# each producer's copy gets a few random bytes appended, making it a different
# video. Use -Duplicates to leave some copies byte-identical on purpose, which
# is what demonstrates duplicate detection.
#
# Usage (from the P3_NtwrkProdCons folder):
#   .\setup-videos.ps1                       # uses config.json
#   .\setup-videos.ps1 -Config other.json    # uses another config file
#   .\setup-videos.ps1 -Count 3              # only copy 3 samples into each folder
#   .\setup-videos.ps1 -Duplicates 2         # 2 of the samples are copied verbatim

param(
    [string]$Config = "config.json",
    [string]$Samples = "samples",
    [string]$Output = "videos",
    [int]$Count = 0,      # 0 = copy all samples
    [int]$Duplicates = 0  # how many samples to copy verbatim into every folder
)

$ErrorActionPreference = "Stop"

# Appends 16 random bytes so this copy is a different video to the consumer.
# Trailing bytes after the last mp4 atom are ignored by players and by ffmpeg,
# so the video still plays and still previews.
function Add-RandomTrailer {
    param([string]$Path)
    # Convert-Path first: .NET resolves a relative path against the process
    # working directory, which Push-Location does NOT change. Without this the
    # append goes looking in whatever directory PowerShell was started in.
    $full = Convert-Path -LiteralPath $Path
    $pad = New-Object byte[] 16
    [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($pad)
    $stream = [System.IO.File]::Open($full, 'Append', 'Write')
    try { $stream.Write($pad, 0, $pad.Length) } finally { $stream.Dispose() }
}

Push-Location $PSScriptRoot
try {

$p = (Get-Content $Config -Raw | ConvertFrom-Json).producers
if (-not $p -or $p -lt 1) { throw "'producers' in $Config must be >= 1" }

if (-not (Test-Path $Samples)) {
    New-Item -ItemType Directory $Samples | Out-Null
    throw "Created '$Samples/'. Put some sample videos in it, then run this script again."
}
$files = Get-ChildItem $Samples -File | Sort-Object Name
if ($files.Count -eq 0) { throw "'$Samples/' is empty. Put some sample videos in it first." }
if ($Count -gt 0) { $files = $files | Select-Object -First $Count }

# Start fresh so old folders/videos from a previous run don't get uploaded.
if (Test-Path $Output) { Remove-Item $Output -Recurse -Force }

# The first -Duplicates samples are left byte-identical in every folder, so the
# consumer saves one copy and reports the rest as duplicates.
$verbatim = @($files | Select-Object -First ([Math]::Min($Duplicates, @($files).Count)))
$verbatimNames = @($verbatim | ForEach-Object { $_.FullName })

for ($i = 1; $i -le $p; $i++) {
    $dir = Join-Path $Output "p$i"
    New-Item -ItemType Directory $dir -Force | Out-Null
    foreach ($f in $files) {
        # -LiteralPath: names like "clip [animation].mp4" contain [ ], which are wildcards otherwise.
        $dest = Join-Path $dir "p${i}_$($f.Name)"
        Copy-Item -LiteralPath $f.FullName -Destination $dest
        if ($verbatimNames -notcontains $f.FullName) { Add-RandomTrailer $dest }
    }
}

$each = @($files).Count
Write-Host "Created $p folder(s) in '$Output/' with $each video(s) each."
if ($verbatimNames.Count -gt 0 -and $p -gt 1) {
    $dupes = $verbatimNames.Count * ($p - 1)
    Write-Host "$($verbatimNames.Count) sample(s) left byte-identical: expect $dupes duplicate rejection(s)."
}
else {
    Write-Host "Every copy is a distinct video, so all $($each * $p) should be accepted."
}
} finally { Pop-Location }
