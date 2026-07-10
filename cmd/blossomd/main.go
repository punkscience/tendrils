// Command blossomd is a minimal reference Blossom (BUD-01/02) server for
// Tendrils: it stores opaque bytes addressed by their SHA-256 and serves them
// back. It is the "file-content transfer layer" the sync engine uploads sealed
// (encrypted) blobs to.
//
// Scope and warnings:
//   - Authorization is OFF by default: with no BLOSSOM_ALLOWED_PUBKEYS set, any
//     client may upload or fetch. That is fine on localhost or a trusted LAN.
//     Before exposing this server to the public internet, set
//     BLOSSOM_ALLOWED_PUBKEYS to your Tendrils key's pubkey(s): the server then
//     verifies the signed BUD-01 authorization every Tendrils client already
//     sends and rejects anyone else — otherwise an open server can be filled with
//     junk (a disk-fill DoS). Blobs are always encrypted before upload, so
//     contents stay private regardless.
//   - It binds to 127.0.0.1 by default. Set BLOSSOM_ADDR=0.0.0.0:8091 to expose
//     it on your LAN or behind a reverse proxy / tunnel.
//
// Environment:
//
//	BLOSSOM_ADDR             listen address (default 127.0.0.1:8091)
//	BLOSSOM_DIR              blob storage directory (default ./blobs)
//	BLOSSOM_ALLOWED_PUBKEYS  comma-separated npub/hex pubkeys allowed to
//	                         upload, fetch, and delete; empty = open (no auth)
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
)

// authKind is the BUD-01 authorization event kind the Tendrils blob client signs.
const authKind = 24242

func main() {
	dir := envOr("BLOSSOM_DIR", "./blobs")
	addr := envOr("BLOSSOM_ADDR", "127.0.0.1:8091")
	allowed, err := parseAllowed(os.Getenv("BLOSSOM_ALLOWED_PUBKEYS"))
	if err != nil {
		log.Fatalf("BLOSSOM_ALLOWED_PUBKEYS: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			if r.URL.Path != "/upload" {
				http.NotFound(w, r)
				return
			}
			if err := authorize(r, allowed, "upload"); err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
			data, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			sum := sha256.Sum256(data)
			h := hex.EncodeToString(sum[:])
			if err := os.WriteFile(filepath.Join(dir, h), data, 0o644); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			log.Printf("PUT /upload -> %s (%d bytes)", h, len(data))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"url":    fmt.Sprintf("http://%s/%s", r.Host, h),
				"sha256": h,
				"size":   len(data),
			})

		case http.MethodGet, http.MethodHead:
			if err := authorize(r, allowed, "get"); err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
			h := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), ".", 2)[0]
			if len(h) != 64 {
				http.NotFound(w, r)
				return
			}
			p := filepath.Join(dir, h)
			info, err := os.Stat(p)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", fmt.Sprint(info.Size()))
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusOK)
				return
			}
			f, err := os.Open(p)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			defer f.Close()
			_, _ = io.Copy(w, f)
			log.Printf("GET /%s (%d bytes)", h, info.Size())

		case http.MethodDelete:
			if err := authorize(r, allowed, "delete"); err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
			h := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), ".", 2)[0]
			if len(h) != 64 {
				http.NotFound(w, r)
				return
			}
			switch err := os.Remove(filepath.Join(dir, h)); {
			case err == nil:
				log.Printf("DELETE /%s", h)
				w.WriteHeader(http.StatusOK)
			case os.IsNotExist(err):
				w.WriteHeader(http.StatusOK) // idempotent: already gone
			default:
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	if len(allowed) == 0 {
		log.Printf("blossomd listening on %s, dir=%s (AUTH OFF — open server, keep off the public internet)", addr, dir)
	} else {
		log.Printf("blossomd listening on %s, dir=%s (auth on, %d allowed key(s))", addr, dir, len(allowed))
	}
	log.Fatal(http.ListenAndServe(addr, nil))
}

// authorize enforces the pubkey allowlist for one verb ("upload"/"get"/"delete"). When no
// keys are configured it is a no-op (open server). Otherwise it requires the
// BUD-01 "Nostr <base64-event>" Authorization header the Tendrils blob client
// sends: a kind-24242 event with a valid signature from an allowed pubkey, a
// matching verb tag, and an unexpired expiration.
func authorize(r *http.Request, allowed map[string]struct{}, verb string) error {
	if len(allowed) == 0 {
		return nil
	}
	raw := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(raw, "Nostr ") {
		return fmt.Errorf("missing Nostr authorization")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(raw, "Nostr "))
	if err != nil {
		return fmt.Errorf("malformed authorization")
	}
	var evt nostr.Event
	if err := json.Unmarshal(decoded, &evt); err != nil {
		return fmt.Errorf("malformed authorization event")
	}
	if evt.Kind != authKind {
		return fmt.Errorf("wrong authorization kind")
	}
	if _, ok := allowed[strings.ToLower(evt.PubKey)]; !ok {
		return fmt.Errorf("pubkey not allowed")
	}
	ok, err := evt.CheckSignature()
	if err != nil || !ok {
		return fmt.Errorf("invalid authorization signature")
	}
	if t := evt.Tags.GetFirst([]string{"t"}); t == nil || t.Value() != verb {
		return fmt.Errorf("authorization not valid for %s", verb)
	}
	if exp := evt.Tags.GetFirst([]string{"expiration"}); exp != nil {
		secs, err := strconv.ParseInt(exp.Value(), 10, 64)
		if err != nil || time.Now().Unix() > secs {
			return fmt.Errorf("authorization expired")
		}
	}
	return nil
}

// parseAllowed builds the set of allowed hex pubkeys from a comma-separated list
// of npub or 64-char hex keys.
func parseAllowed(spec string) (map[string]struct{}, error) {
	set := make(map[string]struct{})
	for _, tok := range strings.Split(spec, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		switch {
		case strings.HasPrefix(tok, "npub1"):
			_, v, err := nip19.Decode(tok)
			if err != nil {
				return nil, fmt.Errorf("invalid npub %q: %w", tok, err)
			}
			hexPub, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("invalid npub %q", tok)
			}
			set[strings.ToLower(hexPub)] = struct{}{}
		case len(tok) == 64:
			set[strings.ToLower(tok)] = struct{}{}
		default:
			return nil, fmt.Errorf("not an npub or 64-char hex pubkey: %q", tok)
		}
	}
	return set, nil
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}
