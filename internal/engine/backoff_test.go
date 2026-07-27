package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"ca.punkscience.tendrils/internal/blob"
	"ca.punkscience.tendrils/internal/crypt"
	"ca.punkscience.tendrils/internal/index"
	"ca.punkscience.tendrils/internal/nostrevent"
)

// rejectingBlobs is a Blossom server that refuses every upload with the given
// error, standing in for one behind a proxy that caps request bodies.
type rejectingBlobs struct {
	*fakeBlobs
	err      string
	tooLarge bool
	attempts int
}

func newRejectingBlobs(tooLarge bool) *rejectingBlobs {
	return &rejectingBlobs{fakeBlobs: newFakeBlobs(), err: "rejected", tooLarge: tooLarge}
}

func (r *rejectingBlobs) Upload(_ context.Context, data []byte) (blob.Descriptor, error) {
	r.attempts++
	if r.tooLarge {
		return blob.Descriptor{}, fmt.Errorf("blob: refused %d bytes: %w", len(data), blob.ErrTooLarge)
	}
	return blob.Descriptor{}, errors.New(r.err)
}

// The transient schedule doubles from one minute and stops at the ceiling, so a
// path that keeps failing is retried ever more slowly but never faster than the
// pass interval or slower than an hour.
func TestRetryDelayTransientSchedule(t *testing.T) {
	for _, tc := range []struct {
		failures int
		want     time.Duration
	}{
		{1, time.Minute},
		{2, 2 * time.Minute},
		{3, 4 * time.Minute},
		{6, 32 * time.Minute},
		{7, time.Hour}, // doubling to 64m would pass the ceiling
		{99, time.Hour},
	} {
		if got := retryDelay(tc.failures, false); got != tc.want {
			t.Errorf("retryDelay(%d, false) = %v, want %v", tc.failures, got, tc.want)
		}
	}
}

// A permanent failure gets one flat long interval regardless of how many times
// it has failed — and crucially a finite one, so a raised server limit is picked
// up without the owner clearing state by hand.
func TestRetryDelayPermanentIsFlatAndFinite(t *testing.T) {
	for _, failures := range []int{1, 5, 500} {
		if got := retryDelay(failures, true); got != retryPermanent {
			t.Errorf("retryDelay(%d, true) = %v, want %v", failures, got, retryPermanent)
		}
	}
	if retryPermanent <= retryMax {
		t.Errorf("retryPermanent (%v) should exceed retryMax (%v)", retryPermanent, retryMax)
	}
}

// Only a size rejection counts as permanent. Misjudging a timeout or a 5xx as
// permanent would stall a healthy file for a day over a momentary blip.
func TestPermanentFailureIsNarrow(t *testing.T) {
	if !permanentFailure(fmt.Errorf("upload: %w", blob.ErrTooLarge)) {
		t.Error("a wrapped ErrTooLarge should be permanent")
	}
	for _, err := range []error{
		errors.New("upload rejected (524 <none>): error code: 524"),
		errors.New("write: connection reset by peer"),
		context.DeadlineExceeded,
		blob.ErrNotFound,
	} {
		if permanentFailure(err) {
			t.Errorf("%v should be treated as transient", err)
		}
	}
}

