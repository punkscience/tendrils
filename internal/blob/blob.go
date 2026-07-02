// Package blob is Tendrils' client for a Blossom server (BUD-01/02): the place
// file bytes live, addressed by the SHA-256 of the stored bytes. It is the
// "file-content transfer layer", kept deliberately separate from the tiny Nostr
// events that describe the tree — either can change without touching the other.
//
// This layer knows nothing about encryption or plaintext hashes. It moves opaque
// bytes and their content address. The engine seals a file with internal/crypt,
// hands the ciphertext here, and records the returned address in the event it
// publishes; a pulling device fetches those bytes by that address and unseals
// them. Because the content address is the hash of the *stored* (sealed) bytes,
// it is what the engine must publish — not the plaintext hash used for file
// identity and dedup.
//
// Every request carries a short-lived BUD-01 authorization: a signed kind-24242
// Nostr event, base64-encoded in the Authorization header, proving the owner's
// key authorized this upload/get. The same key that encrypts is the key that
// authorizes, so no separate credential exists to manage.
package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"ca.punkscience.tendrils/internal/keys"
)

// authKind is the BUD-01 authorization event kind.
const authKind = 24242

// authTTL is how long a signed authorization stays valid. Short by design: the
// header is minted per request, so a leaked one expires quickly.
const authTTL = 60 * time.Second

// ErrNotFound is returned when a Blossom server has no blob for a hash. It lets
// the engine tell "the server doesn't have it (yet)" apart from a transport
// failure it should retry.
var ErrNotFound = errors.New("blob: not found")

// Descriptor is a Blossom server's record of a stored blob (BUD-02).
type Descriptor struct {
	// URL is the server-provided direct URL to the blob.
	URL string `json:"url"`
	// SHA256 is the content address: the lowercase-hex SHA-256 of the stored bytes.
	SHA256 string `json:"sha256"`
	// Size is the stored byte length.
	Size int64 `json:"size"`
}

// Client talks to a single Blossom server as one enrolled identity. Multi-server
// mirroring is out of v1 scope; the engine composes one Client per configured
// server if it needs several.
type Client struct {
	server string // base URL, no trailing slash
	id     *keys.Identity
	http   *http.Client
}

// New returns a Client for server, authorizing as id, using a default HTTP
// client with a sane timeout.
func New(server string, id *keys.Identity) *Client {
	return NewWithHTTP(server, id, &http.Client{Timeout: 30 * time.Second})
}

// NewWithHTTP is New with a caller-supplied *http.Client (for tests, custom
// timeouts, or proxies).
func NewWithHTTP(server string, id *keys.Identity, hc *http.Client) *Client {
	return &Client{server: strings.TrimRight(server, "/"), id: id, http: hc}
}

// Upload stores data and returns its descriptor. The content address is computed
// locally and cross-checked against the server's, so a mangled upload is caught
// here rather than surfacing as a corrupt download later.
func (c *Client) Upload(ctx context.Context, data []byte) (Descriptor, error) {
	sum := hashHex(data)

	auth, err := c.authHeader("upload", sum)
	if err != nil {
		return Descriptor{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.server+"/upload", bytes.NewReader(data))
	if err != nil {
		return Descriptor{}, fmt.Errorf("blob: build upload request: %w", err)
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.http.Do(req)
	if err != nil {
		return Descriptor{}, fmt.Errorf("blob: upload to %s: %w", c.server, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Descriptor{}, fmt.Errorf("blob: upload rejected (%s): %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var d Descriptor
	if err := json.Unmarshal(body, &d); err != nil {
		return Descriptor{}, fmt.Errorf("blob: parse upload response: %w", err)
	}
	if d.SHA256 != sum {
		return Descriptor{}, fmt.Errorf("blob: server stored %s but we sent %s", d.SHA256, sum)
	}
	return d, nil
}

// Download fetches the blob at sha256 and verifies the bytes hash back to it, so
// a wrong or corrupted response never reaches the caller. Returns ErrNotFound
// when the server has no such blob.
func (c *Client) Download(ctx context.Context, sha256 string) ([]byte, error) {
	auth, err := c.authHeader("get", sha256)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.server+"/"+sha256, nil)
	if err != nil {
		return nil, fmt.Errorf("blob: build download request: %w", err)
	}
	req.Header.Set("Authorization", auth)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("blob: download from %s: %w", c.server, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return nil, fmt.Errorf("blob: download failed (%s): %s", resp.Status, strings.TrimSpace(string(msg)))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("blob: read blob body: %w", err)
	}
	if got := hashHex(data); got != sha256 {
		return nil, fmt.Errorf("blob: integrity check failed: got %s want %s", got, sha256)
	}
	return data, nil
}

// Has reports whether the server holds the blob at sha256, via a HEAD request.
func (c *Client) Has(ctx context.Context, sha256 string) (bool, error) {
	auth, err := c.authHeader("get", sha256)
	if err != nil {
		return false, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.server+"/"+sha256, nil)
	if err != nil {
		return false, fmt.Errorf("blob: build head request: %w", err)
	}
	req.Header.Set("Authorization", auth)

	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("blob: head to %s: %w", c.server, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return true, nil
	default:
		return false, fmt.Errorf("blob: head failed (%s)", resp.Status)
	}
}

// authHeader mints a signed BUD-01 authorization for one verb ("upload", "get")
// bound to one blob hash, valid for authTTL. Returns "Nostr <base64-event>".
func (c *Client) authHeader(verb, sha256 string) (string, error) {
	now := time.Now()
	evt := nostr.Event{
		CreatedAt: nostr.Timestamp(now.Unix()),
		Kind:      authKind,
		Content:   "tendrils " + verb,
		Tags: nostr.Tags{
			{"t", verb},
			{"expiration", strconv.FormatInt(now.Add(authTTL).Unix(), 10)},
			{"x", sha256},
		},
	}
	if err := evt.Sign(c.id.SecretHex()); err != nil {
		return "", fmt.Errorf("blob: sign authorization: %w", err)
	}
	raw, err := json.Marshal(&evt)
	if err != nil {
		return "", fmt.Errorf("blob: marshal authorization: %w", err)
	}
	return "Nostr " + base64.StdEncoding.EncodeToString(raw), nil
}

// hashHex is the lowercase-hex SHA-256 of data — the Blossom content address.
func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
