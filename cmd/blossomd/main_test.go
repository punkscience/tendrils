package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

// mintAuth builds the "Nostr <base64>" header the blob client sends: a signed
// kind-24242 event for verb, expiring exp from now.
func mintAuth(t *testing.T, sk, verb string, exp time.Duration) string {
	t.Helper()
	evt := nostr.Event{
		CreatedAt: nostr.Timestamp(time.Now().Unix()),
		Kind:      authKind,
		Content:   "tendrils " + verb,
		Tags: nostr.Tags{
			{"t", verb},
			{"expiration", strconv.FormatInt(time.Now().Add(exp).Unix(), 10)},
		},
	}
	if err := evt.Sign(sk); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(&evt)
	if err != nil {
		t.Fatal(err)
	}
	return "Nostr " + base64.StdEncoding.EncodeToString(raw)
}

func TestAuthorizeOpenWhenNoAllowlist(t *testing.T) {
	r := httptest.NewRequest("PUT", "/upload", nil)
	if err := authorize(r, map[string]struct{}{}, "upload"); err != nil {
		t.Fatalf("open server should allow: %v", err)
	}
}

func TestAuthorizeAllowsListedKey(t *testing.T) {
	sk := nostr.GeneratePrivateKey()
	pub, _ := nostr.GetPublicKey(sk)
	allowed := map[string]struct{}{pub: {}}

	r := httptest.NewRequest("PUT", "/upload", nil)
	r.Header.Set("Authorization", mintAuth(t, sk, "upload", time.Minute))
	if err := authorize(r, allowed, "upload"); err != nil {
		t.Fatalf("listed key should be allowed: %v", err)
	}
}

func TestAuthorizeRejectsUnlistedKey(t *testing.T) {
	listedPub, _ := nostr.GetPublicKey(nostr.GeneratePrivateKey())
	allowed := map[string]struct{}{listedPub: {}}

	otherSK := nostr.GeneratePrivateKey()
	r := httptest.NewRequest("PUT", "/upload", nil)
	r.Header.Set("Authorization", mintAuth(t, otherSK, "upload", time.Minute))
	if err := authorize(r, allowed, "upload"); err == nil {
		t.Fatal("unlisted key must be rejected")
	}
}

func TestAuthorizeRejectsWrongVerb(t *testing.T) {
	sk := nostr.GeneratePrivateKey()
	pub, _ := nostr.GetPublicKey(sk)
	allowed := map[string]struct{}{pub: {}}

	r := httptest.NewRequest("GET", "/"+"a", nil)
	r.Header.Set("Authorization", mintAuth(t, sk, "upload", time.Minute)) // upload auth for a get
	if err := authorize(r, allowed, "get"); err == nil {
		t.Fatal("verb mismatch must be rejected")
	}
}

func TestAuthorizeRejectsExpired(t *testing.T) {
	sk := nostr.GeneratePrivateKey()
	pub, _ := nostr.GetPublicKey(sk)
	allowed := map[string]struct{}{pub: {}}

	r := httptest.NewRequest("PUT", "/upload", nil)
	r.Header.Set("Authorization", mintAuth(t, sk, "upload", -time.Second)) // already expired
	if err := authorize(r, allowed, "upload"); err == nil {
		t.Fatal("expired authorization must be rejected")
	}
}

func TestAuthorizeRejectsMissingHeader(t *testing.T) {
	pub, _ := nostr.GetPublicKey(nostr.GeneratePrivateKey())
	allowed := map[string]struct{}{pub: {}}
	r := httptest.NewRequest("PUT", "/upload", nil)
	if err := authorize(r, allowed, "upload"); err == nil {
		t.Fatal("missing header must be rejected when allowlist is set")
	}
}

func TestParseAllowedFormats(t *testing.T) {
	sk := nostr.GeneratePrivateKey()
	pub, _ := nostr.GetPublicKey(sk)

	set, err := parseAllowed(pub + " , ")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := set[pub]; !ok {
		t.Fatal("hex pubkey should parse")
	}

	if _, err := parseAllowed("not-a-key"); err == nil {
		t.Fatal("garbage pubkey should error")
	}
	if set, err := parseAllowed(""); err != nil || len(set) != 0 {
		t.Fatalf("empty spec should yield empty set: set=%v err=%v", set, err)
	}
}

