package manifest

import (
	"testing"
	"time"
)

func e(hash string, hist ...string) *Entry {
	return &Entry{Kind: KindText, Hash: hash, History: hist}
}

func TestDominates(t *testing.T) {
	a1, a2 := e("h1"), e("h2", "h1")
	if !Dominates(a2, a1) || Dominates(a1, a2) {
		t.Fatal("linear history")
	}
	b := e("h3", "h1")
	if Dominates(a2, b) || Dominates(b, a2) {
		t.Fatal("concurrent versions must not dominate each other")
	}
	if MergeBase(a2, b) != "h1" || MergeBase(e("x"), e("y")) != "" {
		t.Fatal("merge base")
	}

	// Tombstone of h1.
	del := &Entry{Kind: KindText, Deleted: true, History: []string{"h1"}}
	if !Dominates(del, a1) {
		t.Error("delete of h1 must win over unchanged h1")
	}
	if !Dominates(a2, del) || Dominates(del, a2) {
		t.Error("change after h1 must win over the delete of h1")
	}
	if Dominates(del, b) {
		t.Error("delete must not win over a concurrent change")
	}
	del2 := &Entry{Kind: KindText, Deleted: true, History: []string{"h1"}}
	if !Same(del, del2) || Dominates(del, del2) {
		t.Error("equal tombstones")
	}
}

func TestAdvanceTombstoneHistory(t *testing.T) {
	x := &Entry{Kind: KindText}
	x.Advance("h1")
	x.Chunks = []string{"c1"}
	x.Advance("h2")
	x.Chunks = []string{"c2"}
	if x.Hash != "h2" || len(x.History) != 1 || x.History[0] != "h1" || x.BaseChunks["h1"][0] != "c1" {
		t.Fatalf("%+v", x)
	}
	x.Advance("h2")
	if len(x.History) != 1 {
		t.Fatal("same hash must not grow history")
	}
	x.Tombstone(time.Now())
	if !x.Deleted || x.Last() != "h2" || x.Hash != "" || x.Chunks != nil {
		t.Fatalf("%+v", x)
	}
	for i := 0; i < 100; i++ {
		x.Advance(string(rune('a'+i%26)) + string(rune('0'+i/26)))
		x.Chunks = []string{"c"}
	}
	if len(x.History) != 64 || len(x.BaseChunks) > BaseChunksLimit+1 {
		t.Fatalf("history %d bases %d", len(x.History), len(x.BaseChunks))
	}
	tr := &Entry{Kind: KindTranscript}
	for i := 0; i < 20; i++ {
		tr.Advance(string(rune('a' + i)))
	}
	if len(tr.History) != 8 || tr.BaseChunks != nil {
		t.Fatal("transcript history limit")
	}
}

func TestMergedHistory(t *testing.T) {
	h := MergedHistory(KindText, e("a", "base", "old"), e("b", "base"))
	want := []string{"a", "b", "base", "old"}
	if len(h) != len(want) {
		t.Fatalf("%v", h)
	}
	for i := range want {
		if h[i] != want[i] {
			t.Fatalf("%v", h)
		}
	}
}

func TestEncodeDecode(t *testing.T) {
	m := &Manifest{Format: Format, Machine: "work", Seq: 3, Entries: map[string]*Entry{"global/CLAUDE.md": e("h", "g")}}
	b, err := Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(b)
	if err != nil || got.Seq != 3 || got.Entries["global/CLAUDE.md"].History[0] != "g" {
		t.Fatalf("%+v %v", got, err)
	}
	m.Format = Format + 1
	b, _ = Encode(m)
	if _, err := Decode(b); err == nil {
		t.Fatal("newer format accepted")
	}
	if _, err := Decode([]byte("junk")); err == nil {
		t.Fatal("junk accepted")
	}
}
