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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
)

// authKind is the BUD-01 authorization event kind the Tendrils blob client signs.
const authKind = 24242

// emptySHA256 is the content address of zero bytes — the only address a 0-byte
// blob may legitimately be filed under.
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

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
			if err := storeAtomic(dir, h, data); err != nil {
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
			// BUD-02 listing. Enumerating the store is what makes orphan collection
			// possible: a sweeper cannot tell which blobs are unreferenced without
			// first knowing which blobs exist.
			if strings.HasPrefix(r.URL.Path, "/list") {
				if err := authorize(r, allowed, "list"); err != nil {
					http.Error(w, err.Error(), http.StatusUnauthorized)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodHead {
					return
				}
				// Streamed, not built then sent. Reporting a blob's size costs a stat
				// each, and a store with tens of thousands of blobs on a slow disk
				// takes many minutes to walk — long enough that a client waiting for
				// response headers gives up before the first byte. Writing the array
				// as it is walked puts the headers out immediately and lets the body
				// take as long as it needs.
				n, err := streamList(w, dir)
				if err != nil {
					// Too late for a status code; the client's JSON decode will fail,
					// which is the correct outcome — a truncated listing must never be
					// mistaken for a complete one.
					log.Printf("GET %s failed after %d blobs: %v", r.URL.Path, n, err)
					return
				}
				log.Printf("GET %s -> %d blobs", r.URL.Path, n)
				return
			}

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
			// A blob that cannot possibly hash to the address it is filed under is
			// corrupt, and reporting it as present is worse than reporting it absent:
			// clients skip re-uploading what the server claims to hold. Only the
			// empty-file case is detectable without rehashing, and it is the one
			// earlier non-atomic writes actually produced.
			if info.Size() == 0 && h != emptySHA256 {
				log.Printf("corrupt blob %s (0 bytes) reported as absent", h)
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

// blobInfo is one stored blob as reported by the listing.
type blobInfo struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	// inode orders the listing by approximate physical layout; never serialised.
	inode uint64 `json:"-"`
	// Uploaded is the store's own mtime, not a client-supplied time. A sweeper
	// needs it to leave recently-written blobs alone: an upload whose describing
	// event has not propagated yet must not look like an orphan.
	Uploaded int64 `json:"uploaded"`
}

// forEachBlob walks the store, calling fn for every well-formed blob. Only
// 64-hex names qualify, so an in-progress temp file from storeAtomic is never
// mistaken for a blob — a sweeper that treated one as an orphan would delete a
// blob mid-write.
//
// Listing order is not cosmetic, it is the difference between a sweep that takes
// hours and one that takes days. Callers read each blob's contents in the order
// we report, and blob stores sit on spinning disks. Two traps:
//
//   - `os.ReadDir` sorts by filename, and here the filename *is* the content
//     hash — so it hands back an order guaranteed to be random with respect to
//     physical layout. `File.ReadDir` does not sort, which is why it is used here.
//   - ext4's own directory order is by filename *hash* too, so raw readdir order
//     is no better. Sorting by inode is what actually helps: ext4 allocates a
//     file's data near its inode, so ascending inode order approximates ascending
//     disk order.
//
// A real sweep of 73k blobs on a USB disk ran at 5 MB/s in hash order.
func forEachBlob(dir string, fn func(blobInfo) error) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()

	// ReadDir on the *File (not os.ReadDir) returns entries unsorted.
	entries, err := f.ReadDir(-1)
	if err != nil {
		return err
	}

	blobs := make([]blobInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !isBlobName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // vanished mid-listing; it is simply not there
		}
		blobs = append(blobs, blobInfo{
			SHA256:   e.Name(),
			Size:     info.Size(),
			Uploaded: info.ModTime().Unix(),
			inode:    inodeOf(info),
		})
	}
	sort.Slice(blobs, func(i, j int) bool { return blobs[i].inode < blobs[j].inode })

	for _, b := range blobs {
		if err := fn(b); err != nil {
			return err
		}
	}
	return nil
}

// listBlobs enumerates the store into a slice. Used where the whole list is
// wanted at once; the HTTP handler streams instead.
func listBlobs(dir string) ([]blobInfo, error) {
	var out []blobInfo
	err := forEachBlob(dir, func(b blobInfo) error {
		out = append(out, b)
		return nil
	})
	return out, err
}

// streamList writes the listing as a JSON array while it walks, flushing so the
// client sees headers and progress rather than waiting for the whole walk.
func streamList(w http.ResponseWriter, dir string) (int, error) {
	flusher, _ := w.(http.Flusher)
	if _, err := io.WriteString(w, "["); err != nil {
		return 0, err
	}
	if flusher != nil {
		flusher.Flush() // headers out now, before the expensive part
	}

	n := 0
	enc := json.NewEncoder(w)
	err := forEachBlob(dir, func(b blobInfo) error {
		if n > 0 {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		if err := enc.Encode(b); err != nil {
			return err
		}
		n++
		if flusher != nil && n%1000 == 0 {
			flusher.Flush()
		}
		return nil
	})
	if err != nil {
		return n, err
	}
	if _, err := io.WriteString(w, "]"); err != nil {
		return n, err
	}
	if flusher != nil {
		flusher.Flush()
	}
	return n, nil
}

// isBlobName reports whether name is a 64-character lowercase hex content address.
func isBlobName(name string) bool {
	if len(name) != 64 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// storeAtomic writes data to its content address so that the blob is either
// absent or complete, never half-written.
//
// The obvious `os.WriteFile(dir/h, data)` is not safe here, and shipping it cost
// a week of silent corruption. WriteFile opens with O_CREATE|O_TRUNC and *then*
// writes: when the write fails — a full disk is the easy way — the create has
// already succeeded, so an empty file is left sitting at the blob's final
// address. From then on the server answers HEAD for that address with 200, every
// client's "does it already have this?" check says yes and skips the upload, and
// the blob is never repaired. A transient ENOSPC becomes a permanent, silent
// failure for that file, and no log line anywhere says so.
//
// Writing to a temp file in the same directory and renaming only once the bytes
// are down makes presence mean what every caller already assumes it means. The
// rename is atomic within a filesystem, so a reader sees the old state or the new
// one. On any failure the temp file is removed, leaving nothing behind to lie
// about.
func storeAtomic(dir, h string, data []byte) error {
	if _, err := os.Stat(filepath.Join(dir, h)); err == nil {
		return nil // already stored; content-addressed, so it is the same bytes
	}
	tmp, err := os.CreateTemp(dir, ".tmp-"+h+"-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(name) // no-op once the rename below has succeeded
	}()

	if _, err := tmp.Write(data); err != nil {
		return err
	}
	// Durability before visibility: fsync, then rename. Without the sync a crash
	// could leave the rename visible with the contents still in page cache.
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(dir, h))
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
