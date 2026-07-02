package serverlist

import (
	"reflect"
	"testing"

	"github.com/nbd-wtf/go-nostr"
)

func TestSignParseRoundTrip(t *testing.T) {
	sk := nostr.GeneratePrivateKey()
	servers := []string{"https://blossom.example", "http://192.168.1.10:8091"}

	evt, err := Sign(servers, sk)
	if err != nil {
		t.Fatal(err)
	}
	if evt.Kind != Kind {
		t.Fatalf("kind = %d, want %d", evt.Kind, Kind)
	}

	got, err := Parse(evt)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, servers) {
		t.Fatalf("round trip = %v, want %v", got, servers)
	}
}

func TestParseRejectsWrongKind(t *testing.T) {
	sk := nostr.GeneratePrivateKey()
	evt := &nostr.Event{Kind: 1}
	if err := evt.Sign(sk); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(evt); err == nil {
		t.Fatal("expected error for wrong kind")
	}
}

func TestParseRejectsBadSignature(t *testing.T) {
	sk := nostr.GeneratePrivateKey()
	evt, err := Sign([]string{"https://blossom.example"}, sk)
	if err != nil {
		t.Fatal(err)
	}
	evt.Sig = "00" + evt.Sig[2:] // corrupt
	if _, err := Parse(evt); err == nil {
		t.Fatal("expected error for tampered signature")
	}
}

func TestShareableDropsLocalAddresses(t *testing.T) {
	in := []string{
		"http://127.0.0.1:8091",
		"http://localhost:8091",
		"http://0.0.0.0:8091",
		"https://blossom.towerofsong.ca",
		"http://192.168.87.40:8091", // LAN — kept
	}
	got := Shareable(in)
	want := []string{
		"https://blossom.towerofsong.ca",
		"http://192.168.87.40:8091",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Shareable = %v, want %v", got, want)
	}
}

func TestMergeUnionsAndDedupes(t *testing.T) {
	got := Merge(
		[]string{"https://a", "https://b"},
		[]string{"https://b", "https://c"},
	)
	want := []string{"https://a", "https://b", "https://c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Merge = %v, want %v", got, want)
	}
}
