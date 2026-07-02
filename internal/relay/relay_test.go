package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/nbd-wtf/go-nostr"

	"ca.punkscience.tendrils/internal/keys"
	"ca.punkscience.tendrils/internal/nostrevent"
	"ca.punkscience.tendrils/internal/serverlist"
	"ca.punkscience.tendrils/internal/tree"
)

// testRelay is a minimal in-process NIP-01 relay: enough of REQ/EVENT/EOSE/OK to
// exercise the client, with parameterized-replaceable semantics (newest event
// per kind+pubkey+d tag) so it behaves like the real thing for our events.
type testRelay struct {
	mu     sync.Mutex
	events map[string]*nostr.Event // key: kind:pubkey:dtag
}

func newTestRelay(t *testing.T) (url string, r *testRelay) {
	r = &testRelay{events: map[string]*nostr.Event{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conn, err := websocket.Accept(w, req, nil)
		if err != nil {
			return
		}
		r.serve(conn)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http"), r
}

func (tr *testRelay) serve(conn *websocket.Conn) {
	ctx := context.Background()
	defer conn.Close(websocket.StatusNormalClosure, "")
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg []json.RawMessage
		if err := json.Unmarshal(data, &msg); err != nil || len(msg) == 0 {
			continue
		}
		var cmd string
		json.Unmarshal(msg[0], &cmd)
		switch cmd {
		case "EVENT":
			var evt nostr.Event
			if err := json.Unmarshal(msg[1], &evt); err != nil {
				continue
			}
			tr.save(&evt)
			writeJSON(ctx, conn, []any{"OK", evt.ID, true, ""})
		case "REQ":
			var subID string
			json.Unmarshal(msg[1], &subID)
			for i := 2; i < len(msg); i++ {
				var f nostr.Filter
				if err := json.Unmarshal(msg[i], &f); err != nil {
					continue
				}
				for _, e := range tr.match(f) {
					writeJSON(ctx, conn, []any{"EVENT", subID, e})
				}
			}
			writeJSON(ctx, conn, []any{"EOSE", subID})
		case "CLOSE":
			// no-op: single-shot queries
		}
	}
}

func (tr *testRelay) save(evt *nostr.Event) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	key := replaceableKey(evt)
	if prev, ok := tr.events[key]; !ok || evt.CreatedAt >= prev.CreatedAt {
		tr.events[key] = evt
	}
}

