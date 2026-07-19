package scan

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ca.punkscience.tendrils/internal/tree"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTreeHashesFilesWithSlashPaths(t *testing.T) {
	root := t.TempDir()
	write(t, root, "notes/ideas.md", "hello")
	write(t, root, "top.md", "world")

	got, err := Tree(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 files, got %d: %v", len(got), got)
	}
	e, ok := got["notes/ideas.md"]
	if !ok {
		t.Fatalf("expected forward-slash key, keys: %v", got)
	}
	sum := sha256.Sum256([]byte("hello"))
	if e.Sha256 != hex.EncodeToString(sum[:]) {
		t.Errorf("sha mismatch: %s", e.Sha256)
	}
	if e.Size != 5 {
		t.Errorf("size = %d, want 5", e.Size)
	}
}

func TestTreeSkipsTrash(t *testing.T) {
	root := t.TempDir()
	write(t, root, "keep.md", "a")
	write(t, root, TrashDir+"/deleted.md", "b")

	got, err := Tree(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[TrashDir+"/deleted.md"]; ok {
		t.Errorf("trash file was scanned")
	}
	if _, ok := got["keep.md"]; !ok {
		t.Errorf("regular file missing")
	}
}

// A temp file orphaned by a crash mid-write must never be scanned (and so never
// published), at the root or nested in a subdirectory.
func TestTreeSkipsTempFiles(t *testing.T) {
	root := t.TempDir()
	write(t, root, "keep.md", "a")
	write(t, root, TempPrefix+"123", "half-written")
	write(t, root, "notes/"+TempPrefix+"456", "half-written")

	got, err := Tree(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected only keep.md, got %d: %v", len(got), got)
	}
	if _, ok := got["keep.md"]; !ok {
		t.Errorf("regular file missing")
	}
}

// With a matching base entry (same size and mtime), Tree reuses the stored hash
// instead of re-reading the file; when either attribute differs it re-hashes.
func TestTreeReusesUnchangedHashes(t *testing.T) {
	root := t.TempDir()
	write(t, root, "keep.md", "hello")
	mt := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(filepath.Join(root, "keep.md"), mt, mt); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, "keep.md"))
	if err != nil {
		t.Fatal(err)
	}

	// A deliberately wrong stored hash proves reuse: if Tree re-read the file it
	// would overwrite this with the real hash.
	const stale = "0000000000000000000000000000000000000000000000000000000000000000"
	reusing := map[string]*tree.Entry{
		"keep.md": {Path: "keep.md", Sha256: stale, Size: info.Size(), ModTime: info.ModTime()},
	}
	got, err := Tree(root, reusing)
	if err != nil {
		t.Fatal(err)
	}
	if got["keep.md"].Sha256 != stale {
		t.Errorf("unchanged file was re-hashed: got %q, want reused %q", got["keep.md"].Sha256, stale)
	}

	// Size differs from disk → base ignored, file re-hashed to its true content.
	changed := map[string]*tree.Entry{
		"keep.md": {Path: "keep.md", Sha256: stale, Size: 999, ModTime: info.ModTime()},
	}
	got, err = Tree(root, changed)
	if err != nil {
		t.Fatal(err)
	}
	real := sha256.Sum256([]byte("hello"))
	if got["keep.md"].Sha256 != hex.EncodeToString(real[:]) {
		t.Errorf("changed file not re-hashed: got %q", got["keep.md"].Sha256)
	}
}

func TestHashFileMatchesTree(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a/b.md", "content")

	one, err := HashFile(root, "a/b.md")
	if err != nil {
		t.Fatal(err)
	}
	all, _ := Tree(root, nil)
	if one.Sha256 != all["a/b.md"].Sha256 {
		t.Errorf("HashFile and Tree disagree on hash")
	}
}
