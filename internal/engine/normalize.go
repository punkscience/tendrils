package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"ca.punkscience.tendrils/internal/ignore"
	"ca.punkscience.tendrils/internal/portable"
	"ca.punkscience.tendrils/internal/tree"
)

// normalizeNames renames any local file or directory whose name cannot exist on
// every platform, before the pass decides what to publish. Renaming at the
// source is what keeps the local name and the published path identical; see the
// package comment on internal/portable for why rewriting only the published
// name would be worse than the bug.
//
// It reports whether anything moved, which tells the caller the scan it holds is
// stale and must be redone. Ignored paths are left alone: they are never
// published, so nothing downstream can trip over them and renaming them would be
// an edit to the owner's tree that no one asked for.
func (e *Engine) normalizeNames(local map[string]*tree.Entry, ign *ignore.Matcher) (bool, error) {
	targets := unportableTargets(local, ign)
	if len(targets) == 0 {
		return false, nil // the common case: one string scan per path, no I/O
	}

	var errs []error
	moved := false
	for _, rel := range targets {
		to, err := e.renameToPortable(rel)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		moved = true
		e.log.Info("renamed to a portable name", "from", rel, "to", to,
			"reason", "name cannot be created on every platform")
	}
	if len(errs) > 0 {
		return moved, fmt.Errorf("engine: normalize names: %w", errors.Join(errs...))
	}
	return moved, nil
}

// unportableTargets returns every path that needs renaming — files and the
// directories above them alike — deepest first.
//
// Depth order is what makes the renaming safe without any bookkeeping: a
// deeper path is renamed while every directory above it still has the name the
// list was built from, so no entry in the list is ever invalidated by an
// earlier rename.
func unportableTargets(local map[string]*tree.Entry, ign *ignore.Matcher) []string {
	seen := make(map[string]struct{})
	for path := range local {
		if ign.Match(path) {
			continue
		}
		if portable.IsPath(path) {
			continue
		}
		// Record the offending component itself, not the whole path: the bad
		// name may be a directory shared by many files, and it must be renamed
		// once rather than once per file beneath it.
		parts := strings.Split(path, "/")
		for i, p := range parts {
			if !portable.IsName(p) {
				seen[strings.Join(parts[:i+1], "/")] = struct{}{}
			}
		}
	}

	out := make([]string, 0, len(seen))
	for rel := range seen {
		out = append(out, rel)
	}
	sort.Slice(out, func(i, j int) bool {
		di, dj := strings.Count(out[i], "/"), strings.Count(out[j], "/")
		if di != dj {
			return di > dj
		}
		return out[i] < out[j] // stable, so a failure is reproducible
	})
	return out
}

// renameToPortable renames one path's final component in place and returns its
// new sync-root-relative path. The parent is untouched, so the caller's
// deepest-first ordering keeps every later target valid.
func (e *Engine) renameToPortable(rel string) (string, error) {
	dirRel, name := splitRel(rel)
	absDir := e.abs(dirRel)

	target, err := freeName(absDir, portable.Name(name))
	if err != nil {
		return "", fmt.Errorf("%s: %w", rel, err)
	}
	if err := os.Rename(filepath.Join(absDir, name), filepath.Join(absDir, target)); err != nil {
		return "", fmt.Errorf("%s: rename to %q: %w", rel, target, err)
	}
	return joinRel(dirRel, target), nil
}

// freeName returns want, or want with a numeric suffix, so the rename cannot
// overwrite something already there. Two different illegal names can sanitize to
// the same legal one, and the portable name may simply already be taken — either
// way, silently replacing a file to fix a filename would be a poor trade.
func freeName(absDir, want string) (string, error) {
	ext := filepath.Ext(want)
	stem := strings.TrimSuffix(want, ext)
	for n := 1; n < 1000; n++ {
		candidate := want
		if n > 1 {
			candidate = fmt.Sprintf("%s (%d)%s", stem, n, ext)
		}
		_, err := os.Lstat(filepath.Join(absDir, candidate))
		if os.IsNotExist(err) {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("no free portable name for %q", want)
}

// splitRel splits a forward-slash relative path into its parent and final
// component. filepath.Split is not usable here: these paths are always
// slash-separated regardless of platform.
func splitRel(rel string) (dir, name string) {
	i := strings.LastIndexByte(rel, '/')
	if i < 0 {
		return "", rel
	}
	return rel[:i], rel[i+1:]
}

func joinRel(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}