func (tr *testRelay) match(f nostr.Filter) []*nostr.Event {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	var out []*nostr.Event
	for _, e := range tr.events {
		if len(f.Kinds) > 0 && !contains(f.Kinds, e.Kind) {
			continue
		}
		if len(f.Authors) > 0 && !contains(f.Authors, e.PubKey) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func replaceableKey(evt *nostr.Event) string {
	return string(rune(evt.Kind)) + ":" + evt.PubKey + ":" + evt.Tags.GetD()
}

func contains[T comparable](s []T, v T) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	conn.Write(ctx, websocket.MessageText, data)
}

func mustID(t *testing.T) *keys.Identity {
	t.Helper()
	id, err := keys.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func signEntry(t *testing.T, id *keys.Identity, e *tree.Entry) *nostr.Event {
	t.Helper()
	evt, err := nostrevent.Sign(e, id.SecretHex())
	if err != nil {
		t.Fatal(err)
	}
	return evt
}

func TestPublishThenFetch(t *testing.T) {
	url, _ := newTestRelay(t)
	id := mustID(t)
	c := New([]string{url})
	defer c.Close()

	ctx := context.Background()
	evt := signEntry(t, id, &tree.Entry{Path: "a.md", Sha256: "h", BlobHash: "b", Size: 1, ModTime: time.Unix(1_700_000_000, 0)})
	if err := c.Publish(ctx, evt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	got, err := c.Fetch(ctx, id.PublicHex())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("fetched %d events, want 1", len(got))
	}
	entry, err := nostrevent.Parse(got[0])
	if err != nil {
		t.Fatal(err)
	}
	if entry.Path != "a.md" || entry.BlobHash != "b" {
		t.Errorf("round-trip entry wrong: %+v", entry)
	}
}

// The connection is reused across calls, and a later publish for the same path
// replaces the earlier one (parameterized-replaceable), so Fetch sees one entry.
func TestReplacePreservesLatest(t *testing.T) {
	url, _ := newTestRelay(t)
	id := mustID(t)
	c := New([]string{url})
	defer c.Close()
	ctx := context.Background()

	c.Publish(ctx, signEntry(t, id, &tree.Entry{Path: "a.md", Sha256: "old", ModTime: time.Unix(1_700_000_000, 0)}))
	c.Publish(ctx, signEntry(t, id, &tree.Entry{Path: "a.md", Sha256: "new", ModTime: time.Unix(1_700_000_500, 0)}))

	got, err := c.Fetch(ctx, id.PublicHex())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("fetched %d events, want 1 (replaceable)", len(got))
	}
	entry, _ := nostrevent.Parse(got[0])
	if entry.Sha256 != "new" {
		t.Errorf("kept %q, want the newest %q", entry.Sha256, "new")
	}
}

// Two relays holding the same event: Fetch unions and dedupes by event ID.
func TestFetchDedupesAcrossRelays(t *testing.T) {
	url1, r1 := newTestRelay(t)
	url2, r2 := newTestRelay(t)
	id := mustID(t)
	ctx := context.Background()

	evt := signEntry(t, id, &tree.Entry{Path: "a.md", Sha256: "h", ModTime: time.Unix(1_700_000_000, 0)})
	r1.save(evt)
	r2.save(evt) // same event on both relays

	c := New([]string{url1, url2})
	defer c.Close()
	got, err := c.Fetch(ctx, id.PublicHex())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("fetched %d events, want 1 after dedup", len(got))
	}
}

// A publish still succeeds when one of two relays is unreachable.
func TestPublishToleratesDownRelay(t *testing.T) {
	url, _ := newTestRelay(t)
	id := mustID(t)
	// Second URL points nowhere reachable.
	c := New([]string{url, "ws://127.0.0.1:1"})
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	evt := signEntry(t, id, &tree.Entry{Path: "a.md", Sha256: "h", ModTime: time.Unix(1, 0)})
	if err := c.Publish(ctx, evt); err != nil {
		t.Errorf("publish should succeed via the reachable relay, got: %v", err)
	}
}

// A device publishes its Blossom server list; another device with only the key
// and the relay discovers where blobs live via FetchServerList.
func TestFetchServerListRoundTrip(t *testing.T) {
	url, _ := newTestRelay(t)
	id := mustID(t)
	c := New([]string{url})
	defer c.Close()
	ctx := context.Background()

	evt, err := serverlist.Sign([]string{"https://blossom.towerofsong.ca"}, id.SecretHex())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Publish(ctx, evt); err != nil {
		t.Fatalf("publish server list: %v", err)
	}

	got, err := c.FetchServerList(ctx, id.PublicHex())
	if err != nil {
		t.Fatalf("fetch server list: %v", err)
	}
	if len(got) != 1 || got[0] != "https://blossom.towerofsong.ca" {
		t.Fatalf("discovered %v, want [https://blossom.towerofsong.ca]", got)
	}
}

// No published list is an empty result, not an error.
func TestFetchServerListEmpty(t *testing.T) {
	url, _ := newTestRelay(t)
	id := mustID(t)
	c := New([]string{url})
	defer c.Close()

	got, err := c.FetchServerList(context.Background(), id.PublicHex())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no servers, got %v", got)
	}
}

func TestPublishNoRelaysConfigured(t *testing.T) {
	if err := New(nil).Publish(context.Background(), &nostr.Event{}); err == nil {
		t.Error("expected error with no relays configured")
	}
}
