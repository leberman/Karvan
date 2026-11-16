package engine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"

	"github.com/leberman/karvan/internal/engine"
	"github.com/leberman/karvan/internal/sources"
	"github.com/leberman/karvan/internal/store"
)

type machine struct {
	t    *testing.T
	name string
	home string
	eng  *engine.Engine
	clk  *time.Time // shared by all machines of a test
}

var clocks = map[store.Store]*time.Time{}

func newStore(t *testing.T) store.Store {
	st := &store.Folder{Root: t.TempDir()}
	if _, err := engine.InitStore(context.Background(), st, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	return st
}

func newMachine(t *testing.T, name string, st store.Store) *machine {
	home := t.TempDir()
	clk := clocks[st]
	if clk == nil {
		c := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
		clk = &c
		clocks[st] = clk
	}
	m := &machine{t: t, name: name, home: home, clk: clk}
	m.eng = &engine.Engine{
		Store:    st,
		Machine:  name,
		Version:  "test",
		StateDir: filepath.Join(home, "state"),
		Now:      func() time.Time { return *m.clk },
		Backoff:  func(int) time.Duration { return time.Millisecond },
		Sources: []engine.Source{
			&sources.Dir{Label: "CLAUDE.md", Prefix: "global/CLAUDE.md", Root: filepath.Join(home, ".claude", "CLAUDE.md"), File: true},
			&sources.Dir{Label: "skills", Prefix: "global/skills/", Root: filepath.Join(home, ".claude", "skills"),
				Skip: func(rel string) bool { return rel == "synced" || strings.HasPrefix(rel, "synced/") }},
		},
	}
	return m
}

func (m *machine) path(rel string) string {
	return filepath.Join(m.home, ".claude", filepath.FromSlash(rel))
}

func (m *machine) write(rel, content string) {
	m.t.Helper()
	p := m.path(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		m.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		m.t.Fatal(err)
	}
	// Distinct mtimes so "newer" is well defined.
	*m.clk = m.clk.Add(time.Minute)
	_ = os.Chtimes(p, *m.clk, *m.clk)
}

func (m *machine) read(rel string) string {
	m.t.Helper()
	b, err := os.ReadFile(m.path(rel))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

func (m *machine) sync() *engine.Report {
	m.t.Helper()
	*m.clk = m.clk.Add(time.Minute)
	rep, err := m.eng.Run(context.Background(), engine.Options{})
	if err != nil {
		m.t.Fatalf("%s sync: %v", m.name, err)
	}
	for _, w := range rep.Warnings {
		m.t.Logf("%s warning: %s", m.name, w)
	}
	return rep
}

func TestCreateEditDeletePropagate(t *testing.T) {
	st := newStore(t)
	work, home := newMachine(t, "work", st), newMachine(t, "home", st)

	work.write("CLAUDE.md", "be brief\n")
	work.write("skills/review/SKILL.md", "review skill\n")
	work.write("skills/synced/x/SKILL.md", "app-managed\n")
	work.sync()
	rep := home.sync()
	if home.read("CLAUDE.md") != "be brief\n" || home.read("skills/review/SKILL.md") != "review skill\n" {
		t.Fatalf("not received: %+v", rep)
	}
	if home.read("skills/synced/x/SKILL.md") != "<missing>" {
		t.Fatal("skills/synced must never travel")
	}

	// Edit on home goes back to work.
	home.write("CLAUDE.md", "be brief\nanswer in Persian\n")
	home.sync()
	work.sync()
	if work.read("CLAUDE.md") != "be brief\nanswer in Persian\n" {
		t.Fatalf("edit not received: %q", work.read("CLAUDE.md"))
	}

	// Delete on work removes it on home.
	if err := os.RemoveAll(work.path("skills/review")); err != nil {
		t.Fatal(err)
	}
	work.sync()
	home.sync()
	if home.read("skills/review/SKILL.md") != "<missing>" {
		t.Fatal("delete not propagated")
	}
	// Quiet runs publish nothing new.
	if r := work.sync(); r.Published || r.Sent+r.Received != 0 {
		t.Fatalf("idle run did work: %+v", r)
	}
}

// PRD 6-2: both changed different lines -> three-way merge.
func TestThreeWayMerge(t *testing.T) {
	st := newStore(t)
	work, home := newMachine(t, "work", st), newMachine(t, "home", st)
	work.write("CLAUDE.md", "a\nb\nc\n")
	work.sync()
	home.sync()
	work.write("CLAUDE.md", "A\nb\nc\n")
	home.write("CLAUDE.md", "a\nb\nC\n")
	work.sync()
	rep := home.sync()
	if home.read("CLAUDE.md") != "A\nb\nC\n" || rep.Merged != 1 {
		t.Fatalf("home: %q %+v", home.read("CLAUDE.md"), rep)
	}
	work.sync()
	if work.read("CLAUDE.md") != "A\nb\nC\n" {
		t.Fatalf("work: %q", work.read("CLAUDE.md"))
	}
	// Converged: further runs change nothing.
	if r := home.sync(); r.Received+r.Merged != 0 {
		t.Fatalf("not converged: %+v", r)
	}
}

// PRD 6-2: same line changed differently -> both versions kept, identical
// on both machines.
func TestConflictKeepsBoth(t *testing.T) {
	st := newStore(t)
	work, home := newMachine(t, "work", st), newMachine(t, "home", st)
	work.write("CLAUDE.md", "a\nb\nc\n")
	work.sync()
	home.sync()
	work.write("CLAUDE.md", "a\nWORK\nc\n")
	home.write("CLAUDE.md", "a\nHOME\nc\n") // later mtime: home's version stays
	work.sync()
	rep := home.sync()
	if len(rep.Conflicts) != 1 || home.read("CLAUDE.md") != "a\nHOME\nc\n" || home.read("CLAUDE.md.conflict-work") != "a\nWORK\nc\n" {
		t.Fatalf("home: %q / %q %+v", home.read("CLAUDE.md"), home.read("CLAUDE.md.conflict-work"), rep)
	}
	work.sync()
	home.sync()
	work.sync()
	if work.read("CLAUDE.md") != "a\nHOME\nc\n" || work.read("CLAUDE.md.conflict-work") != "a\nWORK\nc\n" {
		t.Fatalf("work: %q / %q", work.read("CLAUDE.md"), work.read("CLAUDE.md.conflict-work"))
	}
}

// PRD 6-2: delete vs change -> change wins.
func TestChangeBeatsDelete(t *testing.T) {
	st := newStore(t)
	work, home := newMachine(t, "work", st), newMachine(t, "home", st)
	work.write("skills/s/SKILL.md", "v1\n")
	work.sync()
	home.sync()
	if err := os.Remove(work.path("skills/s/SKILL.md")); err != nil {
		t.Fatal(err)
	}
	home.write("skills/s/SKILL.md", "v2\n")
	work.sync()
	home.sync()
	work.sync()
	if work.read("skills/s/SKILL.md") != "v2\n" || home.read("skills/s/SKILL.md") != "v2\n" {
		t.Fatalf("work %q home %q", work.read("skills/s/SKILL.md"), home.read("skills/s/SKILL.md"))
	}
}

// First sync of two machines that both already have a file: no data lost.
func TestFirstSyncBothHaveFile(t *testing.T) {
	st := newStore(t)
	work, home := newMachine(t, "work", st), newMachine(t, "home", st)
	work.write("CLAUDE.md", "work rules\n")
	home.write("CLAUDE.md", "home rules\n")
	work.sync()
	home.sync()
	work.sync()
	both := work.read("CLAUDE.md") + work.read("CLAUDE.md.conflict-work") + work.read("CLAUDE.md.conflict-home")
	if !strings.Contains(both, "work rules") || !strings.Contains(both, "home rules") {
		t.Fatalf("lost a version: %q", both)
	}
	if work.read("CLAUDE.md") != home.read("CLAUDE.md") {
		t.Fatal("not converged")
	}
}

// Three machines, a chain of edits: everyone ends up equal.
func TestThreeMachinesConverge(t *testing.T) {
	st := newStore(t)
	ms := []*machine{newMachine(t, "a", st), newMachine(t, "b", st), newMachine(t, "c", st)}
	ms[0].write("CLAUDE.md", "1\n2\n3\n4\n5\n")
	for round := 0; round < 3; round++ {
		for _, m := range ms {
			m.sync()
		}
	}
	ms[0].write("CLAUDE.md", "ONE\n2\n3\n4\n5\n")
	ms[1].write("skills/x/SKILL.md", "x\n")
	ms[2].write("CLAUDE.md", "1\n2\n3\n4\nFIVE\n")
	for round := 0; round < 3; round++ {
		for _, m := range ms {
			m.sync()
		}
	}
	for _, m := range ms {
		if m.read("CLAUDE.md") != "ONE\n2\n3\n4\nFIVE\n" || m.read("skills/x/SKILL.md") != "x\n" {
			t.Fatalf("%s: %q %q", m.name, m.read("CLAUDE.md"), m.read("skills/x/SKILL.md"))
		}
	}
}

func TestIdentityAndRewind(t *testing.T) {
	st := newStore(t)
	work := newMachine(t, "work", st)
	work.write("CLAUDE.md", "x\n")
	work.sync()
	// A second machine that wrongly uses the same name.
	clone := newMachine(t, "work", st)
	if _, err := clone.eng.Run(context.Background(), engine.Options{}); !errors.Is(err, engine.ErrIdentity) {
		t.Fatalf("err = %v", err)
	}
	// The store loses work's manifest (restored from an old backup).
	if err := os.Remove(filepath.Join(st.(*store.Folder).Root, "manifests", "work.krv")); err != nil {
		t.Fatal(err)
	}
	if _, err := work.eng.Run(context.Background(), engine.Options{}); !errors.Is(err, store.ErrRewound) {
		t.Fatalf("err = %v", err)
	}
}

func TestStoreNotInitialized(t *testing.T) {
	st := &store.Folder{Root: t.TempDir()}
	m := newMachine(t, "work", st)
	if _, err := m.eng.Run(context.Background(), engine.Options{}); !errors.Is(err, engine.ErrNotInitialized) {
		t.Fatalf("err = %v", err)
	}
}

func TestGarbageCollection(t *testing.T) {
	st := newStore(t)
	work := newMachine(t, "work", st)
	root := st.(*store.Folder).Root
	count := func() int {
		n := 0
		_ = filepath.Walk(filepath.Join(root, "blobs"), func(_ string, i os.FileInfo, _ error) error {
			if i != nil && !i.IsDir() {
				n++
			}
			return nil
		})
		return n
	}
	for i := 0; i < 12; i++ { // more versions than the 8 kept as merge bases
		work.write("skills/s/SKILL.md", strings.Repeat("v", i+1)+"\n")
		work.sync()
	}
	before := count()
	*work.clk = work.clk.Add(15 * 24 * time.Hour)
	work.sync() // marks candidates
	*work.clk = work.clk.Add(15 * 24 * time.Hour)
	work.write("skills/s/SKILL.md", "final\n") // keep the manifest fresh
	work.sync()
	*work.clk = work.clk.Add(25 * time.Hour)
	work.sync()
	after := count()
	if after >= before {
		t.Fatalf("nothing collected: %d -> %d", before, after)
	}
	if work.read("skills/s/SKILL.md") != "final\n" {
		t.Fatal("lost the file")
	}
	// A new machine can still get everything that is referenced.
	home := newMachine(t, "home", st)
	home.sync()
	if home.read("skills/s/SKILL.md") != "final\n" {
		t.Fatal("GC removed a referenced blob")
	}
}

func gitStore(t *testing.T) store.Store {
	dir := filepath.Join(t.TempDir(), "store.git")
	if _, err := gogit.PlainInit(dir, true); err != nil {
		t.Fatal(err)
	}
	st := &store.Git{URL: "file://" + dir, Branch: "main", Dir: filepath.Join(t.TempDir(), "cache"), Machine: "init"}
	if _, err := engine.InitStore(context.Background(), st, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	return st
}

// PRD 15 #10: two machines push at the same moment; one is rejected,
// re-reads, re-decides and pushes; nothing is lost.
func TestGitConcurrentSync(t *testing.T) {
	url := gitStore(t).(*store.Git).URL
	mk := func(name string) *machine {
		st := &store.Git{URL: url, Branch: "main", Dir: filepath.Join(t.TempDir(), "cache"), Machine: name}
		m := newMachine(t, name, st)
		return m
	}
	work, home := mk("work"), mk("home")
	// Share one clock between the two (different store values).
	home.clk = work.clk
	work.write("CLAUDE.md", "a\nb\nc\n")
	work.sync()
	home.sync()
	work.write("CLAUDE.md", "A\nb\nc\n")
	home.write("skills/h/SKILL.md", "from home\n")
	home.write("CLAUDE.md", "a\nb\nC\n")
	var wg sync.WaitGroup
	reps := make([]*engine.Report, 2)
	for i, m := range []*machine{work, home} {
		wg.Add(1)
		go func(i int, m *machine) {
			defer wg.Done()
			rep, err := m.eng.Run(context.Background(), engine.Options{})
			if err != nil {
				t.Errorf("%s: %v", m.name, err)
			}
			reps[i] = rep
		}(i, m)
	}
	wg.Wait()
	for round := 0; round < 2; round++ {
		work.sync()
		home.sync()
	}
	for _, m := range []*machine{work, home} {
		if m.read("CLAUDE.md") != "A\nb\nC\n" || m.read("skills/h/SKILL.md") != "from home\n" {
			t.Fatalf("%s: %q %q", m.name, m.read("CLAUDE.md"), m.read("skills/h/SKILL.md"))
		}
	}
	t.Logf("attempts: work %d, home %d", reps[0].Attempts, reps[1].Attempts)
}
