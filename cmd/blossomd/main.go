// Command blossomd is a minimal reference Blossom (BUD-01/02) server for
// Tendrils: it stores opaque bytes addressed by their SHA-256 and serves them
// back. It is the "file-content transfer layer" the sync engine uploads sealed
// (encrypted) blobs to.
//
// Scope and warnings:
//   - It does NOT verify the signed BUD-01 Authorization header. Any client may
//     upload or fetch. Blobs are content-addressed, so a blob cannot be
//     overwritten with different bytes, but an open server can be filled with
//     junk. Tendrils encrypts every blob before upload, so contents are private
//     regardless — but treat this server as personal/LAN/testing infrastructure,
//     not a hardened public endpoint.
//   - It binds to 127.0.0.1 by default. Set BLOSSOM_ADDR=0.0.0.0:8091 to expose
//     it on your LAN (only do so on a network you trust).
//
// Environment:
//
//	BLOSSOM_ADDR   listen address (default 127.0.0.1:8091)
//	BLOSSOM_DIR    blob storage directory (default ./blobs)
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	dir := envOr("BLOSSOM_DIR", "./blobs")
	addr := envOr("BLOSSOM_ADDR", "127.0.0.1:8091")
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

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	log.Printf("blossomd listening on %s, dir=%s", addr, dir)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}
