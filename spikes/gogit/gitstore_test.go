package gitstore

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
	"github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/go-git/go-git/v5/storage/memory"
)

// Every scenario runs twice for file:// remotes:
//   - "inproc": go-git's own in-process server (no git binary anywhere),
//   - "gitbin": go-git's default file transport, which runs the system's
//     git-upload-pack / git-receive-pack (closest to a real server).
//
// Over ssh:// (the real use) go-git talks to the remote's own git, so
// cmd/sshtry is the test for that path.
func forEachTransport(t *testing.T, fn func(t *testing.T)) {
	t.Run("inproc", func(t *testing.T) {
		old := client.Protocols["file"]
		client.InstallProtocol("file", server.NewServer(server.NewFilesystemLoader(osfs.New("/"))))
		defer client.InstallProtocol("file", old)
		fn(t)
	})
	t.Run("gitbin", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git binary not installed")
		}
		fn(t)
	})
}

// newRemote makes an empty bare repository (with go-git, not git init).
func newRemote(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainInit(dir, true); err != nil {
		t.Fatal(err)
	}
	return "file://" + dir
}

func diskStorage(t *testing.T) storage.Storer {
	return filesystem.NewStorage(osfs.New(t.TempDir()), cache.NewObjectLRUDefault())
}

func open(t *testing.T, url, machine string, st storage.Storer, depth int) *Store {
	t.Helper()
	if st == nil {
		st = diskStorage(t)
	}
	s, err := Open(st, Options{URL: url, Machine: machine, Depth: depth})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustFetch(t *testing.T, s *Store) plumbing.Hash {
	t.Helper()
	h, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func mustCommit(t *testing.T, s *Store, parent plumbing.Hash, ch map[string][]byte) plumbing.Hash {
	t.Helper()
	c, err := s.Commit(parent, ch, "test")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// remoteState opens the bare remote directly and returns head + all files.
func remoteState(t *testing.T, url string) (*git.Repository, *object.Commit, map[string]string) {
	t.Helper()
	r, err := git.PlainOpen(url[len("file://"):])
	if err != nil {
		t.Fatal(err)
	}
	ref, err := r.Reference(plumbing.NewBranchReferenceName("main"), true)
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.CommitObject(ref.Hash())
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	ft, _ := c.Files()
	_ = ft.ForEach(func(f *object.File) error {
		s, _ := f.Contents()
		files[f.Name] = s
		return nil
	})
	return r, c, files
}

// assertLinear checks every commit on the remote branch has at most one
// parent: no merge commits, ever.
func assertLinear(t *testing.T, r *git.Repository, head *object.Commit) int {
	t.Helper()
	n := 0
	for c := head; ; n++ {
		if len(c.ParentHashes) > 1 {
			t.Fatalf("merge commit %s found", c.Hash)
		}
		if len(c.ParentHashes) == 0 {
			return n + 1
		}
		var err error
		if c, err = r.CommitObject(c.ParentHashes[0]); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFirstPushToEmptyRemote(t *testing.T) {
	forEachTransport(t, func(t *testing.T) {
		url := newRemote(t)
		a := open(t, url, "a", nil, 0)
		head := mustFetch(t, a)
		if !head.IsZero() {
			t.Fatalf("empty remote should give zero head, got %s", head)
		}
		c := mustCommit(t, a, head, map[string][]byte{"FORMAT": []byte("1\n"), "manifests/a.krv": []byte("A1")})
		if err := a.Push(context.Background(), c, head); err != nil {
			t.Fatal(err)
		}
		_, rc, files := remoteState(t, url)
		if rc.Hash != c || files["manifests/a.krv"] != "A1" || files["FORMAT"] != "1\n" {
			t.Fatalf("unexpected remote state %s %v", rc.Hash, files)
		}
	})
}

// The core scenario of PRD 15 #10: B pushes first, A's push is rejected,
// A fetches, rebuilds its commit on top of B's, and succeeds. Never forced.
func TestRejectedPushThenRebuildAndRetry(t *testing.T) {
	forEachTransport(t, func(t *testing.T) {
		ctx := context.Background()
		url := newRemote(t)
		a := open(t, url, "a", nil, 0)
		b := open(t, url, "b", memory.NewStorage(), 0) // also exercise in-memory storage

		// Seed the store.
		h0 := mustFetch(t, a)
		c0 := mustCommit(t, a, h0, map[string][]byte{"FORMAT": []byte("1\n")})
		if err := a.Push(ctx, c0, h0); err != nil {
			t.Fatal(err)
		}

		// Both fetch the same head.
		ha, hb := mustFetch(t, a), mustFetch(t, b)
		if ha != hb {
			t.Fatal("heads differ")
		}

		// B writes and pushes first.
		cb := mustCommit(t, b, hb, map[string][]byte{"manifests/b.krv": []byte("B1"), "blobs/bb/bb01": []byte("blob-b")})
		if err := b.Push(ctx, cb, hb); err != nil {
			t.Fatal(err)
		}

		// A's commit was built on the old head: must be rejected, not forced.
		ca := mustCommit(t, a, ha, map[string][]byte{"manifests/a.krv": []byte("A1"), "blobs/aa/aa01": []byte("blob-a")})
		err := a.Push(ctx, ca, ha)
		if !errors.Is(err, ErrRejected) {
			t.Fatalf("expected ErrRejected, got %v", err)
		}
		t.Logf("rejection as reported by go-git: %v", err)
		if _, rc, _ := remoteState(t, url); rc.Hash != cb {
			t.Fatal("remote moved after a rejected push")
		}

		// Fetch, rebuild on top of B's commit, retry.
		ha2 := mustFetch(t, a)
		if ha2 != cb {
			t.Fatalf("fetch should see B's commit, got %s", ha2)
		}
		ca2 := mustCommit(t, a, ha2, map[string][]byte{"manifests/a.krv": []byte("A1"), "blobs/aa/aa01": []byte("blob-a")})
		if err := a.Push(ctx, ca2, ha2); err != nil {
			t.Fatal(err)
		}

		r, rc, files := remoteState(t, url)
		if rc.Hash != ca2 || len(rc.ParentHashes) != 1 || rc.ParentHashes[0] != cb {
			t.Fatalf("A's commit should sit directly on B's: head %s parents %v", rc.Hash, rc.ParentHashes)
		}
		for p, want := range map[string]string{"FORMAT": "1\n", "manifests/a.krv": "A1", "manifests/b.krv": "B1", "blobs/aa/aa01": "blob-a", "blobs/bb/bb01": "blob-b"} {
			if files[p] != want {
				t.Fatalf("%s = %q, want %q", p, files[p], want)
			}
		}
		if n := assertLinear(t, r, rc); n != 3 {
			t.Fatalf("want 3 commits, got %d", n)
		}

		// Deleting every file still yields a valid (empty-tree) commit.
		if _, err := a.Update(ctx, "wipe", 3, func(h plumbing.Hash) (map[string][]byte, error) {
			return map[string][]byte{"FORMAT": nil, "manifests/a.krv": nil, "manifests/b.krv": nil, "blobs/aa/aa01": nil, "blobs/bb/bb01": nil}, nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, _, files := remoteState(t, url); len(files) != 0 {
			t.Fatalf("expected empty tree, got %v", files)
		}

		// List works on the fetched state without a worktree.
		ls, err := a.List(rc.Hash, "manifests")
		if err != nil || len(ls) != 2 {
			t.Fatalf("list = %v, %v", ls, err)
		}
	})
}

// Same scenario through Update (the loop Karvan will use).
func TestUpdateRetriesUntilFastForward(t *testing.T) {
	forEachTransport(t, func(t *testing.T) {
		ctx := context.Background()
		url := newRemote(t)
		a := open(t, url, "a", nil, 0)
		b := open(t, url, "b", nil, 0)
		if _, err := a.Update(ctx, "seed", 3, func(h plumbing.Hash) (map[string][]byte, error) {
			return map[string][]byte{"FORMAT": []byte("1\n")}, nil
		}); err != nil {
			t.Fatal(err)
		}
		mustFetch(t, a)

		// A's build step runs; while it runs, B pushes. A's first push must fail
		// and the second attempt must see B's manifest.
		calls := 0
		sawB := false
		attempts, err := a.Update(ctx, "a sync", 5, func(h plumbing.Hash) (map[string][]byte, error) {
			calls++
			if calls == 1 {
				if _, err := b.Update(ctx, "b sync", 3, func(plumbing.Hash) (map[string][]byte, error) {
					return map[string][]byte{"manifests/b.krv": []byte("B1")}, nil
				}); err != nil {
					return nil, err
				}
			} else if _, err := a.ReadFile(h, "manifests/b.krv"); err == nil {
				sawB = true
			}
			return map[string][]byte{"manifests/a.krv": []byte("A1")}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if attempts != 2 || !sawB {
			t.Fatalf("attempts=%d sawB=%v, want 2/true", attempts, sawB)
		}
		r, rc, files := remoteState(t, url)
		if files["manifests/a.krv"] != "A1" || files["manifests/b.krv"] != "B1" {
			t.Fatalf("files %v", files)
		}
		assertLinear(t, r, rc)
	})
}

// Several machines syncing at the same moment: every write lands, history
// stays linear.
func TestConcurrentMachines(t *testing.T) {
	forEachTransport(t, func(t *testing.T) {
		if t.Name() == "TestConcurrentMachines/inproc" {
			// go-git's in-process receive-pack updates refs without a
			// compare-and-swap, so two simultaneous pushes can both report
			// success and one is lost (finding 5 in README). Our Fetch then reports
			// ErrRemoteRewound, which is correct, but the scenario only makes
			// sense against a real git server, which is what GitHub runs.
			t.Skip("go-git's own server has no atomic ref update")
		}
		ctx := context.Background()
		url := newRemote(t)
		const machines, rounds = 5, 3
		stores := make([]*Store, machines)
		for i := range stores {
			stores[i] = open(t, url, fmt.Sprintf("m%d", i), nil, 0)
		}
		var wg sync.WaitGroup
		errs := make(chan error, machines*rounds)
		totalAttempts := make([]int, machines)
		for i, s := range stores {
			wg.Add(1)
			go func(i int, s *Store) {
				defer wg.Done()
				for r := 0; r < rounds; r++ {
					n, err := s.Update(ctx, "sync", 20, func(plumbing.Hash) (map[string][]byte, error) {
						return map[string][]byte{
							fmt.Sprintf("manifests/m%d.krv", i):        []byte(fmt.Sprintf("round %d", r)),
							fmt.Sprintf("blobs/%02d/m%d-r%d", i, i, r): []byte("x"),
						}, nil
					})
					totalAttempts[i] += n
					if err != nil {
						errs <- fmt.Errorf("m%d round %d: %w", i, r, err)
					}
				}
			}(i, s)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		r, rc, files := remoteState(t, url)
		for i := 0; i < machines; i++ {
			if got := files[fmt.Sprintf("manifests/m%d.krv", i)]; got != fmt.Sprintf("round %d", rounds-1) {
				t.Errorf("m%d manifest = %q", i, got)
			}
			for r := 0; r < rounds; r++ {
				if _, ok := files[fmt.Sprintf("blobs/%02d/m%d-r%d", i, i, r)]; !ok {
					t.Errorf("blob m%d-r%d missing", i, r)
				}
			}
		}
		n := assertLinear(t, r, rc)
		t.Logf("%d commits, attempts per machine %v", n, totalAttempts)
	})
}

// If someone force-pushes the store (never Karvan), fetch must notice and
// stop, not silently follow or "fix" it by forcing back.
func TestRewoundRemoteIsDetected(t *testing.T) {
	forEachTransport(t, func(t *testing.T) {
		ctx := context.Background()
		url := newRemote(t)
		a := open(t, url, "a", nil, 0)
		h0 := mustFetch(t, a)
		c1 := mustCommit(t, a, h0, map[string][]byte{"f": []byte("1")})
		if err := a.Push(ctx, c1, h0); err != nil {
			t.Fatal(err)
		}
		c2 := mustCommit(t, a, c1, map[string][]byte{"f": []byte("2")})
		if err := a.Push(ctx, c2, c1); err != nil {
			t.Fatal(err)
		}

		// An outside actor rewinds main to c1 with a raw ref update.
		r, err := git.PlainOpen(url[len("file://"):])
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), c1)); err != nil {
			t.Fatal(err)
		}

		_, err = a.Fetch(ctx)
		if !errors.Is(err, ErrRemoteRewound) {
			t.Fatalf("expected ErrRemoteRewound, got %v", err)
		}
	})
}

// Shallow fetches (depth 1) keep the local clone small. Check that commit +
// fast-forward push + reject/retry still work from a shallow clone.
func TestShallowClone(t *testing.T) {
	forEachTransport(t, func(t *testing.T) {
		if t.Name() == "TestShallowClone/inproc" {
			t.Skip("go-git's in-process server does not support shallow fetches (finding 5 in README); GitHub does")
		}
		ctx := context.Background()
		url := newRemote(t)
		full := open(t, url, "full", nil, 0)
		for i := 0; i < 5; i++ {
			if _, err := full.Update(ctx, "hist", 3, func(plumbing.Hash) (map[string][]byte, error) {
				return map[string][]byte{"manifests/full.krv": []byte(fmt.Sprint(i))}, nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		sh := open(t, url, "shallow", nil, 1)
		other := open(t, url, "other", nil, 1)
		hs := mustFetch(t, sh)
		mustFetch(t, other)
		if _, err := other.Update(ctx, "other", 3, func(plumbing.Hash) (map[string][]byte, error) {
			return map[string][]byte{"manifests/other.krv": []byte("O")}, nil
		}); err != nil {
			t.Fatal(err)
		}
		c := mustCommit(t, sh, hs, map[string][]byte{"manifests/shallow.krv": []byte("S")})
		if err := sh.Push(ctx, c, hs); !errors.Is(err, ErrRejected) {
			t.Fatalf("expected rejection, got %v", err)
		}
		if _, err := sh.Update(ctx, "shallow", 3, func(plumbing.Hash) (map[string][]byte, error) {
			return map[string][]byte{"manifests/shallow.krv": []byte("S")}, nil
		}); err != nil {
			t.Fatal(err)
		}
		_, _, files := remoteState(t, url)
		if files["manifests/shallow.krv"] != "S" || files["manifests/other.krv"] != "O" || files["manifests/full.krv"] != "4" {
			t.Fatalf("files %v", files)
		}
	})
}

// Rough numbers for the PRD: push ~64 MiB of incompressible blobs (what
// encrypted, compressed chunks look like) in one commit, then fetch it into a
// fresh clone. Logs time and Go heap; skipped with -short.
func TestLargePushMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	forEachTransport(t, func(t *testing.T) {
		ctx := context.Background()
		url := newRemote(t)
		a := open(t, url, "a", nil, 0)
		const n, size = 256, 256 << 10
		ch := map[string][]byte{}
		for i := 0; i < n; i++ {
			b := make([]byte, size)
			_, _ = rand.Read(b)
			ch[fmt.Sprintf("blobs/%02x/%04d", i%256, i)] = b
		}
		peak := watchHeap()
		start := time.Now()
		if _, err := a.Update(ctx, "big", 3, func(plumbing.Hash) (map[string][]byte, error) { return ch, nil }); err != nil {
			t.Fatal(err)
		}
		pushDur := time.Since(start)
		ch = nil
		runtime.GC()
		b := open(t, url, "b", nil, 0)
		start = time.Now()
		if _, err := b.Fetch(ctx); err != nil {
			t.Fatal(err)
		}
		t.Logf("push %d MiB: %v; fresh fetch: %v; peak Go heap during both: %d MiB",
			n*size>>20, pushDur.Round(time.Millisecond), time.Since(start).Round(time.Millisecond), peak()>>20)
	})
}

// watchHeap samples HeapInuse until the returned func is called.
func watchHeap() func() uint64 {
	var mu sync.Mutex
	var max uint64
	done := make(chan struct{})
	go func() {
		var ms runtime.MemStats
		for {
			runtime.ReadMemStats(&ms)
			mu.Lock()
			if ms.HeapInuse > max {
				max = ms.HeapInuse
			}
			mu.Unlock()
			select {
			case <-done:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	return func() uint64 { close(done); mu.Lock(); defer mu.Unlock(); return max }
}
