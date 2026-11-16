package paths

import "testing"

func env(goos, home string, vars map[string]string) Env {
	return Env{GOOS: goos, Home: home, Getenv: func(k string) string { return vars[k] }}
}

func TestProjectDirName(t *testing.T) {
	for in, want := range map[string]string{
		// Observed on a real machine: /home/user/Karvan -> -home-user-Karvan.
		"/home/user/Karvan":        "-home-user-Karvan",
		"/home/u/temp/koochan-api": "-home-u-temp-koochan-api",
		"/home/u/my.app_v2":        "-home-u-my-app-v2",
		`C:\Users\u\app`:           "C--Users-u-app",
		"/home/u/پروژه":            "-home-u------", // 5 BMP runes -> 5 dashes
		"/home/u/a\U0001F600b":     "-home-u-a--b",  // emoji = 2 UTF-16 units
		"":                         "",
		"/":                        "-",
	} {
		if got := ProjectDirName(in); got != want {
			t.Errorf("ProjectDirName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLinuxDefaults(t *testing.T) {
	e := env("linux", "/home/u", nil)
	for name, c := range map[string][2]string{
		"ClaudeHome":         {e.ClaudeHome(), "/home/u/.claude"},
		"ClaudeProjectsDir":  {e.ClaudeProjectsDir(), "/home/u/.claude/projects"},
		"ClaudeProjectDir":   {e.ClaudeProjectDir("/home/u/work/app"), "/home/u/.claude/projects/-home-u-work-app"},
		"DesktopDataDir":     {e.DesktopDataDir(), "/home/u/.config/Claude"},
		"DesktopSessionsDir": {e.DesktopSessionsDir(), "/home/u/.config/Claude/claude-code-sessions"},
		"ConfigFile":         {e.ConfigFile(), "/home/u/.config/karvan/config.toml"},
		"StateDir":           {e.StateDir(), "/home/u/.local/state/karvan"},
		"CacheDir":           {e.CacheDir(), "/home/u/.cache/karvan"},
	} {
		if c[0] != c[1] {
			t.Errorf("%s = %q, want %q", name, c[0], c[1])
		}
	}
}

func TestLinuxOverrides(t *testing.T) {
	e := env("linux", "/home/u", map[string]string{
		"CLAUDE_CONFIG_DIR": "/data/claude/",
		"XDG_CONFIG_HOME":   "/x/config",
		"XDG_STATE_HOME":    "/x/state",
		"XDG_CACHE_HOME":    "relative/ignored", // relative values are ignored
	})
	for name, c := range map[string][2]string{
		"ClaudeHome":     {e.ClaudeHome(), "/data/claude"},
		"DesktopDataDir": {e.DesktopDataDir(), "/x/config/Claude"},
		"ConfigFile":     {e.ConfigFile(), "/x/config/karvan/config.toml"},
		"StateDir":       {e.StateDir(), "/x/state/karvan"},
		"CacheDir":       {e.CacheDir(), "/home/u/.cache/karvan"},
	} {
		if c[0] != c[1] {
			t.Errorf("%s = %q, want %q", name, c[0], c[1])
		}
	}
}

func TestWindowsAndMac(t *testing.T) {
	w := env("windows", `C:\Users\u`, map[string]string{
		"APPDATA":      `C:\Users\u\AppData\Roaming`,
		"LOCALAPPDATA": `C:\Users\u\AppData\Local`,
	})
	m := env("darwin", "/Users/u", nil)
	for name, c := range map[string][2]string{
		"win ClaudeHome":       {w.ClaudeHome(), `C:\Users\u\.claude`},
		"win ClaudeProjectDir": {w.ClaudeProjectDir(`C:\Users\u\app`), `C:\Users\u\.claude\projects\C--Users-u-app`},
		"win DesktopDataDir":   {w.DesktopDataDir(), `C:\Users\u\AppData\Roaming\Claude`},
		"win ConfigFile":       {w.ConfigFile(), `C:\Users\u\AppData\Roaming\karvan\config.toml`},
		"win StateDir":         {w.StateDir(), `C:\Users\u\AppData\Local\karvan\state`},
		"win CacheDir":         {w.CacheDir(), `C:\Users\u\AppData\Local\karvan\cache`},
		"mac DesktopDataDir":   {m.DesktopDataDir(), "/Users/u/Library/Application Support/Claude"},
		"mac ConfigFile":       {m.ConfigFile(), "/Users/u/Library/Application Support/karvan/config.toml"},
		"mac CacheDir":         {m.CacheDir(), "/Users/u/Library/Caches/karvan"},
	} {
		if c[0] != c[1] {
			t.Errorf("%s = %q, want %q", name, c[0], c[1])
		}
	}
	// Without APPDATA, fall back under the home directory.
	if got := env("windows", `C:\Users\u`, nil).DesktopDataDir(); got != `C:\Users\u\AppData\Roaming\Claude` {
		t.Errorf("fallback DesktopDataDir = %q", got)
	}
}

func TestClean(t *testing.T) {
	for _, c := range []struct{ goos, in, want string }{
		{"linux", "/a//b/../c/", "/a/c"},
		{"windows", `c:/Users//u\app\`, `C:\Users\u\app`},
		{"windows", `C:\`, `C:\`},
		{"windows", `C:\a\..`, `C:\`},
		{"windows", `\\server\share\x\`, `\\server\share\x`},
	} {
		if got := Clean(c.goos, c.in); got != c.want {
			t.Errorf("Clean(%s, %q) = %q, want %q", c.goos, c.in, got, c.want)
		}
	}
}

func TestIsAbs(t *testing.T) {
	for _, c := range []struct {
		goos, p string
		want    bool
	}{
		{"linux", "/x", true},
		{"linux", "x/y", false},
		{"windows", `C:\x`, true},
		{"windows", `c:/x`, true},
		{"windows", `\\srv\share`, true},
		{"windows", `C:x`, false},
		{"windows", `\x`, false},
	} {
		if got := IsAbs(c.goos, c.p); got != c.want {
			t.Errorf("IsAbs(%s, %q) = %v", c.goos, c.p, got)
		}
	}
}

func TestHasPathPrefix(t *testing.T) {
	for _, c := range []struct {
		goos, p, prefix string
		want            bool
	}{
		{"linux", "/a/app", "/a/app", true},
		{"linux", "/a/app/x", "/a/app", true},
		{"linux", "/a/app/x", "/a/app/", true},
		{"linux", "/a/app-old", "/a/app", false},
		{"linux", "/a/application", "/a/app", false},
		{"linux", "/a", "/a/app", false},
		{"linux", "/A/app", "/a/app", false}, // case-sensitive on Linux
		{"linux", "/anything", "/", true},
		{"windows", `C:\Work\App\src`, `c:\work\app`, true},
		{"windows", `C:/Work/App`, `C:\work\app\`, true},
		{"windows", `C:\Work\App-old`, `C:\Work\App`, false},
		{"windows", `D:\Work\App`, `C:\Work\App`, false},
		{"windows", `C:\x`, `C:\`, true},
	} {
		if got := HasPathPrefix(c.goos, c.p, c.prefix); got != c.want {
			t.Errorf("HasPathPrefix(%s, %q, %q) = %v, want %v", c.goos, c.p, c.prefix, got, c.want)
		}
	}
}

func TestFromOS(t *testing.T) {
	e, err := FromOS()
	if err != nil {
		t.Fatal(err)
	}
	if e.Home == "" || e.GOOS == "" || !IsAbs(e.GOOS, e.ClaudeHome()) {
		t.Fatalf("unexpected env %+v", e)
	}
}
