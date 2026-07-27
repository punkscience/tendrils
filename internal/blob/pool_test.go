package blob

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// blobServer is a minimal Blossom server for pool tests: it stores what it is
// given and can be told to fail in specific ways.
type blobServer struct {
	*httptest.Server
	blobs    map[string][]byte
	uploads  int
	gets     int
	status   int  // when non-zero, answer every request with this status
	tooLarge bool // answer uploads with 413
}

func newBlobServer(t *testing.T) *blobServer {
	t.Helper()
	bs := &blobServer{blobs: map[string][]byte{}}
	bs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bs.status != 0 {
			http.Error(w, "configured failure", bs.status)
			return
		}
		switch r.Method {
		case http.MethodPut:
			if bs.tooLarge {
				http.Error(w, "too big", http.StatusRequestEntityTooLarge)
				return
			}
			bs.uploads++
			body := readAll(t, r)
			sum := hexHash(body)
			bs.blobs[sum] = body
			writeJSON(w, Descriptor{SHA256: sum, Size: int64(len(body))})
		case http.MethodGet, http.MethodHead:
			h := strings.TrimPrefix(r.URL.Path, "/")
			data, ok := bs.blobs[h]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Length", itoa(len(data)))
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusOK)
				return
			}
			bs.gets++
			_, _ = w.Write(data)
		default:
			http.Error(w, "nope", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(bs.Close)
	return bs
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// deadClient points at an address nothing is listening on, standing in for a LAN
// server a laptop cannot currently reach.
func deadClient(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listens here now
	c := New(url, testIdentity(t))
	// Fail fast rather than waiting out a real dial timeout.
	c.http = &http.Client{Timeout: 500 * time.Millisecond}
	return c
}

// The headline case: the preferred server is unreachable (laptop away from its
// LAN), so the upload falls through to the next one instead of failing.
func TestPoolUploadFallsThroughUnreachableServer(t *testing.T) {
	id := testIdentity(t)
	up := newBlobServer(t)
	pool := NewPoolWithClients(deadClient(t), New(up.URL, id))

	data := []byte("sealed bytes")
	desc, err := pool.Upload(context.Background(), data)
	if err != nil {
		t.Fatalf("upload should have fallen through: %v", err)
	}
	if desc.SHA256 != hexHash(data) {
		t.Errorf("sha = %s, want %s", desc.SHA256, hexHash(data))
	}
	if up.uploads != 1 {
		t.Errorf("second server got %d uploads, want 1", up.uploads)
	}
}

// Preference order is honoured: when the first server works, the second is never
// touched. That is what puts large files on the uncapped LAN server.
func TestPoolPrefersFirstServer(t *testing.T) {
	id := testIdentity(t)
	first, second := newBlobServer(t), newBlobServer(t)
	pool := NewPoolWithClients(New(first.URL, id), New(second.URL, id))

	if _, err := pool.Upload(context.Background(), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if first.uploads != 1 || second.uploads != 0 {
		t.Errorf("uploads: first=%d second=%d, want 1 and 0", first.uploads, second.uploads)
	}
}

// A size rejection from the preferred server must not end the attempt: the whole
// point of putting a LAN server first is that a proxied one refuses big blobs.
func TestPoolFallsThroughSizeRejection(t *testing.T) {
	id := testIdentity(t)
	capped := newBlobServer(t)
	capped.tooLarge = true
	uncapped := newBlobServer(t)

	pool := NewPoolWithClients(New(capped.URL, id), New(uncapped.URL, id))
	if _, err := pool.Upload(context.Background(), []byte("a large blob")); err != nil {
		t.Fatalf("should have fallen through to the uncapped server: %v", err)
	}
	if uncapped.uploads != 1 {
		t.Errorf("uncapped server got %d uploads, want 1", uncapped.uploads)
	}
}

// When every server refuses for size, the joined error must still answer
// errors.Is(ErrTooLarge) — that is what lets the engine mark the path permanently
// stuck rather than retrying it every minute forever.
func TestPoolAllTooLargeStaysTyped(t *testing.T) {
	id := testIdentity(t)
	a, b := newBlobServer(t), newBlobServer(t)
	a.tooLarge, b.tooLarge = true, true

	pool := NewPoolWithClients(New(a.URL, id), New(b.URL, id))
	_, err := pool.Upload(context.Background(), []byte("huge"))
	if err == nil {
		t.Fatal("expected an error when every server refuses")
	}
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("joined error lost ErrTooLarge: %v", err)
	}
}

// A reader must consult every server, because under failover any one of them may
// be the only holder of a given blob.
func TestPoolDownloadSearchesAllServers(t *testing.T) {
	id := testIdentity(t)
	empty, holder := newBlobServer(t), newBlobServer(t)
	data := []byte("only on the second server")
	holder.blobs[hexHash(data)] = data

	pool := NewPoolWithClients(New(empty.URL, id), New(holder.URL, id))
	got, err := pool.Download(context.Background(), hexHash(data))
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("got %q, want %q", got, data)
	}
}

