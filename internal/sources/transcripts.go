package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/leberman/karvan/internal/engine"
	"github.com/leberman/karvan/internal/manifest"
	"github.com/leberman/karvan/internal/merge"
)

// Placeholder replaces a project's local path in stored data (PRD 5).
const Placeholder = "{{karvan:project}}"

// TranscriptProject is one project whose conversations travel.
type TranscriptProject struct {
	PID  string // project.PID of its ID
	Path string // the project's path on this machine
	Dir  string // Claude Code's folder for it (~/.claude/projects/<dashed>)
}

// Transcripts syncs Claude Code conversations: <session>.jsonl plus the
// session's subagents/*.jsonl and tool-results/* (docs/sync-engine.md, 4,
// 8-1, 9).
type Transcripts struct {
	Projects    []TranscriptProject
	HistoryDays int
	SkipRecent  time.Duration // timer runs skip sessions written this recently
	GOOS        string
	// OpenWindow: a session written this recently counts as open and is
	// not overwritten (default 15 minutes).
	OpenWindow time.Duration
}

// Name implements engine.Source.
func (t *Transcripts) Name() string { return "conversations" }

func (t *Transcripts) split(logical string) (*TranscriptProject, string, bool) {
	pidPart, rest, ok := strings.Cut(logical, "/transcripts/")
	if !ok || !strings.HasPrefix(pidPart, "project:") {
		return nil, "", false
	}
	pid := strings.TrimPrefix(pidPart, "project:")
	for i := range t.Projects {
		if t.Projects[i].PID == pid {
			return &t.Projects[i], rest, rest != ""
		}
	}
	return nil, "", false
}

// Owns implements engine.Source.
func (t *Transcripts) Owns(logical string) bool {
	_, rest, ok := t.split(logical)
	return ok && validRel(rest)
}

