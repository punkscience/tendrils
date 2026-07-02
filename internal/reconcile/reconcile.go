// Package reconcile holds the pure decision at the heart of Tendrils: given
// what a path looked like at the last sync (base), what it looks like on disk
// now (local), and what the relay says is the current truth (remote), decide
// the single action that moves this device toward convergence.
//
// It performs no I/O. The engine feeds it three entries and executes the
// returned Decision. Keeping the conflict rules here — last-writer-wins by
// mtime, delete-is-absolute, re-creation-is-honoured — makes them exhaustively
// unit-testable against the Gherkin, one scenario at a time.
package reconcile

import "ca.punkscience.tendrils/internal/tree"

// Op is the primary action for a path.
type Op int

const (
	// OpNone: already converged, nothing to do.
	OpNone Op = iota
	// OpWriteRemote: fetch the remote blob and write it locally (atomically).
	OpWriteRemote
	// OpDeleteLocal: move the local file to the trash; the remote tombstone wins.
	OpDeleteLocal
	// OpPublishLocal: upload the local blob and publish a live event; local is
	// the authoritative newest version.
	OpPublishLocal
	// OpPublishDelete: publish a tombstone; the file was deleted locally.
	OpPublishDelete
)

func (o Op) String() string {
	switch o {
	case OpWriteRemote:
		return "write-remote"
	case OpDeleteLocal:
		return "delete-local"
	case OpPublishLocal:
		return "publish-local"
	case OpPublishDelete:
		return "publish-delete"
	default:
		return "none"
	}
}

// Decision is the reconciler's verdict for one path.
type Decision struct {
	Op Op
	// ConflictCopy, when true, means: before OpWriteRemote overwrites the local
	// file, preserve the current local file as a conflict copy. It is set only
	// when the local file held changes that would otherwise be lost — a wrong
	// guess costs a rename, never data.
	ConflictCopy bool
	// Reason is a short human-readable explanation for the logs — legibility is
	// a design constraint of this project.
	Reason string
}

// Decide computes the action for one path. Any of base/local/remote may be nil:
//   - base:   the entry recorded at the last successful sync (nil = never synced).
//   - local:  the current on-disk state (nil = not present on disk; never a tombstone).
//   - remote: the latest event truth on the relay (nil = the set has no such path).
func Decide(base, local, remote *tree.Entry) Decision {
	switch {
	case remote == nil:
		return decideNoRemote(base, local)
	case remote.Tomb():
		return decideRemoteTomb(base, local, remote)
	default: // remote is a live file
		return decideRemoteLive(base, local, remote)
	}
}

// decideNoRemote: the set has never heard of this path. Whatever we have
// locally is ours to contribute (or nothing to do).
func decideNoRemote(base, local *tree.Entry) Decision {
	if local.Live() {
		return Decision{Op: OpPublishLocal, Reason: "local-only file, publish to set"}
	}
	return Decision{Op: OpNone, Reason: "nothing here or on the set"}
}

// decideRemoteLive: the set's truth is a present file.
func decideRemoteLive(base, local, remote *tree.Entry) Decision {
	// Local file absent. Either we never had it (bootstrap/offline) or we
	// deleted it locally. Only a recorded live base tells us it was a local
	// delete that must now propagate as a tombstone.
	if !local.Live() {
		if base.Live() {
			return Decision{Op: OpPublishDelete, Reason: "file deleted locally, publish tombstone"}
		}
		return Decision{Op: OpWriteRemote, Reason: "missing locally, pull from set"}
	}

	// Both present with identical content: converged.
	if tree.SameContent(local, remote) {
		return Decision{Op: OpNone, Reason: "content already matches"}
	}

	// Both present, different content: last-writer-wins by mtime.
	if localWins(local, remote) {
		return Decision{Op: OpPublishLocal, Reason: "local is newer, publish"}
	}
	// Remote wins. Preserve local only if it diverged from base (an unpublished
	// local change); a merely-stale copy is overwritten cleanly.
	return Decision{
		Op:           OpWriteRemote,
		ConflictCopy: locallyChanged(base, local),
		Reason:       "remote is newer, pull",
	}
}

// decideRemoteTomb: the set's truth is a deletion.
func decideRemoteTomb(base, local, remote *tree.Entry) Decision {
	// Re-creation: this device already applied the delete (base is a tombstone),
	// and a brand-new file now sits at that path. A deliberate re-creation is
	// honoured over the old delete.
	if base.Tomb() && local.Live() {
		return Decision{Op: OpPublishLocal, Reason: "re-created after delete, publish"}
	}

	// Delete is absolute: it wins even over a newer concurrent local edit. Moving
	// the local file to the trash both removes it and keeps it recoverable.
	if local.Live() {
		return Decision{Op: OpDeleteLocal, Reason: "delete wins, move local to trash"}
	}

	// Already absent locally.
	return Decision{Op: OpNone, Reason: "already deleted here"}
}

// localWins decides a same-path content divergence by mtime, breaking exact
// ties deterministically by content hash so both devices pick the same winner.
func localWins(local, remote *tree.Entry) bool {
	if local.ModTime.Equal(remote.ModTime) {
		return local.Sha256 > remote.Sha256
	}
	return local.ModTime.After(remote.ModTime)
}

// locallyChanged reports whether the local file differs from the last-synced
// base — i.e. it carries an unpublished edit worth preserving.
func locallyChanged(base, local *tree.Entry) bool {
	if base == nil {
		return true // no record of ever syncing this file; treat as divergent
	}
	return !tree.SameContent(base, local)
}
