// Package wip saves and restores unfinished git work in a project (PRD
// 6-5): branch, HEAD, a bundle of commits no remote has, staged and
// unstaged diffs, and untracked (not ignored) files. It runs the system's
// git; it never commits, pushes, stashes or resets in the user's project.
package wip

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// MaxUntracked is the most untracked data a snapshot carries.
const MaxUntracked = 20 << 20

// Meta describes one snapshot.
type Meta struct {
	Machine   string    `json:"machine"`
	ProjectID string    `json:"projectId"`
	Taken     time.Time `json:"taken"`
	Branch    string    `json:"branch,omitempty"` // "" = detached HEAD
	Head      string    `json:"head"`
	Bundle    bool      `json:"bundle,omitempty"`
	Staged    bool      `json:"staged,omitempty"`
	Unstaged  bool      `json:"unstaged,omitempty"`
	Untracked []string  `json:"untracked,omitempty"`
	Unpushed  int       `json:"unpushed,omitempty"`
}

// Files of a snapshot, by name.
const (
	FileMeta      = "meta.json"
	FileBundle    = "commits.bundle"
	FileStaged    = "staged.patch"
	FileUnstaged  = "unstaged.patch"
	FileUntracked = "untracked.tar"
)

// ErrInProgress means a merge, rebase or similar is in progress.
var ErrInProgress = errors.New("a merge, rebase, cherry-pick or revert is in progress; finish or abort it first (it cannot be carried to another machine)")

// ErrNoGit means the git command is not installed.
var ErrNoGit = errors.New("the git command is needed for unfinished project work; install git")

func git(ctx context.Context, dir string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, ErrNoGit
		}
		return out.Bytes(), fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

