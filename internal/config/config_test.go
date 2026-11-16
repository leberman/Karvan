package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimalGit = `
version = 1
machine = "work"

[store]
kind = "git"
url = "ssh://git@ssh.github.com:443/owner/karvan-store.git"
`

func TestParseMinimalKeepsDefaults(t *testing.T) {
	c, err := Parse([]byte(minimalGit), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	d := Default()
	if c.Store.Branch != "main" || c.Sync != d.Sync || c.Items.Transcripts != true || c.Items.Desktop != true {
		t.Fatalf("defaults lost: %+v", c)
	}
	if c.Sync.Interval.Duration != 10*time.Minute || c.Sync.HistoryDays != 7 {
		t.Fatalf("sync = %+v", c.Sync)
	}
}

func TestParseFull(t *testing.T) {
	src := `
version = 1
machine = "home-pc"

[store]
kind = "folder"
path = "~/Dropbox/karvan"

[sync]
history_days = 14
interval = "5m"
skip_recent = "0s"
timeout = "2m"

[items]
claude_md = true
skills = false
memory = true
transcripts = true
desktop = false

[[items.folders]]
name = "notes"
path = "~/notes"

[[projects]]
id = "github.com/owner/app"
name = "app"
path = "/home/u/temp/app"
wip = true

[[projects]]
id = "name:scratch"
path = "~/scratch"

[exclude]
skills = ["work-only"]
projects = ["github.com/owner/secret"]
`
	c, err := Parse([]byte(src), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	if c.Store.Path != "/home/u/Dropbox/karvan" || c.Items.Folders[0].Path != "/home/u/notes" || c.Projects[1].Path != "/home/u/scratch" {
		t.Fatalf("~ not expanded: %+v %+v %+v", c.Store, c.Items.Folders, c.Projects)
	}
	if c.Sync.Interval.Duration != 5*time.Minute || c.Sync.SkipRecent.Duration != 0 || c.Items.Skills || c.Items.Desktop {
		t.Fatalf("values not read: %+v %+v", c.Sync, c.Items)
	}
	if !c.Projects[0].WIP || c.Projects[1].WIP || c.Exclude.Skills[0] != "work-only" {
		t.Fatalf("projects/exclude: %+v %+v", c.Projects, c.Exclude)
	}
}

func TestParseErrors(t *testing.T) {
	for name, c := range map[string]struct{ src, want string }{
		"unknown key (typo)": {minimalGit + "\n[sync]\nintervall = \"5m\"\n", "unknown setting(s): sync.intervall"},
		"bad toml":           {"machine = ", "toml"},
		"bad duration":       {minimalGit + "\n[sync]\ninterval = \"ten minutes\"\n", "invalid duration"},
		"bad machine":        {strings.Replace(minimalGit, `"work"`, `"Work PC"`, 1), "machine ="},
		"no store":           {"version = 1\nmachine = \"a\"\n", "store.kind"},
		"git without url":    {"version = 1\nmachine = \"a\"\n[store]\nkind = \"git\"\n", "store.url is required"},
		"relative folder":    {"version = 1\nmachine = \"a\"\n[store]\nkind = \"folder\"\npath = \"store\"\n", "store.path"},
		"future version":     {strings.Replace(minimalGit, "version = 1", "version = 2", 1), "understands version 1"},
		"interval too short": {minimalGit + "\n[sync]\ninterval = \"10s\"\n", "at least 1m"},
		"duplicate project": {minimalGit + `
[[projects]]
id = "x"
path = "/a"
[[projects]]
id = "x"
path = "/b"
`, "used twice"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(c.src), "/home/u")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

func TestValidateReportsAllProblems(t *testing.T) {
	c := Default()
	c.Machine = "BAD"
	c.Sync.HistoryDays = 0
	err := c.Validate()
	for _, want := range []string{"machine", "store.kind", "history_days"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "config.toml")
	c := Default()
	c.Machine = "work"
	c.Store = Store{Kind: StoreGit, URL: "ssh://git@ssh.github.com:443/o/s.git", Branch: "main", SSHKey: "/home/u/.ssh/id_ed25519"}
	c.Sync.Interval = Duration{90 * time.Minute}
	c.Projects = []Project{{ID: "github.com/o/app", Name: "app", Path: "/home/u/app", WIP: true}}
	if err := Save(path, &c); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(raw), "# Karvan configuration") || !strings.Contains(string(raw), `interval = "1h30m"`) {
		t.Errorf("unexpected file:\n%s", raw)
	}
	got, err := Load(path, "/home/u")
	if err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	if got.Machine != c.Machine || got.Store != c.Store || got.Sync != c.Sync || got.Projects[0] != c.Projects[0] {
		t.Fatalf("round trip changed config:\n got %+v\nwant %+v", got, c)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("temporary files left behind: %v", entries)
	}
}

func TestSaveRefusesInvalid(t *testing.T) {
	c := Default()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := Save(path, &c); err == nil {
		t.Fatal("saved an invalid config")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("file was written")
	}
}

func TestLoadMissing(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "none.toml"), "/home/u")
	if !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("err = %v", err)
	}
}

func TestDurationText(t *testing.T) {
	for d, want := range map[time.Duration]string{
		10 * time.Minute:              "10m",
		time.Hour:                     "1h",
		90 * time.Minute:              "1h30m",
		15 * time.Second:              "15s",
		2*time.Minute + 5*time.Second: "2m5s",
		0:                             "0s",
	} {
		b, _ := Duration{d}.MarshalText()
		if string(b) != want {
			t.Errorf("%v -> %q, want %q", d, b, want)
		}
	}
}

func TestMachineNameFrom(t *testing.T) {
	for in, want := range map[string]string{
		"leberman":              "leberman",
		"Work-Laptop.local":     "work-laptop",
		"My PC_2":               "my-pc-2",
		"--x--":                 "x",
		"":                      "",
		"___":                   "",
		strings.Repeat("a", 40): strings.Repeat("a", 32),
	} {
		if got := MachineNameFrom(in); got != want {
			t.Errorf("MachineNameFrom(%q) = %q, want %q", in, got, want)
		}
	}
}
