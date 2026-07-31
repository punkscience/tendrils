# Installs the tendrils daemon on this Windows machine as a background service
# (scheduled task, S4U logon: runs whether or not anyone is logged in, no window,
# no password stored, survives reboot). Idempotent — re-run to upgrade the binary.
#
# Run from an ELEVATED PowerShell in the repo root:
#   powershell -ExecutionPolicy Bypass -File .claude\skills\install-windows\install-tendrils.ps1
#
# Prerequisites: Go on PATH; the machine already enrolled (tendrils keygen/enroll)
# — the script checks and tells you what to run if not.

$ErrorActionPreference = 'Stop'

$taskName   = 'Tendrils Daemon'
$installDir = Join-Path $env:LOCALAPPDATA 'Programs\Tendrils'
$exe        = Join-Path $installDir 'tendrils.exe'
$stateDir   = if ($env:TENDRILS_HOME) { $env:TENDRILS_HOME } else { Join-Path $env:APPDATA 'tendrils' }
$log        = Join-Path $stateDir 'daemon.log'
$repoRoot   = (Resolve-Path (Join-Path $PSScriptRoot '..\..\..')).Path

$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
if (-not ([Security.Principal.WindowsPrincipal]$identity).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Error "Run this from an elevated (Administrator) PowerShell."
    exit 1
}

# If elevation switched to a different account, %LOCALAPPDATA%/%APPDATA% resolve
# to the admin's profile and the daemon would install against the wrong config.
if ($repoRoot -match '^[A-Za-z]:\\Users\\([^\\]+)\\' -and $Matches[1] -ne $env:USERNAME) {
    Write-Error ("Repo belongs to user '$($Matches[1])' but this shell runs as '$env:USERNAME'. " +
        "Elevate as the daily user (UAC prompt on their own admin account), not a separate admin account.")
    exit 1
}

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    Write-Error "Go is not on PATH; install Go first (https://go.dev/dl/)."
    exit 1
}

# Build to a temp name first: a running daemon locks tendrils.exe (image
# section), so building straight onto it fails on every upgrade — and building
# before stopping means a broken build leaves the old daemon untouched.
Write-Host "Building tendrils.exe from $repoRoot ..."
New-Item -ItemType Directory -Force $installDir | Out-Null
$newExe = Join-Path $installDir 'tendrils-new.exe'
Push-Location $repoRoot
try { go build -o $newExe ./cmd/tendrils; if ($LASTEXITCODE -ne 0) { Write-Error "go build failed"; exit 1 } }
finally { Pop-Location }

# Replace any existing task/daemon before the binary swap. The daemon holds an
# exclusive bbolt index lock and its exe is locked while it runs, so the old
# process must be fully gone first; a hard stop is safe (atomic file writes,
# crash-safe index).
if (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue) {
    Stop-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
    Unregister-ScheduledTask -TaskName $taskName -Confirm:$false
    Write-Host "Removed existing '$taskName' task."
}
Get-Process tendrils -ErrorAction SilentlyContinue | Stop-Process -Force
Start-Sleep -Seconds 2

Move-Item -Force $newExe $exe

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if ($userPath -notlike "*$installDir*") {
    [Environment]::SetEnvironmentVariable('Path', "$userPath;$installDir", 'User')
    Write-Host "Added $installDir to user PATH (open a new shell to pick it up)."
}

# The daemon exits immediately without a key, sync root, and relay — check
# enrollment before registering anything.
$enrolled = $false
if ((Test-Path (Join-Path $stateDir 'key')) -and (Test-Path (Join-Path $stateDir 'config.json'))) {
    $cfg = Get-Content (Join-Path $stateDir 'config.json') -Raw | ConvertFrom-Json
    $enrolled = $cfg.sync_root -and $cfg.relays
}
if (-not $enrolled) {
    Write-Host ""
    Write-Host "This machine is not enrolled yet. Binary is installed; now run either:"
    Write-Host "  New identity:      `"$exe`" keygen"
    Write-Host "                     `"$exe`" enroll --root <folder> --relay wss://... --blossom https://..."
    Write-Host "  Existing sync set: `"$exe`" enroll --key <nsec> --root <folder> --relay wss://..."
    Write-Host "then re-run this script."
    exit 1
}

# S4U runs windowless in session 0, so cmd.exe can host the log redirect
# directly — no hidden-window wrapper needed. ExecutionTimeLimit zero disables
# the 72-hour default that would otherwise kill the daemon.
$action = New-ScheduledTaskAction -Execute 'cmd.exe' `
    -Argument "/c `"`"$exe`" daemon >> `"$log`" 2>&1`""
$triggers = @(
    (New-ScheduledTaskTrigger -AtStartup),
    (New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME)
)
$settings = New-ScheduledTaskSettingsSet `
    -ExecutionTimeLimit ([TimeSpan]::Zero) `
    -RestartCount 3 `
    -RestartInterval (New-TimeSpan -Minutes 1) `
    -AllowStartIfOnBatteries `
    -DontStopIfGoingOnBatteries `
    -StartWhenAvailable
$taskPrincipal = New-ScheduledTaskPrincipal -UserId $env:USERNAME -LogonType S4U

Register-ScheduledTask -TaskName $taskName `
    -Action $action -Trigger $triggers -Settings $settings -Principal $taskPrincipal | Out-Null
Write-Host "Registered '$taskName' (S4U: starts at boot, no login required)."

Start-ScheduledTask -TaskName $taskName
Start-Sleep -Seconds 8

$proc = Get-Process tendrils -ErrorAction SilentlyContinue
if ($proc) {
    Write-Host "Daemon running (PID $($proc.Id))."
    & $exe status
} else {
    Write-Warning "Daemon not detected after start. Check the task's last run result and $log"
    exit 1
}
