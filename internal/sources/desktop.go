package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/leberman/karvan/internal/engine"
	"github.com/leberman/karvan/internal/manifest"
	"github.com/leberman/karvan/internal/paths"
)

// Desktop syncs the Claude desktop app's Code-tab session list:
// <account>/<org>/local_<id>.json, one small file per session that points
// at the Claude Code transcript (cliSessionId) and the folder (cwd).
// Rules (docs/sync-engine.md, 8-2, 9-4 and open question 1):
//   - an entry travels if its cwd is inside a configured project and its
//     conversation travels too;
//   - every JSON string equal to or under the project path is replaced
//     with a placeholder, and back with each machine's own path;
//   - an existing entry is changed only while the app is closed (it would
//     write its own copy over ours); new entries are written any time;
//   - an entry is written only where the app was used with that account
//     (the <account>/<org> folder exists) and the conversation is present.
type Desktop struct {
	Projects      []TranscriptProject
	Dir           string // ~/.config/Claude/claude-code-sessions
	GOOS          string
	TranscriptsOn bool
	HistoryDays   int
	// AppRunning reports whether the desktop app is open; nil = detect.
	AppRunning func() (bool, error)
}

// Name implements engine.Source.
func (d *Desktop) Name() string { return "desktop app sessions" }

func (d *Desktop) split(logical string) (*TranscriptProject, string, bool) {
	pidPart, rest, ok := strings.Cut(logical, "/desktop/")
	if !ok || !strings.HasPrefix(pidPart, "project:") {
		return nil, "", false
	}
	pid := strings.TrimPrefix(pidPart, "project:")
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || !strings.HasPrefix(parts[2], "local_") || !strings.HasSuffix(parts[2], ".json") || !validRel(rest) {
		return nil, "", false
	}
	for i := range d.Projects {
		if d.Projects[i].PID == pid {
			return &d.Projects[i], rest, true
		}
	}
	return nil, "", false
}

// Owns implements engine.Source.
func (d *Desktop) Owns(logical string) bool { _, _, ok := d.split(logical); return ok }

// Resolve implements engine.Source.
func (d *Desktop) Resolve(logical string) (string, string, bool) {
	_, rest, ok := d.split(logical)
	if !ok {
		return "", "", false
	}
	return filepath.Join(d.Dir, filepath.FromSlash(rest)), manifest.KindDesktop, true
}

type desktopFields struct {
	CWD            string          `json:"cwd"`
	CLISessionID   string          `json:"cliSessionId"`
	LastActivityAt json.RawMessage `json:"lastActivityAt"`
}

// ErrUnknownDesktopFormat means an entry lacks the fields Karvan relies on.
var ErrUnknownDesktopFormat = errors.New("desktop app session file in an unknown format: left alone (the app may have changed it; update Karvan)")

func parseDesktop(b []byte) (desktopFields, error) {
	var f desktopFields
	if err := json.Unmarshal(b, &f); err != nil {
		return f, ErrUnknownDesktopFormat
	}
	if f.CWD == "" || f.CLISessionID == "" || len(f.LastActivityAt) == 0 {
		return f, ErrUnknownDesktopFormat
	}
	return f, nil
}

func (d *Desktop) projectFor(cwd string) *TranscriptProject {
	for i := range d.Projects {
		if paths.HasPathPrefix(d.goos(), cwd, d.Projects[i].Path) {
			return &d.Projects[i]
		}
	}
	return nil
}

func (d *Desktop) goos() string {
	if d.GOOS == "" {
		return runtime.GOOS
	}
	return d.GOOS
}

// Scan implements engine.Source.
func (d *Desktop) Scan(_ context.Context, run engine.ScanRun) ([]engine.Local, error) {
	if !d.TranscriptsOn {
		return nil, nil
	}
	files, err := filepath.Glob(filepath.Join(d.Dir, "*", "*", "local_*.json"))
	if err != nil {
		return nil, err
	}
	var out []engine.Local
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		fields, err := parseDesktop(b)
		if err != nil {
			continue
		}
		p := d.projectFor(fields.CWD)
		if p == nil {
			continue // not a configured project
		}
		tr := filepath.Join(p.Dir, fields.CLISessionID+".jsonl")
		ti, err := os.Stat(tr)
		if err != nil {
			continue // its conversation does not travel
		}
		rel, _ := filepath.Rel(d.Dir, f)
		window := d.HistoryDays
		if window == 0 {
			window = 7
		}
		out = append(out, engine.Local{
			Logical:    "project:" + p.PID + "/desktop/" + filepath.ToSlash(rel),
			Kind:       manifest.KindDesktop,
			Path:       f,
			OutOfScope: run.Now.Sub(ti.ModTime()).Hours() > float64(window*24),
		})
	}
	return out, nil
}

