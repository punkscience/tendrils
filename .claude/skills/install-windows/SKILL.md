---
name: install-windows
description: Install the tendrils sync daemon on a Windows machine so it runs in the background and survives reboot. Use when the user asks to install, deploy, or auto-start tendrils on Windows.
---

# Install tendrils on Windows

Goal state: `tendrils.exe` built from this repo, installed to a stable path, running
as a hidden background daemon, auto-starting after reboot, verifiable with
`tendrils status`.

## Layout (keep these paths — they're what existing installs use)

| Thing | Path |
|---|---|
| Binary + helper scripts | `%LOCALAPPDATA%\Programs\Tendrils\` (added to user PATH) |
| Config, key, index | `%APPDATA%\tendrils\` (`config.json`, `key`, `index.db`) — or `$TENDRILS_HOME` |
| Daemon log | `%APPDATA%\tendrils\daemon.log` |
| Scheduled task name | `Tendrils Daemon` |

## Preferred path: the elevated install script

`install-tendrils.ps1` in this skill folder does the whole install. Have the user
run it from an **elevated** PowerShell in the repo root:

```powershell
powershell -ExecutionPolicy Bypass -File .claude\skills\install-windows\install-tendrils.ps1
```

It builds from source (needs Go on PATH), installs the binary, registers the
scheduled task with **S4U logon** (runs whether or not the user is logged in,
windowless in session 0, no password stored) and **boot + logon triggers**, then
starts and verifies the daemon. It is idempotent — safe to re-run for upgrades;
it stops the old daemon first because the daemon holds the bbolt index lock.

You (the agent) cannot run it yourself: S4U task registration fails with
"Access is denied" from a non-elevated shell. Suggest the user run it via
`! <command>` or an admin terminal. If they can't elevate, use the fallback below.

## Enrollment must happen first (or the daemon exits immediately)

The daemon requires an existing key, sync root, and at least one relay. Check
`%APPDATA%\tendrils\` before registering anything:

- **Already enrolled** (key + config.json with `sync_root` and `relays`): nothing to do.
- **Fresh machine, new identity**: `tendrils keygen` then
  `tendrils enroll --root <folder> --relay wss://... --blossom https://...`
- **Fresh machine, existing sync set**: `tendrils enroll --key <nsec> --root <folder> --relay wss://...`
  — no `--blossom` needed; the daemon discovers Blossom servers from the owner's
  published kind-10063 list (the reference relay is `wss://relay.towerofsong.ca`).

The key is the user's identity — never generate a new one if they have an
existing sync set, and never echo the nsec into logs or output.

## Fallback: no elevation available

A non-elevated user can still register a logon-triggered task that runs in their
interactive session. Two traps cost real debugging time:

1. **A plain `cmd.exe` action leaves a console window open** for the daemon's
   whole life. Launch through a `wscript.exe //B` VBScript wrapper that does
   `sh.Run "cmd /c ...", 0, True` — window style 0 hides it; the final `True`
   makes wscript wait so the task tracks the real process and restart-on-failure
   works.
2. **`log` is a reserved VBScript function** (natural logarithm). Assigning to
   it fails with "Illegal assignment", and under `wscript //B` that failure is
   *silent* — the task reports success (exit 0) and nothing runs. Name the
   variable `logPath`. If a task "ran successfully" but no process/log appeared,
   re-run the .vbs with `cscript //nologo` to surface the error.

Register with `-LogonType Interactive`, and always set
`-ExecutionTimeLimit ([TimeSpan]::Zero)` — the scheduled-task default kills the
task after 72 hours. Caveat to tell the user: this variant only runs while they
are logged in; the script above upgrades it in place later.

## Verify

```powershell
Get-Process tendrils            # daemon process exists
tendrils status                 # queries the daemon's loopback status endpoint
Get-Content $env:APPDATA\tendrils\daemon.log -Tail 20
```

Healthy startup logs identity/root/relays and either uses configured Blossom
servers or logs "discovered Blossom server(s) from the relay". The first pass on
a machine that has been offline can run for a long time (it's a full catch-up —
`status` shows per-file progress); that's transfer time, not a hang. `tendrils
status` works while the daemon runs precisely because the daemon serves it —
if it errors about the index lock, the daemon is *not* running.

## Uninstall

```powershell
Stop-ScheduledTask -TaskName "Tendrils Daemon"; Unregister-ScheduledTask -TaskName "Tendrils Daemon" -Confirm:$false
Get-Process tendrils -ErrorAction SilentlyContinue | Stop-Process -Force
Remove-Item -Recurse -Force "$env:LOCALAPPDATA\Programs\Tendrils"
# %APPDATA%\tendrils holds the user's KEY and sync state — only delete if the user explicitly says so.
```

## Traps

- Hard-killing the daemon is safe (atomic file writes, crash-safe bbolt index),
  but the old process must be fully gone before starting a new one — the index
  lock is exclusive.
- If the elevated shell runs as a *different* admin account than the daily user,
  `%LOCALAPPDATA%`/`%APPDATA%` resolve to the wrong profile. The script guards
  against this; don't work around the guard.
- Fyne/CGO is irrelevant here — `cmd/tendrils` is pure Go and cross-compiles/builds
  with no C compiler.
