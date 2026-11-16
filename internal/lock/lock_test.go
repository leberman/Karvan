package lock

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLock(t *testing.T) {
	dir := t.TempDir()
	l, err := Acquire(context.Background(), dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := Acquire(context.Background(), dir, 300*time.Millisecond); !errors.Is(err, ErrBusy) {
		t.Fatalf("second lock: %v", err)
	}
	if time.Since(start) < 250*time.Millisecond {
		t.Fatal("did not wait")
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	l2, err := Acquire(context.Background(), dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = l2.Release()
}
