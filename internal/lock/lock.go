// Package lock makes sure only one Karvan run works on a machine at a time
// (docs/sync-engine.md, 9-6). It uses the OS's file lock, which is released
// automatically if the process dies.
package lock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// ErrBusy means another Karvan run holds the lock.
var ErrBusy = errors.New("another Karvan run is in progress")

// Lock is a held lock.
type Lock struct{ f *flock.Flock }

// Acquire takes the lock in dir, waiting up to wait (0 = do not wait).
func Acquire(ctx context.Context, dir string, wait time.Duration) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "lock")
	f := flock.New(path)
	deadline := time.Now().Add(wait)
	for {
		ok, err := f.TryLock()
		if err != nil {
			return nil, err
		}
		if ok {
			_ = os.WriteFile(path+".info", []byte(fmt.Sprintf("%d %s\n", os.Getpid(), time.Now().Format(time.RFC3339))), 0o600)
			return &Lock{f}, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w%s", ErrBusy, holder(path))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func holder(path string) string {
	b, err := os.ReadFile(path + ".info")
	if err != nil {
		return ""
	}
	parts := strings.Fields(string(b))
	if len(parts) < 2 {
		return ""
	}
	if _, err := strconv.Atoi(parts[0]); err != nil {
		return ""
	}
	return fmt.Sprintf(" (process %s, started %s)", parts[0], parts[1])
}

// Release frees the lock.
func (l *Lock) Release() error { return l.f.Unlock() }
