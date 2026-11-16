// Package gitstore is a phase-0 spike: it checks that go-git can serve as
// Karvan's git blob store without a git binary, a worktree, merges or force
// pushes. The flow is: fetch the remote branch, build a new commit directly on
// top of the remote head (tree surgery, no checkout), push fast-forward only,
// and on rejection fetch again, rebuild the commit and retry.
//
// This is throwaway code that informs docs/sync-engine.md; it is not the
// package the real store (M0 phase 4) will ship.
package gitstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage"
)

// ErrRejected means the remote branch moved since our last fetch, so the push
// was refused (non-fast-forward or the compare-and-swap failed). The caller
// fetches, rebuilds and retries.
var ErrRejected = errors.New("push rejected: remote branch moved")

// ErrRemoteRewound means the remote branch is no longer a descendant of what
// we fetched before (someone force-pushed). Karvan never repairs this by
// forcing; it stops and tells the user.
var ErrRemoteRewound = errors.New("remote branch was rewound (force-pushed by someone else)")

const remoteName = "origin"

// Store is a git remote used as a plain blob store.
type Store struct {
	repo   *git.Repository
	branch string
	auth   transport.AuthMethod
	depth  int // 0 = full history; 1 = shallow fetches
	who    object.Signature
}

// Options configure Open.
type Options struct {
	URL     string
	Branch  string               // default "main"
	Auth    transport.AuthMethod // nil for file:// URLs
	Depth   int                  // 0 = full; >0 = shallow fetch depth
	Machine string               // appears in the commit author name only
}

// Open creates a bare local repository in st (memory or filesystem storage)
// pointing at opts.URL. It does not touch the network; call Fetch next.
func Open(st storage.Storer, opts Options) (*Store, error) {
	if opts.Branch == "" {
		opts.Branch = "main"
	}
	repo, err := git.Init(st, nil) // nil worktree = bare
	if err != nil {
		return nil, fmt.Errorf("init local repo: %w", err)
	}
	// Blobs are compressed (and from M1 encrypted), so delta search finds
	// nothing and only burns CPU and memory when building the push pack
	// (finding 3 in README). pack.window = 0 turns it off.
	cfg, err := repo.Config()
	if err != nil {
		return nil, err
	}
	cfg.Pack.Window = 0
	if err := repo.SetConfig(cfg); err != nil {
		return nil, err
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: remoteName, URLs: []string{opts.URL}}); err != nil {
		return nil, fmt.Errorf("add remote: %w", err)
	}
	return &Store{
		repo:   repo,
		branch: opts.Branch,
		auth:   opts.Auth,
		depth:  opts.Depth,
		// No personal data in commits: a fixed, invalid address.
		who: object.Signature{Name: "Karvan (" + opts.Machine + ")", Email: "karvan@invalid"},
	}, nil
}

func (s *Store) trackingRef() plumbing.ReferenceName {
	return plumbing.NewRemoteReferenceName(remoteName, s.branch)
}

func (s *Store) branchRef() plumbing.ReferenceName { return plumbing.NewBranchReferenceName(s.branch) }

// Head returns the last fetched remote head (ZeroHash if the branch does not
// exist yet).
func (s *Store) Head() plumbing.Hash {
	ref, err := s.repo.Reference(s.trackingRef(), true)
	if err != nil {
		return plumbing.ZeroHash
	}
	return ref.Hash()
}

// Fetch reads the remote branch head (ls-remote), downloads what is missing,
// points the remote-tracking ref at exactly that head and returns it.
//
// It does not rely on go-git's own tracking-ref update: go-git reports
// "already up to date" and leaves the tracking ref alone when it already has
// the advertised objects, even if the remote branch moved backwards
// (finding 1 in README). Reading the advertised head first makes the read exact and lets
// us detect a rewound (force-pushed) remote.
func (s *Store) Fetch(ctx context.Context) (plumbing.Hash, error) {
	before := s.Head()
	remote, err := s.repo.Remote(remoteName)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	refs, err := remote.ListContext(ctx, &git.ListOptions{Auth: s.auth})
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		refs, err = nil, nil
	}
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("ls-remote: %w", err)
	}
	var head plumbing.Hash
	for _, r := range refs {
		if r.Name() == s.branchRef() {
			head = r.Hash()
		}
	}
	if head.IsZero() {
		if !before.IsZero() {
			return head, ErrRemoteRewound // branch deleted under us
		}
		return head, nil
	}
	if head != before {
		if _, err := s.repo.CommitObject(head); err != nil {
			spec := config.RefSpec(fmt.Sprintf("refs/heads/%s:%s", s.branch, s.trackingRef()))
			err = s.repo.FetchContext(ctx, &git.FetchOptions{
				RemoteName: remoteName,
				RefSpecs:   []config.RefSpec{spec},
				Auth:       s.auth,
				Depth:      s.depth,
				Tags:       git.NoTags,
			})
			if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
				return plumbing.ZeroHash, fmt.Errorf("fetch: %w", err)
			}
			if _, err := s.repo.CommitObject(head); err != nil {
				// The branch moved again between ls-remote and fetch; the
				// caller simply retries (the next push would be rejected anyway).
				return plumbing.ZeroHash, fmt.Errorf("%w (moved during fetch)", ErrRejected)
			}
		}
		if !before.IsZero() {
			ok, err := s.isAncestor(before, head)
			if err != nil {
				return head, fmt.Errorf("check remote history: %w", err)
			}
			if !ok {
				return head, ErrRemoteRewound
			}
		}
		if err := s.repo.Storer.SetReference(plumbing.NewHashReference(s.trackingRef(), head)); err != nil {
			return head, err
		}
	}
	return head, nil
}

