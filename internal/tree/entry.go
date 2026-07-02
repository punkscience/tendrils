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

	// Sha256 is the lowercase-hex SHA-256 of the file's *plaintext* content. It is
	// the file's content identity: it drives dedup and the reconciler's
	// same-content check, and a pulling device verifies decrypted bytes against
	// it. It is *not* the Blossom address (that is BlobHash) — the stored blob is
	// the encrypted form, whose hash differs. Empty for a tombstone.
	Sha256 string

	// BlobHash is the lowercase-hex SHA-256 of the *sealed* (encrypted) bytes as
	// stored on the Blossom server — the address a device fetches by. Because
	// encryption uses a random nonce per blob, the same plaintext seals to
	// different bytes on each device, so this is whatever the publishing device
	// actually uploaded. Empty for a tombstone and for a freshly scanned local
	// entry that has not been sealed and uploaded yet.
	BlobHash string

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
