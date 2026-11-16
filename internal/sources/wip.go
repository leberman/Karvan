package sources

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/leberman/karvan/internal/engine"
	"github.com/leberman/karvan/internal/manifest"
	"github.com/leberman/karvan/internal/wip"
)

// WIPProject is a project whose unfinished git work travels.
type WIPProject struct {
	PID  string
	ID   string
	Path string
}

// WIP publishes this machine's snapshots of unfinished project work under
// machine:<name>/wip/<pid>/ (owned: only this machine writes them, nobody
// merges them, other machines read them on `karvan restore`).
type WIP struct {
	Machine  string
	Projects []WIPProject
	Stage    string // local folder where snapshots are prepared
	// Problems collects per-project issues (e.g. a merge in progress).
	Problems []string
}

// Name implements engine.Source.
func (w *WIP) Name() string { return "unfinished work" }

func (w *WIP) prefix() string { return "machine:" + w.Machine + "/wip/" }

// Owns implements engine.Source.
func (w *WIP) Owns(logical string) bool {
	return strings.HasPrefix(logical, w.prefix()) && validRel(strings.TrimPrefix(logical, w.prefix()))
}

// Resolve implements engine.Source.
func (w *WIP) Resolve(logical string) (string, string, bool) {
	if !w.Owns(logical) {
		return "", "", false
	}
	return filepath.Join(w.Stage, filepath.FromSlash(strings.TrimPrefix(logical, w.prefix()))), manifest.KindOwned, true
}

// Scan implements engine.Source: takes fresh snapshots, then lists them.
func (w *WIP) Scan(ctx context.Context, run engine.ScanRun) ([]engine.Local, error) {
	var out []engine.Local
	for _, p := range w.Projects {
		dir := filepath.Join(w.Stage, p.PID)
		if _, err := wip.Save(ctx, p.Path, dir, w.Machine, p.ID, run.Now); err != nil {
			w.Problems = append(w.Problems, p.ID+": "+err.Error())
			// Keep the previous snapshot rather than publishing nothing.
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			out = append(out, engine.Local{Logical: w.prefix() + p.PID + "/" + e.Name(), Kind: manifest.KindOwned, Path: filepath.Join(dir, e.Name())})
		}
	}
	return out, nil
}

// Normalize implements engine.Source.
func (w *WIP) Normalize(_ string, raw []byte) ([]byte, error) { return raw, nil }

// Denormalize implements engine.Source.
func (w *WIP) Denormalize(_ string, c []byte) ([]byte, error) { return c, nil }

// WriteCheck implements engine.Source; owned data is never written locally.
func (w *WIP) WriteCheck(engine.ScanRun, string, string, bool) error { return engine.ErrDefer }
