package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leberman/karvan/internal/hooks"
	"github.com/leberman/karvan/internal/systemd"
)

func fakeSystemctl(calls *[]string) func(args ...string) ([]byte, error) {
	return func(args ...string) ([]byte, error) {
		*calls = append(*calls, strings.Join(args, " "))
		return []byte("active\n"), nil
	}
}

// A first-time setup without a terminal: flags only.
func TestInitNonInteractive(t *testing.T) {
	app, _, _ := newApp(t)
	app.Exe = "/usr/bin/karvan"
	var calls []string
	app.Systemctl = fakeSystemctl(&calls)
	app.PublicRepoCheck = func(string) (bool, bool) { return false, true }
	// Claude Code was used in two folders on this machine.
	proj := filepath.Join(app.Env.Home, "src", "my-app")
	mkdir(t, proj)
	writeOld(t, filepath.Join(app.Env.ClaudeProjectDir(proj), "s1.jsonl"), tline("u1", "", proj, "09:00:00")+"\n")
	put(t, filepath.Join(app.Env.ClaudeHome(), "CLAUDE.md"), "be brief")
	storeDir := filepath.Join(t.TempDir(), "Dropbox", "karvan")

	out := run(t, app, "init", "--machine", "laptop", "--folder", storeDir, "--yes")
	for _, want := range []string{"Found 1 folder(s)", "+ name:my-app", "Added Karvan's SessionStart", "Prepared the new, empty store", "Synced in", "Installed a timer"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	cfg, err := app.LoadConfig()
	if err != nil || cfg.Machine != "laptop" || len(cfg.Projects) != 1 || cfg.Projects[0].Path != proj {
		t.Fatalf("%+v %v", cfg, err)
	}
	if st, _ := hooks.Check(app.Env.ClaudeSettings()); !st.Installed("/usr/bin/karvan") {
		t.Fatal("hooks not installed")
	}
	if b := get(filepath.Join(app.Env.ClaudeHome(), "CLAUDE.md")); b != "be brief\n\n@~/.claude/CLAUDE.machine.md\n" {
		t.Fatalf("CLAUDE.md = %q", b)
	}
	if _, err := os.Stat(filepath.Join(app.Env.ClaudeHome(), "CLAUDE.machine.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(systemd.Dir(app.configHome()), systemd.Timer)); err != nil {
		t.Fatal("timer unit not written")
	}
	if _, err := os.Stat(filepath.Join(storeDir, "FORMAT")); err != nil {
		t.Fatal("store not created")
	}
	// Running init again changes nothing and does not duplicate anything.
	out = run(t, app, "init", "--yes")
	if strings.Count(get(filepath.Join(app.Env.ClaudeHome(), "CLAUDE.md")), "CLAUDE.machine.md") != 1 || strings.Contains(out, "Found") {
		t.Fatalf("second init:\n%s", out)
	}
	// doctor is happy.
	if o := run(t, app, "doctor"); strings.Contains(o, "FAIL") || strings.Contains(o, "warn  hooks") {
		t.Fatalf("doctor:\n%s", o)
	}
	// uninstall removes hooks and timer, keeps the config.
	o := run(t, app, "uninstall")
	if !strings.Contains(o, "Removed the timer") {
		t.Fatalf("%s", o)
	}
	if _, err := app.LoadConfig(); err != nil {
		t.Fatal("config removed")
	}
}

// The questions a person answers in a terminal.
func TestInitInteractive(t *testing.T) {
	app, _, _ := newApp(t)
	app.Exe = "/usr/bin/karvan"
	var calls []string
	app.Systemctl = fakeSystemctl(&calls)
	storeDir := filepath.Join(t.TempDir(), "sync")
	a := filepath.Join(app.Env.Home, "a")
	b := filepath.Join(app.Env.Home, "b")
	mkdir(t, a)
	mkdir(t, b)
	gitT(t, a, "init", "--quiet")
	writeOld(t, filepath.Join(app.Env.ClaudeProjectDir(a), "s.jsonl"), tline("u", "", a, "09:00:00")+"\n")
	writeOld(t, filepath.Join(app.Env.ClaudeProjectDir(b), "s.jsonl"), tline("u", "", b, "09:00:00")+"\n")
	answers := strings.Join([]string{
		"Bad_Name",      // invalid (underscore), asked again
		"\u200fDesk",    // a direction mark and capitals are cleaned up
		"2",             // folder store
		"relative/path", // refused: not a full path
		storeDir,        // folder
		"n",             // not all projects
		"n",             // ~/b (newest first)
		"y",             // ~/a
		"y",             // carry unfinished work
	}, "\n") + "\n"
	var out strings.Builder
	if err := runInit(context.Background(), app, strings.NewReader(answers), &out, initFlags{noSync: true}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	cfg, err := app.LoadConfig()
	if err != nil || cfg.Machine != "desk" || cfg.Store.Path != storeDir || len(cfg.Projects) != 1 || !cfg.Projects[0].WIP {
		t.Fatalf("%+v %v\n%s", cfg, err, out.String())
	}
}

func TestInitRefusesPublicRepo(t *testing.T) {
	app, _, errb := newApp(t)
	app.PublicRepoCheck = func(string) (bool, bool) { return true, true }
	if code := Run(app, []string{"init", "--machine", "m", "--git", "git@github.com:me/open.git", "--yes", "--no-timer"}); code != ExitError {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errb.String(), "PUBLIC") {
		t.Fatal(errb.String())
	}
}

// Ctrl+D (end of input) at any question stops init without saving anything.
func TestInitCtrlDCancels(t *testing.T) {
	app, _, _ := newApp(t)
	var out strings.Builder
	err := runInit(context.Background(), app, strings.NewReader("desk\n"), &out, initFlags{noSync: true, noTimer: true})
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("err = %v\n%s", err, out.String())
	}
	if _, err := os.Stat(app.ConfigPath()); !os.IsNotExist(err) {
		t.Fatal("config saved after cancel")
	}
}
