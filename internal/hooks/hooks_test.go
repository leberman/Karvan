package hooks

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const exe = "/usr/bin/karvan"

func write(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const userSettings = `{
  "model": "opus",
  "permissions": {"allow": ["Bash(ls:*)"]},
  "hooks": {
    "SessionStart": [
      {"matcher": "startup", "hooks": [{"type": "command", "command": "echo hi", "timeout": 5}]}
    ],
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "guard.sh"}]}]
  },
  "zzz": 1
}`

func TestInstallKeepsEverythingElse(t *testing.T) {
	p := write(t, userSettings)
	changed, backup, err := Install(p, exe)
	if err != nil || !changed || backup == "" {
		t.Fatalf("changed=%v backup=%q err=%v", changed, backup, err)
	}
	if read(t, backup) != userSettings {
		t.Fatal("backup differs from original")
	}
	out := read(t, p)
	// Key order kept.
	if i := []int{strings.Index(out, `"model"`), strings.Index(out, `"permissions"`), strings.Index(out, `"hooks"`), strings.Index(out, `"zzz"`)}; i[0] > i[1] || i[1] > i[2] || i[2] > i[3] {
		t.Errorf("key order changed:\n%s", out)
	}
	var v struct {
		Model string
		Hooks map[string][]struct {
			Matcher string
			Hooks   []struct{ Type, Command string }
		}
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatal(err)
	}
	ss := v.Hooks["SessionStart"]
	if len(ss) != 2 || ss[0].Hooks[0].Command != "echo hi" || ss[1].Hooks[0].Command != exe+" sync --hook session-start" {
		t.Errorf("SessionStart = %+v", ss)
	}
	if v.Hooks["SessionEnd"][0].Hooks[0].Command != exe+" sync --hook session-end" || v.Hooks["PreToolUse"][0].Hooks[0].Command != "guard.sh" {
		t.Errorf("hooks = %+v", v.Hooks)
	}
	st, err := Check(p)
	if err != nil || !st.Installed(exe) {
		t.Fatalf("Check: %+v %v", st, err)
	}

	// Second install: no change, no new backup.
	changed, backup, err = Install(p, exe)
	if err != nil || changed || backup != "" {
		t.Fatalf("second install: changed=%v backup=%q err=%v", changed, backup, err)
	}
	if strings.Count(read(t, p), "sync --hook session-start") != 1 {
		t.Fatal("hook added twice")
	}

	// Moving the binary updates the command instead of adding another hook.
	if _, _, err := Install(p, "/opt/new path/karvan"); err != nil {
		t.Fatal(err)
	}
	out = read(t, p)
	if strings.Count(out, "sync --hook session-start") != 1 || !strings.Contains(out, `'/opt/new path/karvan' sync --hook`) {
		t.Fatalf("update failed:\n%s", out)
	}

	// Remove gives back the user's hooks only.
	changed, _, err = Remove(p)
	if err != nil || !changed {
		t.Fatal(err)
	}
	out = read(t, p)
	if strings.Contains(out, "karvan") || strings.Contains(out, "SessionEnd") || !strings.Contains(out, "echo hi") || !strings.Contains(out, "guard.sh") {
		t.Fatalf("after remove:\n%s", out)
	}
	if st, _ := Check(p); st.Present["SessionStart"] {
		t.Fatal("still present")
	}
}

func TestInvalidJSONIsNotTouched(t *testing.T) {
	bad := "{\n  \"model\": \"opus\",\n}\n" // trailing comma (PRD 15 #4)
	p := write(t, bad)
	_, _, err := Install(p, exe)
	if !errors.Is(err, ErrInvalidJSON) {
		t.Fatalf("err = %v", err)
	}
	if read(t, p) != bad {
		t.Fatal("file was changed")
	}
	if m, _ := filepath.Glob(p + ".karvan-backup-*"); len(m) != 0 {
		t.Fatal("backup made for an invalid file")
	}
	if _, err := Check(p); !errors.Is(err, ErrInvalidJSON) {
		t.Fatalf("Check err = %v", err)
	}
	for _, s := range []string{"[]", `{"a":1} {"b":2}`, `{"hooks": 5}`} {
		if _, _, err := Install(write(t, s), exe); err == nil {
			t.Errorf("%s: no error", s)
		}
	}
}

func TestMissingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "settings.json")
	st, err := Check(p)
	if err != nil || st.Present["SessionStart"] {
		t.Fatal(err)
	}
	changed, backup, err := Install(p, exe)
	if err != nil || !changed || backup != "" {
		t.Fatal(changed, backup, err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	if _, _, err := Remove(p); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(read(t, p)) != "{}" {
		t.Fatalf("after remove: %q", read(t, p))
	}
}

func TestSharedGroupKeepsOtherHook(t *testing.T) {
	p := write(t, `{"hooks":{"SessionEnd":[{"hooks":[{"type":"command","command":"x.sh","async":true},{"type":"command","command":"/usr/bin/karvan sync --hook session-end"}]}]}}`)
	if _, _, err := Remove(p); err != nil {
		t.Fatal(err)
	}
	out := read(t, p)
	if !strings.Contains(out, `"async": true`) || strings.Contains(out, "karvan") {
		t.Fatalf("%s", out)
	}
}