// A file the server refuses for its size is attempted once, then held back
// instead of being retried on every pass. It stays counted as outstanding work,
// because a tree with an unsyncable file in it is not synced.
func TestPermanentlyRejectedFileIsDeferredNotRetried(t *testing.T) {
	id := mustID(t)
	ev := newFakeEvents()
	bl := newRejectingBlobs(true)
	root := t.TempDir()
	writeFile(t, root, "huge.flac", "pretend this is 900 MB", time.Unix(1_700_000_000, 0))

	eng := newEngine(t, root, id, ev, bl)
	var stats Stats
	eng.OnStats(func(s Stats) { stats = s })

	// First pass: the upload is attempted and fails.
	if err := eng.Sync(context.Background()); err == nil {
		t.Fatal("first pass should surface the rejection")
	}
	if bl.attempts != 1 {
		t.Fatalf("upload attempts after one pass = %d, want 1", bl.attempts)
	}
	if stats.Pending != 1 || stats.Deferred != 0 {
		t.Errorf("after first pass: pending=%d deferred=%d, want 1 and 0", stats.Pending, stats.Deferred)
	}

	// Second pass, immediately after: the path is inside its backoff, so nothing
	// is uploaded and the pass itself succeeds — one stuck file must not keep
	// reporting the whole pass as failed.
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("second pass should not retry or fail: %v", err)
	}
	if bl.attempts != 1 {
		t.Errorf("upload attempts after two passes = %d, want 1 (held back)", bl.attempts)
	}
	if stats.Pending != 1 {
		t.Errorf("deferred work stopped counting as pending (pending=%d, want 1)", stats.Pending)
	}
	if stats.Deferred != 1 {
		t.Errorf("deferred = %d, want 1", stats.Deferred)
	}
}

// The backoff record carries why the path is stuck and when it will be tried
// again, so status can explain itself rather than just showing a stalled count.
func TestFailureRecordsCauseAndNextAttempt(t *testing.T) {
	id := mustID(t)
	ev := newFakeEvents()
	bl := newRejectingBlobs(true)
	root := t.TempDir()
	writeFile(t, root, "huge.flac", "pretend this is 900 MB", time.Unix(1_700_000_000, 0))

	eng := newEngine(t, root, id, ev, bl)
	before := time.Now()
	if err := eng.Sync(context.Background()); err == nil {
		t.Fatal("expected the pass to surface the rejection")
	}

	retries, err := eng.idx.Retries()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := retries["huge.flac"]
	if !ok {
		t.Fatal("no retry record written for the failing path")
	}
	if r.Failures != 1 {
		t.Errorf("failures = %d, want 1", r.Failures)
	}
	if !r.Permanent {
		t.Error("a size rejection should be recorded as permanent")
	}
	if r.LastError == "" {
		t.Error("the cause should be recorded")
	}
	if got := r.NextAttempt.Sub(before); got < retryPermanent {
		t.Errorf("next attempt in %v, want at least %v out", got, retryPermanent)
	}
}

// A transient failure escalates rather than repeating at a fixed interval: the
// second failure's wait is longer than the first's.
func TestTransientFailureEscalates(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	first := nextRetry(index.Retry{}, errors.New("524"), now)
	second := nextRetry(first, errors.New("524"), now)

	if first.Permanent || second.Permanent {
		t.Error("a plain error should not be recorded as permanent")
	}
	if second.Failures != 2 {
		t.Errorf("failures = %d, want 2", second.Failures)
	}
	if !second.NextAttempt.After(first.NextAttempt) {
		t.Errorf("second wait (%v) should exceed the first (%v)", second.NextAttempt, first.NextAttempt)
	}
}

// Once a path succeeds its history is forgotten, so a file that failed twice
// last week does not start from a long backoff the next time it changes.
func TestSuccessClearsBackoff(t *testing.T) {
	id := mustID(t)
	ev := newFakeEvents()
	bl := newRejectingBlobs(false) // transient: one minute, then eligible again
	root := t.TempDir()
	writeFile(t, root, "a.md", "one", time.Unix(1_700_000_000, 0))

	eng := newEngine(t, root, id, ev, bl)
	if err := eng.Sync(context.Background()); err == nil {
		t.Fatal("expected the first pass to fail")
	}
	if err := eng.idx.SetRetry("a.md", index.Retry{Failures: 1, NextAttempt: time.Now().Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}

	// Let the upload through and sync again.
	eng.blobs = bl.fakeBlobs
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("second pass: %v", err)
	}

	retries, err := eng.idx.Retries()
	if err != nil {
		t.Fatal(err)
	}
	if _, still := retries["a.md"]; still {
		t.Error("retry record survived a successful action")
	}
}

