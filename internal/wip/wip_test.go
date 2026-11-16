package wip

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sh(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func setup(t *testing.T) (work, home string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	sh(t, root, "init", "--quiet", "--bare", "-b", "main", remote)
	work = filepath.Join(root, "work")
	sh(t, root, "clone", "--quiet", remote, work)
	sh(t, work, "checkout", "--quiet", "-b", "main")
	_ = os.WriteFile(filepath.Join(work, "app.go"), []byte("package app\n"), 0o644)
	_ = os.WriteFile(filepath.Join(work, ".gitignore"), []byte("*.log\n"), 0o644)
	sh(t, work, "add", ".")
	sh(t, work, "commit", "--quiet", "-m", "init")
	sh(t, work, "push", "--quiet", "-u", "origin", "main")
	home = filepath.Join(root, "home")
	sh(t, root, "clone", "--quiet", remote, home)
	return work, home
}

func load(t *testing.T, dir string) *Snapshot {
	t.Helper()
	s := &Snapshot{Files: map[string][]byte{}}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		s.Files[e.Name()] = b
	}
	if err := jsonUnmarshal(s.Files[FileMeta], &s.Meta); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSaveRestoreEverything(t *testing.T) {
	ctx := context.Background()
	work, home := setup(t)
	snap := filepath.Join(t.TempDir(), "snap")

	// Clean: nothing to carry.
	if m, err := Save(ctx, work, snap, "work", "p", time.Now()); err != nil || m != nil {
		t.Fatalf("clean tree: %v %v", m, err)
	}

	sh(t, work, "checkout", "--quiet", "-b", "feature")
	_ = os.WriteFile(filepath.Join(work, "app.go"), []byte("package app\n\nfunc A() {}\n"), 0o644)
	sh(t, work, "commit", "--quiet", "-am", "local only")
	_ = os.WriteFile(filepath.Join(work, "staged.go"), []byte("package app // staged\n"), 0o644)
	sh(t, work, "add", "staged.go")
	_ = os.WriteFile(filepath.Join(work, "app.go"), []byte("package app\n\nfunc A() { /* wip */ }\n"), 0o644)
	_ = os.WriteFile(filepath.Join(work, "notes.txt"), []byte("todo\n"), 0o644)
	_ = os.WriteFile(filepath.Join(work, "debug.log"), []byte("ignored\n"), 0o644)

	m, err := Save(ctx, work, snap, "work", "github.com/o/app", time.Now())
	if err != nil || m == nil {
		t.Fatal(m, err)
	}
	if m.Branch != "feature" || !m.Bundle || m.Unpushed != 1 || !m.Staged || !m.Unstaged || len(m.Untracked) != 1 || m.Untracked[0] != "notes.txt" {
		t.Fatalf("%+v", m)
	}

	var steps []string
	if err := Restore(ctx, home, load(t, snap), func(s string) { steps = append(steps, s) }); err != nil {
		t.Fatal(err)
	}
	if sh(t, home, "rev-parse", "--abbrev-ref", "HEAD") != "feature" || sh(t, home, "rev-parse", "HEAD") != m.Head {
		t.Fatal("branch/head")
	}
	if b, _ := os.ReadFile(filepath.Join(home, "app.go")); string(b) != "package app\n\nfunc A() { /* wip */ }\n" {
		t.Fatalf("unstaged: %q", b)
	}
	if !strings.Contains(sh(t, home, "diff", "--cached", "--name-only"), "staged.go") {
		t.Fatal("staged change not staged")
	}
	if b, _ := os.ReadFile(filepath.Join(home, "notes.txt")); string(b) != "todo\n" {
		t.Fatal("untracked file")
	}
	if _, err := os.Stat(filepath.Join(home, "debug.log")); err == nil {
		t.Fatal("ignored file carried")
	}
	// Restoring again refuses to touch the now-dirty tree.
	if err := Restore(ctx, home, load(t, snap), func(string) {}); !errors.Is(err, ErrDirty) {
		t.Fatalf("err = %v", err)
	}
}

// PRD 15 #2: a new branch without a commit of its own.
func TestNewBranchWithoutCommits(t *testing.T) {
	ctx := context.Background()
	work, home := setup(t)
	snap := filepath.Join(t.TempDir(), "snap")
	sh(t, work, "checkout", "--quiet", "-b", "empty-branch")
	_ = os.WriteFile(filepath.Join(work, "new.go"), []byte("package app\n"), 0o644)
	m, err := Save(ctx, work, snap, "work", "p", time.Now())
	if err != nil || m == nil || m.Bundle {
		t.Fatalf("%+v %v", m, err)
	}
	if err := Restore(ctx, home, load(t, snap), func(string) {}); err != nil {
		t.Fatal(err)
	}
	if sh(t, home, "rev-parse", "--abbrev-ref", "HEAD") != "empty-branch" {
		t.Fatal("branch not created")
	}
	if _, err := os.Stat(filepath.Join(home, "new.go")); err != nil {
		t.Fatal(err)
	}
}

// PRD 15 #8: a merge in progress is not saved, and says why.
func TestMergeInProgress(t *testing.T) {
	ctx := context.Background()
	work, _ := setup(t)
	sh(t, work, "checkout", "--quiet", "-b", "b1")
	_ = os.WriteFile(filepath.Join(work, "app.go"), []byte("package app // b1\n"), 0o644)
	sh(t, work, "commit", "--quiet", "-am", "b1")
	sh(t, work, "checkout", "--quiet", "main")
	_ = os.WriteFile(filepath.Join(work, "app.go"), []byte("package app // main\n"), 0o644)
	sh(t, work, "commit", "--quiet", "-am", "main")
	cmd := exec.Command("git", "merge", "b1")
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@invalid")
	_ = cmd.Run() // conflicts
	if _, err := Save(ctx, work, filepath.Join(t.TempDir(), "s"), "work", "p", time.Now()); !errors.Is(err, ErrInProgress) {
		t.Fatalf("err = %v", err)
	}
}
