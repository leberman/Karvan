package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// Git is a store in a git repository, used only as a blob store
// (docs/sync-engine.md, 13-1): fetch, build one commit on top of the remote
// head without a worktree, push fast-forward only. Never merges, never forces.
// No git binary is needed. Lessons from spikes/gogit are applied here.
type Git struct {
	URL     string
	Branch  string
	SSHKey  string // private key file; "" = ssh-agent, then ~/.ssh/id_ed25519, ~/.ssh/id_rsa
	Dir     string // local bare clone (a cache; safe to delete)
	Machine string // commit author name only
	Timeout time.Duration
	// BatchBytes caps the new data in one commit (default 32 MiB; go-git
	// builds the whole pack in memory).
	BatchBytes int

	repo *git.Repository
	auth transport.AuthMethod
}

const gitRemote = "origin"

// Describe implements Store.
func (g *Git) Describe() string { return fmt.Sprintf("git %s (branch %s)", g.URL, g.Branch) }

func (g *Git) branchRef() plumbing.ReferenceName { return plumbing.NewBranchReferenceName(g.Branch) }
func (g *Git) trackingRef() plumbing.ReferenceName {
	return plumbing.NewRemoteReferenceName(gitRemote, g.Branch)
}

func (g *Git) open() error {
	if g.repo != nil {
		return nil
	}
	if g.Branch == "" {
		g.Branch = "main"
	}
	if err := os.MkdirAll(g.Dir, 0o700); err != nil {
		return err
	}
	st := filesystem.NewStorage(osfs.New(g.Dir), cache.NewObjectLRUDefault())
	repo, err := git.Open(st, nil)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		repo, err = git.Init(st, nil)
	}
	if err != nil {
		return fmt.Errorf("local copy of the store (%s): %w", g.Dir, err)
	}
	cfg, err := repo.Config()
	if err != nil {
		return err
	}
	// Blobs are compressed: delta search only costs time (spike finding 3).
	cfg.Pack.Window = 0
	rc := &config.RemoteConfig{Name: gitRemote, URLs: []string{g.URL}}
	cfg.Remotes[gitRemote] = rc
	if err := repo.SetConfig(cfg); err != nil {
		return err
	}
	g.repo = repo
	if strings.HasPrefix(g.URL, "file://") || !strings.Contains(g.URL, "://") && !strings.Contains(g.URL, "@") {
		return nil
	}
	if strings.HasPrefix(g.URL, "https://") || strings.HasPrefix(g.URL, "http://") {
		return nil // public or credential-helper-free HTTPS is not supported for writing; ssh is
	}
	auth, err := SSHAuth(g.URL, g.SSHKey)
	if err != nil {
		return err
	}
	g.auth = auth
	return nil
}

