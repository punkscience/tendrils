package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"ca.punkscience.tendrils/internal/blob"
	"ca.punkscience.tendrils/internal/crypt"
	"ca.punkscience.tendrils/internal/index"
	"ca.punkscience.tendrils/internal/keys"
	"ca.punkscience.tendrils/internal/nostrevent"
	"ca.punkscience.tendrils/internal/scan"
	"ca.punkscience.tendrils/internal/tree"
)

// fakeEvents is an in-memory relay that mimics the one property the engine
// relies on: it keeps only the newest replaceable event per path (d tag).
type fakeEvents struct {
	byPath map[string]*nostr.Event
}

func newFakeEvents() *fakeEvents { return &fakeEvents{byPath: map[string]*nostr.Event{}} }

func (f *fakeEvents) Publish(_ context.Context, evt *nostr.Event) error {
	d := evt.Tags.GetD()
	if prev, ok := f.byPath[d]; !ok || evt.CreatedAt >= prev.CreatedAt {
		f.byPath[d] = evt
	}
	return nil
}

func (f *fakeEvents) Fetch(_ context.Context, _ string) ([]*nostr.Event, error) {
	out := make([]*nostr.Event, 0, len(f.byPath))
	for _, e := range f.byPath {
		out = append(out, e)
	}
	return out, nil
}

// fakeBlobs is an in-memory Blossom server addressed by content hash.
type fakeBlobs struct {
	data map[string][]byte
}

func newFakeBlobs() *fakeBlobs { return &fakeBlobs{data: map[string][]byte{}} }

func (f *fakeBlobs) Upload(_ context.Context, data []byte) (blob.Descriptor, error) {
	sum := hashHex(data)
	cp := append([]byte(nil), data...)
	f.data[sum] = cp
	return blob.Descriptor{SHA256: sum, Size: int64(len(data)), URL: "mem://" + sum}, nil
}

func (f *fakeBlobs) Download(_ context.Context, sha256 string) ([]byte, error) {
	b, ok := f.data[sha256]
	if !ok {
		return nil, blob.ErrNotFound
	}
	return append([]byte(nil), b...), nil
}

func newEngine(t *testing.T, root string, id *keys.Identity, ev EventStore, bl BlobStore) *Engine {
	t.Helper()
	idx, err := index.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { idx.Close() })
	eng, err := New(root, id, idx, bl, ev, nil)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return eng
}

func writeFile(t *testing.T, root, rel, content string, mtime time.Time) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(abs, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
}

func readFile(t *testing.T, root, rel string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b), true
}

