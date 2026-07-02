package scan

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
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

	got, err := Tree(root)
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

	got, err := Tree(root)
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

func TestHashFileMatchesTree(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a/b.md", "content")

	one, err := HashFile(root, "a/b.md")
	if err != nil {
		t.Fatal(err)
	}
	all, _ := Tree(root)
	if one.Sha256 != all["a/b.md"].Sha256 {
		t.Errorf("HashFile and Tree disagree on hash")
	}
}
