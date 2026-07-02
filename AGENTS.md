# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

Tendrils is a folder sync utility for Windows, Linux, and Android that uses [Nostr](https://github.com/nostr-protocol/nostr) relays as the transport to synchronize files between devices.

Module: `ca.punkscience.tendrils` (Go 1.26.3). UI: [Fyne](https://fyne.io) v2 — one Go UI codebase targets all three platforms.

## Current state

The tested pure core and a runnable CLI exist; the sync engine and network I/O are the next milestone. Everything is **pure Go, no CGO** (bbolt for the index, not SQLite) so Windows/Linux builds stay simple — there is no C compiler on the dev box. Fyne is **not** a dependency and, per the concept brief, the v1 UI is a minimal standalone tray, not the full toolkit; the headless engine is proven first.

Built and tested (`go test ./...` green):

| Package | Responsibility |
|---|---|
| `internal/tree` | `Entry` — the one shared type (path, sha256, size, mtime, deleted). Dependency-free leaf. |
| `internal/keys` | Parse nsec/hex, derive the AES-256 blob key via HKDF-SHA256 (domain-separated, no salt so it is reproducible from the key alone). |
| `internal/crypt` | Blob encryption at rest: AES-256-GCM, `nonce‖ciphertext`, random nonce per blob. |
| `internal/reconcile` | The pure conflict decision: LWW-by-mtime, delete-is-absolute, re-creation-honoured, conflict copies. One test per Gherkin scenario. **The correctness-critical heart.** |
| `internal/index` | bbolt store of the last-synced `Entry` per path (the reconcile "base") + last-reconcile time. Retains tombstones. |
| `internal/scan` | Walk the sync root → `Entry` map (sha256, mtime); skips `.tendrils-trash`; conflict-copy naming (`ConflictMarker`). |
| `internal/nostrevent` | `Entry` ⇄ Nostr event codec. Parameterized replaceable event, kind `31337`, `d`=path, `x`=sha256, `mtime`/`deleted` tags, `created_at`=mtime. Signs/verifies. |
| `internal/config` | State dir (`$TENDRILS_HOME` or OS config dir), local `config.json` (overrides discovery), device key at rest (0600 file — deliberately not a keychain). |
| `internal/blob` | Blossom (BUD-01/02) client: `Upload`/`Download`/`Has` opaque bytes addressed by sha256, with a signed per-request kind-24242 auth event. Verifies content addresses locally on both directions. Pure transport — no knowledge of encryption or plaintext hashes. |
| `internal/tree` `internal/nostrevent` | `Entry` carries `BlobHash` (sealed-blob Blossom address) alongside `Sha256` (plaintext identity); the event codec round-trips it in a `blob` tag. |
| `internal/engine` | Orchestrates one full `Sync` reconcile pass: scan → read index base → fetch relay truth → `reconcile.Decide` per path → execute (seal+upload+publish / pull+unseal+atomic-write / trash / publish-tombstone). Network deps are `EventStore`/`BlobStore` interfaces (`*blob.Client` satisfies `BlobStore`), so it runs headless. Tested end-to-end with in-memory fakes: publish, pull, two-device convergence, delete propagation to trash, and conflict-copy preservation. One `Sync` is also the bootstrap and the periodic reconcile. |
| `internal/relay` | Concrete `engine.EventStore`: publishes/fetches file-entry events over go-nostr websockets to one or more relays. Persistent, lazily-reconnecting connections; publish to all (succeed if any accepts), fetch unions + dedupes by event ID. Also `FetchServerList` for Blossom discovery (kind-10063). Tested against a minimal in-process NIP-01 relay (`coder/websocket`). |
| `internal/serverlist` | Blossom server **discovery**: `Entry`-free codec for the BUD-03 kind-10063 "User Server List" (sign/parse a `[]string` of server URLs under the owner's key). `Shareable` strips loopback/unspecified hosts (never advertise `127.0.0.1` to the whole identity); `Merge` unions lists so devices republish instead of clobbering. The daemon publishes the union when it has a server and discovers from it when it doesn't — so a later device enrolls with just `--key`/`--relay`. |
| `cmd/tendrils` | **cobra** CLI: `keygen`, `enroll` (`--key`/`--root`, reuses stored key), `status` (pending/conflicts from scan-vs-index), `daemon` (builds the engine from config and runs a periodic reconcile loop; `--interval`, graceful shutdown on Ctrl-C). |

Not yet built (next milestones): **relay** discovery (NIP-65/kind-10002 — until then `daemon` still requires `--relay` set at enroll; Blossom-server discovery via kind-10063 **is** built, so `--blossom` is optional on later devices), fsnotify watch (with debounce/settle, to complement the periodic reconcile), retry/backoff policy per docs (the loop currently logs a failed pass and retries on the fixed interval), Blossom multi-server mirroring (daemon uses the first server), tombstone re-assertion (the relay may drop a tombstone under a newer concurrent edit — see the known-limitation note in `nostrevent`), and the tray.

**Addressing model (resolved):** a Blossom blob is addressed by the SHA-256 of its *stored* (encrypted) bytes, while `tree.Entry.Sha256` is the *plaintext* hash used for file identity, dedup, and the reconciler's same-content check. Because encryption uses a random nonce per blob, the same file seals to different bytes — a different content address — on every device, so the publishing device records the sealed address it actually uploaded in `tree.Entry.BlobHash` and the event's `blob` tag. A puller fetches by `BlobHash` (verified by `blob.Download`), unseals, and verifies the plaintext against `Sha256` — two independent integrity checks. The reconciler compares only `Sha256`, so identical content with different sealed blobs is correctly "converged".

Dependencies: `github.com/nbd-wtf/go-nostr`, `go.etcd.io/bbolt`, `github.com/spf13/cobra`.

## Commands

```bash
go build ./...        # build all packages
go test ./...         # run all tests
go test ./path/to/pkg -run TestName   # run a single test
go vet ./...          # static checks
gofmt -l -w .         # format (gofmt is the project standard)
```

### Packaging with Fyne

Fyne uses **CGO**, so plain `GOOS=... go build` cross-compilation does not work. Use the `fyne` CLI for native builds and `fyne-cross` (Docker-based) for reproducible cross-platform builds.

```bash
go install fyne.io/tools/cmd/fyne@latest        # the fyne packaging CLI

fyne package -os windows                         # .exe (run on/for Windows)
fyne package -os linux                           # Linux tarball
fyne package -os android -appID ca.punkscience.tendrils   # .apk; needs Android SDK + NDK

go run .                                          # fast local dev iteration on desktop
```

- Desktop builds need a C compiler on PATH (MinGW-w64 on Windows, gcc/clang on Linux).
- Android builds need the Android SDK and NDK; set `ANDROID_HOME` / `ANDROID_NDK_HOME`.
- `-appID` must be the reverse-domain ID `ca.punkscience.tendrils` and stay stable across releases (changing it makes Android treat it as a different app).

## Architecture notes to preserve as the code grows

These are the load-bearing decisions implied by the project's premise. Document the concrete design here once it exists.

- **Nostr is the sync transport, not a file store.** Relays broadcast small events; large file contents need a separate plan (chunking, external blob storage, or NIP-based file transfer). Keep the event-publishing layer separate from the file-content transfer layer so either can change independently.
- **Three platforms, one core, one UI.** Fyne gives a shared UI layer across all three, but OS-specific concerns still differ — isolate filesystem watching, path conventions, background execution, and the Android app lifecycle behind interfaces so the sync engine stays portable. Android is the constraint that shapes the boundary: it has no persistent background daemon (use a foreground service / WorkManager equivalent), scoped/sandboxed storage, and doze-mode network limits. Keep the sync engine independent of Fyne so it can run headless and be tested without a UI.
- **Conflict resolution and identity.** File sync across devices needs a merge/conflict strategy and a device-identity/keypair model (Nostr keys are a natural fit). Decide these early — they are hard to retrofit.
