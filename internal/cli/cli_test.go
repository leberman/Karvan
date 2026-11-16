package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leberman/karvan/internal/config"
	"github.com/leberman/karvan/internal/hooks"
	"github.com/leberman/karvan/internal/paths"
)

// newApp returns an App whose home directory is a fresh temp dir, so nothing
// touches the real ~/.claude or ~/.config.
func newApp(t *testing.T) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	home := t.TempDir()
	var out, errb bytes.Buffer
	return &App{
		NoSpawn: true,
		Env:     paths.Env{GOOS: "linux", Home: home, Getenv: func(string) string { return "" }},
		Stdout:  &out,
		Stderr:  &errb,
		Build:   Build{Version: "1.2.3", Commit: "abcdef0123456789", Date: "2026-11-20"},
	}, &out, &errb
}

func writeConfig(t *testing.T, app *App, mutate func(*config.Config)) {
	t.Helper()
	c := config.Default()
	c.Machine = "work"
	c.Store = config.Store{Kind: config.StoreGit, URL: "ssh://git@ssh.github.com:443/o/store.git", Branch: "main"}
	if mutate != nil {
		mutate(&c)
	}
	if err := config.Save(app.Env.ConfigFile(), &c); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestHelpAndVersion(t *testing.T) {
	for _, args := range [][]string{nil, {"--help"}, {"help"}} {
		app, out, _ := newApp(t)
		if code := Run(app, args); code != ExitOK {
			t.Fatalf("%v: exit %d", args, code)
		}
		for _, cmd := range []string{"init", "sync", "status", "restore", "doctor", "version"} {
			if !strings.Contains(out.String(), cmd) {
				t.Errorf("%v: help does not list %q:\n%s", args, cmd, out)
			}
		}
	}
	for _, args := range [][]string{{"version"}, {"--version"}} {
		app, out, _ := newApp(t)
		if code := Run(app, args); code != ExitOK {
			t.Fatalf("exit %d", code)
		}
		if !strings.HasPrefix(out.String(), "karvan 1.2.3 (commit abcdef0, 2026-11-20) ") {
			t.Errorf("%v: %q", args, out)
		}
	}
}

func TestStubsExitNotImplemented(t *testing.T) {
	for _, args := range [][]string{{"init"}, {"restore"}, {"restore", "home"}} {
		app, _, errb := newApp(t)
		if code := Run(app, args); code != ExitNotImplemented {
			t.Errorf("%v: exit %d, want %d", args, code, ExitNotImplemented)
		}
		if !strings.Contains(errb.String(), "not implemented yet") || !strings.Contains(errb.String(), "phase") {
			t.Errorf("%v: stderr %q", args, errb)
		}
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"bogus"}, {"sync", "extra"}, {"restore", "a", "b"}, {"--nope"}, {"status", "--nope"}} {
		app, _, errb := newApp(t)
		if code := Run(app, args); code != ExitUsage {
			t.Errorf("%v: exit %d, want %d (stderr %q)", args, code, ExitUsage, errb)
		}
	}
}

func TestStatus(t *testing.T) {
	app, out, _ := newApp(t)
	if code := Run(app, []string{"status"}); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "run `karvan init`") {
		t.Errorf("not-initialized status: %q", out)
	}

	app, out, _ = newApp(t)
	writeConfig(t, app, nil)
	if code := Run(app, []string{"status"}); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"machine:      work", "git ssh://git@ssh.github.com:443/o/store.git (branch main)", "last sync:    never; run `karvan sync`"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status missing %q:\n%s", want, out)
		}
	}
}

