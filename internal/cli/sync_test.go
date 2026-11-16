package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leberman/karvan/internal/config"
	"github.com/leberman/karvan/internal/engine"
	"github.com/leberman/karvan/internal/store"
)

// twoMachines sets up "work" and "home" sharing one folder store. The same
// project lives at different paths on each.
func twoMachines(t *testing.T) (work, home *App, workProj, homeProj string) {
	t.Helper()
	storeDir := t.TempDir()
	if _, err := engine.InitStore(context.Background(), &store.Folder{Root: storeDir}, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	mk := func(name, projRel string) (*App, string) {
		app, _, _ := newApp(t)
		proj := filepath.Join(app.Env.Home, projRel)
		mkdir(t, proj)
		writeConfig(t, app, func(c *config.Config) {
			c.Machine = name
			c.Store = config.Store{Kind: config.StoreFolder, Path: storeDir, Branch: "main"}
			c.Projects = []config.Project{{ID: "github.com/o/app", Name: "app", Path: proj}}
			c.Items.Folders = []config.Folder{{Name: "notes", Path: filepath.Join(app.Env.Home, "notes")}}
		})
		return app, proj
	}
	work, workProj = mk("work", "work/app")
	home, homeProj = mk("home", "temp/app")
	return
}

func put(t *testing.T, p, content string) {
	t.Helper()
	mkdir(t, filepath.Dir(p))
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func get(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

func run(t *testing.T, app *App, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	app.Stdout, app.Stderr = &out, &out
	if code := Run(app, args); code != ExitOK {
		t.Fatalf("karvan %v: exit %d\n%s", args, code, out.String())
	}
	return out.String()
}

func TestSyncSharedFiles(t *testing.T) {
	work, home, workProj, homeProj := twoMachines(t)
	put(t, filepath.Join(work.Env.ClaudeHome(), "CLAUDE.md"), "be brief\n")
	put(t, filepath.Join(work.Env.ClaudeHome(), "CLAUDE.machine.md"), "work only\n")
	put(t, filepath.Join(work.Env.ClaudeHome(), "skills", "review", "SKILL.md"), "review\n")
	put(t, filepath.Join(work.Env.ClaudeProjectDir(workProj), "memory", "MEMORY.md"), "the API uses JWT\n")
	put(t, filepath.Join(work.Env.Home, "notes", "todo.md"), "- ship\n")

	out := run(t, work, "sync")
	if !strings.Contains(out, "4 local change(s) sent") {
		t.Fatalf("work: %s", out)
	}
	out = run(t, home, "sync")
	if !strings.Contains(out, "4 received") {
		t.Fatalf("home: %s", out)
	}
	if get(filepath.Join(home.Env.ClaudeHome(), "CLAUDE.md")) != "be brief\n" ||
		get(filepath.Join(home.Env.ClaudeHome(), "skills", "review", "SKILL.md")) != "review\n" ||
		get(filepath.Join(home.Env.Home, "notes", "todo.md")) != "- ship\n" {
		t.Fatal("shared files not received")
	}
	// Memory lands in home's own folder for the project's path there.
	if get(filepath.Join(home.Env.ClaudeProjectDir(homeProj), "memory", "MEMORY.md")) != "the API uses JWT\n" {
		t.Fatalf("memory not placed under %s", home.Env.ClaudeProjectDir(homeProj))
	}
	if get(filepath.Join(home.Env.ClaudeHome(), "CLAUDE.machine.md")) != "<missing>" {
		t.Fatal("CLAUDE.machine.md must never travel")
	}
	out = run(t, home, "status")
	for _, want := range []string{"last sync:    just now (manual, ok)", "work", "last sync just now"} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q:\n%s", want, out)
		}
	}
	if out := run(t, home, "sync"); !strings.Contains(out, "everything is up to date") {
		t.Fatalf("second run: %s", out)
	}
}

func TestHooksQuiet(t *testing.T) {
	app, out, _ := newApp(t)
	// Not set up yet: hooks and timer say nothing and succeed.
	for _, args := range [][]string{{"sync", "--hook", "session-start"}, {"sync", "--hook", "session-end"}, {"sync", "--timer"}} {
		if code := Run(app, args); code != ExitOK || out.Len() != 0 {
			t.Fatalf("%v: exit %d %q", args, code, out)
		}
	}
	work, _, _, _ := twoMachines(t)
	put(t, filepath.Join(work.Env.ClaudeHome(), "CLAUDE.md"), "x\n")
	if o := run(t, work, "sync", "--timer"); o != "" {
		t.Fatalf("timer printed %q", o)
	}
	if o := run(t, work, "sync", "--hook", "session-start"); o != "" {
		t.Fatalf("hook printed %q", o)
	}
}
