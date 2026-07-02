package keys

import (
	"strings"
	"testing"
)

// A known test vector: secret key of all-ones is not used; instead we generate
// and round-trip through nsec, which is the enrollment path a user takes.
func TestParseHexAndNsecAgree(t *testing.T) {
	id, err := Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	nsec, err := id.Nsec()
	if err != nil {
		t.Fatalf("nsec: %v", err)
	}

	fromNsec, err := Parse(nsec)
	if err != nil {
		t.Fatalf("parse nsec: %v", err)
	}
	fromHex, err := Parse(id.SecretHex())
	if err != nil {
		t.Fatalf("parse hex: %v", err)
	}

	if fromNsec.SecretHex() != fromHex.SecretHex() {
		t.Errorf("nsec and hex parse disagree on secret")
	}
	if fromNsec.PublicHex() != id.PublicHex() {
		t.Errorf("public key changed through nsec round-trip")
	}
}

func TestParseRejectsBadInput(t *testing.T) {
	npub, _ := (mustGen(t)).Npub()
	cases := map[string]string{
		"empty":     "",
		"npub":      npub,
		"short hex": "abcd",
		"non-hex":   strings.Repeat("z", 64),
		"wrong len": strings.Repeat("a", 63),
	}
	for name, in := range cases {
		if _, err := Parse(in); err == nil {
			t.Errorf("%s: expected error, got none", name)
		}
	}
}

// SymmetricKey must be deterministic across parses of the same key and differ
// between different keys — the property the encryption rule depends on.
func TestSymmetricKeyDeterministicAndKeyBound(t *testing.T) {
	a := mustGen(t)
	aAgain, err := Parse(a.SecretHex())
	if err != nil {
		t.Fatal(err)
	}
	b := mustGen(t)

	ka, _ := a.SymmetricKey()
	ka2, _ := aAgain.SymmetricKey()
	kb, _ := b.SymmetricKey()

	if ka != ka2 {
		t.Errorf("symmetric key not deterministic for same identity")
	}
	if ka == kb {
		t.Errorf("different keys derived the same symmetric key")
	}
	if ka == [32]byte{} {
		t.Errorf("symmetric key is all zero")
	}
}

func mustGen(t *testing.T) *Identity {
	t.Helper()
	id, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
