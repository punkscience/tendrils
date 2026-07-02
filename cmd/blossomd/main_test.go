package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strconv"
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
