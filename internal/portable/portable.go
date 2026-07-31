// Package portable decides whether a filename can exist on every platform
// Tendrils syncs to, and rewrites it to a close equivalent when it cannot.
//
// A synced path is one shared identity across devices, so the set can only hold
// names every device can represent. Windows is the narrow one: NTFS rejects
// <>:"|?* (it reserves ':' for drive letters and alternate data streams),
// rejects the legacy DOS device names, and silently strips trailing dots and
// spaces. ext4 accepts all of them. A name legal only on the publishing device
// therefore enters the set and permanently breaks every device that cannot
// create it — the reconciler keeps deciding to pull, the write keeps failing,
// and nothing converges.
//
// This package is the policy half and does no I/O: it maps a name to a portable
// one. The engine owns the renaming, and renames the file *on disk* before
// publishing so the local name and the published path stay identical. Rewriting
// only the published name would be worse than the bug — the next scan would see
// one new file and one missing file, and publish a duplicate and a tombstone.
package portable

import (
	"strings"
	"unicode"
)

// replacements maps each character Windows rejects to the closest thing that
// reads the same. They are deliberately ASCII: a full-width lookalike (：？＊)
// survives a round trip but leaves the owner with a name they cannot type.
var replacements = map[rune]rune{
	'<':  '(',
	'>':  ')',
	':':  '-',
	'"':  '\'',
	'|':  '-',
	'?':  '_',
	'*':  '_',
	'\\': '-',
}

// reserved is the set of legacy DOS device names. Windows refuses these as a
// whole basename and as a stem before any extension, so "NUL.txt" is rejected
// as surely as "NUL".
var reserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// Name returns a single path component rewritten so every supported platform
// can hold it. It returns the input unchanged when it is already portable, so
// callers can compare the result to detect that no rename is needed.
//
// "." and ".." are returned unchanged: they are path syntax, not names, and a
// caller that passes them has a bug this function must not paper over.
func Name(name string) string {
	if name == "" || name == "." || name == ".." {
		return name
	}

	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f:
			// Control characters have no sensible visual equivalent and no
			// legitimate use in a filename; drop them rather than invent one.
			continue
		case replacements[r] != 0:
			b.WriteRune(replacements[r])
		default:
			b.WriteRune(r)
		}
	}

	// Windows strips trailing dots and spaces when creating a file, so a name
	// ending in either round-trips to a different name than the one published.
	out := strings.TrimRightFunc(b.String(), func(r rune) bool {
		return r == '.' || unicode.IsSpace(r)
	})

	// Everything was illegal, or the name was only dots and spaces. There is no
	// close equivalent left, so fall back to something that at least exists.
	if out == "" {
		return "_"
	}

	return disarmReserved(out)
}

// IsName reports whether a single path component is already portable.
func IsName(name string) bool { return Name(name) == name }

// Path returns a forward-slash relative path with every component made
// portable. It preserves the separators exactly, so the depth of the path and
// the identity of its parent directories are unchanged.
func Path(path string) string {
	if IsPath(path) {
		return path
	}
	parts := strings.Split(path, "/")
	for i, p := range parts {
		parts[i] = Name(p)
	}
	return strings.Join(parts, "/")
}

// IsPath reports whether every component of a forward-slash relative path is
// already portable. It is the cheap check the engine runs over a whole scan
// before deciding whether any renaming is needed at all.
func IsPath(path string) bool {
	for _, p := range strings.Split(path, "/") {
		if Name(p) != p {
			return false
		}
	}
	return true
}

// disarmReserved suffixes the *stem* of a DOS device name, so "COM1.txt"
// becomes "COM1_.txt" rather than "COM1.txt_". The distinction is not cosmetic:
// suffixing the whole name leaves the stem reserved, so the next pass would
// rename it again, and again, republishing the file forever.
//
// The stem is everything before the first dot, and the comparison is
// case-insensitive, because that is how Windows decides.
func disarmReserved(name string) string {
	stem, rest := name, ""
	if i := strings.IndexByte(name, '.'); i >= 0 {
		stem, rest = name[:i], name[i:]
	}
	if !reserved[strings.ToUpper(stem)] {
		return name
	}
	return stem + "_" + rest
}
