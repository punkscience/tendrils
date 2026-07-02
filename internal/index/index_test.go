package index

import (
	"path/filepath"
	"testing"
	"time"

	"ca.punkscience.tendrils/internal/tree"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestPutGetRoundTrip(t *testing.T) {
	s := openTemp(t)
	mt := time.Unix(1700000000, 0)
	e := &tree.Entry{Path: "notes/a.md", Sha256: "deadbeef", Size: 42, ModTime: mt}

	if err := s.Put(e); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("notes/a.md")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Sha256 != "deadbeef" || got.Size != 42 || !got.ModTime.Equal(mt) {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestGetMissingReturnsNil(t *testing.T) {
	s := openTemp(t)
	got, err := s.Get("nope")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("expected nil for missing path, got %+v", got)
	}
}

func TestAllIncludesTombstones(t *testing.T) {
	s := openTemp(t)
	_ = s.Put(&tree.Entry{Path: "live.md", Sha256: "aa"})
	_ = s.Put(&tree.Entry{Path: "gone.md", Deleted: true})

	all, err := s.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(all))
	}
	if !all["gone.md"].Deleted {
		t.Errorf("tombstone lost its Deleted flag")
	}
}

func TestLastReconcilePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.db")
	at := time.Unix(1700001234, 0)

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetLastReconcile(at); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, err := s2.LastReconcile()
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(at) {
		t.Errorf("last reconcile = %v, want %v", got, at)
	}
}
