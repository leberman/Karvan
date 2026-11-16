// Package store defines the storage interface (docs/sync-engine.md, 13): a
// blob store with list, get, put and one conditional commit. Karvan's sync
// logic never depends on what is behind it.
package store

import (
	"context"
	"errors"
)

// ErrNotFound is returned by Get for a missing path.
var ErrNotFound = errors.New("not found in the store")

// ErrConflict means the store changed since the snapshot was taken: read a
// fresh snapshot, decide again and retry.
var ErrConflict = errors.New("the store changed meanwhile")

// ErrRewound means the store went back in time (force-pushed, restored from a
// backup). Karvan stops instead of repairing it.
var ErrRewound = errors.New("the store went back in time (rewritten or restored from a backup); stopping to avoid data loss")

// Snapshot is a consistent read-only view of the store.
type Snapshot interface {
	List(prefix string) ([]string, error)
	Get(path string) ([]byte, error)
}

// Store is where machines exchange data.
type Store interface {
	// Snapshot fetches the current state.
	Snapshot(ctx context.Context) (Snapshot, error)
	// Commit writes puts (path -> bytes) and deletes on top of base. Blobs
	// are written before manifests. ErrConflict if base is stale.
	Commit(ctx context.Context, base Snapshot, puts map[string][]byte, deletes []string, message string) error
	// Describe is a short human-readable location.
	Describe() string
}
