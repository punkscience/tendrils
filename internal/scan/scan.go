// Package scan walks the sync root and reports the current on-disk state as a
// map of path → *tree.Entry. It is how the engine learns "what the tree looks
// like now" to compare against the index (base) and the relay (remote).
//
// Content is addressed by the SHA-256 of the plaintext file bytes, matching the
// hash the reconciler and event codec use.
package scan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"ca.punkscience.tendrils/internal/tree"
)

// TrashDir is the sync-root-relative folder where deleted files are retained.
// It is Tendrils' own bookkeeping and is never itself synced.
const TrashDir = ".tendrils-trash"

// TempPrefix is the basename prefix of the temp files atomicWrite creates in the
// target's directory before renaming into place. They live inside the sync root,
// so scan must skip them by prefix: a crash between write and rename leaves one
// behind, and it must never be mistaken for a real file and published.
const TempPrefix = ".tendrils-tmp-"

// isTempFile reports whether a path's basename is an atomic-write temp file.
func isTempFile(rel string) bool {
	return strings.HasPrefix(filepath.Base(rel), TempPrefix)
}

// ConflictMarker is embedded in the filename of a preserved losing version when
// two devices diverge. A conflict copy stays in the tree and syncs like any
// other file, so the owner sees it everywhere and can resolve it with a rename.
const ConflictMarker = ".tendrils-conflict-"

// IsConflictCopy reports whether a path is a preserved conflict copy.
func IsConflictCopy(path string) bool {
	return strings.Contains(filepath.Base(path), ConflictMarker)
}

// Tree walks root and returns a live Entry for every regular file, keyed by
// forward-slash relative path. The trash directory and dotfiles Tendrils owns
// are skipped. Symlinks are not followed.
//
// base is the last-synced index (nil for a full hash). A file whose size and
// modification time still match its base entry is assumed unchanged and its
// stored hash is reused instead of re-reading and re-hashing the bytes — the
// difference between a scan that stats every file and one that reads all of them,
// which on a large tree is the whole cost of a pass. The tradeoff is the standard
// one: a change that preserves both size and mtime (rare, and defeats most sync
// tools) is missed until one of them moves.
func Tree(root string, base map[string]*tree.Entry) (map[string]*tree.Entry, error) {
	out := make(map[string]*tree.Entry)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)

		if d.IsDir() {
			if rel != "." && ignoredDir(rel) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // skip symlinks, sockets, devices
		}
		if isTempFile(rel) {
			return nil // an atomic-write temp orphaned by a crash; never sync it
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("scan: stat %s: %w", rel, err)
		}
		if e := reuse(base[rel], rel, info); e != nil {
			out[rel] = e
			return nil
		}

		entry, err := hashFile(path, rel)
		if err != nil {
			return err
		}
		out[rel] = entry
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan: walk %s: %w", root, err)
	}
	return out, nil
}

// reuse returns a scan entry taken from base when the on-disk file's size and
// mtime still match it, so its stored content hash can be trusted without
// re-reading the bytes. It returns nil when there is no live, hashed base entry
// or either attribute differs — the caller then hashes the file.
func reuse(base *tree.Entry, rel string, info fs.FileInfo) *tree.Entry {
	if base == nil || !base.Live() || base.Sha256 == "" {
		return nil
	}
	if base.Size != info.Size() || !base.ModTime.Equal(info.ModTime()) {
		return nil
	}
	return &tree.Entry{
		Path:    rel,
		Sha256:  base.Sha256,
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}
}

// HashFile computes the entry (path, sha256, size, mtime) for a single file.
func HashFile(root, rel string) (*tree.Entry, error) {
	return hashFile(filepath.Join(root, filepath.FromSlash(rel)), rel)
}

func hashFile(abs, rel string) (*tree.Entry, error) {
	f, err := os.Open(abs)
	if err != nil {
		return nil, fmt.Errorf("scan: open %s: %w", rel, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("scan: stat %s: %w", rel, err)
	}

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, fmt.Errorf("scan: hash %s: %w", rel, err)
	}

	return &tree.Entry{
		Path:    rel,
		Sha256:  hex.EncodeToString(h.Sum(nil)),
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}, nil
}

// ignoredDir reports whether a directory (by its root-relative slash path) is
// Tendrils bookkeeping that must not be synced.
func ignoredDir(rel string) bool {
	return rel == TrashDir || strings.HasPrefix(rel, TrashDir+"/")
}
