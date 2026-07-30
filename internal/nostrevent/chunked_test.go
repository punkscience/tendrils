package nostrevent

import (
	"testing"
	"time"

	"ca.punkscience.tendrils/internal/tree"
)

// Chunk order is the file's byte order, so the codec has to preserve it exactly.
// A set-like round trip would reassemble scrambled content that still passes
// every per-chunk check.
func TestSignParseRoundTripChunked(t *testing.T) {
	id := mustID(t)
	in := &tree.Entry{
		Path:    "music/big.flac",
		Sha256:  "plaintexthash",
		Size:    5000,
		ModTime: time.Unix(1_700_000_000, 0),
		Chunks: []tree.Chunk{
			{BlobHash: "aaa", Size: 1028},
			{BlobHash: "bbb", Size: 1028},
			{BlobHash: "ccc", Size: 972},
		},
	}
	evt, err := Sign(in, id.SecretHex())
	if err != nil {
		t.Fatal(err)
	}
	out, err := Parse(evt)
	if err != nil {
		t.Fatal(err)
	}

	if !out.Chunked() {
		t.Fatalf("parsed entry is not chunked: %+v", out)
	}
	if len(out.Chunks) != len(in.Chunks) {
		t.Fatalf("chunk count = %d, want %d", len(out.Chunks), len(in.Chunks))
	}
	for i := range in.Chunks {
		if out.Chunks[i] != in.Chunks[i] {
			t.Errorf("chunk %d = %+v, want %+v", i, out.Chunks[i], in.Chunks[i])
		}
	}
	if out.Sha256 != in.Sha256 || out.Size != in.Size {
		t.Errorf("identity = (%s, %d), want (%s, %d)", out.Sha256, out.Size, in.Sha256, in.Size)
	}
}

// A single-blob entry must not grow chunk tags, so events written for small
// files stay byte-identical to what earlier builds produced.
func TestSingleBlobEntryHasNoChunkTags(t *testing.T) {
	id := mustID(t)
	evt, err := Sign(&tree.Entry{
		Path:     "notes/a.md",
		Sha256:   "x",
		BlobHash: "y",
		Size:     3,
		ModTime:  time.Unix(1_700_000_000, 0),
	}, id.SecretHex())
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range evt.Tags {
		if len(tag) > 0 && tag[0] == "chunk" {
			t.Fatalf("unexpected chunk tag on a single-blob entry: %v", tag)
		}
	}
	out, err := Parse(evt)
	if err != nil {
		t.Fatal(err)
	}
	if out.Chunked() {
		t.Errorf("parsed single-blob entry reports as chunked: %+v", out.Chunks)
	}
}

// A malformed chunk size must fail loudly. Silently dropping the chunk would
// produce an entry that reassembles into a truncated file.
func TestParseRejectsUnparseableChunkSize(t *testing.T) {
	id := mustID(t)
	evt, err := Sign(&tree.Entry{
		Path:    "music/big.flac",
		Sha256:  "h",
		Size:    10,
		ModTime: time.Unix(1_700_000_000, 0),
		Chunks:  []tree.Chunk{{BlobHash: "aaa", Size: 10}},
	}, id.SecretHex())
	if err != nil {
		t.Fatal(err)
	}
	for i, tag := range evt.Tags {
		if len(tag) > 2 && tag[0] == "chunk" {
			evt.Tags[i][2] = "not-a-number"
		}
	}
	if _, err := Parse(evt); err == nil {
		t.Fatal("parse accepted an unparseable chunk size")
	}
}
