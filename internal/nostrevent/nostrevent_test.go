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
		Path:    "notes/ideas.md",
		Sha256:  "abc123",
		Size:    99,
		ModTime: time.Unix(1_700_000_000, 0),
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
	if out.Sha256 != "" {
		t.Errorf("tombstone should carry no hash, got %q", out.Sha256)
	}
	if out.Path != "old.md" || !out.ModTime.Equal(in.ModTime) {
		t.Errorf("tombstone metadata mismatch: %+v", out)
	}
}

func TestCreatedAtTracksMtime(t *testing.T) {
	id := mustID(t)
	mt := time.Unix(1_699_999_999, 0)
	evt, _ := Sign(&tree.Entry{Path: "a.md", Sha256: "x", ModTime: mt}, id.SecretHex())
	if evt.CreatedAt.Time().Unix() != mt.Unix() {
		t.Errorf("created_at %d does not track mtime %d", evt.CreatedAt, mt.Unix())
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