func gitDir(ctx context.Context, dir string) (string, error) {
	out, err := git(ctx, dir, nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func inProgress(gd string) bool {
	for _, f := range []string{"MERGE_HEAD", "rebase-merge", "rebase-apply", "CHERRY_PICK_HEAD", "REVERT_HEAD", "BISECT_LOG"} {
		if _, err := os.Stat(filepath.Join(gd, f)); err == nil {
			return true
		}
	}
	return false
}

// Save takes a snapshot of dir into out (a folder it fills with the files
// above). It returns nil Meta, and empties out, when there is nothing
// unfinished.
func Save(ctx context.Context, dir, out, machine, projectID string, now time.Time) (*Meta, error) {
	gd, err := gitDir(ctx, dir)
	if err != nil {
		return nil, err
	}
	if inProgress(gd) {
		return nil, ErrInProgress
	}
	head, err := git(ctx, dir, nil, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("no commit yet: %w", err)
	}
	m := &Meta{Machine: machine, ProjectID: projectID, Taken: now.UTC(), Head: strings.TrimSpace(string(head))}
	if b, err := git(ctx, dir, nil, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		m.Branch = strings.TrimSpace(string(b))
	}
	files := map[string][]byte{}

	// Commits no remote has.
	if b, err := git(ctx, dir, nil, "rev-list", "--count", "HEAD", "--not", "--remotes"); err == nil {
		_, _ = fmt.Sscan(string(b), &m.Unpushed)
	}
	if m.Unpushed > 0 {
		tmp := filepath.Join(out, ".bundle-tmp")
		if err := os.MkdirAll(out, 0o700); err != nil {
			return nil, err
		}
		ref := "HEAD"
		if m.Branch != "" {
			ref = "refs/heads/" + m.Branch
		}
		if _, err := git(ctx, dir, nil, "bundle", "create", tmp, ref, "--not", "--remotes"); err != nil {
			return nil, err
		}
		b, err := os.ReadFile(tmp)
		_ = os.Remove(tmp)
		if err != nil {
			return nil, err
		}
		files[FileBundle] = b
		m.Bundle = true
	}
	if b, err := git(ctx, dir, nil, "diff", "--cached", "--binary"); err != nil {
		return nil, err
	} else if len(b) > 0 {
		files[FileStaged], m.Staged = b, true
	}
	if b, err := git(ctx, dir, nil, "diff", "--binary"); err != nil {
		return nil, err
	} else if len(b) > 0 {
		files[FileUnstaged], m.Unstaged = b, true
	}
	b, err := git(ctx, dir, nil, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	var untracked []string
	for _, f := range strings.Split(string(b), "\x00") {
		if f != "" {
			untracked = append(untracked, f)
		}
	}
	if len(untracked) > 0 {
		tb, err := tarFiles(dir, untracked)
		if err != nil {
			return nil, err
		}
		files[FileUntracked], m.Untracked = tb, untracked
	}

	_ = os.RemoveAll(out)
	if len(files) == 0 {
		return nil, nil // clean tree, nothing unpushed: nothing to carry
	}
	if err := os.MkdirAll(out, 0o700); err != nil {
		return nil, err
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(out, name), data, 0o600); err != nil {
			return nil, err
		}
	}
	mb, _ := json.MarshalIndent(m, "", "  ")
	return m, os.WriteFile(filepath.Join(out, FileMeta), mb, 0o600)
}

func tarFiles(dir string, names []string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	total := 0
	for _, n := range names {
		p := filepath.Join(dir, filepath.FromSlash(n))
		info, err := os.Lstat(p)
		if err != nil || !info.Mode().IsRegular() {
			continue // symlinks and specials are skipped
		}
		total += int(info.Size())
		if total > MaxUntracked {
			return nil, fmt.Errorf("untracked files are larger than %d MB; add large ones to .gitignore", MaxUntracked>>20)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: int64(info.Mode().Perm()), Size: int64(len(data)), ModTime: info.ModTime()}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Snapshot is a snapshot read back from the store.
type Snapshot struct {
	Meta  Meta
	Files map[string][]byte
}

// ErrDirty means the project has local changes that no snapshot holds.
var ErrDirty = errors.New("this project has uncommitted changes here; commit or stash them yourself first (Karvan will not overwrite them)")

// Restore brings a snapshot into dir: fetch (from the remote, and from the
// bundle if needed), switch to the branch, fast-forward, re-apply staged and
// unstaged changes, and add untracked files. progress gets one line per
// step that may take time.
func Restore(ctx context.Context, dir string, s *Snapshot, progress func(string)) error {
	gd, err := gitDir(ctx, dir)
	if err != nil {
		return err
	}
	if inProgress(gd) {
		return ErrInProgress
	}
	if b, err := git(ctx, dir, nil, "status", "--porcelain"); err != nil {
		return err
	} else if len(bytes.TrimSpace(b)) > 0 {
		return ErrDirty
	}
	for _, f := range s.Meta.Untracked {
		if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(f))); err == nil {
			return fmt.Errorf("%s already exists here; move it away first", f)
		}
	}
	m := s.Meta
	if !hasCommit(ctx, dir, m.Head) {
		progress("Fetching from the project's remote...")
		if _, err := git(ctx, dir, nil, "fetch", "--all", "--quiet"); err != nil {
			progress("Could not fetch (" + err.Error() + "); using the saved commits only.")
		}
	}
	if !hasCommit(ctx, dir, m.Head) && m.Bundle {
		tmp := filepath.Join(gd, "karvan-restore.bundle")
		if err := os.WriteFile(tmp, s.Files[FileBundle], 0o600); err != nil {
			return err
		}
		defer os.Remove(tmp)
		if _, err := git(ctx, dir, nil, "fetch", "--quiet", tmp, m.Head); err != nil {
			// Older git cannot fetch a commit by hash from a bundle: fetch its refs.
			if _, err2 := git(ctx, dir, nil, "bundle", "unbundle", tmp); err2 != nil {
				return fmt.Errorf("saved commits: %w", err)
			}
		}
	}
	if !hasCommit(ctx, dir, m.Head) {
		return fmt.Errorf("commit %s is not available: push it from %s, or let Karvan sync there first", m.Head[:8], m.Machine)
	}
	switch {
	case m.Branch == "":
		if _, err := git(ctx, dir, nil, "checkout", "--quiet", "--detach", m.Head); err != nil {
			return err
		}
	case hasRef(ctx, dir, "refs/heads/"+m.Branch):
		if _, err := git(ctx, dir, nil, "checkout", "--quiet", m.Branch); err != nil {
			return err
		}
		if _, err := git(ctx, dir, nil, "merge", "--ff-only", "--quiet", m.Head); err != nil {
			return fmt.Errorf("branch %s here has commits the other machine does not; not touching it: %w", m.Branch, err)
		}
	default:
		if _, err := git(ctx, dir, nil, "checkout", "--quiet", "-b", m.Branch, m.Head); err != nil {
			return err
		}
	}
	if m.Staged {
		if _, err := git(ctx, dir, s.Files[FileStaged], "apply", "--index", "--whitespace=nowarn", "-"); err != nil {
			return fmt.Errorf("staged changes: %w", err)
		}
	}
	if m.Unstaged {
		if _, err := git(ctx, dir, s.Files[FileUnstaged], "apply", "--whitespace=nowarn", "-"); err != nil {
			return fmt.Errorf("unstaged changes: %w", err)
		}
	}
	if len(m.Untracked) > 0 {
		if err := untar(dir, s.Files[FileUntracked]); err != nil {
			return fmt.Errorf("untracked files: %w", err)
		}
	}
	return nil
}

func hasCommit(ctx context.Context, dir, h string) bool {
	_, err := git(ctx, dir, nil, "cat-file", "-e", h+"^{commit}")
	return err == nil
}

func hasRef(ctx context.Context, dir, ref string) bool {
	_, err := git(ctx, dir, nil, "show-ref", "--verify", "--quiet", ref)
	return err == nil
}

func untar(dir string, data []byte) error {
	tr := tar.NewReader(bytes.NewReader(data))
	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		p := filepath.Join(root, filepath.FromSlash(h.Name))
		if !strings.HasPrefix(p, root+string(filepath.Separator)) {
			return fmt.Errorf("unsafe path %q in snapshot", h.Name)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(h.Mode)&0o777)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// ParseMeta reads a snapshot's meta.json.
func ParseMeta(b []byte) (Meta, error) {
	var m Meta
	err := json.Unmarshal(b, &m)
	return m, err
}
