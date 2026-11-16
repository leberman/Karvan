package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/leberman/karvan/internal/blob"
	"github.com/leberman/karvan/internal/fsutil"
	"github.com/leberman/karvan/internal/manifest"
	"github.com/leberman/karvan/internal/merge"
	"github.com/leberman/karvan/internal/store"
)

// Engine syncs one machine.
type Engine struct {
	Store    store.Store
	Sources  []Source
	Machine  string
	Version  string // Karvan version, recorded in the manifest
	StateDir string
	Projects map[string]manifest.Project
	Now      func() time.Time
	// MaxAttempts bounds retries after a conflicting commit (default 5).
	MaxAttempts int
	// Backoff between attempts (default 200ms * 2^n).
	Backoff func(attempt int) time.Duration
}

// Errors the CLI turns into plain messages.
var (
	ErrNotInitialized = errors.New("the store has no FORMAT file: it is not set up yet (run `karvan init`)")
	ErrIdentity       = errors.New("another machine is writing under this machine's name")
	ErrFormat         = errors.New("the store was made by a newer Karvan; please update Karvan")
)

// Format is the store's FORMAT file.
type Format struct {
	Format     int       `json:"format"`
	MinReader  int       `json:"minReader"`
	Created    time.Time `json:"created"`
	CreatedBy  string    `json:"createdBy"`
	Hash       string    `json:"hash"`
	Chunking   string    `json:"chunking"`
	Encryption string    `json:"encryption"`
}

// InitStore writes FORMAT if the store does not have one yet.
func InitStore(ctx context.Context, st store.Store, version string, now time.Time) (created bool, err error) {
	for attempt := 0; attempt < 5; attempt++ {
		snap, err := st.Snapshot(ctx)
		if err != nil {
			return false, err
		}
		if _, err := snap.Get("FORMAT"); err == nil {
			return false, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return false, err
		}
		f := Format{Format: manifest.Format, MinReader: manifest.Format, Created: now.UTC(), CreatedBy: "karvan " + version,
			Hash: "sha256", Chunking: "lines-v1", Encryption: "none"}
		b, _ := json.MarshalIndent(f, "", "  ")
		err = st.Commit(ctx, snap, map[string][]byte{"FORMAT": append(b, '\n')}, nil, "karvan: create store")
		if errors.Is(err, store.ErrConflict) {
			continue
		}
		return err == nil, err
	}
	return false, store.ErrConflict
}

func checkFormat(snap store.Snapshot) error {
	b, err := snap.Get("FORMAT")
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotInitialized
	}
	if err != nil {
		return err
	}
	var f Format
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("FORMAT: %w", err)
	}
	if f.MinReader > manifest.Format {
		return fmt.Errorf("%w (store format %d, this Karvan reads %d)", ErrFormat, f.Format, manifest.Format)
	}
	return nil
}

// Options for one run.
type Options struct {
	Timer     bool
	SessionID string
}

// Deferred is a path that was not written this run.
type Deferred struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Conflict is a text file both machines changed in the same place.
type Conflict struct {
	Path string `json:"path"` // the logical path
	Copy string `json:"copy"` // where the other version was kept
}

// Report says what a run did.
type Report struct {
	Started   time.Time          `json:"started"`
	Finished  time.Time          `json:"finished"`
	Sent      int                `json:"sent"`     // local changes recorded
	Received  int                `json:"received"` // files written from other machines
	Merged    int                `json:"merged"`
	Deleted   int                `json:"deleted"`
	Uploaded  int                `json:"uploaded"` // blobs
	Conflicts []Conflict         `json:"conflicts,omitempty"`
	Deferred  []Deferred         `json:"deferred,omitempty"`
	Warnings  []string           `json:"warnings,omitempty"`
	Unplaced  int                `json:"unplaced,omitempty"` // paths this machine cannot place (project not set up)
	Machines  map[string]Machine `json:"machines,omitempty"`
	Attempts  int                `json:"attempts"`
	Published bool               `json:"published"`
}

// Machine is what this run saw of another machine.
type Machine struct {
	Seq       int64     `json:"seq"`
	WrittenAt time.Time `json:"writtenAt"`
	Karvan    string    `json:"karvan"`
	SawMe     int64     `json:"sawMe"` // my seq that machine last incorporated
}

type run struct {
	e       *Engine
	opts    Options
	scanRun ScanRun
	now     time.Time
	state   *State
	cur     *manifest.Manifest
	report  *Report
	chunks  map[string][]byte // chunk hash -> content known locally
	scanned map[string]fsutil.Stat
	cache   map[string]scanEntry
	others  map[string]*manifest.Manifest
	snap    store.Snapshot
	content map[string][]byte // version hash -> content
	src     map[string]Source // logical -> owning source (memo)
}

