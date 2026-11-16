package engine

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/leberman/karvan/internal/fsutil"
	"github.com/leberman/karvan/internal/manifest"
)

// State is this machine's local state (docs/sync-engine.md, 6).
type State struct {
	MachineID    string               `json:"machineId"`
	PublishedSeq int64                `json:"publishedSeq"`
	Manifest     *manifest.Manifest   `json:"manifest,omitempty"`
	LastGC       time.Time            `json:"lastGC,omitzero"`
	GC           map[string]time.Time `json:"gcCandidates,omitempty"`
}

type scanEntry struct {
	Size     int64     `json:"size"`
	Mtime    time.Time `json:"mtime"`
	Hash     string    `json:"hash"`
	Chunks   []string  `json:"chunks"`
	NormSize int64     `json:"normSize"`
	Lines    int       `json:"lines,omitempty"`
	Activity int64     `json:"activity,omitempty"`
}

func readJSON(p string, v any) error {
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(p string, v any) error {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(p, b, 0o600)
}

// LoadState reads the local state (an empty State if there is none).
func LoadState(dir string) (*State, error) {
	var s State
	if err := readJSON(filepath.Join(dir, "state.json"), &s); err != nil {
		return nil, err
	}
	if s.GC == nil {
		s.GC = map[string]time.Time{}
	}
	return &s, nil
}

// SaveState writes the local state.
func SaveState(dir string, s *State) error { return writeJSON(filepath.Join(dir, "state.json"), s) }
