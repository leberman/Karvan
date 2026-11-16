// Package fsutil reads files that other programs may be writing, and writes
// files atomically (docs/sync-engine.md, 9-1 and 9-2).
package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Stat is what identifies one state of a file on disk.
type Stat struct {
	Size  int64
	Mtime time.Time
}

// StatOf returns the Stat of p; ok is false if p does not exist.
func StatOf(p string) (Stat, bool, error) {
	st, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return Stat{}, false, nil
	}
	if err != nil {
		return Stat{}, false, err
	}
	return Stat{st.Size(), st.ModTime()}, true, nil
}

// ErrBusy means a file kept changing while it was read.
var ErrBusy = errors.New("file is being written; will try again next run")

// ReadStable reads p and checks that size and mtime did not change during the
// read, retrying a few times.
func ReadStable(p string) ([]byte, Stat, error) {
	for attempt := 0; attempt < 3; attempt++ {
		before, ok, err := StatOf(p)
		if err != nil {
			return nil, Stat{}, err
		}
		if !ok {
			return nil, Stat{}, fs.ErrNotExist
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, Stat{}, err
		}
		after, _, err := StatOf(p)
		if err != nil {
			return nil, Stat{}, err
		}
		if before == after && int64(len(data)) == after.Size {
			return data, after, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil, Stat{}, ErrBusy
}

const tmpPrefix = ".karvan-tmp-"

// WriteAtomic writes data to p through a temporary file in the same folder
// and a rename, creating parent folders.
func WriteAtomic(p string, data []byte, mode fs.FileMode) error {
	return WriteAtomicIf(p, data, mode, time.Time{}, nil)
}

// ErrChanged means the target changed after it was read; nothing was written.
var ErrChanged = errors.New("file changed while syncing; will try again next run")

// WriteAtomicIf is WriteAtomic that also sets the mtime (if not zero) and,
// when expect is not nil, aborts with ErrChanged unless the target still has
// that Stat (or, for a zero Stat, still does not exist) right before the
// rename.
func WriteAtomicIf(p string, data []byte, mode fs.FileMode, mtime time.Time, expect *Stat) error {
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if st, err := os.Stat(p); err == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(dir, tmpPrefix+"*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(name, mtime, mtime); err != nil {
			return err
		}
	}
	if expect != nil {
		cur, ok, err := StatOf(p)
		if err != nil {
			return err
		}
		if ok != (*expect != Stat{}) || ok && !sameStat(cur, *expect) {
			return ErrChanged
		}
	}
	if err := os.Rename(name, p); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

func sameStat(a, b Stat) bool { return a.Size == b.Size && a.Mtime.Equal(b.Mtime) }

// AppendIf appends data to p if p still has the Stat expect, then sets mtime.
func AppendIf(p string, data []byte, mtime time.Time, expect Stat) error {
	cur, ok, err := StatOf(p)
	if err != nil {
		return err
	}
	if !ok || !sameStat(cur, expect) {
		return ErrChanged
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if !mtime.IsZero() {
		return os.Chtimes(p, mtime, mtime)
	}
	return nil
}

// CleanTemp removes temporary files left in dir by an interrupted write.
func CleanTemp(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
