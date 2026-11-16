package merge

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func TestText3(t *testing.T) {
	for _, c := range []struct{ base, a, b, want string }{
		{"a\nb\nc\n", "A\nb\nc\n", "a\nb\nC\n", "A\nb\nC\n"},
		{"a\nb\nc\n", "a\nb\nc\nd\n", "z\na\nb\nc\n", "z\na\nb\nc\nd\n"},
		{"a\nb\nc\n", "a\nc\n", "a\nb\nc\nd\n", "a\nc\nd\n"},
		{"a\n", "a\n", "b\n", "b\n"},
		{"x\n", "y\n", "y\n", "y\n"},
		{"", "a\n", "", "a\n"},
	} {
		got, ok := Text3([]byte(c.base), []byte(c.a), []byte(c.b))
		if !ok || string(got) != c.want {
			t.Errorf("Text3(%q,%q,%q) = %q,%v want %q", c.base, c.a, c.b, got, ok, c.want)
		}
	}
	if _, ok := Text3([]byte("a\nb\nc\n"), []byte("a\nX\nc\n"), []byte("a\nY\nc\n")); ok {
		t.Error("conflicting edits must not merge")
	}
	if _, ok := Text3(nil, []byte("a\n"), []byte("b\n")); ok {
		t.Error("two different additions with an empty base must conflict")
	}
}

// line builds a transcript line.
func line(uuid, parent, ts string) string {
	if uuid == "" {
		return fmt.Sprintf(`{"type":"last-prompt","leafUuid":"%s"}`, parent)
	}
	p := "null"
	if parent != "" {
		p = `"` + parent + `"`
	}
	return fmt.Sprintf(`{"type":"user","uuid":"%s","parentUuid":%s,"timestamp":"2026-11-16T%s.000Z"}`, uuid, p, ts)
}

func doc(lines ...string) []byte { return []byte(strings.Join(lines, "\n") + "\n") }

func keys(b []byte) string {
	var ks []string
	for _, l := range ParseTranscript(b) {
		ks = append(ks, strings.TrimPrefix(l.Key, "u:")[:min(4, len(strings.TrimPrefix(l.Key, "u:")))])
	}
	return strings.Join(ks, " ")
}

var (
	u1 = line("u1", "", "09:00:00")
	u2 = line("u2", "u1", "09:01:00")
	u3 = line("u3", "u2", "09:02:00")
)

func TestTranscriptPrefix(t *testing.T) {
	short := doc(u1, u2, line("", "u2", ""))
	long := doc(u1, u2, line("", "u2", ""), u3, line("", "u3", ""))
	for _, got := range [][]byte{Transcripts(short, long), Transcripts(long, short)} {
		if !bytes.Equal(got, long) {
			t.Fatalf("got\n%s", got)
		}
	}
}

// PRD 15 #1 / spec example 2: a session continued on both machines keeps
// both branches, contiguous, and both machines produce identical bytes.
func TestTranscriptBranches(t *testing.T) {
	a1, a2 := line("a1", "u3", "10:00:00"), line("a2", "a1", "10:01:00")
	b1, b2 := line("b1", "u3", "10:05:00"), line("b2", "b1", "10:06:00")
	work := doc(u1, u2, u3, a1, a2, line("", "a2", ""))
	home := doc(u1, u2, u3, b1, b2, line("", "b2", ""))
	m1, m2 := Transcripts(work, home), Transcripts(home, work)
	if !bytes.Equal(m1, m2) {
		t.Fatal("merge depends on input order")
	}
	if k := keys(m1); k != "u1 u2 u3 a1 a2 h:a2 b1 b2 h:b2" {
		t.Fatalf("order: %s", k)
	}
	// Idempotent: merging the result again with either side changes nothing.
	if !bytes.Equal(Transcripts(m1, work), m1) || !bytes.Equal(Transcripts(home, m1), m1) {
		t.Fatal("not idempotent")
	}
}