// Run executes one sync cycle.
func (e *Engine) Run(ctx context.Context, opts Options) (*Report, error) {
	now := time.Now
	if e.Now != nil {
		now = e.Now
	}
	r := &run{e: e, opts: opts, now: now(), report: &Report{}, chunks: map[string][]byte{},
		scanned: map[string]fsutil.Stat{}, content: map[string][]byte{}, src: map[string]Source{}}
	r.report.Started = r.now
	r.scanRun = ScanRun{Now: r.now, Timer: opts.Timer, SessionID: opts.SessionID}
	err := r.do(ctx)
	r.report.Finished = now()
	return r.report, err
}

func (r *run) do(ctx context.Context) error {
	var err error
	if err := os.MkdirAll(r.e.StateDir, 0o700); err != nil {
		return err
	}
	if r.state, err = LoadState(r.e.StateDir); err != nil {
		return fmt.Errorf("local state: %w", err)
	}
	if r.state.MachineID == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		r.state.MachineID = hex.EncodeToString(b)
	}
	r.cache = map[string]scanEntry{}
	_ = readJSON(filepath.Join(r.e.StateDir, "scan-cache.json"), &r.cache)

	r.cur = &manifest.Manifest{Entries: map[string]*manifest.Entry{}}
	if r.state.Manifest != nil {
		for p, en := range r.state.Manifest.Entries {
			r.cur.Entries[p] = en.Clone()
		}
	}
	// The cache also records files this run writes, so save it at the end.
	defer func() { _ = writeJSON(filepath.Join(r.e.StateDir, "scan-cache.json"), r.cache) }()
	if err := r.scan(ctx); err != nil {
		return err
	}

	maxAttempts := r.e.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = 5
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		r.report.Attempts = attempt
		err := r.exchange(ctx)
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
		d := time.Duration(200<<attempt) * time.Millisecond
		if r.e.Backoff != nil {
			d = r.e.Backoff(attempt)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
	}
	return fmt.Errorf("could not publish after %d attempts (other machines kept writing); will try again next run: %w", maxAttempts, store.ErrConflict)
}

// ---- step 2: scan ----

func (r *run) sourceFor(logical string) Source {
	if s, ok := r.src[logical]; ok {
		return s
	}
	for _, s := range r.e.Sources {
		if s.Owns(logical) {
			r.src[logical] = s
			return s
		}
	}
	r.src[logical] = nil
	return nil
}

// readLocal reads, normalizes and hashes a local file (using the scan cache).
func (r *run) readLocal(src Source, logical, path string) (scanEntry, error) {
	st, ok, err := fsutil.StatOf(path)
	if err != nil {
		return scanEntry{}, err
	}
	if !ok {
		return scanEntry{}, fs.ErrNotExist
	}
	if c, hit := r.cache[path]; hit && c.Size == st.Size && c.Mtime.Equal(st.Mtime) {
		r.scanned[logical] = st
		return c, nil
	}
	raw, st, err := fsutil.ReadStable(path)
	if err != nil {
		return scanEntry{}, err
	}
	content, err := src.Normalize(logical, raw)
	if err != nil {
		return scanEntry{}, err
	}
	hs, parts := blob.Split(content)
	for h, p := range parts {
		r.chunks[h] = p
	}
	se := scanEntry{Size: st.Size, Mtime: st.Mtime, Hash: blob.Hash(content), Chunks: hs, NormSize: int64(len(content))}
	if strings.HasSuffix(path, ".jsonl") {
		se.Lines = bytes.Count(content, []byte{'\n'})
	}
	if a, err := merge.DesktopActivity(content); err == nil {
		se.Activity = a
	}
	r.cache[path] = se
	r.scanned[logical] = st
	r.content[se.Hash] = content
	return se, nil
}