func validRel(rest string) bool {
	for _, part := range strings.Split(rest, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func kindOfRel(rest string) string {
	if strings.Contains(rest, "/tool-results/") {
		return manifest.KindImmutable
	}
	return manifest.KindTranscript
}

// Resolve implements engine.Source.
func (t *Transcripts) Resolve(logical string) (string, string, bool) {
	p, rest, ok := t.split(logical)
	if !ok || !validRel(rest) {
		return "", "", false
	}
	return filepath.Join(p.Dir, filepath.FromSlash(rest)), kindOfRel(rest), true
}

// Scan implements engine.Source.
func (t *Transcripts) Scan(_ context.Context, run engine.ScanRun) ([]engine.Local, error) {
	var out []engine.Local
	window := time.Duration(t.HistoryDays) * 24 * time.Hour
	for _, p := range t.Projects {
		entries, err := os.ReadDir(p.Dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".jsonl") || strings.HasPrefix(name, ".") {
				continue
			}
			info, err := e.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			session := strings.TrimSuffix(name, ".jsonl")
			age := run.Now.Sub(info.ModTime())
			l := engine.Local{
				Logical:    "project:" + p.PID + "/transcripts/" + name,
				Kind:       manifest.KindTranscript,
				Path:       filepath.Join(p.Dir, name),
				OutOfScope: age > window,
				Skip:       run.Timer && age < t.SkipRecent,
			}
			out = append(out, l)
			// The session's subagent transcripts and stored tool outputs
			// travel with it.
			sub := filepath.Join(p.Dir, session)
			_ = filepath.WalkDir(sub, func(fp string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() || d.Type()&os.ModeSymlink != 0 || strings.HasPrefix(d.Name(), ".") {
					return nil
				}
				rel, _ := filepath.Rel(p.Dir, fp)
				rel = filepath.ToSlash(rel)
				parts := strings.Split(rel, "/")
				if len(parts) < 3 || parts[1] != "subagents" && parts[1] != "tool-results" {
					return nil
				}
				kind := kindOfRel(rel)
				if kind == manifest.KindTranscript && !strings.HasSuffix(rel, ".jsonl") {
					return nil
				}
				out = append(out, engine.Local{
					Logical: "project:" + p.PID + "/transcripts/" + rel, Kind: kind, Path: fp,
					OutOfScope: l.OutOfScope, Skip: l.Skip,
				})
				return nil
			})
		}
	}
	return out, nil
}

// ErrUnknownFormat means a transcript does not look like Claude Code's
// format; transcripts are left alone (PRD 12).
var ErrUnknownFormat = fmt.Errorf("conversation file in an unknown format: left alone (Claude Code may have changed it; update Karvan)")

// Normalize implements engine.Source: drops an unfinished last line and
// replaces the project path in the top-level "cwd" with Placeholder.
func (t *Transcripts) Normalize(logical string, raw []byte) ([]byte, error) {
	p, rest, ok := t.split(logical)
	if !ok || kindOfRel(rest) != manifest.KindTranscript {
		return raw, nil
	}
	content := completeLines(raw)
	st := merge.Stats(content)
	if st.Lines > 0 && (st.MissingUUIDs > 0 || st.Invalid*20 > st.Lines) {
		return nil, ErrUnknownFormat
	}
	return replaceCwd(content, p.Path, Placeholder), nil
}

// completeLines keeps whole lines only: anything after the last newline is
// being written right now, and so is a last line that is not valid JSON.
func completeLines(raw []byte) []byte {
	i := bytes.LastIndexByte(raw, '\n')
	if i < 0 {
		return nil
	}
	content := raw[:i+1]
	j := bytes.LastIndexByte(content[:len(content)-1], '\n')
	last := content[j+1 : len(content)-1]
	if len(last) > 0 && !json.Valid(last) {
		content = content[:j+1]
	}
	return content
}

// Denormalize implements engine.Source.
func (t *Transcripts) Denormalize(logical string, content []byte) ([]byte, error) {
	p, rest, ok := t.split(logical)
	if !ok || kindOfRel(rest) != manifest.KindTranscript {
		return content, nil
	}
	return replaceCwd(content, Placeholder, p.Path), nil
}

// replaceCwd rewrites the first "cwd" value of each line when it equals
// from or lies under it (whole path components only).
func replaceCwd(content []byte, from, to string) []byte {
	fromJ, toJ := jsonString(from), jsonString(to)
	key := []byte(`"cwd":"`)
	var out bytes.Buffer
	out.Grow(len(content))
	for len(content) > 0 {
		nl := bytes.IndexByte(content, '\n')
		line := content
		if nl >= 0 {
			line = content[:nl+1]
		}
		content = content[len(line):]
		k := bytes.Index(line, key)
		if k < 0 {
			out.Write(line)
			continue
		}
		v := k + len(key)
		if bytes.HasPrefix(line[v:], fromJ) {
			after := line[v+len(fromJ):]
			if len(after) > 0 && (after[0] == '"' || after[0] == '/' || after[0] == '\\') {
				out.Write(line[:v])
				out.Write(toJ)
				out.Write(after)
				continue
			}
		}
		out.Write(line)
	}
	return out.Bytes()
}

// jsonString is s as it appears inside a JSON string (no quotes).
func jsonString(s string) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	out := bytes.TrimSuffix(b.Bytes(), []byte{'\n'})
	return out[1 : len(out)-1]
}

// WriteCheck implements engine.Source: a conversation that is open on this
// machine is never overwritten (docs/sync-engine.md, 9-3).
func (t *Transcripts) WriteCheck(run engine.ScanRun, logical, path string, exists bool) error {
	if !exists {
		return nil
	}
	_, rest, _ := t.split(logical)
	if kindOfRel(rest) != manifest.KindTranscript {
		return nil
	}
	session := strings.TrimSuffix(filepath.Base(rest), ".jsonl")
	if run.SessionID != "" && strings.HasPrefix(rest, run.SessionID) {
		return fmt.Errorf("%w: this conversation is being opened right now", engine.ErrDefer)
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	open := t.OpenWindow
	if open == 0 {
		open = 15 * time.Minute
	}
	if age := run.Now.Sub(info.ModTime()); age < open && age > -time.Minute {
		return fmt.Errorf("%w: conversation %s was written in the last %d minutes and may be open", engine.ErrDefer, short(session), int(open.Minutes()))
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if info.Size() > 0 {
		b := make([]byte, 1)
		if _, err := f.ReadAt(b, info.Size()-1); err == nil && b[0] != '\n' {
			return fmt.Errorf("%w: conversation %s is being written", engine.ErrDefer, short(session))
		}
	}
	return nil
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
