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

// Fetch returns every file-entry event the owner's key has published, unioned
// across reachable relays and deduped by event ID. It errors only when no relay
// could be reached; a partial answer from the relays that responded is returned
// as-is (the engine folds duplicates by mtime regardless).
func (c *Client) Fetch(ctx context.Context, pubkey string) ([]*nostr.Event, error) {
	if len(c.urls) == 0 {
		return nil, errors.New("relay: no relays configured")
	}
	filter := nostr.Filter{
		Kinds:   []int{nostrevent.KindFileEntry},
		Authors: []string{pubkey},
	}

	seen := make(map[string]struct{})
	var out []*nostr.Event
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
			if _, dup := seen[e.ID]; dup {
				continue
			}
			seen[e.ID] = struct{}{}
			out = append(out, e)
		}
	}
	if !reached {
		return nil, fmt.Errorf("relay: no relay reachable: %w", errors.Join(errs...))
	}
	return out, nil
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
