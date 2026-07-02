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
}

// Engine holds the wiring for one synced folder on one device.
type Engine struct {
	root   string
	id     *keys.Identity
	symKey [32]byte
	idx    *index.Store
	blobs  BlobStore
	events EventStore
	log    *slog.Logger
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
	local, err := scan.Tree(e.root)
	if err != nil {
		return fmt.Errorf("engine: scan: %w", err)
	}
	base, err := e.idx.All()
	if err != nil {
		return fmt.Errorf("engine: read index: %w", err)
	}
	remote, err := e.fetchRemote(ctx)
	if err != nil {
		return fmt.Errorf("engine: fetch remote: %w", err)
	}
	// The ignore file (.tendrilsignore at the root) is itself a synced file, read
	// fresh each pass so edits take effect without a restart.
	ign := e.loadIgnore()

	var errs []error
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
		e.log.Info("reconcile", "path", path, "op", d.Op.String(), "reason", d.Reason)
		if err := e.execute(ctx, path, d, local[path], remote[path]); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			e.log.Error("action failed", "path", path, "op", d.Op.String(), "err", err)
		}
	}

	if err := e.idx.SetLastReconcile(time.Now()); err != nil {
		errs = append(errs, fmt.Errorf("record last-reconcile: %w", err))
	}
	return errors.Join(errs...)
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
	desc, err := e.blobs.Upload(ctx, sealed)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}

	// Re-hash from the bytes we just read so the published identity matches what
	// we uploaded, not a possibly-stale scan result.
	entry := &tree.Entry{
		Path:     local.Path,
		Sha256:   local.Sha256,
		BlobHash: desc.SHA256,
		Size:     int64(len(plaintext)),
		ModTime:  local.ModTime,
	}
	if err := e.publish(ctx, entry); err != nil {
		return err
	}
	return e.idx.Put(entry)
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
		if prev, ok := out[entry.Path]; !ok || entry.ModTime.After(prev.ModTime) {
			out[entry.Path] = entry
		}
	}
	return out, nil
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
	tmp, err := os.CreateTemp(dir, ".tendrils-tmp-*")
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
