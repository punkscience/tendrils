# Tendrils installer for Windows. Builds the CLI from source.
#
#   irm https://raw.githubusercontent.com/punkscience/tendrils/main/install.ps1 | iex
#
# Requirements: Go 1.26+ and git on PATH.
# Env: TENDRILS_BIN_DIR overrides the install directory
#      (default %LOCALAPPDATA%\Programs\tendrils).
$ErrorActionPreference = 'Stop'
$repo = 'https://github.com/punkscience/tendrils.git'

function Info($m) { Write-Host "==> $m" -ForegroundColor Cyan }
function Die($m)  { Write-Host "ERROR: $m" -ForegroundColor Red; exit 1 }

if (-not (Get-Command go  -ErrorAction SilentlyContinue)) { Die "Go 1.26+ is required. Install from https://go.dev/dl/ and re-run." }
if (-not (Get-Command git -ErrorAction SilentlyContinue)) { Die "git is required." }

Info "Detected $((go version) -replace '^go version ','')"

$tmp = Join-Path $env:TEMP ("tendrils-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    Info "Fetching source"
    git clone --depth 1 $repo "$tmp\src" 2>$null
    if ($LASTEXITCODE -ne 0) { Die "git clone failed" }

    $dest = if ($env:TENDRILS_BIN_DIR) { $env:TENDRILS_BIN_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\tendrils' }
    New-Item -ItemType Directory -Force -Path $dest | Out-Null

    Info "Building tendrils.exe"
    Push-Location "$tmp\src"
    try { & go build -o (Join-Path $dest 'tendrils.exe') ./cmd/tendrils }
    finally { Pop-Location }
    if ($LASTEXITCODE -ne 0) { Die "build failed" }
    Info "Installed $dest\tendrils.exe"

    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (($userPath -split ';') -notcontains $dest) {
        [Environment]::SetEnvironmentVariable('Path', "$userPath;$dest", 'User')
        Info "Added $dest to your user PATH (open a new shell to pick it up)."
    }
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

Write-Host ""
Write-Host "Tendrils installed. Next steps:"
Write-Host "  1. tendrils keygen                            # create your master key — BACK UP the nsec"
Write-Host "  2. tendrils enroll --key <nsec> --root <folder> --relay wss://<relay> --blossom http://<blossom>:8091"
Write-Host "  3. tendrils daemon --interval 1m              # start syncing"
Write-Host ""
Write-Host "Enroll every device with the SAME key. See https://github.com/punkscience/tendrils"
