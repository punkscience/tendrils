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

// A random nonce means identical plaintext seals to different ciphertext.
func TestNonceRandomized(t *testing.T) {
	key := [32]byte{7}
	pt := []byte("same content")
	a, _ := Seal(key, pt)
	b, _ := Seal(key, pt)
	if bytes.Equal(a, b) {
		t.Errorf("two seals of identical plaintext produced identical ciphertext")
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
