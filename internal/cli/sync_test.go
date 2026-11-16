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

func tline(uuid, parent, cwd, ts string) string {
	p := "null"
	if parent != "" {
		p = `"` + parent + `"`
	}
	return `{"parentUuid":` + p + `,"cwd":"` + cwd + `","sessionId":"s1","type":"user","uuid":"` + uuid + `","timestamp":"2026-11-16T` + ts + `.000Z","message":{"content":"hi"}}`
}

// writeOld writes a file and dates it an hour back (so it is not "open").
func writeOld(t *testing.T, p, content string) {
	t.Helper()
	put(t, p, content)
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(p, old, old)
}

// PRD 15 #1 and #9 with real paths: the same conversation continued on both
// machines keeps both sides; each machine sees its own project path; an
// unfinished line never travels.
func TestSyncTranscripts(t *testing.T) {
	work, home, workProj, homeProj := twoMachines(t)
	wfile := filepath.Join(work.Env.ClaudeProjectDir(workProj), "s1.jsonl")
	hfile := filepath.Join(home.Env.ClaudeProjectDir(homeProj), "s1.jsonl")
	base := tline("u1", "", workProj, "09:00:00") + "\n" + tline("u2", "u1", workProj, "09:01:00") + "\n"
	writeOld(t, wfile, base+`{"parentUuid":"u2","cwd":"`) // unfinished line
	run(t, work, "sync")
	run(t, home, "sync")
	got := get(hfile)
	if strings.Count(got, "\n") != 2 || strings.Contains(got, workProj) || !strings.Contains(got, `"cwd":"`+homeProj+`"`) {
		t.Fatalf("home transcript:\n%s", got)
	}

	// Both continue the conversation.
	writeOld(t, wfile, base+tline("a1", "u2", workProj, "10:00:00")+"\n")
	writeOld(t, hfile, get(hfile)+tline("b1", "u2", homeProj, "10:05:00")+"\n")
	run(t, work, "sync")
	run(t, home, "sync")
	run(t, work, "sync")
	w, h := get(wfile), get(hfile)
	for _, id := range []string{"u1", "u2", "a1", "b1"} {
		if !strings.Contains(w, `"uuid":"`+id+`"`) || !strings.Contains(h, `"uuid":"`+id+`"`) {
			t.Fatalf("line %s missing:\nwork:\n%s\nhome:\n%s", id, w, h)
		}
	}
	if strings.Contains(h, workProj) || strings.Contains(w, homeProj) {
		t.Fatal("a machine got the other machine's path")
	}
	if strings.Count(w, "\n") != 4 || strings.Count(h, "\n") != 4 {
		t.Fatalf("duplicates:\n%s", w)
	}
	// Same order on both machines: only the cwd differs.
	if strings.ReplaceAll(w, workProj, "P") != strings.ReplaceAll(h, homeProj, "P") {
		t.Fatalf("machines disagree:\n%s\n---\n%s", w, h)
	}
}

// PRD 9-3: a conversation written in the last minutes is not overwritten;
// it is merged on a later run.
func TestSyncOpenTranscriptWaits(t *testing.T) {
	work, home, workProj, homeProj := twoMachines(t)
	wfile := filepath.Join(work.Env.ClaudeProjectDir(workProj), "s1.jsonl")
	hfile := filepath.Join(home.Env.ClaudeProjectDir(homeProj), "s1.jsonl")
	writeOld(t, wfile, tline("u1", "", workProj, "09:00:00")+"\n")
	run(t, work, "sync")
	run(t, home, "sync")
	writeOld(t, wfile, get(wfile)+tline("a1", "u1", workProj, "10:00:00")+"\n")
	run(t, work, "sync")
	put(t, hfile, get(hfile)+tline("b1", "u1", homeProj, "10:05:00")+"\n") // open right now
	out := run(t, home, "sync")
	if !strings.Contains(out, "may be open") || strings.Contains(get(hfile), "a1") {
		t.Fatalf("open conversation was touched:\n%s", out)
	}
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(hfile, old, old)
	run(t, home, "sync")
	if !strings.Contains(get(hfile), `"uuid":"a1"`) || !strings.Contains(get(hfile), `"uuid":"b1"`) {
		t.Fatalf("not merged later:\n%s", get(hfile))
	}
}
