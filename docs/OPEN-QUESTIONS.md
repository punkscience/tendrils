# Open questions

Deferred decisions and known risks from the brainstorm phase. These were **not**
silently assumed — they are flagged here so the build phase resolves them
deliberately. Scenarios that touch an unresolved item carry a `@question` tag.

## Q8 — Symmetric-key derivation from the Nostr key

**Status:** decided. Only the low-level crypto details remain as build choices.
**Decision:** encrypt every blob at rest with a **single symmetric key derived
deterministically from the owner's Nostr key**, so the one key that enrolls a
device also decrypts on it. Keep it **simple for the MVP** — no key rotation and
no key recovery. Accepted consequence, eyes open: **the Nostr key is the master
key. If it leaks, all synced files are readable; if it is lost with no backup,
the encrypted blobs are unrecoverable.** The owner is responsible for backing up
the key.
**Left to the build (implementation detail, not a product decision):** the exact
KDF and domain separation, and how the per-file nonce is generated (a random
nonce per blob is fine). Hardening (rotation, per-folder keys) is a future change
that would re-encrypt data once.
**Touched by:** `features/local-first-sync.feature` → "A stored blob is
unreadable without Sam's key".

## Known risk — Clock skew under mtime last-writer-wins

**Accepted, not resolved.** Conflict resolution trusts plain file `mtime`. A
device with a fast/slow clock can systematically "win" or "lose" timing races.
This is tolerated because the losing version is always preserved as a conflict
copy, so skew costs a rename, never data. Revisit only if skew proves annoying
in practice (e.g. loosely sanity-check clocks, or warn on large drift).

## Resolved — Relay / Blossom-server retry and backoff policy

When a relay or Blossom server is unreachable:

1. **Retry every minute for the first 10 minutes.**
2. Then **back off to once per hour.**
3. After **12 hours** still failing, **report it as a critical issue and stop
   retrying** (surface via the error tray state and status/log).

Pending changes stay queued locally throughout so nothing is lost while offline.
Mirror fail-over is out of v1 scope.

## Deferred features (intentionally out of v1)

Not open questions — recorded so they are not re-litigated as bugs:

- **Single-file share links** (Blossom download URL). Killed from v1; natural v2.
- **Android app.** v2. Drives the periodic-reconcile design that v1 already
  builds in.
- **Fyne / rich GUI.** No toolkit commitment; v1 tray uses a standalone systray
  library.
- **Multiple synced folders, line-level merge, mirror redundancy, selective
  sync.** All deferred.
