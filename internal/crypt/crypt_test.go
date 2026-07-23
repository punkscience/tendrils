package crypt

import (
	"bytes"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	key := [32]byte{1, 2, 3}
	plaintext := []byte("Sam's daily note, written locally.")

	sealed, err := Seal(key, plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	got, err := Open(key, sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("round-trip mismatch: got %q want %q", got, plaintext)
	}
}

// Gherkin: "anyone who fetches the blob by its hash receives only ciphertext"
// — the sealed bytes must not contain the plaintext.
func TestSealedIsCiphertextOnly(t *testing.T) {
	key := [32]byte{9}
	plaintext := []byte("SECRET-MARKER-STRING")

	sealed, err := Seal(key, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, plaintext) {
		t.Errorf("plaintext leaked into sealed blob")
	}
}

// Gherkin: "only a device holding Sam's key can decrypt it".
func TestWrongKeyFails(t *testing.T) {
	sealed, err := Seal([32]byte{1}, []byte("private"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open([32]byte{2}, sealed); err == nil {
		t.Errorf("decryption succeeded with the wrong key")
	}
}

// Deterministic sealing: identical plaintext under the same key must produce
// byte-identical output, so two devices that copy in the same file converge on
// one Blossom address instead of uploading a duplicate blob each.
func TestSealIsDeterministic(t *testing.T) {
	key := [32]byte{7}
	pt := []byte("same content")
	a, _ := Seal(key, pt)
	b, _ := Seal(key, pt)
	if !bytes.Equal(a, b) {
		t.Errorf("two seals of identical plaintext produced different ciphertext")
	}
}

// The nonce must still vary with the plaintext: reusing a GCM nonce across
// *different* plaintexts under one key is catastrophic, so this is the property
// that keeps determinism safe.
func TestDistinctPlaintextsGetDistinctNonces(t *testing.T) {
	key := [32]byte{7}
	a, _ := Seal(key, []byte("one"))
	b, _ := Seal(key, []byte("two"))
	if bytes.Equal(a[:nonceSize], b[:nonceSize]) {
		t.Errorf("different plaintexts reused a nonce")
	}
}

// The nonce is keyed, so the same file under two identities shares nothing —
// the equality leak is scoped to a single owner's own blobs.
func TestSealsDifferUnderDifferentKeys(t *testing.T) {
	pt := []byte("same content")
	a, _ := Seal([32]byte{1}, pt)
	b, _ := Seal([32]byte{2}, pt)
	if bytes.Equal(a[:nonceSize], b[:nonceSize]) {
		t.Errorf("nonce did not depend on the key")
	}
}

// Blobs sealed before this change carry a random nonce. Open reads the nonce
// from the blob itself, so it must still decrypt them.
func TestOpenAcceptsArbitraryNonce(t *testing.T) {
	key := [32]byte{3}
	pt := []byte("sealed by an older build")

	gcm, err := newGCM(key)
	if err != nil {
		t.Fatal(err)
	}
	nonce := bytes.Repeat([]byte{0xAB}, nonceSize) // not what Seal would derive
	legacy := gcm.Seal(nonce, nonce, pt, nil)

	got, err := Open(key, legacy)
	if err != nil {
		t.Fatalf("open legacy blob: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Errorf("legacy round-trip mismatch: got %q want %q", got, pt)
	}
}

func TestTamperDetected(t *testing.T) {
	key := [32]byte{4}
	sealed, _ := Seal(key, []byte("trustworthy"))
	sealed[len(sealed)-1] ^= 0xff // flip a ciphertext bit
	if _, err := Open(key, sealed); err == nil {
		t.Errorf("tampered blob opened without error")
	}
}

func TestShortBlobRejected(t *testing.T) {
	if _, err := Open([32]byte{1}, []byte{0, 1, 2}); err == nil {
		t.Errorf("expected error for too-short blob")
	}
}