func TestStatusInvalidConfig(t *testing.T) {
	app, _, errb := newApp(t)
	mkdir(t, filepath.Dir(app.Env.ConfigFile()))
	if err := os.WriteFile(app.Env.ConfigFile(), []byte("version = 1\nmachine = \"BAD\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := Run(app, []string{"status"}); code != ExitError {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errb.String(), "machine") {
		t.Errorf("stderr %q", errb)
	}
}

func TestConfigFlag(t *testing.T) {
	app, out, _ := newApp(t)
	other := filepath.Join(t.TempDir(), "other.toml")
	c := config.Default()
	c.Machine = "laptop"
	c.Store = config.Store{Kind: config.StoreFolder, Path: "/srv/karvan", Branch: "main"}
	if err := config.Save(other, &c); err != nil {
		t.Fatal(err)
	}
	if code := Run(app, []string{"--config", other, "status"}); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "machine:      laptop") || !strings.Contains(out.String(), "folder /srv/karvan") {
		t.Errorf("status:\n%s", out)
	}
}

func TestDoctor(t *testing.T) {
	// Nothing exists: Claude Code folder missing is a failure.
	app, out, _ := newApp(t)
	if code := Run(app, []string{"doctor"}); code != ExitError {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out.String(), "FAIL  Claude Code folder") || !strings.Contains(out.String(), "warn  config") {
		t.Errorf("doctor:\n%s", out)
	}

	// Healthy machine with one project used in Claude Code and one not.
	app, out, _ = newApp(t)
	home := app.Env.Home
	used, unused := filepath.Join(home, "work", "app"), filepath.Join(home, "work", "other")
	mkdir(t, used)
	mkdir(t, unused)
	mkdir(t, app.Env.ClaudeProjectDir(used))
	writeConfig(t, app, func(c *config.Config) {
		c.Projects = []config.Project{{ID: "github.com/o/app", Path: used}, {ID: "name:other", Path: unused}}
	})
	if code := Run(app, []string{"doctor"}); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, want := range []string{
		"ok    config:",
		"ok    Claude Code folder:",
		"ok    Claude Code projects folder:",
		"info  Claude desktop app data not found",
		"ok    project github.com/o/app:",
		"warn  project name:other: no Claude Code folder",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("doctor missing %q:\n%s", want, out)
		}
	}

	// A configured project whose folder is gone is a failure.
	app, out, _ = newApp(t)
	mkdir(t, app.Env.ClaudeProjectsDir())
	writeConfig(t, app, func(c *config.Config) {
		c.Projects = []config.Project{{ID: "name:gone", Path: filepath.Join(app.Env.Home, "gone")}}
	})
	if code := Run(app, []string{"doctor"}); code != ExitError {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out.String(), "FAIL  project name:gone") {
		t.Errorf("doctor:\n%s", out)
	}
}

func TestBuildStringDev(t *testing.T) {
	s := Build{}.String()
	if !strings.HasPrefix(s, "karvan ") || !strings.Contains(s, "/") {
		t.Errorf("%q", s)
	}
}

func TestDoctorHooksAndUninstall(t *testing.T) {
	app, out, _ := newApp(t)
	app.Exe = "/usr/bin/karvan"
	mkdir(t, app.Env.ClaudeProjectsDir())
	if code := Run(app, []string{"doctor"}); code != ExitOK || !strings.Contains(out.String(), "warn  hooks: not installed") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, _, err := hooks.Install(app.Env.ClaudeSettings(), app.Exe); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	Run(app, []string{"doctor"})
	if !strings.Contains(out.String(), "ok    hooks: SessionStart and SessionEnd run /usr/bin/karvan") {
		t.Fatalf("%s", out)
	}
	out.Reset()
	if code := Run(app, []string{"uninstall"}); code != ExitOK || !strings.Contains(out.String(), "Removed Karvan's hooks") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if err := os.WriteFile(app.Env.ClaudeSettings(), []byte("{,}"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := Run(app, []string{"doctor"}); code != ExitError || !strings.Contains(out.String(), "will not change an invalid file") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestSyncHookFlag(t *testing.T) {
	app, _, _ := newApp(t)
	if code := Run(app, []string{"sync", "--hook", "bogus"}); code != ExitUsage {
		t.Fatalf("exit %d", code)
	}
}
