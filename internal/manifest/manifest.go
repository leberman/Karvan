// Package manifest defines the per-machine manifest (docs/sync-engine.md, 5)
// and the version-history rules that decide which version of a file is newer.
package manifest

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/leberman/karvan/internal/blob"
)

// Format is the store/manifest format this build reads and writes.
const Format = 1

// Kinds of logical paths (docs/sync-engine.md, 4).
const (
	KindText       = "text"
	KindBinary     = "binary"
	KindTranscript = "transcript"
	KindDesktop    = "desktop"
	KindImmutable  = "immutable"
	KindOwned      = "owned"
)

// KnownKind reports whether this build knows how to handle k.
func KnownKind(k string) bool {
	switch k {
	case KindText, KindBinary, KindTranscript, KindDesktop, KindImmutable, KindOwned:
		return true
	}
	return false
}

// HistoryLimit is how many ancestor hashes an entry of kind k keeps.
func HistoryLimit(k string) int {
	switch k {
	case KindTranscript, KindImmutable:
		return 8
	}
	return 64
}

// BaseChunksLimit is how many past versions keep their chunks listed (so the
// blobs survive garbage collection and can serve as a three-way merge base).
const BaseChunksLimit = 8

// Manifest is one machine's view of every logical path.
type Manifest struct {
	Format    int                `json:"format"`
	Machine   string             `json:"machine"`
	MachineID string             `json:"machineId"`
	Karvan    string             `json:"karvan"`
	Seq       int64              `json:"seq"`
	WrittenAt time.Time          `json:"writtenAt"`
	Seen      map[string]int64   `json:"seen,omitempty"`
	Projects  map[string]Project `json:"projects,omitempty"`
	Entries   map[string]*Entry  `json:"entries"`
}

// Project describes a project ID (never its local path).
type Project struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// Entry is the state of one logical path on one machine.
type Entry struct {
	Kind       string              `json:"kind"`
	Hash       string              `json:"hash,omitempty"`
	Size       int64               `json:"size,omitempty"`
	Mtime      time.Time           `json:"mtime,omitzero"`
	Chunks     []string            `json:"chunks,omitempty"`
	History    []string            `json:"history,omitempty"`
	BaseChunks map[string][]string `json:"historyChunks,omitempty"`
	Deleted    bool                `json:"deleted,omitempty"`
	Retired    bool                `json:"retired,omitempty"`
	Held       bool                `json:"held,omitempty"`
	ChangedAt  time.Time           `json:"changedAt,omitzero"`
	Lines      int                 `json:"lines,omitempty"`
	Schema     string              `json:"schema,omitempty"`
	Activity   int64               `json:"activity,omitempty"` // desktop: lastActivityAt
}

// Active reports whether other machines should consider this entry.
func (e *Entry) Active() bool {
	return e != nil && !e.Retired && !e.Held && (e.Deleted || e.Hash != "")
}

// Clone returns a deep copy.
func (e *Entry) Clone() *Entry {
	c := *e
	c.Chunks = append([]string(nil), e.Chunks...)
	c.History = append([]string(nil), e.History...)
	if e.BaseChunks != nil {
		c.BaseChunks = make(map[string][]string, len(e.BaseChunks))
		for k, v := range e.BaseChunks {
			c.BaseChunks[k] = append([]string(nil), v...)
		}
	}
	return &c
}

