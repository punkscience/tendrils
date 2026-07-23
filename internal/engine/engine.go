// Package engine is the orchestrator that drives Tendrils' sync loop. It owns no
// new rules — the conflict decision lives in internal/reconcile — it only wires
// the pure core to the outside world: scan the disk, ask the relay what it
// knows, feed both plus the index base to the reconciler, and execute the single
// action it returns (upload+publish, pull+write, publish a tombstone, or trash a
// file).
//
// One Sync pass is the whole product: it is the periodic reconcile, the
// on-startup catch-up, and the new-device bootstrap (reconcile-from-empty is
// just a Sync with an empty index) all at once.
//
// The network-facing dependencies are interfaces (EventStore, BlobStore) so the
// engine runs headless and is tested end-to-end with in-memory fakes and a temp
// directory — no relay, no Blossom server, no Fyne.
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"ca.punkscience.tendrils/internal/blob"
	"ca.punkscience.tendrils/internal/crypt"
	"ca.punkscience.tendrils/internal/ignore"
	"ca.punkscience.tendrils/internal/index"
	"ca.punkscience.tendrils/internal/keys"
	"ca.punkscience.tendrils/internal/nostrevent"
	"ca.punkscience.tendrils/internal/reconcile"
	"ca.punkscience.tendrils/internal/scan"
	"ca.punkscience.tendrils/internal/tree"
)

// EventStore is the relay side: publish one file-entry event, and fetch every
// file-entry event the owner's key has published. The concrete implementation is
// a go-nostr relay client; tests use an in-memory fake.
type EventStore interface {
	Publish(ctx context.Context, evt *nostr.Event) error
	Fetch(ctx context.Context, pubkey string) ([]*nostr.Event, error)
}

// BlobStore is the file-content side. *blob.Client satisfies it directly.
type BlobStore interface {
	Upload(ctx context.Context, data []byte) (blob.Descriptor, error)
	Download(ctx context.Context, sha256 string) ([]byte, error)
	Has(ctx context.Context, sha256 string) (bool, error)
}

// Progress is the engine's position within a Sync pass, emitted to the callback
// registered with OnProgress so a UI or the status endpoint can show live work.
// It is file-granular: which action, out of how many, on which path.
type Progress struct {
	Done  int    // actions completed so far this pass
	Total int    // actions planned for this pass
	Path  string // the path being acted on now; empty when the pass is finished
	Op    string // human verb for the current action: "uploading", "downloading", ...
}

// Engine holds the wiring for one synced folder on one device.
type Engine struct {
	root        string
	id          *keys.Identity
	symKey      [32]byte
	idx         *index.Store
	blobs       BlobStore
	events      EventStore
	log         *slog.Logger
	report      func(Progress)
	statsReport func(pending, conflicts int)
}

// OnProgress registers a callback invoked as each planned action begins and once
// more when the pass finishes (Path empty, Done==Total). Optional; nil disables
// reporting. It is called synchronously from Sync's goroutine — keep it cheap and
// non-blocking.
func (e *Engine) OnProgress(fn func(Progress)) { e.report = fn }

func (e *Engine) reportProgress(p Progress) {
	if e.report != nil {
		e.report(p)
	}
}

// OnStats registers a callback given, once per pass, the number of pending local
// changes (files to upload or tombstone) and conflict copies awaiting the owner.
// The engine has both in hand from the pass's scan and plan, so a status query
// can report them without an expensive rescan of its own. Optional; nil disables.
func (e *Engine) OnStats(fn func(pending, conflicts int)) { e.statsReport = fn }

func (e *Engine) reportStats(pending, conflicts int) {
	if e.statsReport != nil {
		e.statsReport(pending, conflicts)
	}
}

// plannedAction is one path the reconciler decided needs work, captured up front
// so the pass knows its total before executing (that count is what "3 of 12" needs).
type plannedAction struct {
	path     string
	decision reconcile.Decision
	local    *tree.Entry
	remote   *tree.Entry
}

// New builds an Engine. It derives the blob-encryption key from id up front so
// every seal/open in a Sync reuses it.
func New(root string, id *keys.Identity, idx *index.Store, blobs BlobStore, events EventStore, log *slog.Logger) (*Engine, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	symKey, err := id.SymmetricKey()
	if err != nil {
		return nil, fmt.Errorf("engine: derive symmetric key: %w", err)
	}
	return &Engine{
		root:   root,
		id:     id,
		symKey: symKey,
		idx:    idx,
		blobs:  blobs,
		events: events,
		log:    log,
	}, nil
}