// Repeated identical uuid-less lines in the shared part are not duplicated.
func TestTranscriptRepeatedLines(t *testing.T) {
	latch := `{"type":"atis-latch","atis":1}`
	base := []string{u1, latch, u2, latch, latch}
	a := doc(append(base, line("a", "u2", "10:00:00"), latch)...)
	b := doc(append(base, line("b", "u2", "10:01:00"), latch)...)
	m := Transcripts(a, b)
	if n := strings.Count(string(m), "atis-latch"); n != 5 {
		t.Fatalf("%d latch lines, want 5:\n%s", n, m)
	}
}

// Property: random branchings of a shared session merged in any order give
// the same result, contain every line exactly once and keep each input's
// order.
func TestTranscriptProperties(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for iter := 0; iter < 200; iter++ {
		shared := []string{u1}
		prev := "u1"
		for i := 0; i < rng.Intn(5); i++ {
			id := fmt.Sprintf("s%d", i)
			shared = append(shared, line(id, prev, fmt.Sprintf("09:%02d:00", i+10)))
			prev = id
		}
		var versions [][]byte
		for v := 0; v < 3; v++ {
			ls := append([]string(nil), shared...)
			p := prev
			for i := 0; i < rng.Intn(4); i++ {
				id := fmt.Sprintf("v%d-%d", v, i)
				ls = append(ls, line(id, p, fmt.Sprintf("1%d:%02d:%02d", v, i, rng.Intn(60))))
				if rng.Intn(2) == 0 {
					ls = append(ls, `{"type":"atis-latch"}`)
				}
				p = id
			}
			versions = append(versions, doc(ls...))
		}
		ref := Transcripts(versions...)
		perms := [][]int{{0, 1, 2}, {2, 1, 0}, {1, 0, 2}}
		for _, pm := range perms {
			got := Transcripts(versions[pm[0]], versions[pm[1]], versions[pm[2]])
			if !bytes.Equal(got, ref) {
				t.Fatalf("iteration %d: order dependence", iter)
			}
			// Pairwise associativity.
			got2 := Transcripts(Transcripts(versions[pm[0]], versions[pm[1]]), versions[pm[2]])
			if !bytes.Equal(got2, ref) {
				t.Fatalf("iteration %d: pairwise merge differs:\n%s\nvs\n%s", iter, got2, ref)
			}
		}
		mk := map[string]int{}
		for _, l := range ParseTranscript(ref) {
			mk[l.Key]++
		}
		for _, v := range versions {
			last := -1
			pos := map[string]int{}
			for i, l := range ParseTranscript(ref) {
				pos[l.Key] = i
			}
			for _, l := range ParseTranscript(v) {
				if mk[l.Key] != 1 {
					t.Fatalf("key %s appears %d times", l.Key, mk[l.Key])
				}
				if pos[l.Key] < last {
					t.Fatalf("order of an input not kept")
				}
				last = pos[l.Key]
			}
		}
	}
}

func TestTranscriptInvalidLinesKept(t *testing.T) {
	a := doc(u1, "not json", u2)
	b := doc(u1, "not json", u2, u3)
	if m := Transcripts(a, b); !bytes.Equal(m, b) {
		t.Fatalf("%s", m)
	}
	s := Stats(doc(u1, "garbage", `{"type":"assistant"}`))
	if s.Lines != 3 || s.Invalid != 1 || s.MissingUUIDs != 1 {
		t.Fatalf("%+v", s)
	}
}

func TestDesktopActivity(t *testing.T) {
	if n, err := DesktopActivity([]byte(`{"lastActivityAt":1763300000123,"cwd":"/x"}`)); err != nil || n != 1763300000123 {
		t.Fatal(n, err)
	}
	if _, err := DesktopActivity([]byte(`{"cwd":"/x"}`)); err == nil {
		t.Fatal("missing field accepted")
	}
}

func BenchmarkTranscript50k(b *testing.B) {
	var ls []string
	prev := ""
	for i := 0; i < 50000; i++ {
		id := fmt.Sprintf("x%d", i)
		ls = append(ls, line(id, prev, fmt.Sprintf("%02d:%02d:%02d", i/3600%24, i/60%60, i%60)))
		prev = id
	}
	a := doc(append(ls, line("a", prev, "23:59:58"))...)
	c := doc(append(ls, line("c", prev, "23:59:59"))...)
	for i := 0; i < b.N; i++ {
		Transcripts(a, c)
	}
}
