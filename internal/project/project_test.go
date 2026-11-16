package project

import (
	"fmt"
	"os"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
)

func TestNormalizeRemote(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:Owner/App.git":               "github.com/owner/app",
		"https://github.com/owner/app/":              "github.com/owner/app",
		"https://github.com/owner/app.git":           "github.com/owner/app",
		"ssh://git@ssh.github.com:443/owner/app.git": "github.com/owner/app",
		"ssh://git@github.com/owner/app":             "github.com/owner/app",
		"git@gitlab.com:group/sub/proj.git":          "gitlab.com/group/sub/proj",
		"https://git.example.com:8443/Team/Proj.git": "git.example.com/Team/Proj",
		"ssh://git@altssh.bitbucket.org:443/o/r.git": "bitbucket.org/o/r",
	} {
		got, ok := NormalizeRemote(in)
		if !ok || got != want {
			t.Errorf("NormalizeRemote(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "/local/path", "file:///srv/repo.git", "https://"} {
		if got, ok := NormalizeRemote(bad); ok {
			t.Errorf("%q accepted as %q", bad, got)
		}
	}
}

func TestPIDAndDetect(t *testing.T) {
	if PID("github.com/owner/app") != "p-"+PID("github.com/owner/app")[2:] || len(PID("x")) != 18 || PID("a") == PID("b") {
		t.Fatal("PID")
	}
	dir := t.TempDir()
	if _, err := Detect(dir); err == nil {
		t.Fatal("not a repo")
	}
	r, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Detect(dir); err == nil {
		t.Fatal("repo without remote")
	}
	if _, err := r.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{"git@github.com:Owner/App.git"}}); err != nil {
		t.Fatal(err)
	}
	if id, err := Detect(dir); err != nil || id != "github.com/owner/app" {
		t.Fatal(id, err)
	}
}

func TestDiscover(t *testing.T) {
	root := t.TempDir()
	projects := root + "/.claude/projects"
	app := root + "/work/app"
	sub := app + "/api"
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	r, _ := git.PlainInit(app, false)
	_, _ = r.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{"git@github.com:o/app.git"}})
	notes := root + "/notes"
	_ = os.MkdirAll(notes, 0o755)
	write := func(dir, cwd string, n int) {
		d := projects + "/" + strings.ReplaceAll(dir, "/", "-")
		_ = os.MkdirAll(d, 0o755)
		for i := 0; i < n; i++ {
			_ = os.WriteFile(fmt.Sprintf("%s/s%d.jsonl", d, i), []byte(`{"type":"queue-operation"}`+"\n"+`{"type":"user","cwd":"`+cwd+`","uuid":"u"}`+"\n"), 0o600)
		}
	}
	write(app, app, 3)
	write(sub, sub, 1)
	write(notes, notes, 2)
	write(root+"/gone", root+"/gone", 1)
	found, err := Discover(projects)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Found{}
	for _, f := range found {
		got[f.Path] = f
	}
	if len(got) != 3 {
		t.Fatalf("%+v", found)
	}
	if f := got[app]; f.ID != "github.com/o/app" || !f.Remote || f.Sessions != 3 {
		t.Fatalf("%+v", f)
	}
	if f := got[sub]; f.ID != "github.com/o/app/api" {
		t.Fatalf("%+v", f)
	}
	if f := got[notes]; f.ID != "name:notes" || f.Remote {
		t.Fatalf("%+v", f)
	}
}