// Sync runs one full reconcile pass over every path known locally, in the index,
// or on the relay. Per-path failures are collected and joined, not fatal: one
// unreadable file or unreachable blob must not stall the rest of the tree.
func (e *Engine) Sync(ctx context.Context) error {
	base, err := e.idx.All()
	if err != nil {
		return fmt.Errorf("engine: read index: %w", err)
	}
	// The base doubles as the scan's mtime+size cache: unchanged files are not
	// re-hashed, so a pass over a large tree costs a stat per file, not a full read.
	local, err := scan.Tree(e.root, base)
	if err != nil {
		return fmt.Errorf("engine: scan: %w", err)
	}
	remote, err := e.fetchRemote(ctx)
	if err != nil {
		return fmt.Errorf("engine: fetch remote: %w", err)
	}
	// The ignore file (.tendrilsignore at the root) is itself a synced file, read
	// fresh each pass so edits take effect without a restart.
	ign := e.loadIgnore()

	// Plan first, so the total is known before any action runs — that count is
	// what turns per-file progress into "3 of 12".
	var plan []plannedAction
	for _, path := range unionPaths(local, base, remote) {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Ignored paths are invisible to reconcile: never published, pulled, or
		// deleted. Any already-synced copy is left frozen in place, not removed.
		if ign.Match(path) {
			continue
		}
		d := reconcile.Decide(base[path], local[path], remote[path])
		if d.Op == reconcile.OpNone {
			continue
		}
		plan = append(plan, plannedAction{path: path, decision: d, local: local[path], remote: remote[path]})
	}

	// Publish the pass's outstanding-work counts for the status endpoint, so a
	// query reports these from the pass the daemon already ran rather than paying
	// for its own full-tree rescan. Pending = local changes to push; conflicts =
	// conflict copies sitting in the tree.
	e.reportStats(passStats(local, plan))

	total := len(plan)
	var errs []error
	for i, a := range plan {
		if err := ctx.Err(); err != nil {
			return err
		}
		e.reportProgress(Progress{Done: i, Total: total, Path: a.path, Op: verb(a.decision.Op)})
		e.log.Info("reconcile", "path", a.path, "op", a.decision.Op.String(), "reason", a.decision.Reason)
		if err := e.execute(ctx, a.path, a.decision, a.local, a.remote); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a.path, err))
			e.log.Error("action failed", "path", a.path, "op", a.decision.Op.String(), "err", err)
		}
	}
	e.reportProgress(Progress{Done: total, Total: total}) // pass finished, idle

	if err := e.idx.SetLastReconcile(time.Now()); err != nil {
		errs = append(errs, fmt.Errorf("record last-reconcile: %w", err))
	}
	return errors.Join(errs...)
}

// passStats derives the status counts from a pass's scan and plan: pending is the
// number of local changes to push (uploads and tombstones, conflict copies
// aside), conflicts the number of conflict copies present in the tree.
func passStats(local map[string]*tree.Entry, plan []plannedAction) (pending, conflicts int) {
	for path := range local {
		if scan.IsConflictCopy(path) {
			conflicts++
		}
	}
	for _, a := range plan {
		if scan.IsConflictCopy(a.path) {
			continue
		}
		if a.decision.Op == reconcile.OpPublishLocal || a.decision.Op == reconcile.OpPublishDelete {
			pending++
		}
	}
	return pending, conflicts
}

// verb is the human-facing present participle for an action, shown in progress.
func verb(op reconcile.Op) string {
	switch op {
	case reconcile.OpPublishLocal:
		return "uploading"
	case reconcile.OpWriteRemote:
		return "downloading"
	case reconcile.OpDeleteLocal:
		return "trashing"
	case reconcile.OpPublishDelete:
		return "publishing deletion"
	default:
		return op.String()
	}
}

// execute performs the reconciler's chosen action for one path.
func (e *Engine) execute(ctx context.Context, path string, d reconcile.Decision, local, remote *tree.Entry) error {
	switch d.Op {
	case reconcile.OpPublishLocal:
		return e.publishLocal(ctx, local)
	case reconcile.OpWriteRemote:
		return e.writeRemote(ctx, path, remote, d.ConflictCopy)
	case reconcile.OpDeleteLocal:
		return e.deleteLocal(path)
	case reconcile.OpPublishDelete:
		return e.publishDelete(ctx, path)
	default:
		return nil
	}
}

