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
func Tree(root string) (map[string]*tree.Entry, error) {
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
