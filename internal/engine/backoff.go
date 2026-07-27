package engine

import (
	"errors"
	"strings"
	"time"

	"ca.punkscience.tendrils/internal/blob"
	"ca.punkscience.tendrils/internal/index"
)

// Retry backoff. A pass acts on every path the reconciler says needs work, which
// is right until an action fails for a reason the next pass will hit again: a
// file the server refuses for its size is retried every interval forever, and
// each attempt costs a planned slot, a log line, and — for a transient failure —
// a real upload that gets cut off. Backoff holds a failing path back so the rest
// of the tree keeps moving.
//
// Nothing here is ever abandoned. The longest wait is retryPermanent, so raising
// a server's body limit or shrinking the file is picked up on its own without the
// owner clearing state by hand.
const (
	retryBase      = time.Minute    // first wait after a single failure
	retryMax       = time.Hour      // ceiling for the transient schedule
	retryPermanent = 24 * time.Hour // for failures repeating cannot fix
	maxRetryError  = 300            // chars of cause kept in the record
)

// retryDelay is how long to hold a path back after failures consecutive
// failures: exponential from retryBase, capped at retryMax, or the flat
// retryPermanent when the cause is one retrying cannot resolve.
func retryDelay(failures int, permanent bool) time.Duration {
	if permanent {
		return retryPermanent
	}
	if failures < 1 {
		failures = 1
	}
	d := retryBase
	for i := 1; i < failures && d < retryMax; i++ {
		d *= 2
	}
	if d > retryMax {
		return retryMax
	}
	return d
}

// permanentFailure reports whether repeating an action cannot plausibly fix it.
// Deliberately narrow: only a server refusing the blob for its size qualifies.
// Timeouts, 5xx, and resets are all transient and get the exponential schedule —
// misjudging one of those as permanent would stall a file for a day over a blip.
func permanentFailure(err error) bool {
	return errors.Is(err, blob.ErrTooLarge)
}

// nextRetry builds the record to store after an action failed, given whatever
// was recorded before (the zero Retry for a path failing for the first time).
func nextRetry(prev index.Retry, cause error, now time.Time) index.Retry {
	r := index.Retry{
		Failures:  prev.Failures + 1,
		LastError: truncate(cause.Error(), maxRetryError),
		Permanent: permanentFailure(cause),
	}
	r.NextAttempt = now.Add(retryDelay(r.Failures, r.Permanent))
	return r
}

// truncate bounds a stored error string. The index is not a log: a pass that
// fails on a thousand paths must not grow the database by a megabyte of prose.
func truncate(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