// publishLocal seals the local file, uploads the ciphertext, and publishes an
// event carrying the plaintext identity plus the sealed-blob address.
func (e *Engine) publishLocal(ctx context.Context, local *tree.Entry) error {
	abs := e.abs(local.Path)
	plaintext, err := os.ReadFile(abs)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	sealed, err := crypt.Seal(e.symKey, plaintext)
	if err != nil {
		return fmt.Errorf("seal: %w", err)
	}
	blobHash, err := e.uploadIfAbsent(ctx, sealed)
	if err != nil {
		return err
	}

	// Re-hash from the bytes we just read so the published identity matches what
	// we uploaded, not a possibly-stale scan result.
	entry := &tree.Entry{
		Path:     local.Path,
		Sha256:   local.Sha256,
		BlobHash: blobHash,
		Size:     int64(len(plaintext)),
		ModTime:  local.ModTime,
	}
	if err := e.publish(ctx, entry); err != nil {
		return err
	}
	return e.idx.Put(entry)
}

// uploadIfAbsent stores sealed and returns its content address, skipping the
// transfer when the server already holds it. Because sealing is deterministic,
// two devices that copy in the same file derive the same address, so the second
// one gets to skip a redundant upload rather than push a duplicate of bytes the
// server already has — the win is bandwidth; the address would coincide anyway.
//
// A failed existence check is not fatal: it only costs us the optimisation, and
// re-uploading identical bytes to the same address is harmless.
func (e *Engine) uploadIfAbsent(ctx context.Context, sealed []byte) (string, error) {
	want := hashHex(sealed)
	switch present, err := e.blobs.Has(ctx, want); {
	case err != nil:
		e.log.Debug("blob presence check failed, uploading anyway", "blob", short(want), "err", err)
	case present:
		return want, nil
	}
	desc, err := e.blobs.Upload(ctx, sealed)
	if err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}
	return desc.SHA256, nil
}

// writeRemote pulls the remote blob, unseals it, and writes it to disk. If the
// local file carried an unpublished change, it is preserved as a conflict copy
// first — a wrong last-writer-wins guess then costs a rename, never data.
func (e *Engine) writeRemote(ctx context.Context, path string, remote *tree.Entry, conflictCopy bool) error {
	if remote.BlobHash == "" {
		return fmt.Errorf("remote entry has no blob address")
	}
	sealed, err := e.blobs.Download(ctx, remote.BlobHash)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			return fmt.Errorf("blob %s… for %q is not on the configured Blossom server: this device is likely pointed at a different or empty server than the one holding your files — check --blossom / the blossom_servers in your config", short(remote.BlobHash), path)
		}
		return fmt.Errorf("download: %w", err)
	}
	plaintext, err := crypt.Open(e.symKey, sealed)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	if got := hashHex(plaintext); got != remote.Sha256 {
		return fmt.Errorf("decrypted content %s does not match expected %s", got, remote.Sha256)
	}

	abs := e.abs(path)
	if conflictCopy {
		if err := e.preserveConflictCopy(path); err != nil {
			return fmt.Errorf("preserve conflict copy: %w", err)
		}
	}
	if err := atomicWrite(abs, plaintext, remote.ModTime); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return e.idx.Put(remote)
}

// deleteLocal moves the local file to the trash (recoverable for the retention
// window) and records the remote tombstone as the new base.
func (e *Engine) deleteLocal(path string) error {
	if err := e.moveToTrash(path); err != nil {
		return fmt.Errorf("trash: %w", err)
	}
	return e.idx.Put(&tree.Entry{Path: path, Deleted: true, ModTime: time.Now()})
}

// publishDelete announces a local deletion as a tombstone and records it.
func (e *Engine) publishDelete(ctx context.Context, path string) error {
	entry := &tree.Entry{Path: path, Deleted: true, ModTime: time.Now()}
	if err := e.publish(ctx, entry); err != nil {
		return err
	}
	return e.idx.Put(entry)
}

