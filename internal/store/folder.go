package store

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/leberman/karvan/internal/fsutil"
)

// Folder is a store in a local directory, which may be synced by a cloud
// service (Dropbox, Google Drive, Syncthing...). Only this machine writes its
// own manifest, so plain atomic writes are enough (docs/sync-engine.md, 13-3).
type Folder struct{ Root string }

// Describe implements Store.
func (f *Folder) Describe() string { return "folder " + f.Root }

// Snapshot implements Store; a folder snapshot reads the live directory.
func (f *Folder) Snapshot(context.Context) (Snapshot, error) {
	if st, err := os.Stat(f.Root); err != nil {
		return nil, err
	} else if !st.IsDir() {
		return nil, errors.New(f.Root + " is not a folder")
	}
	return folderSnap{f.Root}, nil
}

type folderSnap struct{ root string }

func (s folderSnap) List(prefix string) ([]string, error) {
	var out []string
	start := filepath.Join(s.root, filepath.FromSlash(dirOf(prefix)))
	err := filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		rel, err := filepath.Rel(s.root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, prefix) {
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

func dirOf(prefix string) string {
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		return prefix[:i]
	}
	return ""
}

func (s folderSnap) Get(p string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(s.root, filepath.FromSlash(p)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return b, err
}

// Commit implements Store.
func (f *Folder) Commit(_ context.Context, _ Snapshot, puts map[string][]byte, deletes []string, _ string) error {
	for _, p := range orderPuts(puts) {
		full := filepath.Join(f.Root, filepath.FromSlash(p))
		if strings.HasPrefix(p, "blobs/") {
			if _, err := os.Stat(full); err == nil {
				continue // content-addressed: already there
			}
		}
		if err := fsutil.WriteAtomic(full, puts[p], 0o600); err != nil {
			return err
		}
	}
	for _, p := range deletes {
		if err := os.Remove(filepath.Join(f.Root, filepath.FromSlash(p))); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// orderPuts puts blobs first, manifests last, so a manifest never points at
// a blob that is not written yet.
func orderPuts(puts map[string][]byte) []string {
	ps := make([]string, 0, len(puts))
	for p := range puts {
		ps = append(ps, p)
	}
	rank := func(p string) int {
		switch {
		case strings.HasPrefix(p, "blobs/"):
			return 0
		case strings.HasPrefix(p, "manifests/"):
			return 2
		}
		return 1
	}
	sort.Slice(ps, func(i, j int) bool {
		if rank(ps[i]) != rank(ps[j]) {
			return rank(ps[i]) < rank(ps[j])
		}
		return ps[i] < ps[j]
	})
	return ps
}
