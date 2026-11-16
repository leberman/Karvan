package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

func bareRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "store.git")
	if _, err := git.PlainInit(dir, true); err != nil {
		t.Fatal(err)
	}
	return "file://" + dir
}

func newGit(t *testing.T, url, machine string) *Git {
	return &Git{URL: url, Branch: "main", Dir: filepath.Join(t.TempDir(), "cache"), Machine: machine}
}

func TestGitStoreBasics(t *testing.T) {
	ctx := context.Background()
	url := bareRepo(t)
	a, b := newGit(t, url, "a"), newGit(t, url, "b")
	snap, err := a.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := snap.Get("FORMAT"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := a.Commit(ctx, snap, map[string][]byte{"FORMAT": []byte("1"), "blobs/ab/abc": []byte("x"), "manifests/a.krv": []byte("A")}, nil, "a"); err != nil {
		t.Fatal(err)
	}
	sb, err := b.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := sb.Get("manifests/a.krv"); string(got) != "A" {
		t.Fatalf("%q", got)
	}
	if ls, _ := sb.List("manifests/"); len(ls) != 1 || ls[0] != "manifests/a.krv" {
		t.Fatalf("%v", ls)
	}
	if ls, _ := sb.List("blobs/"); len(ls) != 1 {
		t.Fatalf("%v", ls)
	}
	if ls, _ := sb.List("nothing/"); len(ls) != 0 {
		t.Fatalf("%v", ls)
	}
	// a writes again; b's snapshot is now stale -> ErrConflict, nothing lost.
	sa, _ := a.Snapshot(ctx)
	if err := a.Commit(ctx, sa, map[string][]byte{"manifests/a.krv": []byte("A2")}, nil, "a2"); err != nil {
		t.Fatal(err)
	}
	if err := b.Commit(ctx, sb, map[string][]byte{"manifests/b.krv": []byte("B")}, nil, "b"); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v", err)
	}
	sb, _ = b.Snapshot(ctx)
	if err := b.Commit(ctx, sb, map[string][]byte{"manifests/b.krv": []byte("B")}, []string{"blobs/ab/abc"}, "b"); err != nil {
		t.Fatal(err)
	}
	s, _ := a.Snapshot(ctx)
	ga, _ := s.Get("manifests/a.krv")
	gb, _ := s.Get("manifests/b.krv")
	if string(ga) != "A2" || string(gb) != "B" {
		t.Fatalf("%q %q", ga, gb)
	}
	if _, err := s.Get("blobs/ab/abc"); !errors.Is(err, ErrNotFound) {
		t.Fatal("delete failed")
	}
}

func TestGitStoreBatchesAndRewind(t *testing.T) {
	ctx := context.Background()
	url := bareRepo(t)
	a := newGit(t, url, "a")
	a.BatchBytes = 10
	s, _ := a.Snapshot(ctx)
	puts := map[string][]byte{"manifests/a.krv": []byte("M")}
	for i := 0; i < 5; i++ {
		puts[fmt.Sprintf("blobs/0%d/b%d", i, i)] = []byte("0123456789")
	}
	if err := a.Commit(ctx, s, puts, nil, "big"); err != nil {
		t.Fatal(err)
	}
	r, _ := git.PlainOpen(url[len("file://"):])
	ref, _ := r.Reference(plumbing.NewBranchReferenceName("main"), true)
	n := 0
	c, _ := r.CommitObject(ref.Hash())
	for c != nil {
		n++
		if len(c.ParentHashes) == 0 {
			break
		}
		c, _ = r.CommitObject(c.ParentHashes[0])
	}
	if n != 5 {
		t.Fatalf("%d commits, want 5 batches", n)
	}
	s, _ = a.Snapshot(ctx)
	if ls, _ := s.List("blobs/"); len(ls) != 5 {
		t.Fatalf("%v", ls)
	}
	// Someone rewinds the branch.
	c, _ = r.CommitObject(ref.Hash())
	_ = r.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), c.ParentHashes[0]))
	if _, err := a.Snapshot(ctx); !errors.Is(err, ErrRewound) {
		t.Fatalf("err = %v", err)
	}
}

func TestGitStoreConcurrent(t *testing.T) {
	ctx := context.Background()
	url := bareRepo(t)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			g := newGit(t, url, fmt.Sprint(i))
			for attempt := 0; attempt < 30; attempt++ {
				s, err := g.Snapshot(ctx)
				if errors.Is(err, ErrConflict) {
					continue
				}
				if err != nil {
					errs <- err
					return
				}
				err = g.Commit(ctx, s, map[string][]byte{fmt.Sprintf("manifests/m%d.krv", i): []byte("x")}, nil, "m")
				if err == nil {
					return
				}
				if !errors.Is(err, ErrConflict) {
					errs <- err
					return
				}
			}
			errs <- fmt.Errorf("machine %d never got through", i)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	s, _ := newGit(t, url, "x").Snapshot(ctx)
	if ls, _ := s.List("manifests/"); len(ls) != 4 {
		t.Fatalf("%v", ls)
	}
}