// Stored causes are bounded: a pass failing on a thousand paths must not grow
// the index by a megabyte of prose.
func TestTruncateBoundsStoredCause(t *testing.T) {
	long := strings.Repeat("x", 5000)
	got := truncate(long, maxRetryError)
	if len([]rune(got)) != maxRetryError+1 { // +1 for the ellipsis
		t.Errorf("truncated to %d runes, want %d plus an ellipsis", len([]rune(got)), maxRetryError)
	}
	if short := truncate("brief   cause\nhere", maxRetryError); short != "brief cause here" {
		t.Errorf("truncate collapsed whitespace to %q", short)
	}
}

// corruptingBlobs is a store that accepts an upload but keeps a truncated copy —
// exactly what blossomd did when its disk filled: the blob ends up present at its
// real address, holding no bytes.
type corruptingBlobs struct {
	*fakeBlobs
	truncateNext bool
}

func (c *corruptingBlobs) Upload(ctx context.Context, data []byte) (blob.Descriptor, error) {
	if c.truncateNext {
		c.truncateNext = false
		c.fakeBlobs.uploads++
		c.fakeBlobs.data[hashHex(data)] = nil // present, but empty
		return blob.Descriptor{SHA256: hashHex(data), Size: int64(len(data))}, nil
	}
	return c.fakeBlobs.Upload(ctx, data)
}

// A blob left truncated on the server must be re-uploaded, not trusted because it
// exists. Trusting bare existence is what turned one transient disk-full error
// into a file that could never be pulled by any device, while every subsequent
// pass reported success.
func TestCorruptBlobIsReuploadedNotTrusted(t *testing.T) {
	id := mustID(t)
	ev := newFakeEvents()
	bl := &corruptingBlobs{fakeBlobs: newFakeBlobs(), truncateNext: true}
	root := t.TempDir()
	writeFile(t, root, "note.md", "the real contents", time.Unix(1_700_000_000, 0))

	engA := newEngine(t, root, id, ev, bl)

	// First pass publishes, and the store keeps a truncated copy at the blob's
	// real address — the exact state a disk-full blossomd used to leave behind.
	if err := engA.Sync(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	evt := ev.byPath["note.md"]
	if evt == nil {
		t.Fatal("nothing published")
	}
	entry, err := nostrevent.Parse(evt)
	if err != nil {
		t.Fatal(err)
	}
	if len(bl.fakeBlobs.data[entry.BlobHash]) != 0 {
		t.Fatal("test setup: expected a truncated blob on the server")
	}

	// It is genuinely unusable: a pulling device cannot open it.
	rootB := t.TempDir()
	engB := newEngine(t, rootB, id, ev, bl)
	if err := engB.Sync(context.Background()); err == nil {
		t.Fatal("a truncated blob should fail the puller, not silently succeed")
	}

	// Any later attempt to store those same bytes must re-upload rather than skip
	// on existence. This is the check that turns the failure above from permanent
	// into self-healing.
	before := bl.fakeBlobs.uploads
	sealed, err := crypt.Seal(engA.symKey, []byte("the real contents"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := engA.uploadIfAbsent(context.Background(), sealed)
	if err != nil {
		t.Fatalf("uploadIfAbsent: %v", err)
	}
	if got != entry.BlobHash {
		t.Errorf("address = %s, want %s", got, entry.BlobHash)
	}
	if bl.fakeBlobs.uploads == before {
		t.Error("the truncated blob was trusted on existence alone and never repaired")
	}
	if len(bl.fakeBlobs.data[entry.BlobHash]) != len(sealed) {
		t.Errorf("stored %d bytes, want %d", len(bl.fakeBlobs.data[entry.BlobHash]), len(sealed))
	}

	// And now a device pulling it succeeds. A fresh one, because B is holding the
	// path in its own retry backoff after the failure above — the two mechanisms
	// composing exactly as intended.
	rootC := t.TempDir()
	engC := newEngine(t, rootC, id, ev, bl)
	if err := engC.Sync(context.Background()); err != nil {
		t.Fatalf("C sync after repair: %v", err)
	}
	if got, ok := readFile(t, rootC, "note.md"); !ok || got != "the real contents" {
		t.Errorf("C has %q (present=%v), want %q", got, ok, "the real contents")
	}
}
