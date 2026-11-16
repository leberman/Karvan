// Package engine runs the sync cycle of docs/sync-engine.md section 7 over a
// store and a set of sources.
package engine

import (
	"context"
	"errors"
	"time"
)

// Local is one file a source tracks on this machine.
type Local struct {
	Logical string // logical path (docs/sync-engine.md, 4)
	Kind    string // manifest kind
	Path    string // absolute local path
	// OutOfScope means the file exists but should not be sent now (e.g. an
	// old transcript). A previously tracked entry becomes "retired".
	OutOfScope bool
	// Skip means: do not read it this run (e.g. written in the last minutes
	// and this is a timer run). Its entry stays as it is.
	Skip bool
}

// ScanRun describes the current run to sources.
type ScanRun struct {
	Now       time.Time
	Timer     bool   // started by the timer (not a hook, not by hand)
	SessionID string // the Claude Code session a SessionStart hook is for
}

// ErrDefer is wrapped by WriteCheck reasons: do not write now, try next run.
var ErrDefer = errors.New("deferred")

// Source maps one kind of local data to logical paths.
type Source interface {
	// Name is shown in reports ("CLAUDE.md", "transcripts"...).
	Name() string
	// Owns reports whether a logical path belongs to this source.
	Owns(logical string) bool
	// Scan lists the files this source tracks on this machine.
	Scan(ctx context.Context, run ScanRun) ([]Local, error)
	// Resolve maps a logical path to this machine's file. ok is false if
	// this machine cannot place it (e.g. the project is not set up here).
	Resolve(logical string) (path, kind string, ok bool)
	// Normalize turns file bytes into the stored form (placeholders, no
	// unfinished last line...). Denormalize is the reverse.
	Normalize(logical string, raw []byte) ([]byte, error)
	Denormalize(logical string, content []byte) ([]byte, error)
	// WriteCheck returns an error wrapping ErrDefer when the file must not
	// be written now (open session, app running...).
	WriteCheck(run ScanRun, logical, path string, exists bool) error
}
