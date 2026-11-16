package project

import (
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
	if PID("github.com/owner/app") != PID("github.com/owner/app") || len(PID("x")) != 18 || PID("a") == PID("b") {
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