func (r *run) scan(ctx context.Context) error {
	seen := map[string]bool{}
	scannedSources := map[Source]bool{}
	for _, src := range r.e.Sources {
		locals, err := src.Scan(ctx, r.scanRun)
		if err != nil {
			r.warn("%s: %v", src.Name(), err)
			continue
		}
		scannedSources[src] = true
		for _, l := range locals {
			r.src[l.Logical] = src
			seen[l.Logical] = true
			en := r.cur.Entries[l.Logical]
			if l.Skip {
				continue
			}
			if l.OutOfScope {
				if en != nil && !en.Deleted && !en.Retired {
					en.Retired = true
					en.ChangedAt = r.now
				}
				continue
			}
			se, err := r.readLocal(src, l.Logical, l.Path)
			if err != nil {
				r.postpone(l.Logical, err.Error())
				continue
			}
			if en == nil {
				en = &manifest.Entry{Kind: l.Kind}
				r.cur.Entries[l.Logical] = en
			}
			if en.Hash != se.Hash || en.Deleted || en.Retired {
				en.Advance(se.Hash)
				en.Chunks, en.Size, en.Mtime, en.ChangedAt = se.Chunks, se.NormSize, se.Mtime.UTC(), r.now
				en.Lines, en.Activity = se.Lines, se.Activity
				r.report.Sent++
			}
		}
	}
	// Tracked entries that were not listed: deleted or out of scope.
	for _, p := range r.cur.Paths() {
		en := r.cur.Entries[p]
		if seen[p] || en.Deleted || en.Retired || en.Kind == manifest.KindOwned {
			continue
		}
		src := r.sourceFor(p)
		if src == nil || !scannedSources[src] {
			if src == nil {
				en.Retired, en.ChangedAt = true, r.now // its source was turned off
			}
			continue
		}
		path, _, ok := src.Resolve(p)
		if ok {
			if _, exists, _ := fsutil.StatOf(path); exists {
				en.Retired, en.ChangedAt = true, r.now
				continue
			}
		}
		switch en.Kind {
		case manifest.KindText, manifest.KindBinary:
			en.Tombstone(r.now)
			r.report.Sent++
		default:
			en.Retired, en.ChangedAt = true, r.now
		}
	}
	return nil
}

func (r *run) warn(format string, a ...any) {
	r.report.Warnings = append(r.report.Warnings, fmt.Sprintf(format, a...))
}

func (r *run) postpone(path, reason string) {
	for _, d := range r.report.Deferred {
		if d.Path == path {
			return
		}
	}
	r.report.Deferred = append(r.report.Deferred, Deferred{path, reason})
}

// ---- steps 3-7 ----

func (r *run) exchange(ctx context.Context) error {
	snap, err := r.e.Store.Snapshot(ctx)
	if err != nil {
		return err
	}
	r.snap = snap
	if err := checkFormat(snap); err != nil {
		return err
	}
	if err := r.readOthers(); err != nil {
		return err
	}
	r.decideAll()
	return r.publish(ctx)
}

func (r *run) readOthers() error {
	names, err := r.snap.List("manifests/")
	if err != nil {
		return err
	}
	r.others = map[string]*manifest.Manifest{}
	r.report.Machines = map[string]Machine{}
	mine := manifest.Path(r.e.Machine)
	for _, n := range names {
		if !strings.HasSuffix(n, ".krv") || strings.Count(n, "/") != 1 {
			continue
		}
		b, err := r.snap.Get(n)
		if err != nil {
			return err
		}
		m, err := manifest.Decode(b)
		if err != nil {
			r.warn("%s: %v (ignored this run)", n, err)
			continue
		}
		if n == mine {
			if m.MachineID != r.state.MachineID && r.state.MachineID != "" && r.state.PublishedSeq > 0 {
				return fmt.Errorf("%w (%q)", ErrIdentity, r.e.Machine)
			}
			if r.state.PublishedSeq == 0 && m.MachineID != r.state.MachineID {
				return fmt.Errorf("%w (%q): pick another machine name", ErrIdentity, r.e.Machine)
			}
			if m.Seq > r.state.PublishedSeq {
				return fmt.Errorf("%w (%q)", ErrIdentity, r.e.Machine)
			}
			if m.Seq < r.state.PublishedSeq {
				return store.ErrRewound
			}
			continue
		}
		if m.Machine+".krv" != strings.TrimPrefix(n, "manifests/") {
			r.warn("%s claims to be machine %q (ignored)", n, m.Machine)
			continue
		}
		r.others[m.Machine] = m
		r.report.Machines[m.Machine] = Machine{Seq: m.Seq, WrittenAt: m.WrittenAt, Karvan: m.Karvan, SawMe: m.Seen[r.e.Machine]}
		if m.WrittenAt.After(r.now.Add(5 * time.Minute)) {
			r.warn("the clock of %s is ahead of this machine's by %s; check that both use network time", m.Machine, m.WrittenAt.Sub(r.now).Round(time.Minute))
		}
	}
	if r.state.PublishedSeq > 0 {
		if _, err := r.snap.Get(mine); errors.Is(err, store.ErrNotFound) {
			return store.ErrRewound
		}
	}
	return nil
}

type version struct {
	entry   *manifest.Entry
	machine string // "" = this machine
}

