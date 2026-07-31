package gc

import (
	"testing"

	"ca.punkscience.tendrils/internal/tree"
)

// A chunked entry keeps every one of its parts. Missing this is not a tuning
// bug: the sweep would classify each chunk as an orphan and delete the file's
// contents while the entry referencing them is still live.
func TestLiveBlobsKeepsChunks(t *testing.T) {
	entries := []*tree.Entry{
		{Path: "small.txt", Sha256: "aa", BlobHash: "blob-small"},
		{Path: "big.flac", Sha256: "bb", Chunks: []tree.Chunk{
			{BlobHash: "chunk-1", Size: 10},
			{BlobHash: "chunk-2", Size: 10},
			{BlobHash: "chunk-3", Size: 4},
		}},
	}

	live := LiveBlobs(entries)

	for _, want := range []string{"blob-small", "chunk-1", "chunk-2", "chunk-3"} {
		if _, ok := live[want]; !ok {
			t.Errorf("%s missing from keep-set — a sweep would delete it", want)
		}
	}
	if len(live) != 4 {
		t.Errorf("keep-set size = %d, want 4: %v", len(live), live)
	}
}

// A chunked tombstone releases its parts, the same way a single-blob tombstone
// releases its blob — otherwise deleting a large file reclaims nothing.
func TestLiveBlobsDropsChunksOfTombstone(t *testing.T) {
	entries := []*tree.Entry{
		{Path: "gone.flac", Deleted: true, Chunks: []tree.Chunk{
			{BlobHash: "chunk-1", Size: 10},
			{BlobHash: "chunk-2", Size: 10},
		}},
	}

	if live := LiveBlobs(entries); len(live) != 0 {
		t.Errorf("keep-set = %v, want empty for a tombstone", live)
	}
}
