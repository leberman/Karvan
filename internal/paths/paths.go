// Package paths knows where Claude Code, the Claude desktop app and Karvan
// keep their files on each operating system, and how Claude Code names a
// project's folder.
//
// Everything is computed from an Env value instead of the real process
// environment, so tests can ask "where would this be on Windows?" from Linux.
// Paths are built with the target OS's separator, not the host's.
package paths

import (
	"errors"
	"os"
	"runtime"
	"strings"
)

// Env is everything path resolution depends on.
type Env struct {
	GOOS   string              // "linux", "windows", "darwin"
	Home   string              // the user's home directory
	Getenv func(string) string // environment lookup; nil means "nothing set"
}

// FromOS returns the Env of the running process.
func FromOS() (Env, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Env{}, err
	}
	if home == "" {
		return Env{}, errors.New("home directory is not set")
	}
	return Env{GOOS: runtime.GOOS, Home: home, Getenv: os.Getenv}, nil
}

func (e Env) getenv(k string) string {
	if e.Getenv == nil {
		return ""
	}
	return e.Getenv(k)
}

// absEnv returns the variable only if it holds an absolute path (the XDG spec
// says relative values must be ignored; we apply the same rule everywhere).
func (e Env) absEnv(k string) string {
	v := e.getenv(k)
	if v == "" || !IsAbs(e.GOOS, v) {
		return ""
	}
	return v
}

// Join joins path elements with the target OS's separator.
func (e Env) Join(elem ...string) string { return Join(e.GOOS, elem...) }

// ClaudeHome is Claude Code's directory: $CLAUDE_CONFIG_DIR if set, else
// ~/.claude on every OS.
func (e Env) ClaudeHome() string {
	if v := e.absEnv("CLAUDE_CONFIG_DIR"); v != "" {
		return Clean(e.GOOS, v)
	}
	return e.Join(e.Home, ".claude")
}

// ClaudeSettings is Claude Code's user settings file (hooks live there).
func (e Env) ClaudeSettings() string { return e.Join(e.ClaudeHome(), "settings.json") }

// ClaudeProjectsDir holds one folder per project (transcripts and memory).
func (e Env) ClaudeProjectsDir() string { return e.Join(e.ClaudeHome(), "projects") }

// ClaudeProjectDir is the folder Claude Code uses for the project at
// projectPath (an absolute path on this machine).
func (e Env) ClaudeProjectDir(projectPath string) string {
	return e.Join(e.ClaudeProjectsDir(), ProjectDirName(projectPath))
}

// DesktopDataDir is the Claude desktop app's data directory (PRD 11).
func (e Env) DesktopDataDir() string {
	switch e.GOOS {
	case "windows":
		if v := e.absEnv("APPDATA"); v != "" {
			return e.Join(v, "Claude")
		}
		return e.Join(e.Home, "AppData", "Roaming", "Claude")
	case "darwin":
		return e.Join(e.Home, "Library", "Application Support", "Claude")
	default:
		return e.Join(e.xdg("XDG_CONFIG_HOME", ".config"), "Claude")
	}
}

// DesktopSessionsDir holds the desktop app's Code-tab session list, as
// <account>/<org>/local_<id>.json.
func (e Env) DesktopSessionsDir() string { return e.Join(e.DesktopDataDir(), "claude-code-sessions") }

// ConfigDir is Karvan's own configuration directory (per machine, never
// synced).
func (e Env) ConfigDir() string {
	switch e.GOOS {
	case "windows":
		if v := e.absEnv("APPDATA"); v != "" {
			return e.Join(v, "karvan")
		}
		return e.Join(e.Home, "AppData", "Roaming", "karvan")
	case "darwin":
		return e.Join(e.Home, "Library", "Application Support", "karvan")
	default:
		return e.Join(e.xdg("XDG_CONFIG_HOME", ".config"), "karvan")
	}
}

// ConfigFile is Karvan's configuration file.
func (e Env) ConfigFile() string { return e.Join(e.ConfigDir(), "config.toml") }

// StateDir holds Karvan's local state: its own manifest, scan cache, lock
// and logs (docs/sync-engine.md, section 6).
func (e Env) StateDir() string {
	switch e.GOOS {
	case "windows":
		return e.Join(e.localAppData(), "karvan", "state")
	case "darwin":
		return e.Join(e.Home, "Library", "Application Support", "karvan", "state")
	default:
		return e.Join(e.xdg("XDG_STATE_HOME", ".local/state"), "karvan")
	}
}

// CacheDir holds data that can be rebuilt, such as the local clone of a git
// store. Deleting it is always safe.
func (e Env) CacheDir() string {
	switch e.GOOS {
	case "windows":
		return e.Join(e.localAppData(), "karvan", "cache")
	case "darwin":
		return e.Join(e.Home, "Library", "Caches", "karvan")
	default:
		return e.Join(e.xdg("XDG_CACHE_HOME", ".cache"), "karvan")
	}
}

func (e Env) xdg(name, fallback string) string {
	if v := e.absEnv(name); v != "" {
		return Clean(e.GOOS, v)
	}
	return e.Join(e.Home, fallback)
}

func (e Env) localAppData() string {
	if v := e.absEnv("LOCALAPPDATA"); v != "" {
		return v
	}
	return e.Join(e.Home, "AppData", "Local")
}

// ProjectDirName is the name Claude Code gives a project's folder under
// ~/.claude/projects: every character that is not an ASCII letter or digit
// becomes "-" (so "/home/u/app" -> "-home-u-app", and on Windows
// "C:\Users\u\app" -> "C--Users-u-app").
//
// Claude Code does this in JavaScript on UTF-16 code units, so a character
// outside the Basic Multilingual Plane (e.g. an emoji) becomes "--". We do
// the same.
//
// Not verified yet: whether Claude Code shortens very long names (see the
// open questions in docs/sync-engine.md). `karvan doctor` will compare this
// name with the folders that actually exist.
func ProjectDirName(projectPath string) string {
	var b strings.Builder
	b.Grow(len(projectPath))
	for _, r := range projectPath {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r > 0xFFFF:
			b.WriteString("--")
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}
