package engine

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"ca.punkscience.tendrils/internal/ignore"
	"ca.punkscience.tendrils/internal/tree"
)

func localSet(paths ...string) map[string]*tree.Entry {
	out := make(map[string]*tree.Entry, len(paths))
	for _, p := range paths {
		out[p] = &tree.Entry{Path: p}
	}
	return out
}

// Deepest-first is the whole safety argument for renaming without bookkeeping:
// each target's parent directories still have the names the list was built from
// at the moment it is renamed.
func TestUnportableTargetsAreDeepestFirst(t *testing.T) {
	local := localSet("music/a:b/c:d/track?.flac")
	got := unportableTargets(local, ignore.Compile(nil))

	want := []string{"music/a:b/c:d/track?.flac", "music/a:b/c:d", "music/a:b"}
	if len(got) != len(want) {
		t.Fatalf("got %d targets %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("target %d = %q, want %q (full order %v)", i, got[i], want[i], got)
		}
	}
}

// A bad directory shared by many files must be renamed once, not once per file
// beneath it — the second rename would fail, having no source left.
func TestUnportableTargetsDedupesSharedDirectory(t *testing.T) {
	local := localSet(
		"music/Covert:Two/01.flac",
		"music/Covert:Two/02.flac",
		"music/Covert:Two/03.flac",
	)
	got := unportableTargets(local, ignore.Compile(nil))
	if len(got) != 1 || got[0] != "music/Covert:Two" {
		t.Fatalf("got %v, want exactly [music/Covert:Two]", got)
	}
}

func TestUnportableTargetsIgnoresCleanPaths(t *testing.T) {
	local := localSet("music/album/track.flac", "notes/todo.md")
	if got := unportableTargets(local, ignore.Compile(nil)); len(got) != 0 {
		t.Fatalf("got %v, want none — every path is already portable", got)
	}
}

// An ignored file is never published, so nothing downstream can trip over its
// name. Renaming it would be an edit to the owner's tree that no one asked for.
func TestUnportableTargetsSkipsIgnoredPaths(t *testing.T) {
	local := localSet("scratch/what?.flac", "music/what?.flac")
	got := unportableTargets(local, ignore.Compile([]string{"scratch/"}))
	if len(got) != 1 || got[0] != "music/what?.flac" {
		t.Fatalf("got %v, want only [music/what?.flac]", got)
	}
}

func TestFreeNameAvoidsCollision(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "track.flac"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := freeName(dir, "track.flac")
	if err != nil {
		t.Fatal(err)
	}
	if want := "track (2).flac"; got != want {
		t.Fatalf("freeName = %q, want %q — the suffix must go before the extension", got, want)
	}

	if err := os.WriteFile(filepath.Join(dir, got), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = freeName(dir, "track.flac")
	if err != nil {
		t.Fatal(err)
	}
	if want := "track (3).flac"; got != want {
		t.Fatalf("freeName = %q, want %q", got, want)
	}
}

func TestFreeNameLeavesAnUntakenNameAlone(t *testing.T) {
	got, err := freeName(t.TempDir(), "track.flac")
	if err != nil {
		t.Fatal(err)
	}
	if want := "track.flac"; got != want {
		t.Fatalf("freeName = %q, want %q", got, want)
	}
}

func TestSplitRel(t *testing.T) {
	cases := []struct{ in, dir, name string }{
		{"a/b/c.flac", "a/b", "c.flac"},
		{"c.flac", "", "c.flac"},
		{"a/c.flac", "a", "c.flac"},
	}
	for _, c := range cases {
		dir, name := splitRel(c.in)
		if dir != c.dir || name != c.name {
			t.Errorf("splitRel(%q) = (%q, %q), want (%q, %q)", c.in, dir, name, c.dir, c.name)
		}
	}
}

// The end-to-end proof: a file whose directory Windows could never create is
// renamed on disk and published under the portable path, so the set never
// carries a name some device cannot hold.
//
// It needs a filesystem that accepts the bad name in the first place, which
// Windows is not — that is the entire bug. Run it on Linux.
func TestSyncRenamesUnportablePathBeforePublishing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a filesystem that can create a name containing ':' — the case under test")
	}

	root := t.TempDir()
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()
	eng := newEngine(t, root, id, ev, bl)

	mtime := time.Now().Add(-time.Hour).Truncate(time.Second)
	writeFile(t, root, "music/Covert:Two/what?.flac", "audio", mtime)

	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// Renamed on disk, under both the directory and the file component.
	if _, ok := readFile(t, root, "music/Covert-Two/what_.flac"); !ok {
		t.Error("the portable path does not exist on disk after the pass")
	}
	if _, ok := readFile(t, root, "music/Covert:Two/what?.flac"); ok {
		t.Error("the unportable path is still on disk after the pass")
	}

	// Published under the portable path, and never under the original.
	if _, ok := ev.byPath["music/Covert-Two/what_.flac"]; !ok {
		t.Errorf("no event published for the portable path; published: %v", publishedPaths(ev))
	}
	if _, ok := ev.byPath["music/Covert:Two/what?.flac"]; ok {
		t.Error("the unportable path was published to the set")
	}
}

// The rename must survive into the index, or the next pass sees the portable
// path as brand new and the file is republished every time.
func TestSyncAfterRenameIsStable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a filesystem that can create a name containing ':' — the case under test")
	}

	root := t.TempDir()
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()
	eng := newEngine(t, root, id, ev, bl)

	writeFile(t, root, "music/Covert:Two/track.flac", "audio", time.Now().Add(-time.Hour).Truncate(time.Second))

	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	after := bl.uploads

	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if bl.uploads != after {
		t.Errorf("second pass uploaded again (%d then %d) — the rename did not settle", after, bl.uploads)
	}
	if _, ok := readFile(t, root, "music/Covert-Two/track.flac"); !ok {
		t.Error("the portable path vanished on the second pass")
	}
}

func publishedPaths(ev *fakeEvents) []string {
	out := make([]string, 0, len(ev.byPath))
	for p := range ev.byPath {
		out = append(out, p)
	}
	return out
}