// SSHAuth builds SSH authentication for url: the key file if given, else
// ssh-agent, else ~/.ssh/id_ed25519 or ~/.ssh/id_rsa. Host keys come from
// known_hosts, and the key types recorded there are offered to the server,
// which go-git v5.19 forgets to do (spike finding 6).
func SSHAuth(url, keyFile string) (transport.AuthMethod, error) {
	var auth transport.AuthMethod
	var err error
	user := "git"
	if ep, e := transport.NewEndpoint(url); e == nil && ep.User != "" {
		user = ep.User
	}
	switch {
	case keyFile != "":
		auth, err = gitssh.NewPublicKeysFromFile(user, keyFile, os.Getenv("KARVAN_SSH_PASSPHRASE"))
	case os.Getenv("SSH_AUTH_SOCK") != "":
		auth, err = gitssh.NewSSHAgentAuth(user)
	}
	if auth == nil && keyFile == "" {
		home, _ := os.UserHomeDir()
		for _, k := range []string{"id_ed25519", "id_rsa"} {
			p := home + "/.ssh/" + k
			if _, e := os.Stat(p); e == nil {
				auth, err = gitssh.NewPublicKeysFromFile(user, p, os.Getenv("KARVAN_SSH_PASSPHRASE"))
				break
			}
		}
		if auth == nil && err == nil {
			err = errors.New("no SSH key: start ssh-agent, or set store.ssh_key in the config")
		}
	}
	if err != nil {
		return nil, fmt.Errorf("SSH key: %w", err)
	}
	ep, err := transport.NewEndpoint(url)
	if err != nil {
		return nil, err
	}
	port := ep.Port
	if port == 0 {
		port = 22
	}
	hostPort := net.JoinHostPort(ep.Host, strconv.Itoa(port))
	db, err := gitssh.NewKnownHostsDb()
	if err != nil {
		return nil, fmt.Errorf("known_hosts: %w", err)
	}
	algos := db.HostKeyAlgorithms(hostPort)
	if len(algos) == 0 {
		hint := "ssh -T git@" + ep.Host
		if port != 22 {
			hint = fmt.Sprintf("ssh -T -p %d git@%s", port, ep.Host)
		}
		return nil, fmt.Errorf("%s is not in ~/.ssh/known_hosts; run `%s` once and check the fingerprint", hostPort, hint)
	}
	switch a := auth.(type) {
	case *gitssh.PublicKeysCallback:
		a.HostKeyCallback, a.HostKeyAlgorithms = db.HostKeyCallback(), algos
	case *gitssh.PublicKeys:
		a.HostKeyCallback, a.HostKeyAlgorithms = db.HostKeyCallback(), algos
	}
	return auth, nil
}

func (g *Git) ctx(ctx context.Context) (context.Context, context.CancelFunc) {
	t := g.Timeout
	if t == 0 {
		t = 2 * time.Minute
	}
	return context.WithTimeout(ctx, t)
}

// remoteHead asks where the branch is (ZeroHash if it does not exist).
func (g *Git) remoteHead(ctx context.Context) (plumbing.Hash, error) {
	remote, err := g.repo.Remote(gitRemote)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	c, cancel := g.ctx(ctx)
	defer cancel()
	refs, err := remote.ListContext(c, &git.ListOptions{Auth: g.auth})
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return plumbing.ZeroHash, nil
	}
	if err != nil {
		return plumbing.ZeroHash, networkError("read", g.URL, err)
	}
	for _, r := range refs {
		if r.Name() == g.branchRef() {
			return r.Hash(), nil
		}
	}
	return plumbing.ZeroHash, nil
}

func networkError(what, url string, err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() || errors.Is(err, context.DeadlineExceeded) {
		msg := fmt.Sprintf("could not %s %s in time; will try again", what, url)
		if strings.Contains(url, "github.com") && !strings.Contains(url, ":443") {
			msg += ". If this network blocks SSH on port 22, use ssh://git@ssh.github.com:443/<owner>/<repo>.git"
		}
		return fmt.Errorf("%s: %w", msg, err)
	}
	return fmt.Errorf("could not %s %s: %w", what, url, err)
}

func (g *Git) head() plumbing.Hash {
	ref, err := g.repo.Reference(g.trackingRef(), true)
	if err != nil {
		return plumbing.ZeroHash
	}
	return ref.Hash()
}

// Snapshot implements Store: ls-remote, fetch what is missing, check that
// the branch did not go back in time (spike findings 1 and 2).
func (g *Git) Snapshot(ctx context.Context) (Snapshot, error) {
	if err := g.open(); err != nil {
		return nil, err
	}
	before := g.head()
	head, err := g.remoteHead(ctx)
	if err != nil {
		return nil, err
	}
	if head.IsZero() {
		if !before.IsZero() {
			return nil, ErrRewound
		}
		return &gitSnap{}, nil
	}
	if _, err := g.repo.CommitObject(head); err != nil {
		c, cancel := g.ctx(ctx)
		defer cancel()
		spec := config.RefSpec(fmt.Sprintf("refs/heads/%s:%s", g.Branch, g.trackingRef()))
		// Full fetch, not shallow: go-git's repeated depth-1 fetches into an
		// already shallow clone fail with "object not found" after a few
		// rounds (finding 9, found by the end-to-end test).
		err = g.repo.FetchContext(c, &git.FetchOptions{RemoteName: gitRemote, RefSpecs: []config.RefSpec{spec},
			Auth: g.auth, Tags: git.NoTags})
		if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
			return nil, networkError("fetch", g.URL, err)
		}
		if _, err := g.repo.CommitObject(head); err != nil {
			return nil, ErrConflict // moved between ls-remote and fetch
		}
	}
	if !before.IsZero() && before != head {
		if ok, err := g.isAncestor(before, head); err == nil && !ok {
			return nil, ErrRewound
		}
	}
	if err := g.repo.Storer.SetReference(plumbing.NewHashReference(g.trackingRef(), head)); err != nil {
		return nil, err
	}
	commit, err := g.repo.CommitObject(head)
	if err != nil {
		return nil, err
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, err
	}
	return &gitSnap{head: head, tree: tree}, nil
}

