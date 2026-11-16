// Package sources maps Claude Code's files and the user's folders to logical
// paths (docs/sync-engine.md, 4) for the engine.
package sources

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/leberman/karvan/internal/engine"
	"github.com/leberman/karvan/internal/manifest"
)

// MaxFileSize is the largest plain file synced; bigger ones are reported.
const MaxFileSize = 20 << 20

// Dir syncs one folder (or one file) as plain text/binary files.
type Dir struct {
	Label  string // shown in reports
	Prefix string // logical prefix, ending in "/" for folders
	Root   string // local folder (or file, when File is true)
	File   bool   // Root is a single file; Prefix is its logical path
	// Skip reports whether a path relative to Root (slash-separated) is
	// left out, e.g. skills/synced.
	Skip func(rel string) bool
}

// Name implements engine.Source.
func (d *Dir) Name() string { return d.Label }

// Owns implements engine.Source.
func (d *Dir) Owns(logical string) bool {
	if d.File {
		return logical == d.Prefix || strings.HasPrefix(logical, d.Prefix+".conflict-")
	}
	if !strings.HasPrefix(logical, d.Prefix) {
		return false
	}
	rel := strings.TrimPrefix(logical, d.Prefix)
	return rel != "" && (d.Skip == nil || !d.Skip(rel))
}

// Resolve implements engine.Source. The kind is decided by content, so ""
// (text or binary) is returned.
func (d *Dir) Resolve(logical string) (string, string, bool) {
	if !d.Owns(logical) {
		return "", "", false
	}
	if d.File {
		return d.Root + strings.TrimPrefix(logical, d.Prefix), "", true
	}
	rel := strings.TrimPrefix(logical, d.Prefix)
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." || part == ".." {
			return "", "", false
		}
	}
	return filepath.Join(d.Root, filepath.FromSlash(rel)), "", true
}

// Scan implements engine.Source.
func (d *Dir) Scan(_ context.Context, _ engine.ScanRun) ([]engine.Local, error) {
	var out []engine.Local
	add := func(path, logical string, info fs.FileInfo) {
		if !info.Mode().IsRegular() || strings.HasPrefix(info.Name(), ".karvan-tmp-") {
			return
		}
		if info.Size() > MaxFileSize {
			return
		}
		out = append(out, engine.Local{Logical: logical, Kind: KindOf(path), Path: path})
	}
	if d.File {
		dir, base := filepath.Split(d.Root)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, err
		}
		for _, e := range entries {
			n := e.Name()
			if n != base && !strings.HasPrefix(n, base+".conflict-") {
				continue
			}
			info, err := e.Info() // Lstat: symlinks are not followed
			if err != nil {
				continue
			}
			add(filepath.Join(dir, n), d.Prefix+strings.TrimPrefix(n, base), info)
		}
		return out, nil
	}
	err := filepath.WalkDir(d.Root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if p == d.Root {
			return nil
		}
		rel, _ := filepath.Rel(d.Root, p)
		rel = filepath.ToSlash(rel)
		if d.Skip != nil && d.Skip(rel) {
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if e.IsDir() || e.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return nil
		}
		add(p, d.Prefix+rel, info)
		return nil
	})
	return out, err
}

// KindOf sniffs a file: text if its start is valid UTF-8 without NUL bytes.
func KindOf(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return manifest.KindText
	}
	defer f.Close()
	buf := make([]byte, 8192)
	n, _ := f.Read(buf)
	return kindOfBytes(buf[:n])
}

func kindOfBytes(b []byte) string {
	if bytes.IndexByte(b, 0) >= 0 {
		return manifest.KindBinary
	}
	// Allow a multi-byte rune cut at the buffer's end.
	for i := 0; i < 4 && len(b) > 0 && !utf8.Valid(b); i++ {
		b = b[:len(b)-1]
	}
	if !utf8.Valid(b) {
		return manifest.KindBinary
	}
	return manifest.KindText
}

// Normalize implements engine.Source.
func (d *Dir) Normalize(_ string, raw []byte) ([]byte, error) { return raw, nil }

// Denormalize implements engine.Source.
func (d *Dir) Denormalize(_ string, content []byte) ([]byte, error) { return content, nil }

// WriteCheck implements engine.Source.
func (d *Dir) WriteCheck(engine.ScanRun, string, string, bool) error { return nil }
