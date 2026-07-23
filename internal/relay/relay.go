// Package relay is the concrete Nostr relay adapter behind engine.EventStore: it
// publishes signed file-entry events and fetches the owner's current set from one
// or more relays over websockets (go-nostr).
//
// Connections are held open and reused across a device's lifetime — a Sync pass
// publishes one event per changed file, so reconnecting each time would be
// wasteful — and lazily re-established if a relay drops. Writes go to every
// configured relay and succeed if any accepts; reads union the results and dedupe
// by event ID, so one unreachable relay neither loses a publish nor hides an
// event another relay still has.
package relay

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/nbd-wtf/go-nostr"

	"ca.punkscience.tendrils/internal/nostrevent"
	"ca.punkscience.tendrils/internal/serverlist"
)

// Client talks to a fixed set of relay URLs. Safe for concurrent use.
type Client struct {
	urls []string

	mu    sync.Mutex
	conns map[string]*nostr.Relay
}

// New returns a Client for the given relay URLs (ws:// or wss://).
func New(urls []string) *Client {
	return &Client{urls: urls, conns: make(map[string]*nostr.Relay)}
}

// Publish sends evt to every configured relay, succeeding if at least one
// accepts it. It returns an error only when no relay took the event, so a single
// down relay never drops a change.
func (c *Client) Publish(ctx context.Context, evt *nostr.Event) error {
	if len(c.urls) == 0 {
		return errors.New("relay: no relays configured")
	}
	var errs []error
	accepted := false
	for _, url := range c.urls {
		r, err := c.relay(ctx, url)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", url, err))
			continue
		}
		if err := r.Publish(ctx, *evt); err != nil {
			c.drop(url)
			errs = append(errs, fmt.Errorf("%s: %w", url, err))
			continue
		}
		accepted = true
	}
	if accepted {
		return nil
	}
	return fmt.Errorf("relay: publish rejected by all relays: %w", errors.Join(errs...))
}

// pageSize is how many events one paginated REQ asks for. Relays impose their
// own maximum (the reference relay behind this deployment caps at 400) and will
// return fewer, which pagination handles — asking for more than a relay allows
// costs nothing, asking for fewer only costs round trips.
const pageSize = 500

// maxPages bounds pagination so a misbehaving relay — one ignoring `until`, say —
// cannot spin forever. Hitting it means the result is incomplete, which Fetch
// reports rather than passing off a partial set as the whole truth.
const maxPages = 1000

// Fetch returns every file-entry event the owner's key has published, unioned
// across reachable relays and deduped by event ID. It errors only when no relay
// could be reached; a partial answer from the relays that responded is returned
// as-is (the engine folds duplicates by mtime regardless).
//
// Each relay is walked with NIP-01 pagination rather than a single query, because
// a relay caps how many events one REQ may return. Without paging, a tree larger
// than that cap yields a *silently truncated* view in which most paths look like
// they were never published — and the engine, seeing no remote entry, republishes
// the entire tree on every pass and can resurrect deleted files whose tombstones
// fell outside the window. The remote set must be complete or the reconciler is
// deciding against a fiction.
func (c *Client) Fetch(ctx context.Context, pubkey string) ([]*nostr.Event, error) {
	if len(c.urls) == 0 {
		return nil, errors.New("relay: no relays configured")
	}

	seen := make(map[string]struct{})
	var out []*nostr.Event
	var errs []error
	reached := false
	for _, url := range c.urls {
		if err := c.fetchAll(ctx, url, pubkey, seen, &out); err != nil {
			c.drop(url)
			errs = append(errs, fmt.Errorf("%s: %w", url, err))
			continue
		}
		reached = true
	}
	if !reached {
		return nil, fmt.Errorf("relay: no relay reachable: %w", errors.Join(errs...))
	}
	return out, nil
}

