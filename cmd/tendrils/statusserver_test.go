package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"ca.punkscience.tendrils/internal/config"
	"ca.punkscience.tendrils/internal/index"
	"ca.punkscience.tendrils/internal/keys"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestStatusEndpointRoundTrip proves the whole point of the change: while a
// process holds the index lock, its loopback endpoint still answers status, and
// the CLI's queryDaemon reads it back — no second open of the locked index.
func TestStatusEndpointRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TENDRILS_HOME", home)

	store, err := index.Open(filepath.Join(home, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	id, err := keys.Generate()
	if err != nil {
		t.Fatal(err)
	}
	npub, _ := id.Npub()

	cfg := &config.Config{
		SyncRoot:       t.TempDir(),
		Relays:         []string{"wss://relay.example"},
		BlossomServers: []string{"https://blossom.example"},
	}

	state := &syncState{}
	state.begin() // simulate a reconcile in flight

	stop := serveStatus(&statusServer{id: id, cfg: cfg, idx: store, state: state}, discardLogger())
	defer stop()

	snap, running, err := queryDaemon()
	if err != nil {
		t.Fatalf("queryDaemon: %v", err)
	}
	if !running {
		t.Fatal("expected daemon to be reported running")
	}
	if snap.Identity != npub {
		t.Errorf("identity: got %q want %q", snap.Identity, npub)
	}
	if snap.SyncRoot != cfg.SyncRoot {
		t.Errorf("sync root: got %q want %q", snap.SyncRoot, cfg.SyncRoot)
	}
	if !snap.Syncing {
		t.Error("expected Syncing=true (a pass was in flight)")
	}
	if len(snap.Storage) != 1 || snap.Storage[0] != "https://blossom.example" {
		t.Errorf("storage: got %v", snap.Storage)
	}
}

// TestQueryDaemonNotRunning covers the two ways the CLI concludes no daemon is
// listening — no address file, and a stale one from a crash — both of which must
// let the CLI fall back to opening the index directly.
func TestQueryDaemonNotRunning(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TENDRILS_HOME", home)

	// No address file at all.
	if _, running, err := queryDaemon(); running || err != nil {
		t.Fatalf("no addr file: got running=%v err=%v, want running=false err=nil", running, err)
	}

	// Stale address file pointing at a port nobody is listening on.
	addrPath, err := config.DaemonAddrPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(addrPath, []byte("127.0.0.1:1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, running, err := queryDaemon(); running || err != nil {
		t.Fatalf("stale addr file: got running=%v err=%v, want running=false err=nil", running, err)
	}
}