// Last is the version a tombstone deleted (or "" if unknown).
func (e *Entry) Last() string {
	if len(e.History) == 0 {
		return ""
	}
	return e.History[0]
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// Dominates reports whether x was derived from y, so y can be dropped.
// For a tombstone y, x dominates it if x was derived from the deleted version
// (a change after the delete wins, PRD 6-2).
func Dominates(x, y *Entry) bool {
	if x.Deleted && y.Deleted {
		return contains(x.History, y.Last()) && x.Last() != y.Last()
	}
	if y.Deleted {
		return y.Last() != "" && contains(x.History, y.Last())
	}
	return contains(x.History, y.Hash)
}

// Same reports whether two entries describe the same version.
func Same(x, y *Entry) bool {
	if x.Deleted || y.Deleted {
		return x.Deleted && y.Deleted && x.Last() == y.Last()
	}
	return x.Hash == y.Hash
}

// MergeBase is the newest version both entries descend from, or "".
func MergeBase(x, y *Entry) string {
	ys := map[string]bool{y.Hash: true}
	for _, h := range y.History {
		ys[h] = true
	}
	for _, h := range append([]string{x.Hash}, x.History...) {
		if h != "" && ys[h] {
			return h
		}
	}
	return ""
}

// Advance records that the content changed from e's version to newHash.
func (e *Entry) Advance(newHash string) {
	if e.Hash == newHash && !e.Deleted {
		return
	}
	if e.Hash != "" {
		e.rememberBase(e.Hash, e.Chunks)
		e.History = trim(append([]string{e.Hash}, e.History...), HistoryLimit(e.Kind))
	}
	e.Hash = newHash
	e.Deleted = false
	e.Retired = false
}

// Tombstone turns e into a deletion marker.
func (e *Entry) Tombstone(now time.Time) {
	if e.Hash != "" {
		e.rememberBase(e.Hash, e.Chunks)
		e.History = trim(append([]string{e.Hash}, e.History...), HistoryLimit(e.Kind))
	}
	e.Hash, e.Chunks, e.Size, e.Lines = "", nil, 0, 0
	e.Deleted = true
	e.ChangedAt = now
}

// MergedHistory is the history of a version made by merging a and b.
func MergedHistory(kind string, a, b *Entry) []string {
	out := []string{}
	seen := map[string]bool{}
	add := func(h string) {
		if h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	add(a.Hash)
	add(b.Hash)
	add(a.Last())
	add(b.Last())
	for i := 0; i < max(len(a.History), len(b.History)); i++ {
		if i < len(a.History) {
			add(a.History[i])
		}
		if i < len(b.History) {
			add(b.History[i])
		}
	}
	return trim(out, HistoryLimit(kind))
}

func trim(h []string, n int) []string {
	if len(h) > n {
		return h[:n]
	}
	return h
}

// rememberBase keeps chunk lists of recent text versions (merge bases).
func (e *Entry) rememberBase(h string, chunks []string) {
	if e.Kind != KindText && e.Kind != KindBinary || len(chunks) == 0 {
		return
	}
	if e.BaseChunks == nil {
		e.BaseChunks = map[string][]string{}
	}
	e.BaseChunks[h] = append([]string(nil), chunks...)
	e.PruneBases()
}

// PruneBases keeps base chunks only for the newest BaseChunksLimit history
// entries.
func (e *Entry) PruneBases() {
	keep := map[string]bool{}
	for i, h := range e.History {
		if i >= BaseChunksLimit {
			break
		}
		keep[h] = true
	}
	if e.Hash != "" {
		keep[e.Hash] = true // the version being replaced right now
	}
	for h := range e.BaseChunks {
		if !keep[h] {
			delete(e.BaseChunks, h)
		}
	}
	if len(e.BaseChunks) == 0 {
		e.BaseChunks = nil
	}
}

// Encode serializes and seals m.
func Encode(m *Manifest) ([]byte, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return blob.Seal(data), nil
}

// Decode opens and parses a sealed manifest.
func Decode(sealed []byte) (*Manifest, error) {
	data, err := blob.Open(sealed)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%w: %w", blob.ErrCorrupt, err)
	}
	if m.Format > Format {
		return nil, fmt.Errorf("manifest of %s has format %d, newer than this Karvan (%d)", m.Machine, m.Format, Format)
	}
	if m.Entries == nil {
		m.Entries = map[string]*Entry{}
	}
	return &m, nil
}

// Referenced returns every chunk hash the manifest needs kept.
func (m *Manifest) Referenced() map[string]bool {
	out := map[string]bool{}
	for _, e := range m.Entries {
		for _, c := range e.Chunks {
			out[c] = true
		}
		for _, cs := range e.BaseChunks {
			for _, c := range cs {
				out[c] = true
			}
		}
	}
	return out
}

// Paths returns the entry paths sorted.
func (m *Manifest) Paths() []string {
	ps := make([]string, 0, len(m.Entries))
	for p := range m.Entries {
		ps = append(ps, p)
	}
	sort.Strings(ps)
	return ps
}

// Path is where machine's manifest lives in the store.
func Path(machine string) string { return "manifests/" + machine + ".krv" }