func (g *Git) isAncestor(old, new plumbing.Hash) (bool, error) {
	oc, err := g.repo.CommitObject(old)
	if err != nil {
		return true, nil // not in the shallow history: cannot tell
	}
	nc, err := g.repo.CommitObject(new)
	if err != nil {
		return false, err
	}
	ok, err := oc.IsAncestor(nc)
	if err != nil {
		return true, nil // shallow history ran out; manifest seq numbers still catch a rewind
	}
	return ok, nil
}

type gitSnap struct {
	head plumbing.Hash
	tree *object.Tree
}

func (s *gitSnap) List(prefix string) ([]string, error) {
	if s.tree == nil {
		return nil, nil
	}
	var out []string
	t := s.tree
	dir := prefix
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		dir = prefix[:i]
		sub, err := s.tree.Tree(dir)
		if errors.Is(err, object.ErrDirectoryNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		t = sub
		dir += "/"
	} else {
		dir = ""
	}
	err := t.Files().ForEach(func(f *object.File) error {
		p := dir + f.Name
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

func (s *gitSnap) Get(p string) ([]byte, error) {
	if s.tree == nil {
		return nil, ErrNotFound
	}
	f, err := s.tree.File(p)
	if errors.Is(err, object.ErrFileNotFound) || errors.Is(err, object.ErrDirectoryNotFound) {
		return nil, ErrNotFound
	}
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

// Commit implements Store. Large uploads go in several commits (blobs
// first); only the last one carries the manifests.
func (g *Git) Commit(ctx context.Context, base Snapshot, puts map[string][]byte, deletes []string, message string) error {
	if err := g.open(); err != nil {
		return err
	}
	bs, ok := base.(*gitSnap)
	if !ok {
		return errors.New("snapshot from another store")
	}
	limit := g.BatchBytes
	if limit == 0 {
		limit = 32 << 20
	}
	var batches []map[string][]byte
	cur, size := map[string][]byte{}, 0
	for _, p := range orderPuts(puts) {
		if strings.HasPrefix(p, "blobs/") && size > 0 && size+len(puts[p]) > limit {
			batches = append(batches, cur)
			cur, size = map[string][]byte{}, 0
		}
		cur[p] = puts[p]
		size += len(puts[p])
	}
	batches = append(batches, cur)
	parent := bs.head
	for i, b := range batches {
		var dels []string
		msg := message
		if i == len(batches)-1 {
			dels = deletes
		} else {
			msg = fmt.Sprintf("%s (part %d/%d)", message, i+1, len(batches))
		}
		if len(b) == 0 && len(dels) == 0 {
			continue
		}
		c, err := g.commit(parent, b, dels, msg)
		if err != nil {
			return err
		}
		if err := g.push(ctx, c, parent); err != nil {
			return err
		}
		parent = c
	}
	return nil
}

func (g *Git) commit(parent plumbing.Hash, puts map[string][]byte, deletes []string, msg string) (plumbing.Hash, error) {
	changes := map[string][]byte{}
	for p, d := range puts {
		changes[p] = d
	}
	for _, p := range deletes {
		changes[p] = nil
	}
	var baseTree plumbing.Hash
	var parents []plumbing.Hash
	if !parent.IsZero() {
		c, err := g.repo.CommitObject(parent)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		baseTree, parents = c.TreeHash, []plumbing.Hash{parent}
	}
	tree, err := g.applyTree(baseTree, changes)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if tree.IsZero() {
		if tree, err = g.store(&object.Tree{}); err != nil {
			return plumbing.ZeroHash, err
		}
	}
	// No personal data: a fixed invalid address.
	sig := object.Signature{Name: "Karvan (" + g.Machine + ")", Email: "karvan@invalid", When: time.Now()}
	return g.store(&object.Commit{Author: sig, Committer: sig, Message: msg, TreeHash: tree, ParentHashes: parents})
}

type encoder interface {
	Encode(plumbing.EncodedObject) error
}

func (g *Git) store(o encoder) (plumbing.Hash, error) {
	obj := g.repo.Storer.NewEncodedObject()
	if err := o.Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	return g.repo.Storer.SetEncodedObject(obj)
}

func (g *Git) applyTree(base plumbing.Hash, changes map[string][]byte) (plumbing.Hash, error) {
	entries := map[string]object.TreeEntry{}
	if !base.IsZero() {
		t, err := g.repo.TreeObject(base)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		for _, e := range t.Entries {
			entries[e.Name] = e
		}
	}
	sub := map[string]map[string][]byte{}
	for p, data := range changes {
		head, rest, nested := strings.Cut(p, "/")
		if !nested {
			if data == nil {
				delete(entries, head)
				continue
			}
			obj := g.repo.Storer.NewEncodedObject()
			obj.SetType(plumbing.BlobObject)
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
			h, err := g.repo.Storer.SetEncodedObject(obj)
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
		h, err := g.applyTree(childBase, ch)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		if h.IsZero() {
			delete(entries, dir)
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
	key := func(e object.TreeEntry) string {
		if e.Mode == filemode.Dir {
			return e.Name + "/"
		}
		return e.Name
	}
	sort.Slice(t.Entries, func(i, j int) bool { return key(t.Entries[i]) < key(t.Entries[j]) })
	return g.store(t)
}

// push sends newHead fast-forward only and only if the branch is still at
// expectedOld. Never forces.
func (g *Git) push(ctx context.Context, newHead, expectedOld plumbing.Hash) error {
	if err := g.repo.Storer.SetReference(plumbing.NewHashReference(g.branchRef(), newHead)); err != nil {
		return err
	}
	opts := &git.PushOptions{
		RemoteName: gitRemote,
		RefSpecs:   []config.RefSpec{config.RefSpec(fmt.Sprintf("%s:%s", g.branchRef(), g.branchRef()))},
		Auth:       g.auth,
	}
	if !expectedOld.IsZero() {
		opts.RequireRemoteRefs = []config.RefSpec{config.RefSpec(fmt.Sprintf("%s:%s", expectedOld, g.branchRef()))}
	}
	c, cancel := g.ctx(ctx)
	defer cancel()
	err := g.repo.PushContext(c, opts)
	// Never leave the local branch on an unpushed commit (spike finding 2).
	defer func() { _ = g.repo.Storer.SetReference(plumbing.NewHashReference(g.branchRef(), g.head())) }()
	switch {
	case err == nil, errors.Is(err, git.NoErrAlreadyUpToDate):
	case errors.Is(err, git.ErrForceNeeded), strings.Contains(err.Error(), "required to be"):
		return fmt.Errorf("%w: %w", ErrConflict, err)
	default:
		// Server-side refusal wording varies by git version (spike finding
		// 8): look at the branch instead of parsing the message.
		now, lerr := g.remoteHead(ctx)
		switch {
		case lerr != nil:
			return networkError("send to", g.URL, err)
		case now == newHead:
		case now != expectedOld:
			return fmt.Errorf("%w: %w", ErrConflict, err)
		default:
			return networkError("send to", g.URL, err)
		}
	}
	return g.repo.Storer.SetReference(plumbing.NewHashReference(g.trackingRef(), newHead))
}
