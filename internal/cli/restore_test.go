package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leberman/karvan/internal/config"
	"github.com/leberman/karvan/internal/engine"
	"github.com/leberman/karvan/internal/store"
)

func gitT(t *testing.T, dir string, args ...string) string {
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

func TestRestoreUnfinishedWork(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	remote := filepath.Join(root, "app.git")
	gitT(t, root, "init", "--quiet", "--bare", "-b", "main", remote)
	storeDir := t.TempDir()
	if _, err := engine.InitStore(context.Background(), &store.Folder{Root: storeDir}, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	mk := func(name string) (*App, string) {
		app, _, _ := newApp(t)
		proj := filepath.Join(app.Env.Home, "code", "app")
		gitT(t, root, "clone", "--quiet", remote, proj)
		writeConfig(t, app, func(c *config.Config) {
			c.Machine = name
			c.Store = config.Store{Kind: config.StoreFolder, Path: storeDir, Branch: "main"}
			c.Projects = []config.Project{{ID: "github.com/o/app", Name: "app", Path: proj, WIP: true}}
		})
		return app, proj
	}
	work, wproj := mk("work")
	gitT(t, wproj, "checkout", "--quiet", "-b", "main")
	_ = os.WriteFile(filepath.Join(wproj, "a.go"), []byte("package a\n"), 0o644)
	gitT(t, wproj, "add", ".")
	gitT(t, wproj, "commit", "--quiet", "-m", "init")
	gitT(t, wproj, "push", "--quiet", "-u", "origin", "main")
	home, hproj := mk("home")

	// Clean trees: nothing waiting.
	run(t, work, "sync")
	if out := run(t, home, "sync", "--hook", "session-start"); out != "" {
		t.Fatalf("notice for clean work: %q", out)
	}

	gitT(t, wproj, "checkout", "--quiet", "-b", "feature")
	_ = os.WriteFile(filepath.Join(wproj, "a.go"), []byte("package a\n// wip\n"), 0o644)
	_ = os.WriteFile(filepath.Join(wproj, "b.go"), []byte("package a // new\n"), 0o644)
	run(t, work, "sync")
	out := run(t, home, "sync", "--hook", "session-start")
	if !strings.Contains(out, "unfinished work from work") || !strings.Contains(out, "branch feature") || !strings.Contains(out, "karvan restore") {
		t.Fatalf("no notice:\n%s", out)
	}

	// Without --yes and with "n": nothing changes.
	var buf bytes.Buffer
	home.Stdout = &buf
	if err := runRestore(context.Background(), home, strings.NewReader("n\n"), &buf, "", "github.com/o/app", false); err != nil {
		t.Fatal(err)
	}
	if gitT(t, hproj, "rev-parse", "--abbrev-ref", "HEAD") == "feature" {
		t.Fatal("restored without consent")
	}
	out = run(t, home, "restore", "--yes", "--project", "github.com/o/app")
	if !strings.Contains(out, "Done: you are on branch feature") {
		t.Fatalf("%s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(hproj, "a.go")); string(b) != "package a\n// wip\n" {
		t.Fatalf("a.go = %q", b)
	}
	if _, err := os.Stat(filepath.Join(hproj, "b.go")); err != nil {
		t.Fatal("new file not restored")
	}
	// Once restored, the notice is gone.
	if out := run(t, home, "sync", "--hook", "session-start"); strings.Contains(out, "unfinished work from work") {
		t.Fatalf("notice after restore:\n%s", out)
	}
	// When work becomes clean, its snapshot disappears from the store.
	gitT(t, wproj, "add", ".")
	gitT(t, wproj, "commit", "--quiet", "-m", "done")
	gitT(t, wproj, "push", "--quiet", "-u", "origin", "feature")
	run(t, work, "sync")
	_ = os.Remove(filepath.Join(home.Env.StateDir(), "restored.json"))
	if out := run(t, home, "sync", "--hook", "session-start"); strings.Contains(out, "unfinished work") {
		t.Fatalf("stale snapshot still announced:\n%s", out)
	}
}
