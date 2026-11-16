package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteAtomicIf(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "f.txt")
	if err := WriteAtomicIf(p, []byte("one"), 0o600, time.Unix(1000, 0), &Stat{}); err != nil {
		t.Fatal(err)
	}
	st, ok, _ := StatOf(p)
	if !ok || st.Size != 3 || !st.Mtime.Equal(time.Unix(1000, 0)) {
		t.Fatalf("%+v", st)
	}
	// Expecting "does not exist" when it exists: refused.
	if err := WriteAtomicIf(p, []byte("x"), 0o600, time.Time{}, &Stat{}); !errors.Is(err, ErrChanged) {
		t.Fatalf("err = %v", err)
	}
	// Someone wrote meanwhile: refused, file untouched.
	old := st
	_ = os.WriteFile(p, []byte("changed!"), 0o600)
	if err := WriteAtomicIf(p, []byte("two"), 0o600, time.Time{}, &old); !errors.Is(err, ErrChanged) {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "changed!" {
		t.Fatal("overwrote a changed file")
	}
	cur, _, _ := StatOf(p)
	if err := AppendIf(p, []byte("+more"), time.Unix(2000, 0), cur); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "changed!+more" {
		t.Fatalf("%q", b)
	}
	if err := AppendIf(p, []byte("x"), time.Time{}, cur); !errors.Is(err, ErrChanged) {
		t.Fatal("append on a changed file")
	}
	data, _, err := ReadStable(p)
	if err != nil || string(data) != "changed!+more" {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "sub", ".karvan-tmp-123"), nil, 0o600)
	CleanTemp(filepath.Join(dir, "sub"))
	if _, err := os.Stat(filepath.Join(dir, "sub", ".karvan-tmp-123")); !os.IsNotExist(err) {
		t.Fatal("temp file left")
	}
}