// ErrNotFound survives only when every server was asked and none had it, so the
// engine's "the server does not have this yet" handling still works.
func TestPoolDownloadNotFoundWhenNoServerHasIt(t *testing.T) {
	id := testIdentity(t)
	a, b := newBlobServer(t), newBlobServer(t)
	pool := NewPoolWithClients(New(a.URL, id), New(b.URL, id))

	_, err := pool.Download(context.Background(), strings.Repeat("a", 64))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// A blob on any server is enough to skip the upload.
func TestPoolHasIsTrueIfAnyServerHoldsIt(t *testing.T) {
	id := testIdentity(t)
	empty, holder := newBlobServer(t), newBlobServer(t)
	data := []byte("stored over here")
	holder.blobs[hexHash(data)] = data

	pool := NewPoolWithClients(New(empty.URL, id), New(holder.URL, id))
	ok, err := pool.Has(context.Background(), hexHash(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("Has should be true when any server holds the blob")
	}
}

// If a server could not be asked, Has reports the uncertainty rather than a
// confident "no": the caller re-uploads, which is the cheap mistake.
func TestPoolHasSurfacesUncertainty(t *testing.T) {
	id := testIdentity(t)
	empty := newBlobServer(t)
	pool := NewPoolWithClients(deadClient(t), New(empty.URL, id))

	ok, err := pool.Has(context.Background(), strings.Repeat("b", 64), 10)
	if ok {
		t.Error("Has should not claim the blob is present")
	}
	if err == nil {
		t.Error("Has should report that a server could not be reached")
	}
}

// An unreachable server is demoted so the next operation does not pay its dial
// timeout again — but it is still tried last, never dropped, because it may be
// the only server holding some blob.
func TestPoolDemotesUnreachableServerButStillTriesIt(t *testing.T) {
	id := testIdentity(t)
	dead := deadClient(t)
	live := newBlobServer(t)
	pool := NewPoolWithClients(dead, New(live.URL, id))

	if _, err := pool.Upload(context.Background(), []byte("first")); err != nil {
		t.Fatal(err)
	}
	order := pool.order()
	if len(order) != 2 {
		t.Fatalf("order has %d clients, want 2 — a server was dropped entirely", len(order))
	}
	if order[0].server == dead.server {
		t.Error("the unreachable server should have been demoted, not left first")
	}
	if order[1].server != dead.server {
		t.Error("the unreachable server should still be tried last, not removed")
	}
}

// A server that answers — even with a failure — is not treated as unreachable.
// Demoting on a 500 would push traffic away from a healthy store over a blip.
func TestPoolDoesNotDemoteOnServerAnswer(t *testing.T) {
	id := testIdentity(t)
	failing := newBlobServer(t)
	failing.status = http.StatusInternalServerError
	ok := newBlobServer(t)

	pool := NewPoolWithClients(New(failing.URL, id), New(ok.URL, id))
	if _, err := pool.Upload(context.Background(), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if got := pool.order()[0].server; got != failing.URL {
		t.Errorf("first server = %s, want %s (a 500 is an answer, not an outage)", got, failing.URL)
	}
}

// A demoted server is restored once its cooldown passes.
func TestPoolRestoresServerAfterCooldown(t *testing.T) {
	id := testIdentity(t)
	dead := deadClient(t)
	live := newBlobServer(t)
	pool := NewPoolWithClients(dead, New(live.URL, id))

	base := time.Unix(1_700_000_000, 0)
	pool.nowFunc = func() time.Time { return base }
	if _, err := pool.Upload(context.Background(), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if pool.order()[0].server == dead.server {
		t.Fatal("expected the dead server to be demoted")
	}
	pool.nowFunc = func() time.Time { return base.Add(defaultDownFor + time.Second) }
	if pool.order()[0].server != dead.server {
		t.Error("after the cooldown the preferred server should be tried first again")
	}
}

// One server behaves exactly like a bare client, so the single-server setup that
// everything ran on before is unchanged.
func TestPoolWithOneServerMatchesClient(t *testing.T) {
	id := testIdentity(t)
	srv := newBlobServer(t)
	pool := NewPoolWithClients(New(srv.URL, id))

	data := []byte("single server")
	if _, err := pool.Upload(context.Background(), data); err != nil {
		t.Fatal(err)
	}
	got, err := pool.Download(context.Background(), hexHash(data))
	if err != nil || string(got) != string(data) {
		t.Errorf("round trip failed: %q, %v", got, err)
	}
	if _, err := pool.Download(context.Background(), strings.Repeat("c", 64)); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing blob should be ErrNotFound, got %v", err)
	}
}
