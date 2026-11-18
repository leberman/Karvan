package cli

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"

	gogit "github.com/go-git/go-git/v5"
)

// TestEndToEnd runs Karvan the way a person would on two computers that
// share a private git repository (here a local bare one): `karvan init` on
// both, then the PRD 15 scenarios through the command line.
func TestEndToEnd(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "karvan-data.git")
	if _, err := gogit.PlainInit(repo, true); err != nil {
		t.Fatal(err)
	}
	url := "file://" + repo

	type pc struct {
		app  *App
		proj string
	}
	setup := func(name, projRel string) pc {
		app, _, _ := newApp(t)
		app.Exe = "/usr/bin/karvan"
		var calls []string
		app.Systemctl = fakeSystemctl(&calls)
		app.DesktopAppRunning = func() (bool, error) { return false, nil }
		proj := filepath.Join(app.Env.Home, projRel)
		mkdir(t, proj)
		// Claude Code was already used in the project on this machine.
		writeOld(t, filepath.Join(app.Env.ClaudeProjectDir(proj), "first-"+name+".jsonl"),
			tline("first-"+name, "", proj, "08:00:00")+"\n")
		out := run(t, app, "init", "--machine", name, "--git", url, "--project", proj, "--yes")
		if !strings.Contains(out, "It is ready to use.") {
			t.Fatalf("init on %s:\n%s", name, out)
		}
		return pc{app, proj}
	}
	work := setup("work", "work/koochan")
	home := setup("home", "temp/koochan")
	run(t, work.app, "sync")

	cdir := func(m pc) string { return m.app.Env.ClaudeProjectDir(m.proj) }

	// Each machine sees the other's first conversation, in its own folder.
	if !strings.Contains(get(filepath.Join(cdir(work), "first-home.jsonl")), `"cwd":"`+work.proj+`"`) {
		t.Fatal("work did not get home's conversation with its own path")
	}
	if !strings.Contains(get(filepath.Join(cdir(home), "first-work.jsonl")), `"cwd":"`+home.proj+`"`) {
		t.Fatal("home did not get work's conversation with its own path")
	}

	// PRD 15 #7: home is off for a day while work keeps going.
	put(t, filepath.Join(work.app.Env.ClaudeHome(), "skills", "deploy", "SKILL.md"), "deploy steps\n")
	put(t, filepath.Join(cdir(work), "memory", "MEMORY.md"), "staging is on port 8080\n")
	conv := filepath.Join(cdir(work), "s1.jsonl")
	writeOld(t, conv, tline("w1", "", work.proj, "09:00:00")+"\n"+tline("w2", "w1", work.proj, "09:10:00")+"\n")
	run(t, work.app, "sync")
	writeOld(t, conv, get(conv)+tline("w3", "w2", work.proj, "17:00:00")+"\n")
	run(t, work.app, "sync")

	// Home comes back and continues the same conversation before syncing
	// (PRD 15 #1) while also editing CLAUDE.md.
	run(t, home.app, "sync")
	hconv := filepath.Join(cdir(home), "s1.jsonl")
	writeOld(t, hconv, get(hconv)+tline("h1", "w3", home.proj, "20:00:00")+"\n")
	put(t, filepath.Join(home.app.Env.ClaudeHome(), "CLAUDE.md"), get(filepath.Join(home.app.Env.ClaudeHome(), "CLAUDE.md"))+"Answer in Persian.\n")
	writeOld(t, conv, get(conv)+tline("w4", "w3", work.proj, "20:01:00")+"\n")

	// PRD 15 #10: both sync at the same moment.
	var wg sync.WaitGroup
	for _, m := range []pc{work, home} {
		wg.Add(1)
		go func(m pc) {
			defer wg.Done()
			if code := Run(m.app, []string{"sync", "--background"}); code != ExitOK {
				t.Errorf("concurrent sync: exit %d", code)
			}
		}(m)
	}
	wg.Wait()
	run(t, work.app, "sync")
	run(t, home.app, "sync")

	// Both machines agree on everything.
	for _, m := range []pc{work, home} {
		c := get(filepath.Join(cdir(m), "s1.jsonl"))
		for _, id := range []string{"w1", "w2", "w3", "w4", "h1"} {
			if strings.Count(c, `"uuid":"`+id+`"`) != 1 {
				t.Fatalf("%s: line %s count %d\n%s", m.proj, id, strings.Count(c, `"uuid":"`+id+`"`), c)
			}
		}
		if get(filepath.Join(m.app.Env.ClaudeHome(), "skills", "deploy", "SKILL.md")) != "deploy steps\n" ||
			get(filepath.Join(cdir(m), "memory", "MEMORY.md")) != "staging is on port 8080\n" ||
			!strings.Contains(get(filepath.Join(m.app.Env.ClaudeHome(), "CLAUDE.md")), "Answer in Persian.") {
			t.Fatalf("%s: shared files differ", m.proj)
		}
		// CLAUDE.machine.md stays per machine.
		if !strings.Contains(get(filepath.Join(m.app.Env.ClaudeHome(), "CLAUDE.md")), "@~/.claude/CLAUDE.machine.md") {
			t.Fatal("import line missing")
		}
	}
	w := strings.ReplaceAll(get(conv), work.proj, "P")
	h := strings.ReplaceAll(get(hconv), home.proj, "P")
	if w != h {
		t.Fatalf("the merged conversation differs between machines:\n%s\n---\n%s", w, h)
	}
	// A further round is quiet.
	if out := run(t, home.app, "sync"); !strings.Contains(out, "everything is up to date") {
		t.Fatalf("not converged:\n%s", out)
	}
	st := run(t, home.app, "status")
	if !strings.Contains(st, "work") || !strings.Contains(st, "manual, ok") {
		t.Fatalf("status:\n%s", st)
	}
	if out := run(t, home.app, "doctor"); strings.Contains(out, "FAIL") {
		t.Fatalf("doctor:\n%s", out)
	}

}