// fetchAll pages through one relay's copy of the owner's file-entry events,
// appending newly-seen ones to out.
//
// The cursor is `until`, which NIP-01 defines inclusively (created_at <= until),
// so each page re-reads the boundary timestamp; duplicates are absorbed by the
// seen set. Termination is guaranteed by forcing the cursor strictly downward
// whenever a page fails to advance it — the case where a full page shares one
// created_at, which would otherwise re-request the same events forever.
func (c *Client) fetchAll(ctx context.Context, url, pubkey string, seen map[string]struct{}, out *[]*nostr.Event) error {
	r, err := c.relay(ctx, url)
	if err != nil {
		return err
	}

	var until *nostr.Timestamp
	for page := 1; ; page++ {
		if page > maxPages {
			return fmt.Errorf("pagination did not terminate after %d pages; result would be incomplete", maxPages)
		}
		filter := nostr.Filter{
			Kinds:   []int{nostrevent.KindFileEntry},
			Authors: []string{pubkey},
			Limit:   pageSize,
			Until:   until,
		}
		evts, err := r.QuerySync(ctx, filter)
		if err != nil {
			return err
		}
		if len(evts) == 0 {
			return nil // walked past the oldest event
		}

		oldest := evts[0].CreatedAt
		for _, e := range evts {
			if e.CreatedAt < oldest {
				oldest = e.CreatedAt
			}
			if _, dup := seen[e.ID]; dup {
				continue
			}
			seen[e.ID] = struct{}{}
			*out = append(*out, e)
		}

		next := oldest
		if until != nil && next >= *until {
			next = *until - 1 // force progress; the page did not move the cursor
		}
		if next < 0 {
			return nil // exhausted the timestamp range
		}
		until = &next
	}
}

// FetchServerList returns the Blossom servers the owner's key advertises via its
// kind-10063 event (BUD-03), so a device enrolled with only the key + a relay can
// discover where blobs live. The newest event across reachable relays wins. It
// returns (nil, nil) when the key has published no list — an empty result is not
// an error, only an unreachable relay is.
func (c *Client) FetchServerList(ctx context.Context, pubkey string) ([]string, error) {
	if len(c.urls) == 0 {
		return nil, errors.New("relay: no relays configured")
	}
	filter := nostr.Filter{
		Kinds:   []int{serverlist.Kind},
		Authors: []string{pubkey},
		Limit:   1,
	}

	var newest *nostr.Event
	var errs []error
	reached := false
	for _, url := range c.urls {
		r, err := c.relay(ctx, url)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", url, err))
			continue
		}
		evts, err := r.QuerySync(ctx, filter)
		if err != nil {
			c.drop(url)
			errs = append(errs, fmt.Errorf("%s: %w", url, err))
			continue
		}
		reached = true
		for _, e := range evts {
			if newest == nil || e.CreatedAt > newest.CreatedAt {
				newest = e
			}
		}
	}
	if !reached {
		return nil, fmt.Errorf("relay: no relay reachable: %w", errors.Join(errs...))
	}
	if newest == nil {
		return nil, nil
	}
	return serverlist.Parse(newest)
}

// Close disconnects every open relay connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var errs []error
	for url, r := range c.conns {
		if err := r.Close(); err != nil {
			errs = append(errs, err)
		}
		delete(c.conns, url)
	}
	return errors.Join(errs...)
}

// relay returns a live connection to url, reusing an open one or dialing a fresh
// connection (also replacing one that has since dropped).
func (c *Client) relay(ctx context.Context, url string) (*nostr.Relay, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.conns[url]; r != nil && r.IsConnected() {
		return r, nil
	}
	r, err := nostr.RelayConnect(ctx, url)
	if err != nil {
		return nil, err
	}
	c.conns[url] = r
	return r, nil
}

// drop forgets a connection so the next call re-dials it.
func (c *Client) drop(url string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.conns[url]; r != nil {
		r.Close()
		delete(c.conns, url)
	}
}
