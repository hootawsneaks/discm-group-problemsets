# Creates videos/p1 ... videos/p<producers> (producers is read from config.json)
# and copies every video in samples/ into each folder, renamed p<i>_<name>
# so you can tell which producer uploaded which video.
#
# Usage (from the P3_NtwrkProdCons folder):
#   .\setup-videos.ps1                       # uses config.json
#   .\setup-videos.ps1 -Config other.json    # uses another config file
#   .\setup-videos.ps1 -Count 3              # only copy 3 samples into each folder

param(
    [string]$Config = "config.json",
    [string]$Samples = "samples",
    [string]$Output = "videos",
    [int]$Count = 0   # 0 = copy all samples
)

$ErrorActionPreference = "Stop"
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

for ($i = 1; $i -le $p; $i++) {
    $dir = Join-Path $Output "p$i"
    New-Item -ItemType Directory $dir -Force | Out-Null
    foreach ($f in $files) {
        # -LiteralPath: names like "clip [animation].mp4" contain [ ], which are wildcards otherwise.
        Copy-Item -LiteralPath $f.FullName -Destination (Join-Path $dir "p${i}_$($f.Name)")
    }
}

Write-Host "Created $p folder(s) in '$Output/' with $(@($files).Count) video(s) each."
} finally { Pop-Location }
