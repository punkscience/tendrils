// Package crypt encrypts file contents before they leave the device and
// decrypts them on the way back in. Every blob is encrypted at rest, always,
// with the symmetric key derived from the owner's Nostr key (see internal/keys).
//
// A sealed blob is self-describing: [12-byte nonce][AES-256-GCM ciphertext].
// The nonce is random per blob, so encrypting the same bytes twice yields
// different ciphertext — a passive observer on the Blossom server cannot tell
// two identical files apart.
package crypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// nonceSize is the standard GCM nonce length in bytes.
const nonceSize = 12

// Seal encrypts plaintext under key, returning nonce||ciphertext.
func Seal(key [32]byte, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("crypt: read nonce: %w", err)
	}
	// Seal appends the ciphertext to nonce, so the nonce prefixes the result.
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Open reverses Seal. It returns an error if the blob is malformed or if the
// key is wrong (GCM authentication fails) — the same guard that makes a blob
// unreadable without the owner's key.
func Open(key [32]byte, sealed []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < nonceSize {
		return nil, errors.New("crypt: sealed blob too short")
	}
	nonce, ciphertext := sealed[:nonceSize], sealed[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("crypt: open: %w", err)
	}
	return plaintext, nil
}

func newGCM(key [32]byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("crypt: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypt: new gcm: %w", err)
	}
	return gcm, nil
}