// isAncestor reports whether old is reachable from new. With a shallow clone
// the walk can end at the shallow boundary; that counts as "not found" only if
// old is not itself a shallow root we still have.
func (s *Store) isAncestor(old, new plumbing.Hash) (bool, error) {
	if new.IsZero() {
		return false, nil
	}
	oc, err := s.repo.CommitObject(old)
	if err != nil {
		return false, err
	}
	nc, err := s.repo.CommitObject(new)
	if err != nil {
		return false, err
	}
	ok, err := oc.IsAncestor(nc)
	if errors.Is(err, plumbing.ErrObjectNotFound) && s.depth > 0 {
		// Shallow history ran out before reaching old. Karvan's manifest
		// sequence numbers catch a rewound store anyway (sync-engine.md).
		return true, nil
	}
	return ok, err
}

// ReadFile reads path from the tree of commit head.
func (s *Store) ReadFile(head plumbing.Hash, p string) ([]byte, error) {
	c, err := s.repo.CommitObject(head)
	if err != nil {
		return nil, err
	}
	f, err := c.File(p)
	if err != nil {
		return nil, err
	}
	r, err := f.Reader()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// List returns every file path under dir ("" = all) in commit head.
func (s *Store) List(head plumbing.Hash, dir string) ([]string, error) {
	if head.IsZero() {
		return nil, nil
	}
	c, err := s.repo.CommitObject(head)
	if err != nil {
		return nil, err
	}
	t, err := c.Tree()
	if err != nil {
		return nil, err
	}
	var out []string
	err = t.Files().ForEach(func(f *object.File) error {
		if dir == "" || strings.HasPrefix(f.Name, strings.TrimSuffix(dir, "/")+"/") {
			out = append(out, f.Name)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

// Commit builds a commit whose parent is parent (ZeroHash = root commit) and
// whose tree is parent's tree with changes applied (nil value = delete). No
// worktree, index or checkout is involved.
func (s *Store) Commit(parent plumbing.Hash, changes map[string][]byte, msg string) (plumbing.Hash, error) {
	var base plumbing.Hash
	var parents []plumbing.Hash
	if !parent.IsZero() {
		c, err := s.repo.CommitObject(parent)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("parent commit: %w", err)
		}
		base = c.TreeHash
		parents = []plumbing.Hash{parent}
	}
	tree, err := s.applyTree(base, changes)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if tree.IsZero() { // everything deleted: the root must still be a real (empty) tree
		obj := s.repo.Storer.NewEncodedObject()
		if err := (&object.Tree{}).Encode(obj); err != nil {
			return plumbing.ZeroHash, err
		}
		if tree, err = s.repo.Storer.SetEncodedObject(obj); err != nil {
			return plumbing.ZeroHash, err
		}
	}
	sig := s.who
	sig.When = time.Now()
	c := &object.Commit{Author: sig, Committer: sig, Message: msg, TreeHash: tree, ParentHashes: parents}
	obj := s.repo.Storer.NewEncodedObject()
	if err := c.Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	return s.repo.Storer.SetEncodedObject(obj)
}

// applyTree rewrites the tree at base (ZeroHash = empty) with changes, whose
// keys are slash-separated paths relative to that tree.
func (s *Store) applyTree(base plumbing.Hash, changes map[string][]byte) (plumbing.Hash, error) {
	entries := map[string]object.TreeEntry{}
	if !base.IsZero() {
		t, err := s.repo.TreeObject(base)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		for _, e := range t.Entries {
			entries[e.Name] = e
		}
	}
	// Group changes by first path component.
	sub := map[string]map[string][]byte{}
	for p, data := range changes {
		head, rest, nested := strings.Cut(p, "/")
		if !nested {
			if data == nil {
				delete(entries, head)
				continue
			}
			h, err := s.writeBlob(data)
			if err != nil {
				return plumbing.ZeroHash, err
			}
			entries[head] = object.TreeEntry{Name: head, Mode: filemode.Regular, Hash: h}
			continue
		}
		if sub[head] == nil {
			sub[head] = map[string][]byte{}
		}
		sub[head][rest] = data
	}
	for dir, ch := range sub {
		var childBase plumbing.Hash
		if e, ok := entries[dir]; ok && e.Mode == filemode.Dir {
			childBase = e.Hash
		}
		h, err := s.applyTree(childBase, ch)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		if h.IsZero() {
			delete(entries, dir) // directory became empty
		} else {
			entries[dir] = object.TreeEntry{Name: dir, Mode: filemode.Dir, Hash: h}
		}
	}
	if len(entries) == 0 {
		return plumbing.ZeroHash, nil
	}
	t := &object.Tree{}
	for _, e := range entries {
		t.Entries = append(t.Entries, e)
	}
	// git sorts tree entries by name, with directories compared as "name/".
	sort.Slice(t.Entries, func(i, j int) bool { return sortKey(t.Entries[i]) < sortKey(t.Entries[j]) })
	obj := s.repo.Storer.NewEncodedObject()
	if err := t.Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	return s.repo.Storer.SetEncodedObject(obj)
}

func sortKey(e object.TreeEntry) string {
	if e.Mode == filemode.Dir {
		return e.Name + "/"
	}
	return e.Name
}

func (s *Store) writeBlob(data []byte) (plumbing.Hash, error) {
	obj := s.repo.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	obj.SetSize(int64(len(data)))
	w, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := w.Write(data); err != nil {
		return plumbing.ZeroHash, err
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, err
	}
	return s.repo.Storer.SetEncodedObject(obj)
}

// Push sends newHead to the remote branch, fast-forward only, and only if the
// remote branch is still at expectedOld (compare-and-swap). It never forces.
func (s *Store) Push(ctx context.Context, newHead, expectedOld plumbing.Hash) error {
	if err := s.repo.Storer.SetReference(plumbing.NewHashReference(s.branchRef(), newHead)); err != nil {
		return err
	}
	opts := &git.PushOptions{
		RemoteName: remoteName,
		RefSpecs:   []config.RefSpec{config.RefSpec(fmt.Sprintf("%s:%s", s.branchRef(), s.branchRef()))}, // no "+"
		Auth:       s.auth,
		Force:      false,
	}
	if !expectedOld.IsZero() {
		opts.RequireRemoteRefs = []config.RefSpec{config.RefSpec(fmt.Sprintf("%s:%s", expectedOld, s.branchRef()))}
	}
	err := s.repo.PushContext(ctx, opts)
	// Never leave the local branch pointing at an unpushed commit: go-git
	// advertises local refs as "haves" on the next fetch, and go-git's own
	// in-process server fails that fetch with "object not found" (finding 2 in README).
	defer s.repo.Storer.SetReference(plumbing.NewHashReference(s.branchRef(), s.Head()))
	switch {
	case err == nil:
	case errors.Is(err, git.NoErrAlreadyUpToDate):
		return nil
	case isRejection(err):
		return fmt.Errorf("%w (%v)", ErrRejected, err)
	default:
		return fmt.Errorf("push: %w", err)
	}
	// Our push is now the remote head; record it so the next Commit builds on it.
	return s.repo.Storer.SetReference(plumbing.NewHashReference(s.trackingRef(), newHead))
}

func isRejection(err error) bool {
	if errors.Is(err, git.ErrForceNeeded) {
		return true
	}
	m := err.Error()
	// "failed to update ref" / "cannot lock ref": another push landed between
	// the server's ref advertisement and its ref update (see README).
	for _, needle := range []string{"required to be", "non-fast-forward", "fetch first", "rejected", "stale info",
		"failed to update ref", "cannot lock ref"} {
		if strings.Contains(m, needle) {
			return true
		}
	}
	return false
}

// Update is the store's conditional write as Karvan will use it: fetch, let
// build compute this machine's changes against the fresh remote state, commit
// on top, push fast-forward; on rejection repeat with a fresh fetch. build is
// called again on every attempt because the other machines' manifests may
// have changed. It returns how many attempts were needed.
func (s *Store) Update(ctx context.Context, msg string, maxAttempts int,
	build func(head plumbing.Hash) (map[string][]byte, error)) (int, error) {
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		head, err := s.Fetch(ctx)
		if errors.Is(err, ErrRejected) {
			continue
		}
		if err != nil {
			return attempt, err
		}
		changes, err := build(head)
		if err != nil {
			return attempt, err
		}
		if len(changes) == 0 {
			return attempt, nil
		}
		c, err := s.Commit(head, changes, msg)
		if err != nil {
			return attempt, err
		}
		err = s.Push(ctx, c, head)
		if err == nil {
			return attempt, nil
		}
		if !errors.Is(err, ErrRejected) {
			return attempt, err
		}
		// Back off a little so racing machines spread out (Karvan will add jitter).
		select {
		case <-ctx.Done():
			return attempt, ctx.Err()
		case <-time.After(time.Duration(attempt*attempt) * 20 * time.Millisecond):
		}
	}
	return maxAttempts, fmt.Errorf("gave up after %d attempts: %w", maxAttempts, ErrRejected)
}
