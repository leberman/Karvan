package blob

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestSealOpen(t *testing.T) {
	rnd := make([]byte, 4096)
	_, _ = rand.Read(rnd)
	for _, in := range [][]byte{nil, []byte("x"), bytes.Repeat([]byte("hello\n"), 1000), rnd} {
		s := Seal(in)
		out, err := Open(s)
		if err != nil || !bytes.Equal(out, in) {
			t.Fatalf("round trip failed: %v", err)
		}
	}
	if s := Seal(bytes.Repeat([]byte("a"), 10000)); len(s) > 200 || s[5] != codecZstd {
		t.Errorf("compressible data not compressed: %d bytes", len(s))
	}
	if s := Seal(rnd); s[5] != codecNone {
		t.Error("random data should be stored raw")
	}
	for _, bad := range [][]byte{nil, []byte("KRVN"), []byte("XXXX\x01\x00\x00\x00"), []byte("KRVN\x02\x00\x00\x00"), []byte("KRVN\x01\x00\x01\x00"), []byte("KRVN\x01\x01\x00\x00junk")} {
		if _, err := Open(bad); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
	h := Hash([]byte("a"))
	if _, err := OpenChunk(h, Seal([]byte("b"))); !errors.Is(err, ErrCorrupt) {
		t.Error("hash mismatch not detected")
	}
	if !Valid(h) || Valid("sha256:xyz") || Path(h) != "blobs/ca/"+h[7:] {
		t.Error("Valid/Path")
	}
}

func transcript(lines int, seed string) []byte {
	var b strings.Builder
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&b, `{"type":"user","uuid":"%s-%06d","message":{"content":"%s"}}`+"\n", seed, i, strings.Repeat("x", 200))
	}
	return []byte(b.String())
}

func TestChunkSmallAndJoin(t *testing.T) {
	if c := Chunk([]byte("abc")); len(c) != 1 {
		t.Fatal(len(c))
	}
	for _, in := range [][]byte{transcript(20000, "a"), bytes.Repeat([]byte("z"), 9<<20), append(transcript(5000, "b"), bytes.Repeat([]byte("q"), 6<<20)...)} {
		parts := Chunk(in)
		if !bytes.Equal(bytes.Join(parts, nil), in) {
			t.Fatal("chunks do not join back")
		}
		for i, p := range parts {
			if len(p) > hardMax+lineForce {
				t.Errorf("chunk %d too big: %d", i, len(p))
			}
			if i < len(parts)-1 && bytes.IndexByte(in, '\n') >= 0 && p[len(p)-1] != '\n' && len(p) != hardMax {
				t.Errorf("text chunk %d does not end at a newline", i)
			}
		}
	}
}

// Appending lines to a transcript must keep all earlier chunks identical:
// that is what keeps uploads (and git history) small.
func TestChunkAppendStable(t *testing.T) {
	base := transcript(20000, "s")
	more := append(append([]byte(nil), base...), transcript(300, "t")...)
	a, _ := Split(base)
	b, _ := Split(more)
	if len(a) < 5 {
		t.Fatalf("expected several chunks, got %d", len(a))
	}
	same := 0
	for i := range a {
		if i < len(b) && a[i] == b[i] {
			same++
		}
	}
	if same < len(a)-1 {
		t.Fatalf("only %d of %d chunks kept after append", same, len(a))
	}
}
