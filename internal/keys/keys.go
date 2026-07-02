// Package keys handles the single Nostr keypair that is a Tendrils device's
// whole identity. The same key enrolls a device, authorizes uploads, and
// derives the symmetric key that encrypts every blob at rest.
//
// The Nostr secret key is the master key: if it leaks, all synced files are
// readable; if it is lost with no backup, the encrypted blobs are
// unrecoverable. This is an accepted, eyes-open consequence of the MVP (see
// docs/OPEN-QUESTIONS.md Q8) — the owner is responsible for backing up the key.
package keys

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
)

// blobKeyInfo is the HKDF domain-separation label for the blob-encryption key.
// It is versioned so a future key-derivation change can coexist with v1 data.
const blobKeyInfo = "ca.punkscience.tendrils/blob-encryption/v1"

// Identity is a parsed, validated device key. The secret is held in hex form;
// derived values (public key, symmetric key) are computed on demand.
type Identity struct {
	sec string // 64-char hex secret key
	pub string // 64-char hex x-only public key
}

// Parse accepts either a bech32 nsec ("nsec1…") or a raw 64-char hex secret key
// and returns the corresponding Identity. Public-key-only inputs (npub/hex
// pubkey) are rejected: a device needs the secret to decrypt and to sign.
func Parse(input string) (*Identity, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return nil, errors.New("keys: empty key")
	}

	var sec string
	switch {
	case strings.HasPrefix(s, "nsec1"):
		prefix, value, err := nip19.Decode(s)
		if err != nil {
			return nil, fmt.Errorf("keys: decode nsec: %w", err)
		}
		if prefix != "nsec" {
			return nil, fmt.Errorf("keys: expected an nsec, got %q", prefix)
		}
		sec = value.(string)
	case strings.HasPrefix(s, "npub1"):
		return nil, errors.New("keys: an npub is a public key; enrollment needs the secret key (nsec or hex)")
	default:
		sec = strings.ToLower(s)
	}

	if err := validateHex32(sec); err != nil {
		return nil, fmt.Errorf("keys: invalid secret key: %w", err)
	}

	pub, err := nostr.GetPublicKey(sec)
	if err != nil {
		return nil, fmt.Errorf("keys: derive public key: %w", err)
	}

	return &Identity{sec: sec, pub: pub}, nil
}

// Generate creates a brand-new random device identity.
func Generate() (*Identity, error) {
	return Parse(nostr.GeneratePrivateKey())
}

// SecretHex returns the 64-char hex secret key. Handle with care: this is the
// master key.
func (id *Identity) SecretHex() string { return id.sec }

// PublicHex returns the 64-char hex x-only public key. This is the owner's
// stable identity that configuration is published under.
func (id *Identity) PublicHex() string { return id.pub }

// Nsec returns the bech32-encoded secret key for display/backup.
func (id *Identity) Nsec() (string, error) { return nip19.EncodePrivateKey(id.sec) }

// Npub returns the bech32-encoded public key.
func (id *Identity) Npub() (string, error) { return nip19.EncodePublicKey(id.pub) }

// SymmetricKey derives the 32-byte AES-256 key used to encrypt every blob.
// Derivation is deterministic: the same Nostr key always yields the same
// symmetric key, so any enrolled device can decrypt what any other uploaded.
func (id *Identity) SymmetricKey() ([32]byte, error) {
	var out [32]byte
	raw, err := hex.DecodeString(id.sec)
	if err != nil {
		return out, fmt.Errorf("keys: decode secret: %w", err)
	}
	// No salt: derivation must be reproducible from the key alone on any device,
	// with nothing else to carry. Domain separation comes from blobKeyInfo.
	dk, err := hkdf.Key(sha256.New, raw, nil, blobKeyInfo, 32)
	if err != nil {
		return out, fmt.Errorf("keys: hkdf: %w", err)
	}
	copy(out[:], dk)
	return out, nil
}

func validateHex32(s string) error {
	if len(s) != 64 {
		return fmt.Errorf("expected 64 hex chars, got %d", len(s))
	}
	if _, err := hex.DecodeString(s); err != nil {
		return err
	}
	return nil
}