func (r *run) decideAll() {
	paths := map[string]bool{}
	for p := range r.cur.Entries {
		paths[p] = true
	}
	for _, m := range r.others {
		for p, en := range m.Entries {
			if en.Active() {
				paths[p] = true
			}
		}
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	// Desktop entries last: they point at transcripts that must be in place
	// first (docs/sync-engine.md, open question 1).
	sort.Slice(sorted, func(i, j int) bool {
		di, dj := strings.Contains(sorted[i], "/desktop/"), strings.Contains(sorted[j], "/desktop/")
		if di != dj {
			return dj
		}
		return sorted[i] < sorted[j]
	})
	for _, p := range sorted {
		if err := r.decide(p); err != nil {
			r.warn("%s: %v", p, err)
		}
	}
}

func (r *run) decide(p string) error {
	if strings.HasPrefix(p, "machine:") {
		return nil // owned state is never merged (8-5)
	}
	src := r.sourceFor(p)
	if src == nil {
		r.report.Unplaced++
		return nil
	}
	path, kind, ok := src.Resolve(p)
	if !ok {
		r.report.Unplaced++
		return nil
	}
	var vs []version
	mine := r.cur.Entries[p]
	if kind == "" && mine != nil {
		kind = mine.Kind
	}
	if mine != nil && mine.Active() {
		vs = append(vs, version{mine, ""})
	}
	for _, name := range sortedKeys(r.others) {
		en := r.others[name].Entries[p]
		if en == nil || !en.Active() {
			continue
		}
		if !manifest.KnownKind(en.Kind) {
			r.warn("%s: unknown kind %q from %s (left alone)", p, en.Kind, name)
			return nil
		}
		if kind == "" && (en.Kind == manifest.KindText || en.Kind == manifest.KindBinary) {
			kind = en.Kind // plain files: the kind follows the content
		}
		if en.Kind != kind && !plain(en.Kind, kind) {
			r.warn("%s: %s calls it %q, this machine %q (left alone)", p, name, en.Kind, kind)
			return nil
		}
		vs = append(vs, version{en, name})
	}
	if len(vs) == 0 {
		return nil
	}
	// A file on disk that this machine does not track (e.g. an old
	// transcript, or a first sync) takes part as a version without history.
	if mine == nil || !mine.Active() {
		if _, exists, _ := fsutil.StatOf(path); exists {
			se, err := r.readLocal(src, p, path)
			if err != nil {
				r.postpone(p, err.Error())
				return nil
			}
			en := &manifest.Entry{Kind: kind, Hash: se.Hash, Chunks: se.Chunks, Size: se.NormSize, Mtime: se.Mtime.UTC(),
				ChangedAt: r.now, Lines: se.Lines, Activity: se.Activity}
			if mine != nil { // retired: keep its history
				en.History = mine.History
				if mine.Hash != "" && mine.Hash != se.Hash {
					en.History = append([]string{mine.Hash}, mine.History...)
				}
			}
			r.cur.Entries[p] = en
			mine = en
			vs = append([]version{{en, ""}}, vs...)
		}
	}

	// Drop duplicates and dominated versions.
	var keep []version
	for i, v := range vs {
		drop := false
		for j, w := range vs {
			if i == j {
				continue
			}
			if manifest.Dominates(w.entry, v.entry) || manifest.Same(w.entry, v.entry) && j < i {
				drop = true
				break
			}
		}
		if !drop {
			keep = append(keep, v)
		}
	}
	if len(keep) == 1 && keep[0].machine == "" {
		return nil // mine is the newest
	}
	if len(keep) == 1 && mine != nil && mine.Active() && manifest.Same(keep[0].entry, mine) {
		return nil
	}
	var live []version
	for _, v := range keep {
		if !v.entry.Deleted {
			live = append(live, v)
		}
	}
	if len(keep) == 0 { // cannot happen with consistent histories; be safe
		r.warn("%s: inconsistent version history; left alone", p)
		return nil
	}
	switch {
	case len(live) == 0: // only deletions remain
		if mine == nil || mine.Deleted {
			if mine != nil {
				mine.History = unionHistory(kind, append([]*manifest.Entry{mine}, entriesOf(keep)...)...)
			}
			return nil
		}
		return r.applyDelete(src, p, path, keep[0].entry)
	case len(live) == 1:
		return r.adopt(src, p, path, live[0], keep)
	default:
		return r.mergeVersions(src, p, path, kind, live, keep)
	}
}

// plain reports whether both kinds are plain files (text and binary mix
// when a file changes type).
func plain(a, b string) bool {
	return (a == manifest.KindText || a == manifest.KindBinary) && (b == manifest.KindText || b == manifest.KindBinary)
}

func entriesOf(vs []version) []*manifest.Entry {
	out := make([]*manifest.Entry, len(vs))
	for i, v := range vs {
		out[i] = v.entry
	}
	return out
}

// unionHistory is the history of a version that supersedes all of es: their
// own hashes (or deleted versions) first, then their histories interleaved.
func unionHistory(kind string, es ...*manifest.Entry) []string {
	var out []string
	seen := map[string]bool{}
	add := func(h string) {
		if h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	for _, e := range es {
		add(e.Hash)
	}
	for i := 0; ; i++ {
		more := false
		for _, e := range es {
			if i < len(e.History) {
				add(e.History[i])
				more = true
			}
		}
		if !more {
			break
		}
	}
	if n := manifest.HistoryLimit(kind); len(out) > n {
		out = out[:n]
	}
	return out
}

func sortedKeys[T any](m map[string]T) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// versionContent assembles a version's content from local memory or the store.
func (r *run) versionContent(hash string, chunks []string) ([]byte, error) {
	if c, ok := r.content[hash]; ok {
		return c, nil
	}
	var buf bytes.Buffer
	for _, ch := range chunks {
		part, ok := r.chunks[ch]
		if !ok {
			sealed, err := r.snap.Get(blob.Path(ch))
			if err != nil {
				if c, ok := r.localVersion(hash); ok {
					return c, nil
				}
				return nil, fmt.Errorf("blob %s: %w", ch[:15], err)
			}
			if part, err = blob.OpenChunk(ch, sealed); err != nil {
				return nil, err
			}
			r.chunks[ch] = part
		}
		buf.Write(part)
	}
	c := buf.Bytes()
	if blob.Hash(c) != hash {
		return nil, fmt.Errorf("%w: content does not match %s", blob.ErrCorrupt, hash[:15])
	}
	r.content[hash] = c
	return c, nil
}

// localVersion re-reads the local file whose tracked version is hash.
func (r *run) localVersion(hash string) ([]byte, bool) {
	for p, en := range r.cur.Entries {
		if en.Hash != hash {
			continue
		}
		src := r.sourceFor(p)
		if src == nil {
			continue
		}
		path, _, ok := src.Resolve(p)
		if !ok {
			continue
		}
		raw, _, err := fsutil.ReadStable(path)
		if err != nil {
			continue
		}
		c, err := src.Normalize(p, raw)
		if err == nil && blob.Hash(c) == hash {
			r.content[hash] = c
			return c, true
		}
	}
	return nil, false
}

// adopt makes this machine's file equal to v (another machine's version),
// keeping the histories of every other remaining version.
func (r *run) adopt(src Source, p, path string, v version, keep []version) error {
	content, err := r.versionContent(v.entry.Hash, v.entry.Chunks)
	if err != nil {
		r.postpone(p, err.Error())
		return nil
	}
	en := v.entry.Clone()
	en.ChangedAt = r.now
	var others []*manifest.Entry
	for _, k := range keep { // tombstones that lost (a change wins)
		if k.entry != v.entry {
			others = append(others, k.entry)
		}
	}
	if len(others) > 0 {
		h := unionHistory(en.Kind, append([]*manifest.Entry{{History: en.History}}, others...)...)
		en.History = h
	}
	if err := r.writeLocal(src, p, path, content, en); err != nil {
		return err
	}
	r.report.Received++
	return nil
}

func (r *run) applyDelete(src Source, p, path string, tomb *manifest.Entry) error {
	mine := r.cur.Entries[p]
	exp, ok := r.scanned[p]
	if err := src.WriteCheck(r.scanRun, p, path, true); err != nil {
		r.postpone(p, err.Error())
		return nil
	}
	if cur, exists, _ := fsutil.StatOf(path); exists {
		if ok && (cur.Size != exp.Size || !cur.Mtime.Equal(exp.Mtime)) {
			r.postpone(p, fsutil.ErrChanged.Error())
			return nil
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	en := tomb.Clone()
	en.ChangedAt = r.now
	if mine != nil {
		en.BaseChunks = mine.BaseChunks
		en.PruneBases()
	}
	r.cur.Entries[p] = en
	r.report.Deleted++
	return nil
}

// writeLocal writes content to path (if allowed) and records en as mine.
func (r *run) writeLocal(src Source, p, path string, content []byte, en *manifest.Entry) error {
	st, exists, err := fsutil.StatOf(path)
	if err != nil {
		return err
	}
	if err := src.WriteCheck(r.scanRun, p, path, exists); err != nil {
		r.postpone(p, err.Error())
		return nil
	}
	data, err := src.Denormalize(p, content)
	if err != nil {
		return err
	}
	expect := fsutil.Stat{}
	if exists {
		e, ok := r.scanned[p]
		if !ok {
			e = st
		}
		expect = e
	}
	mtime := en.Mtime
	if mtime.IsZero() {
		mtime = r.now
	}
	mine := r.cur.Entries[p]
	appended := false
	// Transcripts usually only grew: append the new lines instead of
	// replacing a file Claude Code may be about to append to (9-2).
	if exists && en.Kind == manifest.KindTranscript && mine != nil && mine.Hash != "" {
		if old, err := r.versionContent(mine.Hash, mine.Chunks); err == nil && len(old) > 0 && bytes.HasPrefix(content, old) {
			oldRaw, err1 := src.Denormalize(p, old)
			suffix, err2 := src.Denormalize(p, content[len(old):])
			if err1 == nil && err2 == nil && int64(len(oldRaw)) == expect.Size {
				if err := fsutil.AppendIf(path, suffix, mtime, expect); err != nil {
					r.postpone(p, err.Error())
					return nil
				}
				appended = true
			}
		}
	}
	if !appended {
		var exp *fsutil.Stat
		if exists {
			exp = &expect
		} else {
			exp = &fsutil.Stat{}
		}
		if err := fsutil.WriteAtomicIf(path, data, 0o600, mtime, exp); err != nil {
			if errors.Is(err, fsutil.ErrChanged) {
				r.postpone(p, err.Error())
				return nil
			}
			return err
		}
	}
	if mine != nil && en.Kind != manifest.KindTranscript {
		// keep base chunks of versions we knew
		if en.BaseChunks == nil {
			en.BaseChunks = map[string][]string{}
		}
		for h, cs := range mine.BaseChunks {
			if _, ok := en.BaseChunks[h]; !ok {
				en.BaseChunks[h] = cs
			}
		}
		if mine.Hash != "" && len(mine.Chunks) > 0 {
			en.BaseChunks[mine.Hash] = mine.Chunks
		}
		en.PruneBases()
	}
	r.cur.Entries[p] = en
	r.content[en.Hash] = content
	if nst, ok, _ := fsutil.StatOf(path); ok {
		r.scanned[p] = nst
		hs := en.Chunks
		r.cache[path] = scanEntry{Size: nst.Size, Mtime: nst.Mtime, Hash: en.Hash, Chunks: hs, NormSize: int64(len(content)), Lines: en.Lines, Activity: en.Activity}
	}
	return nil
}

// mergeVersions merges two or more concurrent live versions by kind.
func (r *run) mergeVersions(src Source, p, path, kind string, live, keep []version) error {
	sort.Slice(live, func(i, j int) bool { return live[i].entry.Hash < live[j].entry.Hash })
	contents := make([][]byte, len(live))
	for i, v := range live {
		c, err := r.versionContent(v.entry.Hash, v.entry.Chunks)
		if err != nil {
			r.postpone(p, err.Error())
			return nil
		}
		contents[i] = c
	}
	var all []*manifest.Entry
	for _, k := range keep {
		all = append(all, k.entry)
	}
	history := unionHistory(kind, all...)
	newestMtime := time.Time{}
	for _, v := range live {
		if v.entry.Mtime.After(newestMtime) {
			newestMtime = v.entry.Mtime
		}
	}

	var result []byte
	var conflictCopy *version
	switch kind {
	case manifest.KindTranscript:
		result = merge.Transcripts(contents...)
	case manifest.KindDesktop, manifest.KindImmutable:
		best := 0
		for i := range live {
			a, b := live[i].entry, live[best].entry
			better := false
			if kind == manifest.KindDesktop {
				better = a.Activity > b.Activity || a.Activity == b.Activity && a.Hash < b.Hash
			} else {
				better = a.Size > b.Size || a.Size == b.Size && a.Hash < b.Hash
			}
			if better {
				best = i
			}
		}
		result = contents[best]
	default: // text, binary
		result = contents[0]
		cur := live[0]
		for i := 1; i < len(live); i++ {
			merged, ok := []byte(nil), false
			if kind == manifest.KindText {
				if base, found := r.baseContent(cur.entry, live[i].entry); found {
					merged, ok = merge.Text3(base, result, contents[i])
				}
			}
			if ok {
				result = merged
				continue
			}
			// Keep both: the newer stays, the other goes next to it.
			winner, loser := cur, live[i]
			wc, lc := result, contents[i]
			if loser.entry.Mtime.After(winner.entry.Mtime) || loser.entry.Mtime.Equal(winner.entry.Mtime) && loser.entry.Hash < winner.entry.Hash {
				winner, loser, wc, lc = loser, winner, lc, wc
			}
			result = wc
			cur = winner
			l := loser
			conflictCopy = &l
			if err := r.writeConflict(src, p, l, lc); err != nil {
				return err
			}
		}
	}
	h := blob.Hash(result)
	hs, parts := blob.Split(result)
	for ch, part := range parts {
		r.chunks[ch] = part
	}
	r.content[h] = result
	en := &manifest.Entry{Kind: kind, Hash: h, Chunks: hs, Size: int64(len(result)), Mtime: newestMtime, ChangedAt: r.now}
	en.History = history
	if kind == manifest.KindTranscript {
		en.Lines = bytes.Count(result, []byte{'\n'})
	}
	if a, err := merge.DesktopActivity(result); err == nil {
		en.Activity = a
	}
	// The result's own hash must not be in its history.
	out := en.History[:0]
	for _, x := range en.History {
		if x != h {
			out = append(out, x)
		}
	}
	en.History = out
	mine := r.cur.Entries[p]
	if mine != nil && mine.Hash == h {
		mine.History = en.History
		return nil
	}
	if err := r.writeLocal(src, p, path, result, en); err != nil {
		return err
	}
	if conflictCopy == nil {
		r.report.Merged++
	}
	return nil
}

// baseContent finds the content of the merge base of a and b.
func (r *run) baseContent(a, b *manifest.Entry) ([]byte, bool) {
	base := manifest.MergeBase(a, b)
	if base == "" {
		return nil, false
	}
	if c, ok := r.content[base]; ok {
		return c, true
	}
	for _, en := range []*manifest.Entry{a, b} {
		if cs, ok := en.BaseChunks[base]; ok {
			if c, err := r.versionContent(base, cs); err == nil {
				return c, true
			}
		}
	}
	for _, m := range r.others {
		for _, en := range m.Entries {
			if cs, ok := en.BaseChunks[base]; ok {
				if c, err := r.versionContent(base, cs); err == nil {
					return c, true
				}
			}
		}
	}
	return nil, false
}

// writeConflict keeps the losing version next to the file as
// <name>.conflict-<machine> (8-3).
func (r *run) writeConflict(src Source, p string, loser version, content []byte) error {
	machine := loser.machine
	if machine == "" {
		machine = r.e.Machine
	}
	// The machine that made this version first, so every machine picks the
	// same name.
	first := loser.entry.ChangedAt
	for _, name := range sortedKeys(r.others) {
		en := r.others[name].Entries[p]
		if en != nil && en.Hash == loser.entry.Hash && (en.ChangedAt.Before(first) || en.ChangedAt.Equal(first) && name < machine) {
			first, machine = en.ChangedAt, name
		}
	}
	if mine := r.cur.Entries[p]; mine != nil && mine.Hash == loser.entry.Hash && (mine.ChangedAt.Before(first) || mine.ChangedAt.Equal(first) && r.e.Machine < machine) {
		machine = r.e.Machine
	}
	cp := p + ".conflict-" + machine
	path, _, ok := src.Resolve(cp)
	if !ok {
		return fmt.Errorf("cannot place conflict copy %s", cp)
	}
	data, err := src.Denormalize(cp, content)
	if err != nil {
		return err
	}
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		r.report.Conflicts = append(r.report.Conflicts, Conflict{p, cp})
		return nil
	}
	if err := fsutil.WriteAtomic(path, data, 0o600); err != nil {
		return err
	}
	r.report.Conflicts = append(r.report.Conflicts, Conflict{p, cp})
	// Track the copy now so it goes out in this run.
	if se, err := r.readLocal(src, cp, path); err == nil {
		r.src[cp] = src
		if _, ok := r.cur.Entries[cp]; !ok {
			r.cur.Entries[cp] = &manifest.Entry{Kind: loser.entry.Kind, Hash: se.Hash, Chunks: se.Chunks,
				Size: se.NormSize, Mtime: se.Mtime.UTC(), ChangedAt: r.now}
		}
	}
	return nil
}

// ---- steps 6-7: upload and publish ----

func (r *run) publish(ctx context.Context) error {
	have := map[string]bool{}
	names, err := r.snap.List("blobs/")
	if err != nil {
		return err
	}
	for _, n := range names {
		have[n] = true
	}
	puts := map[string][]byte{}
	for _, p := range r.cur.Paths() {
		en := r.cur.Entries[p]
		if en.Retired || en.Held {
			continue
		}
		need := append([]string(nil), en.Chunks...)
		for _, cs := range en.BaseChunks {
			need = append(need, cs...)
		}
		for _, ch := range need {
			bp := blob.Path(ch)
			if have[bp] || puts[bp] != nil {
				continue
			}
			part, ok := r.chunks[ch]
			if !ok {
				part, ok = r.rereadChunk(p, en, ch)
			}
			if !ok {
				if contains(en.Chunks, ch) {
					r.warn("%s: a piece of this file is missing locally and in the store; it will be sent again when the file changes", p)
				}
				continue
			}
			puts[bp] = blob.Seal(part)
		}
	}
	r.report.Uploaded = len(puts)

	m := &manifest.Manifest{
		Format: manifest.Format, Machine: r.e.Machine, MachineID: r.state.MachineID, Karvan: r.e.Version,
		Seq: r.state.PublishedSeq + 1, WrittenAt: r.now.UTC(), Seen: map[string]int64{}, Projects: r.e.Projects,
		Entries: r.cur.Entries,
	}
	for name, o := range r.others {
		m.Seen[name] = o.Seq
	}
	if prev := r.state.Manifest; prev != nil && len(puts) == 0 && r.state.PublishedSeq > 0 && prev.Seq == r.state.PublishedSeq && sameContent(prev, m) {
		r.gc(ctx, prev)
		return nil // nothing new to say
	}
	enc, err := manifest.Encode(m)
	if err != nil {
		return err
	}
	puts[manifest.Path(r.e.Machine)] = enc
	r.saveState(m, false) // record locally first (step 7)
	msg := fmt.Sprintf("sync %s seq %d", r.e.Machine, m.Seq)
	if err := r.e.Store.Commit(ctx, r.snap, puts, nil, msg); err != nil {
		return err
	}
	r.state.PublishedSeq = m.Seq
	r.saveState(m, true)
	r.report.Published = true
	r.gc(ctx, m)
	return nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// sameContent compares what matters to other machines. Seen is left out on
// purpose: publishing only because another machine published would make two
// machines answer each other forever.
func sameContent(a, b *manifest.Manifest) bool {
	type view struct {
		E map[string]*manifest.Entry
		P map[string]manifest.Project
	}
	ja, _ := json.Marshal(view{a.Entries, a.Projects})
	jb, _ := json.Marshal(view{b.Entries, b.Projects})
	return bytes.Equal(ja, jb)
}

func (r *run) rereadChunk(p string, en *manifest.Entry, ch string) ([]byte, bool) {
	src := r.sourceFor(p)
	if src == nil {
		return nil, false
	}
	path, _, ok := src.Resolve(p)
	if !ok {
		return nil, false
	}
	raw, _, err := fsutil.ReadStable(path)
	if err != nil {
		return nil, false
	}
	content, err := src.Normalize(p, raw)
	if err != nil || blob.Hash(content) != en.Hash {
		return nil, false
	}
	_, parts := blob.Split(content)
	for h, part := range parts {
		r.chunks[h] = part
	}
	part, ok := r.chunks[ch]
	return part, ok
}

func (r *run) saveState(m *manifest.Manifest, published bool) {
	cp := *m
	r.state.Manifest = &cp
	if err := SaveState(r.e.StateDir, r.state); err != nil {
		r.warn("could not save local state: %v", err)
	}
	_ = published
}

// gc deletes blobs that no manifest has needed for two weeks (10).
func (r *run) gc(ctx context.Context, mine *manifest.Manifest) {
	const grace = 14 * 24 * time.Hour
	if r.now.Sub(r.state.LastGC) < 24*time.Hour {
		return
	}
	snap, err := r.e.Store.Snapshot(ctx)
	if err != nil {
		return
	}
	names, err := snap.List("manifests/")
	if err != nil {
		return
	}
	referenced := mine.Referenced()
	for _, n := range names {
		if n == manifest.Path(r.e.Machine) || !strings.HasSuffix(n, ".krv") {
			continue
		}
		b, err := snap.Get(n)
		if err != nil {
			return
		}
		m, err := manifest.Decode(b)
		if err != nil {
			return // cannot be sure: skip GC entirely
		}
		if r.now.Sub(m.WrittenAt) > grace {
			r.state.LastGC = r.now
			r.saveState(mine, true)
			return // a machine has been away too long: wait for it
		}
		for h := range m.Referenced() {
			referenced[h] = true
		}
	}
	blobs, err := snap.List("blobs/")
	if err != nil {
		return
	}
	var dels []string
	still := map[string]time.Time{}
	for _, bp := range blobs {
		h := blob.Prefix + bp[strings.LastIndex(bp, "/")+1:]
		if referenced[h] {
			continue
		}
		first, ok := r.state.GC[bp]
		if !ok {
			first = r.now
		}
		if r.now.Sub(first) >= grace {
			dels = append(dels, bp)
		} else {
			still[bp] = first
		}
	}
	if len(dels) > 0 {
		if err := r.e.Store.Commit(ctx, snap, nil, dels, fmt.Sprintf("gc %s: %d unused blobs", r.e.Machine, len(dels))); err != nil {
			return
		}
	}
	r.state.GC = still
	r.state.LastGC = r.now
	r.saveState(mine, true)
}
