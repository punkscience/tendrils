package nostrevent

import (
	"testing"
	"time"

	"ca.punkscience.tendrils/internal/keys"
	"ca.punkscience.tendrils/internal/tree"
)

func mustID(t *testing.T) *keys.Identity {
	t.Helper()
	id, err := keys.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSignParseRoundTripLive(t *testing.T) {
	id := mustID(t)
	in := &tree.Entry{
		Path:     "notes/ideas.md",
		Sha256:   "abc123",
		BlobHash: "def456",
		Size:     99,
		ModTime:  time.Unix(1_700_000_000, 0),
	}
	evt, err := Sign(in, id.SecretHex())
	if err != nil {
		t.Fatal(err)
	}
	if evt.PubKey != id.PublicHex() {
		t.Errorf("event signed by unexpected key")
	}
	out, err := Parse(evt)
	if err != nil {
		t.Fatal(err)
	}
	if out.Path != in.Path || out.Sha256 != in.Sha256 || out.Size != in.Size || !out.ModTime.Equal(in.ModTime) {
		t.Errorf("round-trip mismatch: %+v vs %+v", out, in)
	}
	if out.BlobHash != in.BlobHash {
		t.Errorf("blob hash mismatch: got %q want %q", out.BlobHash, in.BlobHash)
	}
	if out.Deleted {
		t.Errorf("live entry parsed as tombstone")
	}
}

func TestSignParseRoundTripTombstone(t *testing.T) {
	id := mustID(t)
	in := &tree.Entry{Path: "old.md", ModTime: time.Unix(1_700_000_500, 0), Deleted: true}

	evt, err := Sign(in, id.SecretHex())
	if err != nil {
		t.Fatal(err)
	}
	out, err := Parse(evt)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Deleted {
		t.Errorf("tombstone did not round-trip as deleted")
	}
	if out.Sha256 != "" || out.BlobHash != "" {
		t.Errorf("tombstone should carry no hashes, got x=%q blob=%q", out.Sha256, out.BlobHash)
	}
	if out.Path != "old.md" || !out.ModTime.Equal(in.ModTime) {
		t.Errorf("tombstone metadata mismatch: %+v", out)
	}
}

// created_at is publication time, so a file with an old mtime still produces an
// event the relay accepts as the newest for its path. Were created_at pinned to
// mtime, restoring an old file (or re-creating a deleted one) would be silently
// discarded by the relay as stale.
func TestCreatedAtIsPublicationTimeNotMtime(t *testing.T) {
	id := mustID(t)
	mt := time.Unix(1_000_000_000, 0) // long in the past
	before := time.Now().Unix()
	evt, err := Sign(&tree.Entry{Path: "a.md", Sha256: "x", ModTime: mt}, id.SecretHex())
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now().Unix()

	if got := evt.CreatedAt.Time().Unix(); got < before || got > after {
		t.Errorf("created_at %d is not the publication time (want %d..%d)", got, before, after)
	}
	// The mtime must survive intact in its own tag — that is what LWW reads.
	out, err := Parse(evt)
	if err != nil {
		t.Fatal(err)
	}
	if !out.ModTime.Equal(mt) {
		t.Errorf("mtime round-trip lost: got %v want %v", out.ModTime, mt)
	}
}

// A tombstone stamped now, then the file re-created with its original older
// mtime: the re-creation must still publish an event that supersedes the
// tombstone at the relay. This is the case that was previously unpublishable.
func TestRecreationAfterDeleteOutranksTombstone(t *testing.T) {
	id := mustID(t)
	tombstone, err := Sign(&tree.Entry{Path: "a.md", Deleted: true, ModTime: time.Now()}, id.SecretHex())
	if err != nil {
		t.Fatal(err)
	}
	recreated, err := Sign(&tree.Entry{
		Path:    "a.md",
		Sha256:  "x",
		ModTime: time.Unix(1_000_000_000, 0), // restored file keeps its old mtime
	}, id.SecretHex())
	if err != nil {
		t.Fatal(err)
	}
	if recreated.CreatedAt < tombstone.CreatedAt {
		t.Errorf("re-creation created_at %d predates the tombstone's %d; the relay would discard it",
			recreated.CreatedAt, tombstone.CreatedAt)
	}
}

func TestParseRejectsTamperedSignature(t *testing.T) {
	id := mustID(t)
	evt, _ := Sign(&tree.Entry{Path: "a.md", Sha256: "x", ModTime: time.Unix(1, 0)}, id.SecretHex())
	evt.Content = "tampered" // invalidates the signature over the serialized event
	if _, err := Parse(evt); err == nil {
		t.Errorf("expected signature failure on tampered event")
	}
}

func TestParseRejectsWrongKind(t *testing.T) {
	id := mustID(t)
	evt, _ := Sign(&tree.Entry{Path: "a.md", Sha256: "x", ModTime: time.Unix(1, 0)}, id.SecretHex())
	evt.Kind = 1
	if _, err := Parse(evt); err == nil {
		t.Errorf("expected wrong-kind rejection")
	}
}
