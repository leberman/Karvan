package sources

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leberman/karvan/internal/engine"
	"github.com/leberman/karvan/internal/manifest"
)

const sess = "0b7e2c11-aaaa-bbbb-cccc-000000000001"

func line(uuid, cwd string) string {
	return `{"parentUuid":null,"cwd":"` + cwd + `","sessionId":"` + sess + `","type":"user","uuid":"` + uuid + `","timestamp":"2026-11-16T09:00:00.000Z","message":{"content":"see /home/u/work/app/x"}}`
}

func newT(t *testing.T) (*Transcripts, string) {
	dir := t.TempDir()
	return &Transcripts{Projects: []TranscriptProject{{PID: "p-1", Path: "/home/u/work/app", Dir: dir}}, HistoryDays: 7, SkipRecent: 15 * time.Minute}, dir
}

func TestTranscriptNormalize(t *testing.T) {
	tr, _ := newT(t)
	lg := "project:p-1/transcripts/" + sess + ".jsonl"
	raw := line("u1", "/home/u/work/app") + "\n" + line("u2", "/home/u/work/app/sub") + "\n" + line("u3", "/home/u/work/app-old") + "\n" + `{"type":"user","uu`
	got, err := tr.Normalize(lg, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if strings.Count(s, "\n") != 3 || strings.Contains(s, `"uu`+"\n") {
		t.Fatalf("unfinished line kept:\n%s", s)
	}
	if !strings.Contains(s, `"cwd":"{{karvan:project}}","sessionId"`) || !strings.Contains(s, `"cwd":"{{karvan:project}}/sub"`) {
		t.Fatalf("cwd not replaced:\n%s", s)
	}
	if !strings.Contains(s, `"cwd":"/home/u/work/app-old"`) {
		t.Fatal("a different folder with the same prefix must not be replaced")
	}
	if !strings.Contains(s, `"content":"see /home/u/work/app/x"`) {
		t.Fatal("message text must not be touched")
	}
	// Another machine with a different path gets its own path back.
	home := &Transcripts{Projects: []TranscriptProject{{PID: "p-1", Path: "/home/v/temp/app", Dir: t.TempDir()}}}
	back, _ := home.Denormalize(lg, got)
	if !strings.Contains(string(back), `"cwd":"/home/v/temp/app/sub"`) || strings.Contains(string(back), "{{karvan") {
		t.Fatalf("denormalize:\n%s", back)
	}
	// A complete but invalid last line is still being written.
	got, _ = tr.Normalize(lg, []byte(line("u1", "/x")+"\n"+`{"broken`+"\n"))
	if strings.Count(string(got), "\n") != 1 {
		t.Fatalf("%q", got)
	}
	// Unknown format: user lines without uuid.
	if _, err := tr.Normalize(lg, []byte(`{"type":"user","message":"x"}`+"\n")); !errors.Is(err, ErrUnknownFormat) {
		t.Fatalf("err = %v", err)
	}
	// Tool results are copied byte for byte.
	tool := "project:p-1/transcripts/" + sess + "/tool-results/abc.txt"
	if b, _ := tr.Normalize(tool, []byte("partial")); string(b) != "partial" {
		t.Fatal("tool result changed")
	}
}

func TestTranscriptScan(t *testing.T) {
	tr, dir := newT(t)
	now := time.Now()
	write := func(rel string, age time.Duration) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte(line("u", "/x")+"\n"), 0o600)
		_ = os.Chtimes(p, now.Add(-age), now.Add(-age))
	}
	write(sess+".jsonl", time.Hour)
	write(sess+"/subagents/agent-1.jsonl", time.Hour)
	write(sess+"/tool-results/out.txt", time.Hour)
	write("old.jsonl", 10*24*time.Hour)
	write("fresh.jsonl", time.Minute)
	write("memory/MEMORY.md", time.Hour) // not a transcript
	locals, err := tr.Scan(context.Background(), engine.ScanRun{Now: now, Timer: true})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]engine.Local{}
	for _, l := range locals {
		got[strings.TrimPrefix(l.Logical, "project:p-1/transcripts/")] = l
	}
	if len(got) != 5 {
		t.Fatalf("%v", got)
	}
	if got[sess+"/tool-results/out.txt"].Kind != manifest.KindImmutable || got[sess+"/subagents/agent-1.jsonl"].Kind != manifest.KindTranscript {
		t.Fatal("kinds")
	}
	if !got["old.jsonl"].OutOfScope || got[sess+".jsonl"].OutOfScope {
		t.Fatal("history window")
	}
	if !got["fresh.jsonl"].Skip || got[sess+".jsonl"].Skip {
		t.Fatal("timer skip")
	}
	if p, k, ok := tr.Resolve("project:p-1/transcripts/" + sess + ".jsonl"); !ok || k != manifest.KindTranscript || p != filepath.Join(dir, sess+".jsonl") {
		t.Fatal(p, k, ok)
	}
	for _, bad := range []string{"project:p-2/transcripts/x.jsonl", "project:p-1/transcripts/../x", "project:p-1/memory/x"} {
		if tr.Owns(bad) {
			t.Errorf("owns %q", bad)
		}
	}
}

func TestTranscriptWriteCheck(t *testing.T) {
	tr, dir := newT(t)
	p := filepath.Join(dir, sess+".jsonl")
	lg := "project:p-1/transcripts/" + sess + ".jsonl"
	now := time.Now()
	_ = os.WriteFile(p, []byte(line("u", "/x")+"\n"), 0o600)
	_ = os.Chtimes(p, now.Add(-time.Hour), now.Add(-time.Hour))
	if err := tr.WriteCheck(engine.ScanRun{Now: now}, lg, p, true); err != nil {
		t.Fatal(err)
	}
	if err := tr.WriteCheck(engine.ScanRun{Now: now, SessionID: sess}, lg, p, true); !errors.Is(err, engine.ErrDefer) {
		t.Fatal("session being opened must not be overwritten")
	}
	_ = os.Chtimes(p, now.Add(-time.Minute), now.Add(-time.Minute))
	if err := tr.WriteCheck(engine.ScanRun{Now: now}, lg, p, true); !errors.Is(err, engine.ErrDefer) {
		t.Fatal("recently written session must not be overwritten")
	}
	_ = os.WriteFile(p, []byte(line("u", "/x")+"\n{\"par"), 0o600)
	_ = os.Chtimes(p, now.Add(-time.Hour), now.Add(-time.Hour))
	if err := tr.WriteCheck(engine.ScanRun{Now: now}, lg, p, true); !errors.Is(err, engine.ErrDefer) {
		t.Fatal("session with an unfinished line must not be overwritten")
	}
	if err := tr.WriteCheck(engine.ScanRun{Now: now}, lg, p+"x", false); err != nil {
		t.Fatal("new files are always written")
	}
}
