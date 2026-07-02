package blob

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"ca.punkscience.tendrils/internal/keys"
)

func testIdentity(t *testing.T) *keys.Identity {
	t.Helper()
	id, err := keys.Generate()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	return id
}

func hexHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// checkAuth validates the Authorization header the way a real Blossom server
// does: a signed kind-24242 event, the right verb, an unexpired expiration, and
// (for uploads) the blob hash. It returns the x-tag hash it saw.
func checkAuth(t *testing.T, r *http.Request, wantVerb string) string {
	t.Helper()
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Nostr ") {
		t.Fatalf("authorization header = %q, want Nostr prefix", h)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, "Nostr "))
	if err != nil {
		t.Fatalf("decode auth base64: %v", err)
	}
	var evt nostr.Event
	if err := json.Unmarshal(raw, &evt); err != nil {
		t.Fatalf("parse auth event: %v", err)
	}
	if evt.Kind != authKind {
		t.Errorf("auth kind = %d, want %d", evt.Kind, authKind)
	}
	ok, err := evt.CheckSignature()
	if err != nil || !ok {
		t.Errorf("auth signature invalid: ok=%v err=%v", ok, err)
	}
	if v := evt.Tags.GetFirst([]string{"t"}); v == nil || v.Value() != wantVerb {
		t.Errorf("auth verb = %v, want %q", v, wantVerb)
	}
	exp := evt.Tags.GetFirst([]string{"expiration"})
	if exp == nil {
		t.Fatalf("auth missing expiration tag")
	}
	secs, err := strconv.ParseInt(exp.Value(), 10, 64)
	if err != nil {
		t.Fatalf("parse expiration: %v", err)
	}
	if time.Unix(secs, 0).Before(time.Now()) {
		t.Errorf("auth already expired at %v", time.Unix(secs, 0))
	}
	x := evt.Tags.GetFirst([]string{"x"})
	if x == nil {
		t.Fatalf("auth missing x tag")
	}
	return x.Value()
}

func TestUploadRoundTrip(t *testing.T) {
	id := testIdentity(t)
	data := []byte("nonce||ciphertext of Sam's note")
	want := hexHash(data)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/upload" {
			t.Errorf("got %s %s, want PUT /upload", r.Method, r.URL.Path)
		}
		xHash := checkAuth(t, r, "upload")
		body := readAll(t, r)
		if hexHash(body) != want {
			t.Errorf("uploaded bytes hash = %s, want %s", hexHash(body), want)
		}
		if xHash != want {
			t.Errorf("auth x tag = %s, want %s", xHash, want)
		}
		writeJSON(w, Descriptor{
			URL:    "https://blossom.example/" + want,
			SHA256: want,
			Size:   int64(len(body)),
		})
	}))
	defer srv.Close()

	d, err := New(srv.URL, id).Upload(context.Background(), data)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if d.SHA256 != want {
		t.Errorf("descriptor sha = %s, want %s", d.SHA256, want)
	}
	if d.Size != int64(len(data)) {
		t.Errorf("descriptor size = %d, want %d", d.Size, len(data))
	}
}

// A server that stores different bytes than we sent (its reported hash differs)
// must be caught locally, not trusted.
func TestUploadHashMismatchRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, Descriptor{SHA256: strings.Repeat("0", 64), Size: 1})
	}))
	defer srv.Close()

	if _, err := New(srv.URL, testIdentity(t)).Upload(context.Background(), []byte("data")); err == nil {
		t.Fatal("expected error on hash mismatch, got nil")
	}
}

func TestUploadServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "quota exceeded", http.StatusForbidden)
	}))
	defer srv.Close()

	_, err := New(srv.URL, testIdentity(t)).Upload(context.Background(), []byte("data"))
	if err == nil {
		t.Fatal("expected error on 403, got nil")
	}
	if !strings.Contains(err.Error(), "quota exceeded") {
		t.Errorf("error should surface server message, got: %v", err)
	}
}

func TestDownloadRoundTrip(t *testing.T) {
	data := []byte("the sealed blob bytes")
	sum := hexHash(data)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/"+sum {
			t.Errorf("got %s %s, want GET /%s", r.Method, r.URL.Path, sum)
		}
		checkAuth(t, r, "get")
		w.Write(data)
	}))
	defer srv.Close()

	got, err := New(srv.URL, testIdentity(t)).Download(context.Background(), sum)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("downloaded %q, want %q", got, data)
	}
}

// A server returning bytes that do not match the requested hash is a corruption
// or substitution the client must reject.
func TestDownloadIntegrityCheck(t *testing.T) {
	sum := hexHash([]byte("what we asked for"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("something else entirely"))
	}))
	defer srv.Close()

	if _, err := New(srv.URL, testIdentity(t)).Download(context.Background(), sum); err == nil {
		t.Fatal("expected integrity error, got nil")
	}
}

func TestDownloadNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	_, err := New(srv.URL, testIdentity(t)).Download(context.Background(), strings.Repeat("a", 64))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestHas(t *testing.T) {
	present := strings.Repeat("b", 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("Has should use HEAD, got %s", r.Method)
		}
		checkAuth(t, r, "get")
		if r.URL.Path == "/"+present {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	c := New(srv.URL, testIdentity(t))
	if ok, err := c.Has(context.Background(), present); err != nil || !ok {
		t.Errorf("Has(present) = %v, %v; want true, nil", ok, err)
	}
	if ok, err := c.Has(context.Background(), strings.Repeat("c", 64)); err != nil || ok {
		t.Errorf("Has(absent) = %v, %v; want false, nil", ok, err)
	}
}

// Server URLs with a trailing slash must not produce a double slash in requests.
func TestTrailingSlashServer(t *testing.T) {
	sum := hexHash([]byte("x"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "//") {
			t.Errorf("double slash in path: %q", r.URL.Path)
		}
		w.Write([]byte("x"))
	}))
	defer srv.Close()

	if _, err := New(srv.URL+"/", testIdentity(t)).Download(context.Background(), sum); err != nil {
		t.Fatalf("download with trailing-slash server: %v", err)
	}
}

func readAll(t *testing.T, r *http.Request) []byte {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	return body
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
