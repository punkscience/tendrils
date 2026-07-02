// Package nostrevent maps between a tree.Entry and the Nostr event that carries
// it across the relay. There is no monolithic manifest: each changed file is
// one small event, and the latest event per path is the current truth.
//
// Event model — parameterized replaceable event (NIP-01 addressable range):
//
//	kind:       KindFileEntry
//	d tag:      the sync-root-relative path (the replaceable identifier)
//	x tag:      SHA-256 of the plaintext content (NIP-94 convention); absent on a tombstone
//	size tag:   plaintext byte length; absent on a tombstone
//	mtime tag:  file modification time as unix seconds (drives last-writer-wins)
//	deleted tag: present iff this is a tombstone
//	created_at: set to the file mtime, so the relay's own "keep the newest
//	            replaceable event per (pubkey, kind, d)" rule aligns with mtime LWW.
//	content:    empty — an entry is metadata; the bytes live on Blossom.
//
// Known limitation (resolved at the engine layer, not here): because the relay
// keeps only the newest event per path, a delete published with an older mtime
// than a concurrent edit will be dropped by the relay in favour of the edit,
// even though "a delete is absolute". A device that recorded the tombstone in
// its index keeps enforcing and re-asserting it; the engine is responsible for
// that re-assertion. This codec only serializes; it does not arbitrate.
package nostrevent

import (
	"fmt"
	"strconv"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"ca.punkscience.tendrils/internal/tree"
)

// KindFileEntry is Tendrils' parameterized replaceable event kind for one
// file's state. It sits in the addressable range (30000–39999) so relays retain
// only the latest event per (pubkey, kind, d=path).
const KindFileEntry = 31337

// Build constructs an unsigned event from an entry.
func Build(e *tree.Entry) (*nostr.Event, error) {
	if e == nil || e.Path == "" {
		return nil, fmt.Errorf("nostrevent: entry has no path")
	}
	tags := nostr.Tags{
		{"d", e.Path},
		{"mtime", strconv.FormatInt(e.ModTime.Unix(), 10)},
	}
	if e.Deleted {
		tags = append(tags, nostr.Tag{"deleted"})
	} else {
		tags = append(tags,
			nostr.Tag{"x", e.Sha256},
			nostr.Tag{"size", strconv.FormatInt(e.Size, 10)},
		)
	}
	return &nostr.Event{
		CreatedAt: nostr.Timestamp(e.ModTime.Unix()),
		Kind:      KindFileEntry,
		Tags:      tags,
		Content:   "",
	}, nil
}

// Sign builds and signs an event for entry e using the hex secret key.
func Sign(e *tree.Entry, secretHex string) (*nostr.Event, error) {
	evt, err := Build(e)
	if err != nil {
		return nil, err
	}
	if err := evt.Sign(secretHex); err != nil {
		return nil, fmt.Errorf("nostrevent: sign %s: %w", e.Path, err)
	}
	return evt, nil
}

// Parse converts a received event back into a tree.Entry. It verifies the
// signature and rejects events of the wrong kind or without a path.
func Parse(evt *nostr.Event) (*tree.Entry, error) {
	if evt.Kind != KindFileEntry {
		return nil, fmt.Errorf("nostrevent: wrong kind %d", evt.Kind)
	}
	ok, err := evt.CheckSignature()
	if err != nil {
		return nil, fmt.Errorf("nostrevent: check signature: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("nostrevent: invalid signature")
	}

	path := evt.Tags.GetD()
	if path == "" {
		return nil, fmt.Errorf("nostrevent: event has no d/path tag")
	}

	e := &tree.Entry{Path: path, ModTime: modTime(evt)}
	if evt.Tags.GetFirst([]string{"deleted"}) != nil {
		e.Deleted = true
		return e, nil
	}
	if x := evt.Tags.GetFirst([]string{"x"}); x != nil {
		e.Sha256 = x.Value()
	}
	if s := evt.Tags.GetFirst([]string{"size"}); s != nil {
		e.Size, _ = strconv.ParseInt(s.Value(), 10, 64)
	}
	return e, nil
}

// modTime prefers the explicit mtime tag, falling back to created_at.
func modTime(evt *nostr.Event) time.Time {
	if m := evt.Tags.GetFirst([]string{"mtime"}); m != nil {
		if secs, err := strconv.ParseInt(m.Value(), 10, 64); err == nil {
			return time.Unix(secs, 0)
		}
	}
	return evt.CreatedAt.Time()
}
