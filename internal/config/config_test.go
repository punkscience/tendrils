package config

import (
	"testing"

	"ca.punkscience.tendrils/internal/keys"
)

// isolate points TENDRILS_HOME at a temp dir for the duration of a test.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv(envHome, t.TempDir())
}

func TestLoadMissingReturnsNotFound(t *testing.T) {
	isolate(t)
	_, found, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Errorf("expected not found for absent config")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	isolate(t)
	in := Config{
		SyncRoot:       "/home/sam/vault",
		Relays:         []string{"wss://relay.example"},
		BlossomServers: []string{"https://blossom.example"},
	}
	if err := Save(in); err != nil {
		t.Fatal(err)
	}
	got, found, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected found after save")
	}
	if got.SyncRoot != in.SyncRoot || len(got.Relays) != 1 || got.Relays[0] != in.Relays[0] {
		t.Errorf("config round-trip mismatch: %+v", got)
	}
}

func TestKeyPersistsAndResumes(t *testing.T) {
	isolate(t)
	id, err := keys.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveKey(id); err != nil {
		t.Fatal(err)
	}
	// Simulates a reboot: a fresh LoadKey with no re-entry.
	got, found, err := LoadKey()
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected stored key to be found")
	}
	if got.SecretHex() != id.SecretHex() {
		t.Errorf("resumed key differs from stored key")
	}
}

func TestLoadKeyMissingReturnsNotFound(t *testing.T) {
	isolate(t)
	_, found, err := LoadKey()
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Errorf("expected no key before enrollment")
	}
}