// A failed write must leave nothing behind. The bug this replaces used
// os.WriteFile, whose O_CREATE|O_TRUNC succeeds before the write does — so a full
// disk left an empty file at the blob's real address, the server then answered
// HEAD for it with 200, and every client skipped re-uploading a blob that held no
// bytes. Simulated here with an unwritable directory.
func TestStoreStreamLeavesNothingOnFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		// os.Chmod on Windows only toggles the read-only attribute, which does not
		// stop file creation inside a directory — the write would simply succeed and
		// the test would assert nothing.
		t.Skip("cannot make a directory unwritable via Chmod on Windows")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil { // readable, not writable
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	data := []byte("sealed bytes")
	h := hexOf(data)
	if _, _, err := storeStream(dir, bytes.NewReader(data)); err == nil {
		t.Fatal("expected the write to fail on a read-only directory")
	}
	if _, err := os.Stat(filepath.Join(dir, h)); !os.IsNotExist(err) {
		t.Errorf("a file was left at the blob address after a failed write (stat err = %v)", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if ents, err := os.ReadDir(dir); err != nil || len(ents) != 0 {
		t.Errorf("directory not empty after a failed write: %v (err %v)", ents, err)
	}
}

func TestStoreStreamWritesCompleteBlob(t *testing.T) {
	dir := t.TempDir()
	data := []byte("sealed bytes")
	h := hexOf(data)
	gotHash, n, err := storeStream(dir, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	// The address is derived from the bytes that actually landed, not supplied by
	// the caller: a client cannot file content under an address it does not hash to.
	if gotHash != h {
		t.Errorf("stored under %s, want %s", gotHash, h)
	}
	if n != int64(len(data)) {
		t.Errorf("reported %d bytes, want %d", n, len(data))
	}
	got, err := os.ReadFile(filepath.Join(dir, h))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Errorf("stored %q, want %q", got, data)
	}
	// Storing again is a no-op, not a rewrite: the address determines the bytes.
	if _, _, err := storeStream(dir, bytes.NewReader(data)); err != nil {
		t.Errorf("re-store should succeed: %v", err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("expected exactly one file, got %d — a temp file leaked", len(ents))
	}
}

// A read that fails partway must not leave a blob behind. The streaming version
// writes as it reads, so unlike the buffered one it has already put bytes on disk
// by the time it learns the upload was truncated — and those bytes hash to an
// address the client never intended.
func TestStoreStreamLeavesNothingOnTruncatedBody(t *testing.T) {
	dir := t.TempDir()
	r := io.MultiReader(bytes.NewReader([]byte("half a blob")), errReader{})
	if _, _, err := storeStream(dir, r); err == nil {
		t.Fatal("expected a read error to propagate")
	}
	if ents, err := os.ReadDir(dir); err != nil || len(ents) != 0 {
		t.Errorf("directory not empty after a truncated upload: %v (err %v)", ents, err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// listBlobs must report only real content addresses. A temp file from an upload
// in flight is not a blob, and a sweeper that treated one as an orphan would
// delete a blob mid-write.
func TestListBlobsIgnoresTempAndBadNames(t *testing.T) {
	dir := t.TempDir()
	real := hexOf([]byte("a"))
	// Uppercase a *different* hash: on a case-insensitive filesystem (NTFS, APFS)
	// strings.ToUpper(real) is the same file as real, and whichever the map wrote
	// last would decide the blob's contents.
	upper := strings.ToUpper(hexOf([]byte("b")))
	for name, content := range map[string]string{
		real:                      "a",
		".tmp-" + real + "-12345": "partial",
		"not-a-hash":              "x",
		upper:                     "uppercase is not our naming",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	blobs, err := listBlobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(blobs) != 1 {
		t.Fatalf("listed %d blobs, want 1: %+v", len(blobs), blobs)
	}
	if blobs[0].SHA256 != real {
		t.Errorf("listed %s, want %s", blobs[0].SHA256, real)
	}
	if blobs[0].Size != 1 {
		t.Errorf("size = %d, want 1", blobs[0].Size)
	}
	if blobs[0].Uploaded == 0 {
		t.Error("Uploaded should carry the store's mtime, for the sweeper's grace period")
	}
}

func TestIsBlobName(t *testing.T) {
	valid := hexOf([]byte("x"))
	for _, tc := range []struct {
		name string
		want bool
	}{
		{valid, true},
		{strings.ToUpper(valid), false},
		{valid[:63], false},
		{valid + "a", false},
		{".tmp-" + valid + "-9", false},
		{strings.Repeat("g", 64), false},
	} {
		if got := isBlobName(tc.name); got != tc.want {
			t.Errorf("isBlobName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// hexOf is the content address of data, for tests.
func hexOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
