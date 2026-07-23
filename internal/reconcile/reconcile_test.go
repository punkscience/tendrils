package reconcile

import (
	"testing"
	"time"

	"ca.punkscience.tendrils/internal/tree"
)

var (
	t0 = time.Unix(1_000_000, 0)
	t1 = t0.Add(time.Hour)
	t2 = t1.Add(time.Hour)
)

func file(path, sha string, mt time.Time) *tree.Entry {
	return &tree.Entry{Path: path, Sha256: sha, Size: 1, ModTime: mt}
}
func tomb(path string, mt time.Time) *tree.Entry {
	return &tree.Entry{Path: path, ModTime: mt, Deleted: true}
}

func want(t *testing.T, got Decision, op Op, conflict bool) {
	t.Helper()
	if got.Op != op {
		t.Errorf("op = %v, want %v (%s)", got.Op, op, got.Reason)
	}
	if got.ConflictCopy != conflict {
		t.Errorf("conflictCopy = %v, want %v (%s)", got.ConflictCopy, conflict, got.Reason)
	}
}

// "A delete is absolute." This device deleted the file and recorded the
// tombstone; a concurrent edit on another device then displaced that tombstone
// on the relay (one event survives per path). The edit predates our delete, so
// re-assert the tombstone instead of pulling the file back.
func TestConcurrentEditDoesNotResurrectDeletedFile(t *testing.T) {
	base := tomb("notes/gone.md", t2)             // we deleted it at t2
	remote := file("notes/gone.md", "edited", t1) // someone edited at t1, published later
	want(t, Decide(base, nil, remote), OpPublishDelete, false)
}

// The counterpart: a remote entry *newer* than our tombstone is a deliberate
// re-creation on another device, which is honoured rather than re-deleted.
func TestRecreationAfterOurDeleteIsPulled(t *testing.T) {
	base := tomb("notes/gone.md", t1)                // we deleted it at t1
	remote := file("notes/gone.md", "recreated", t2) // re-created afterwards at t2
	want(t, Decide(base, nil, remote), OpWriteRemote, false)
}

// A never-synced device (no base at all) must still pull, not mistake an absent
// base for an applied delete.
func TestNoBaseStillPulls(t *testing.T) {
	want(t, Decide(nil, nil, file("notes/new.md", "aaa", t1)), OpWriteRemote, false)
}

// local-first-sync: "A new file appears on the other device" — from the
// receiving device's view, a file it lacks is pulled.
func TestNewRemoteFileIsPulled(t *testing.T) {
	d := Decide(nil, nil, file("projects/tendrils.md", "aaa", t1))
	want(t, d, OpWriteRemote, false)
}

// local-first-sync: "A new file appears on the other device" — from the
// originating device's view, a local-only file is published.
func TestLocalOnlyFileIsPublished(t *testing.T) {
	d := Decide(nil, file("projects/tendrils.md", "aaa", t1), nil)
	want(t, d, OpPublishLocal, false)
}

// local-first-sync: "An edit propagates to the other device" — remote newer,
// local unchanged since base: pull cleanly, no conflict copy.
func TestRemoteEditPulledCleanly(t *testing.T) {
	base := file("notes/ideas.md", "v1", t0)
	local := file("notes/ideas.md", "v1", t0) // unchanged here
	remote := file("notes/ideas.md", "v2", t1)
	want(t, Decide(base, local, remote), OpWriteRemote, false)
}

// The originating side of an edit: local newer than remote → publish.
func TestLocalEditPublished(t *testing.T) {
	base := file("notes/ideas.md", "v1", t0)
	local := file("notes/ideas.md", "v2", t1)
	remote := file("notes/ideas.md", "v1", t0)
	want(t, Decide(base, local, remote), OpPublishLocal, false)
}

// Converged: identical content is a no-op regardless of mtime jitter.
func TestSameContentIsNoop(t *testing.T) {
	local := file("a.md", "same", t1)
	remote := file("a.md", "same", t2)
	want(t, Decide(local, local, remote), OpNone, false)
}

// local-first-sync: "The same file diverges while one device is offline" —
// both sides changed; remote is newer; the local change is preserved as a
// conflict copy so no edit is destroyed.
func TestDivergenceKeepsLoserAsConflictCopy(t *testing.T) {
	base := file("daily/2026-07-02.md", "v0", t0)
	local := file("daily/2026-07-02.md", "laptopEdit", t1)
	remote := file("daily/2026-07-02.md", "desktopEdit", t2) // agent rewrote later
	want(t, Decide(base, local, remote), OpWriteRemote, true)
}

// local-first-sync: "A delete removes the file everywhere" — a remote tombstone
// removes the unchanged local file (to trash).
func TestDeletePropagates(t *testing.T) {
	base := file("old.md", "v1", t0)
	local := file("old.md", "v1", t0)
	remote := tomb("old.md", t1)
	want(t, Decide(base, local, remote), OpDeleteLocal, false)
}

// local-first-sync: "A delete beats a concurrent edit" — remote tombstone,
// local edited later; delete still wins and the edit lands in the trash.
func TestDeleteBeatsConcurrentEdit(t *testing.T) {
	base := file("stale.md", "v1", t0)
	local := file("stale.md", "edited", t2) // edited AFTER the delete
	remote := tomb("stale.md", t1)
	want(t, Decide(base, local, remote), OpDeleteLocal, false)
}

// local-first-sync: "Re-creating a deleted filename later is honoured" — the
// delete was already applied here (base is a tombstone); a new file at that
// path publishes and supersedes the old delete.
func TestRecreationAfterDeleteIsHonoured(t *testing.T) {
	base := tomb("notes.md", t0)              // last week's delete, already applied
	local := file("notes.md", "brandnew", t2) // created today
	remote := tomb("notes.md", t0)
	want(t, Decide(base, local, remote), OpPublishLocal, false)
}

// A local delete (file gone from disk, base was live, set still has it live)
// propagates as a tombstone.
func TestLocalDeletePublishesTombstone(t *testing.T) {
	base := file("gone.md", "v1", t0)
	remote := file("gone.md", "v1", t0)
	want(t, Decide(base, nil, remote), OpPublishDelete, false)
}

// Already-deleted-here against a remote tombstone is a no-op.
func TestBothDeletedIsNoop(t *testing.T) {
	want(t, Decide(tomb("x.md", t0), nil, tomb("x.md", t0)), OpNone, false)
}

// Exact mtime tie on divergent content resolves deterministically and the same
// way from both sides (one publishes, the other pulls-with-conflict-copy).
func TestMtimeTieIsDeterministic(t *testing.T) {
	base := file("t.md", "v0", t0)
	high := file("t.md", "zzz", t1)
	low := file("t.md", "aaa", t1)

	// Device holding the higher-hash content publishes it.
	want(t, Decide(base, high, low), OpPublishLocal, false)
	// Device holding the lower-hash content pulls and keeps its own as conflict.
	want(t, Decide(base, low, high), OpWriteRemote, true)
}
