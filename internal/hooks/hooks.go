// Package hooks adds, checks and removes Karvan's SessionStart and SessionEnd
// hooks in Claude Code's settings.json (PRD 6-4, 10).
//
// settings.json belongs to the user. This package therefore keeps every other
// setting byte-for-byte equivalent (same keys, same order, same values),
// refuses to touch a file that is not valid JSON, makes a backup before the
// first change and adds its hooks exactly once.
package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Events Karvan hooks into, with the argument passed to `karvan sync --hook`.
var Events = []struct {
	Event   string // Claude Code event name
	Arg     string // value for --hook
	Timeout int    // seconds Claude Code waits
}{
	{"SessionStart", "session-start", 15},
	{"SessionEnd", "session-end", 15},
}

// Marker identifies Karvan's own hook commands.
const Marker = "sync --hook "

// Command is the hook command for one event.
func Command(exe, arg string) string { return shellQuote(exe) + " " + Marker + arg }

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, needsQuote) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func needsQuote(r rune) bool {
	switch {
	case r == '/', r == '.', r == '-', r == '_', r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		return false
	}
	return true
}

// ErrInvalidJSON wraps a parse error of settings.json; nothing is written.
var ErrInvalidJSON = errors.New("settings.json is not valid JSON")

// State says which of Karvan's hooks are present.
type State struct {
	Present map[string]bool   // event -> a Karvan hook exists
	Command map[string]string // event -> its command
}

// Installed reports whether every event has a Karvan hook running exe.
func (s State) Installed(exe string) bool {
	for _, e := range Events {
		if !s.Present[e.Event] || s.Command[e.Event] != Command(exe, e.Arg) {
			return false
		}
	}
	return true
}

// Check reads settings.json (a missing file means no hooks).
func Check(path string) (State, error) {
	st := State{Present: map[string]bool{}, Command: map[string]string{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	root, err := parseObject(data)
	if err != nil {
		return st, err
	}
	hooksObj, err := root.object("hooks")
	if err != nil {
		return st, err
	}
	for _, e := range Events {
		groups, err := hooksObj.groups(e.Event)
		if err != nil {
			return st, err
		}
		for _, g := range groups {
			for _, h := range g.Hooks {
				if isOurs(h) {
					st.Present[e.Event] = true
					st.Command[e.Event] = h.Command
				}
			}
		}
	}
	return st, nil
}

// Install adds (or updates) Karvan's hooks so they run exe. It returns
// whether the file changed and the backup path if one was made.
func Install(path, exe string) (changed bool, backup string, err error) {
	return edit(path, func(hooksObj *object) error {
		for _, e := range Events {
			groups, err := hooksObj.groups(e.Event)
			if err != nil {
				return err
			}
			groups = removeOurs(groups)
			groups = append(groups, group{Hooks: []hook{{Type: "command", Command: Command(exe, e.Arg), Timeout: e.Timeout}}})
			if err := hooksObj.setGroups(e.Event, groups); err != nil {
				return err
			}
		}
		return nil
	})
}

// Remove deletes Karvan's hooks and nothing else.
func Remove(path string) (changed bool, backup string, err error) {
	return edit(path, func(hooksObj *object) error {
		for _, e := range Events {
			groups, err := hooksObj.groups(e.Event)
			if err != nil {
				return err
			}
			groups = removeOurs(groups)
			if len(groups) == 0 {
				hooksObj.delete(e.Event)
				continue
			}
			if err := hooksObj.setGroups(e.Event, groups); err != nil {
				return err
			}
		}
		return nil
	})
}

func edit(path string, change func(*object) error) (bool, string, error) {
	data, err := os.ReadFile(path)
	mode := fs.FileMode(0o600)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		data = []byte("{}")
	case err != nil:
		return false, "", err
	default:
		if st, err := os.Stat(path); err == nil {
			mode = st.Mode().Perm()
		}
	}
	root, err := parseObject(data)
	if err != nil {
		return false, "", err
	}
	hooksObj, err := root.object("hooks")
	if err != nil {
		return false, "", err
	}
	if err := change(&hooksObj); err != nil {
		return false, "", err
	}
	if len(hooksObj.members) == 0 {
		root.delete("hooks")
	} else {
		root.set("hooks", hooksObj.marshal())
	}
	out := root.marshal()
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, out, "", "  "); err != nil {
		return false, "", err
	}
	pretty.WriteByte('\n')
	if sameJSON(data, pretty.Bytes()) {
		return false, "", nil
	}
	backup := ""
	if len(data) > 0 && !bytes.Equal(data, []byte("{}")) {
		backup = path + ".karvan-backup-" + time.Now().Format("20060102-150405")
		if err := os.WriteFile(backup, data, mode); err != nil {
			return false, "", fmt.Errorf("backup: %w", err)
		}
	}
	if err := writeAtomic(path, pretty.Bytes(), mode); err != nil {
		return false, backup, err
	}
	return true, backup, nil
}

func sameJSON(a, b []byte) bool {
	var ca, cb bytes.Buffer
	if json.Compact(&ca, a) != nil || json.Compact(&cb, b) != nil {
		return false
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}

func writeAtomic(path string, data []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".settings-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// group is one entry of an event's array. Unknown fields are kept.
type group struct {
	Matcher string          `json:"matcher,omitempty"`
	Hooks   []hook          `json:"hooks"`
	extra   json.RawMessage // the original entry, if it had fields we do not model
}

type hook struct {
	Type    string `json:"type"`
	Command string `json:"command,omitempty"`
	Timeout int    `json:"timeout,omitempty"`
	raw     json.RawMessage
}

func isOurs(h hook) bool { return h.Type == "command" && strings.Contains(h.Command, Marker) }

func removeOurs(groups []group) []group {
	var out []group
	for _, g := range groups {
		var keep []hook
		removed := false
		for _, h := range g.Hooks {
			if isOurs(h) {
				removed = true
				continue
			}
			keep = append(keep, h)
		}
		if removed {
			if len(keep) == 0 {
				continue
			}
			g.Hooks = keep
			g.extra = nil // re-encode this entry
		}
		out = append(out, g)
	}
	return out
}