// Normalize implements engine.Source.
func (d *Desktop) Normalize(logical string, raw []byte) ([]byte, error) {
	p, _, ok := d.split(logical)
	if !ok {
		return raw, nil
	}
	if _, err := parseDesktop(raw); err != nil {
		return nil, err
	}
	return swapPaths(raw, p.Path, Placeholder, d.goos())
}

// Denormalize implements engine.Source.
func (d *Desktop) Denormalize(logical string, content []byte) ([]byte, error) {
	p, _, ok := d.split(logical)
	if !ok {
		return content, nil
	}
	return swapPaths(content, Placeholder, p.Path, d.goos())
}

// swapPaths rewrites every JSON string value that equals from or lies under
// it, keeping every other byte of the document as it was.
func swapPaths(doc []byte, from, to, goos string) ([]byte, error) {
	var out bytes.Buffer
	out.Grow(len(doc))
	for i := 0; i < len(doc); {
		if doc[i] != '"' {
			out.WriteByte(doc[i])
			i++
			continue
		}
		j := i + 1
		for j < len(doc) && doc[j] != '"' {
			if doc[j] == '\\' {
				j++
			}
			j++
		}
		if j >= len(doc) {
			return nil, ErrUnknownDesktopFormat
		}
		lit := doc[i : j+1]
		i = j + 1
		var s string
		if json.Unmarshal(lit, &s) == nil {
			if r, ok := swapOne(s, from, to, goos); ok {
				out.WriteByte('"')
				out.Write(jsonString(r))
				out.WriteByte('"')
				continue
			}
		}
		out.Write(lit)
	}
	if !json.Valid(out.Bytes()) {
		return nil, ErrUnknownDesktopFormat
	}
	return out.Bytes(), nil
}

func swapOne(s, from, to, goos string) (string, bool) {
	if s == from {
		return to, true
	}
	if from == Placeholder {
		if strings.HasPrefix(s, from+"/") {
			return to + s[len(from):], true
		}
		return "", false
	}
	if !paths.IsAbs(goos, s) || !paths.HasPathPrefix(goos, s, from) {
		return "", false
	}
	return to + s[len(from):], true
}

// WriteCheck implements engine.Source.
func (d *Desktop) WriteCheck(_ engine.ScanRun, logical, path string, exists bool) error {
	if !exists {
		if _, err := os.Stat(filepath.Dir(path)); err != nil {
			return fmt.Errorf("%w: the desktop app has not been used with this account on this machine", engine.ErrDefer)
		}
		return nil
	}
	running, err := d.appRunning()
	if err != nil || running {
		return fmt.Errorf("%w: the Claude desktop app is open; quit it with Ctrl+Q and open it again to see sessions from your other machines", engine.ErrDefer)
	}
	return nil
}

func (d *Desktop) appRunning() (bool, error) {
	if d.AppRunning != nil {
		return d.AppRunning()
	}
	return DesktopAppRunning()
}

// DesktopAppRunning looks for the Claude desktop app among running processes.
// Unknown platforms report "running" so nothing is overwritten by mistake.
func DesktopAppRunning() (bool, error) {
	if runtime.GOOS != "linux" {
		return true, nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return true, err
	}
	for _, e := range entries {
		if e.Name()[0] < '0' || e.Name()[0] > '9' {
			continue
		}
		cmd, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil || len(cmd) == 0 {
			continue
		}
		if isDesktopApp(string(bytes.ReplaceAll(cmd, []byte{0}, []byte{' '}))) {
			return true, nil
		}
	}
	return false, nil
}

// isDesktopApp recognizes the desktop app's command line, but not the
// `claude` command-line tool (Claude Code).
func isDesktopApp(cmdline string) bool {
	first := strings.Fields(cmdline)
	if len(first) == 0 {
		return false
	}
	exe := filepath.Base(first[0])
	lower := strings.ToLower(cmdline)
	switch {
	case exe == "claude": // Claude Code itself
		return false
	case exe == "Claude", exe == "claude-desktop", strings.Contains(lower, "/claude-desktop/"),
		strings.Contains(lower, "claude-desktop.asar"), strings.Contains(cmdline, "/Claude.app/"):
		return true
	}
	return false
}
