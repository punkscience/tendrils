// Package serverlist maps between a set of Blossom server URLs and the Nostr
// event that publishes them under the owner's key, so a device enrolled with
// only the key and a relay can *discover* where the blobs live instead of being
// told at enroll time.
//
// This closes the architecture's original gap: the blob *location* was never
// part of the synced state. Identity, the file-entry events, and the encryption
// key all travel with the key; the Blossom URL was purely per-device local
// config, so a fresh device saw the file hashes but had no idea which server
// held the bytes. Publishing the list under the key makes location discoverable.
//
// Event model — Blossom User Server List (BUD-03), a replaceable event:
//
//	kind:       Kind (10063)
//	server tag: one per Blossom server URL, in preference order
//	content:    empty
//
// Because every Tendrils device shares one identity (one pubkey), the list is a
// single replaceable event for the whole identity: "the servers this identity
// uses". Callers publish the *union* of the existing list and their own servers
// (see Merge) so two devices refreshing it do not clobber each other.
package serverlist

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/nbd-wtf/go-nostr"
)

// Kind is the Blossom User Server List event kind (BUD-03). It is a replaceable
// event (10000–19999): the relay keeps only the newest per (pubkey, kind).
const Kind = 10063

// Build constructs an unsigned kind-10063 event advertising servers, in order.
func Build(servers []string) *nostr.Event {
	tags := make(nostr.Tags, 0, len(servers))
	for _, s := range servers {
		tags = append(tags, nostr.Tag{"server", s})
	}
	return &nostr.Event{
		Kind:    Kind,
		Tags:    tags,
		Content: "",
	}
}

// Sign builds and signs a server-list event with the hex secret key.
func Sign(servers []string, secretHex string) (*nostr.Event, error) {
	evt := Build(servers)
	if err := evt.Sign(secretHex); err != nil {
		return nil, fmt.Errorf("serverlist: sign: %w", err)
	}
	return evt, nil
}

// Parse extracts the advertised server URLs from a received event, verifying the
// signature and the kind first. Order is preserved.
func Parse(evt *nostr.Event) ([]string, error) {
	if evt.Kind != Kind {
		return nil, fmt.Errorf("serverlist: wrong kind %d", evt.Kind)
	}
	ok, err := evt.CheckSignature()
	if err != nil {
		return nil, fmt.Errorf("serverlist: check signature: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("serverlist: invalid signature")
	}
	var servers []string
	for _, t := range evt.Tags {
		if len(t) >= 2 && t[0] == "server" && strings.TrimSpace(t[1]) != "" {
			servers = append(servers, strings.TrimSpace(t[1]))
		}
	}
	return servers, nil
}

// Shareable drops URLs no other device could reach — loopback and unspecified
// hosts (127.0.0.1, ::1, localhost, 0.0.0.0). A device configured to talk to its
// own Blossom over localhost must not advertise that address to the whole
// identity, or a remote puller would try to fetch blobs from its own empty box.
// LAN addresses (192.168.x etc.) are kept: they are valid for same-LAN discovery.
func Shareable(servers []string) []string {
	out := make([]string, 0, len(servers))
	for _, s := range servers {
		if reachableByOthers(s) {
			out = append(out, s)
		}
	}
	return out
}

// Merge returns the order-preserving union of two server lists, de-duplicated,
// so republishing the list accumulates servers instead of overwriting them.
func Merge(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range append(append([]string{}, a...), b...) {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// reachableByOthers reports whether a URL's host is something a *different*
// device could plausibly reach — i.e. not loopback/unspecified. A URL that fails
// to parse is treated as reachable (kept): the filter's job is to strip the
// obviously-local, not to validate.
func reachableByOthers(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return true
	}
	host := u.Hostname()
	if host == "" || strings.EqualFold(host, "localhost") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsLoopback() && !ip.IsUnspecified()
	}
	return true
}