// seedRemote simulates another device having already sealed, uploaded, and
// published a file, so a single engine can be tested against a populated relay.
func seedRemote(t *testing.T, ev EventStore, bl BlobStore, id *keys.Identity, path, content string, mtime time.Time) {
	t.Helper()
	symKey, err := id.SymmetricKey()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := crypt.Seal(symKey, []byte(content))
	if err != nil {
		t.Fatal(err)
	}
	desc, err := bl.Upload(context.Background(), sealed)
	if err != nil {
		t.Fatal(err)
	}
	entry := &tree.Entry{
		Path:     path,
		Sha256:   hashHex([]byte(content)),
		BlobHash: desc.SHA256,
		Size:     int64(len(content)),
		ModTime:  mtime,
	}
	evt, err := nostrevent.Sign(entry, id.SecretHex())
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.Publish(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
}

func mustID(t *testing.T) *keys.Identity {
	t.Helper()
	id, err := keys.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Sync reports per-file progress: one callback as each planned action begins,
// counting up to a stable total, then a final idle report with no current path.
func TestProgressReporting(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()
	root := t.TempDir()
	writeFile(t, root, "a.md", "one", time.Unix(1_700_000_000, 0))
	writeFile(t, root, "b.md", "two", time.Unix(1_700_000_000, 0))
	writeFile(t, root, "c.md", "three", time.Unix(1_700_000_000, 0))

	eng := newEngine(t, root, id, ev, bl)
	var seen []Progress
	eng.OnProgress(func(p Progress) { seen = append(seen, p) })

	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// Three publishes plus the final idle report.
	if len(seen) != 4 {
		t.Fatalf("expected 4 progress reports, got %d: %+v", len(seen), seen)
	}
	for i, p := range seen[:3] {
		if p.Total != 3 {
			t.Errorf("report %d: total=%d want 3", i, p.Total)
		}
		if p.Done != i {
			t.Errorf("report %d: done=%d want %d", i, p.Done, i)
		}
		if p.Path == "" || p.Op != "uploading" {
			t.Errorf("report %d: path=%q op=%q, want a path and \"uploading\"", i, p.Path, p.Op)
		}
	}
	if final := seen[3]; final.Done != 3 || final.Total != 3 || final.Path != "" {
		t.Errorf("final report = %+v, want {Done:3 Total:3 Path:\"\"}", final)
	}
}

// A pass with nothing to do still emits a single idle report, so a UI can clear
// any stale "in progress" line.
func TestProgressNoActions(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()
	eng := newEngine(t, t.TempDir(), id, ev, bl)

	var seen []Progress
	eng.OnProgress(func(p Progress) { seen = append(seen, p) })
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(seen) != 1 || seen[0].Total != 0 || seen[0].Path != "" {
		t.Fatalf("expected one idle report, got %+v", seen)
	}
}

// OnStats reports the outstanding-work counts a pass computed for free: two
// local-only files to push, and one conflict copy sitting in the tree (which is
// counted as a conflict, not double-counted as pending).
func TestStatsReporting(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()
	root := t.TempDir()
	writeFile(t, root, "a.md", "one", time.Unix(1_700_000_000, 0))
	writeFile(t, root, "b.md", "two", time.Unix(1_700_000_000, 0))
	writeFile(t, root, "c"+scan.ConflictMarker+"deadbeef.md", "conflict", time.Unix(1_700_000_000, 0))

	eng := newEngine(t, root, id, ev, bl)
	var pending, conflicts int
	var calls int
	eng.OnStats(func(p, c int) { pending, conflicts, calls = p, c, calls+1 })

	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if calls != 1 {
		t.Errorf("OnStats called %d times, want 1 per pass", calls)
	}
	if pending != 2 {
		t.Errorf("pending = %d, want 2 (a.md, b.md)", pending)
	}
	if conflicts != 1 {
		t.Errorf("conflicts = %d, want 1", conflicts)
	}
}

// A local-only file is sealed, uploaded, and published on Sync.
func TestPublishLocalFile(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()
	root := t.TempDir()
	writeFile(t, root, "notes/a.md", "hello", time.Unix(1_700_000_000, 0))

	eng := newEngine(t, root, id, ev, bl)
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	got, err := ev.Fetch(context.Background(), id.PublicHex())
	if err != nil || len(got) != 1 {
		t.Fatalf("expected 1 published event, got %d (err %v)", len(got), err)
	}
	entry, err := nostrevent.Parse(got[0])
	if err != nil {
		t.Fatal(err)
	}
	if entry.Path != "notes/a.md" || entry.BlobHash == "" {
		t.Errorf("published entry wrong: %+v", entry)
	}
	if _, ok := bl.data[entry.BlobHash]; !ok {
		t.Errorf("blob %s was not uploaded", entry.BlobHash)
	}
	// The uploaded blob must be ciphertext, never the plaintext.
	if string(bl.data[entry.BlobHash]) == "hello" {
		t.Errorf("blob stored plaintext")
	}
}

// Paths matched by .tendrilsignore are invisible to reconcile: an ignored local
// file is never published, an ignored remote file is never pulled, and an
// existing local copy is left untouched.
func TestSyncSkipsIgnored(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()

	// A file present only on the relay whose path is ignored must not be pulled.
	seedRemote(t, ev, bl, id, ".obsidian/workspace.json", "remote ui state", time.Unix(1_700_000_050, 0))

	root := t.TempDir()
	writeFile(t, root, ".tendrilsignore", ".obsidian/workspace*.json\n.trash/\n", time.Unix(1_700_000_000, 0))
	writeFile(t, root, "note.md", "hello", time.Unix(1_700_000_000, 0))
	writeFile(t, root, ".trash/old.md", "trashed", time.Unix(1_700_000_000, 0))

	eng := newEngine(t, root, id, ev, bl)
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	got, err := ev.Fetch(context.Background(), id.PublicHex())
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]*tree.Entry{}
	for _, e := range got {
		ent, err := nostrevent.Parse(e)
		if err != nil {
			t.Fatal(err)
		}
		byPath[ent.Path] = ent
	}
	if e, ok := byPath["note.md"]; !ok || e.BlobHash == "" {
		t.Errorf("note.md not published properly: %+v", e)
	}
	if _, ok := byPath[".trash/old.md"]; ok {
		t.Errorf("ignored local file was published")
	}
	if _, ok := readFile(t, root, ".obsidian/workspace.json"); ok {
		t.Errorf("ignored remote file was pulled to disk")
	}
	if got, ok := readFile(t, root, ".trash/old.md"); !ok || got != "trashed" {
		t.Errorf("ignored local file changed: %q present=%v", got, ok)
	}
}

// A file present only on the relay is pulled and written locally.
func TestPullRemoteFile(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()
	seedRemote(t, ev, bl, id, "docs/b.txt", "remote content", time.Unix(1_700_000_100, 0))

	root := t.TempDir()
	eng := newEngine(t, root, id, ev, bl)
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	got, ok := readFile(t, root, "docs/b.txt")
	if !ok || got != "remote content" {
		t.Errorf("pulled file = %q (present=%v), want %q", got, ok, "remote content")
	}
}

// Two devices sharing one key and one relay/Blossom converge: what A publishes,
// B receives — the whole product, headless.
func TestTwoDeviceConvergence(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()

	rootA := t.TempDir()
	writeFile(t, rootA, "shared/note.md", "written on A", time.Unix(1_700_000_200, 0))
	engA := newEngine(t, rootA, id, ev, bl)
	if err := engA.Sync(context.Background()); err != nil {
		t.Fatalf("A sync: %v", err)
	}

	rootB := t.TempDir()
	engB := newEngine(t, rootB, id, ev, bl)
	if err := engB.Sync(context.Background()); err != nil {
		t.Fatalf("B sync: %v", err)
	}

	got, ok := readFile(t, rootB, "shared/note.md")
	if !ok || got != "written on A" {
		t.Errorf("B has %q (present=%v), want %q", got, ok, "written on A")
	}
}

// A locally deleted file becomes a tombstone on the relay, and a second device
// then moves its copy to the trash — the delete propagates and stays recoverable.
func TestDeletePropagation(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()

	rootA := t.TempDir()
	writeFile(t, rootA, "gone.md", "temporary", time.Unix(1_700_000_300, 0))
	engA := newEngine(t, rootA, id, ev, bl)
	if err := engA.Sync(context.Background()); err != nil {
		t.Fatalf("A publish: %v", err)
	}

	rootB := t.TempDir()
	engB := newEngine(t, rootB, id, ev, bl)
	if err := engB.Sync(context.Background()); err != nil {
		t.Fatalf("B pull: %v", err)
	}
	if _, ok := readFile(t, rootB, "gone.md"); !ok {
		t.Fatal("precondition: B should have the file before the delete")
	}

	// A deletes the file and syncs → tombstone published.
	if err := os.Remove(filepath.Join(rootA, "gone.md")); err != nil {
		t.Fatal(err)
	}
	if err := engA.Sync(context.Background()); err != nil {
		t.Fatalf("A delete sync: %v", err)
	}

	// B syncs → its copy moves to the trash.
	if err := engB.Sync(context.Background()); err != nil {
		t.Fatalf("B delete sync: %v", err)
	}
	if _, ok := readFile(t, rootB, "gone.md"); ok {
		t.Errorf("B still has the deleted file")
	}
	if _, ok := readFile(t, rootB, filepath.ToSlash(filepath.Join(scan.TrashDir, "gone.md"))); !ok {
		t.Errorf("deleted file was not preserved in the trash")
	}
}

// When the remote wins over a diverged local edit, the local version is kept as
// a conflict copy — a wrong last-writer-wins guess costs a rename, never data.
func TestConflictCopyPreservesLocalEdit(t *testing.T) {
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()

	root := t.TempDir()
	// Local file diverged from a recorded base and is older than the remote.
	writeFile(t, root, "c.md", "local edit", time.Unix(1_700_000_000, 0))
	eng := newEngine(t, root, id, ev, bl)
	// Record a base that differs from the local content (an earlier synced state).
	if err := eng.idx.Put(&tree.Entry{Path: "c.md", Sha256: hashHex([]byte("v1 base")), ModTime: time.Unix(1_699_000_000, 0)}); err != nil {
		t.Fatal(err)
	}
	// Remote is newer and wins.
	seedRemote(t, ev, bl, id, "c.md", "remote wins", time.Unix(1_700_000_500, 0))

	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	if got, _ := readFile(t, root, "c.md"); got != "remote wins" {
		t.Errorf("c.md = %q, want %q", got, "remote wins")
	}
	conflict := conflictCopyPath("c.md", id.PublicHex())
	if got, ok := readFile(t, root, conflict); !ok || got != "local edit" {
		t.Errorf("conflict copy %q = %q (present=%v), want %q", conflict, got, ok, "local edit")
	}
}