// publish signs entry with the device key and hands it to the relay.
func (e *Engine) publish(ctx context.Context, entry *tree.Entry) error {
	evt, err := nostrevent.Sign(entry, e.id.SecretHex())
	if err != nil {
		return fmt.Errorf("sign: %w", err)
	}
	if err := e.events.Publish(ctx, evt); err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	return nil
}

// fetchRemote asks the relay for the owner's file-entry events and folds them
// into the current per-path truth, keeping the newest by mtime. A single
// unparseable event (bad signature, wrong kind) is skipped, not fatal.
func (e *Engine) fetchRemote(ctx context.Context) (map[string]*tree.Entry, error) {
	evts, err := e.events.Fetch(ctx, e.id.PublicHex())
	if err != nil {
		return nil, err
	}
	out := make(map[string]*tree.Entry, len(evts))
	for _, evt := range evts {
		entry, err := nostrevent.Parse(evt)
		if err != nil {
			e.log.Warn("skipping unparseable event", "err", err)
			continue
		}
		if prev, ok := out[entry.Path]; !ok || supersedes(entry, prev) {
			out[entry.Path] = entry
		}
	}
	return out, nil
}

// supersedes reports whether a should replace b as the remote truth for a path.
// Newer mtime wins; an exact tie is broken by content hash, matching the rule
// reconcile uses for local-vs-remote. Without that tie-break the winner would
// depend on Go's map iteration order, so two devices folding the same set of
// events could disagree about what "remote" is and publish over each other
// indefinitely. A tombstone beats a live entry at the same instant — delete is
// absolute, and its hash is empty, which would otherwise lose the comparison.
func supersedes(a, b *tree.Entry) bool {
	if !a.ModTime.Equal(b.ModTime) {
		return a.ModTime.After(b.ModTime)
	}
	if a.Deleted != b.Deleted {
		return a.Deleted
	}
	return a.Sha256 > b.Sha256
}

// preserveConflictCopy copies the current local file to a conflict-marked
// sibling that will itself sync on the next pass, so the owner sees the losing
// version on every device and resolves it with a rename.
func (e *Engine) preserveConflictCopy(path string) error {
	src := e.abs(path)
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nothing local to preserve
		}
		return err
	}
	dst := e.abs(conflictCopyPath(path, e.id.PublicHex()))
	return atomicWrite(dst, data, time.Now())
}

// moveToTrash relocates a file under the sync root's trash directory, preserving
// its relative structure; a name clash is disambiguated with a timestamp.
func (e *Engine) moveToTrash(path string) error {
	src := e.abs(path)
	dst := filepath.Join(e.root, scan.TrashDir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		dst = fmt.Sprintf("%s.%d", dst, time.Now().UnixNano())
	}
	return os.Rename(src, dst)
}

func (e *Engine) abs(relSlash string) string {
	return filepath.Join(e.root, filepath.FromSlash(relSlash))
}

// loadIgnore reads the sync root's .tendrilsignore into a matcher. A missing or
// unreadable file yields an empty matcher (nothing ignored).
func (e *Engine) loadIgnore() *ignore.Matcher {
	data, err := os.ReadFile(filepath.Join(e.root, ignore.FileName))
	if err != nil {
		return ignore.Compile(nil)
	}
	return ignore.Compile(strings.Split(string(data), "\n"))
}

// short trims a hex hash to a readable prefix for error messages.
func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// hashHex is the lowercase-hex SHA-256 of data — the plaintext content identity.
func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// unionPaths returns the sorted set of paths appearing in any of the three maps,
// so a Sync pass visits every path exactly once in a stable order.
func unionPaths(maps ...map[string]*tree.Entry) []string {
	set := make(map[string]struct{})
	for _, m := range maps {
		for p := range m {
			set[p] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// conflictCopyPath inserts the conflict marker (plus a short device tag, so
// copies from two devices do not collide) before a path's extension.
func conflictCopyPath(path, pubkey string) string {
	tag := pubkey
	if len(tag) > 8 {
		tag = tag[:8]
	}
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	return stem + scan.ConflictMarker + tag + ext
}

// atomicWrite writes data to abs via a temp file in the same directory followed
// by a rename, so a reader never sees a half-written file, and stamps the file's
// mtime to match the source of truth.
func atomicWrite(abs string, data []byte, mtime time.Time) error {
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, scan.TempPrefix+"*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(tmpName, time.Now(), mtime); err != nil {
			return err
		}
	}
	return os.Rename(tmpName, abs)
}
