// Package tree defines the smallest shared vocabulary of Tendrils: what one
// file's state looks like. An Entry is the payload of one Nostr event, one row
// in the local index, and one result of scanning the disk. Keeping it in a
// dependency-free leaf package lets index, scan, reconcile, and the event
// codec all speak it without importing each other.
package tree

import "time"

// Entry is the state of a single path in the synced tree at a point in time.
// The latest Entry per path (by the event that carries it) is the current
// truth. A deletion is an Entry with Deleted set — a tombstone — not the
// absence of an Entry.
type Entry struct {
	// Path is the file's location relative to the sync root, always using
	// forward slashes so the same file has the same identity on Windows and
	// Linux regardless of the local root path.
	Path string

	// Sha256 is the lowercase-hex SHA-256 of the file's *plaintext* content.
	// It is the blob's content address on the Blossom server (the blob stored
	// there is the encrypted form, but files are identified by plaintext hash so
	// identical content dedupes). Empty for a tombstone.
	Sha256 string

	// Size is the plaintext byte length. Zero for a tombstone.
	Size int64

	// ModTime is the file's modification time. It is the sole input to
	// last-writer-wins conflict resolution, trusted as-is (see the clock-skew
	// risk in docs/OPEN-QUESTIONS.md).
	ModTime time.Time

	// Deleted marks this Entry as a tombstone: the file was removed. A delete is
	// absolute and beats a concurrent edit, but the removed bytes stay
	// recoverable in the trash for the retention window.
	Deleted bool
}

// Live reports whether e is a present file (non-nil and not a tombstone).
func (e *Entry) Live() bool { return e != nil && !e.Deleted }

// Tomb reports whether e is a tombstone.
func (e *Entry) Tomb() bool { return e != nil && e.Deleted }

// SameContent reports whether two entries name the same present-file content.
func SameContent(a, b *Entry) bool {
	return a.Live() && b.Live() && a.Sha256 == b.Sha256
}
