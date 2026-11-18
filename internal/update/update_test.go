package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		tag, cur string
		want     bool
	}{
		{"v0.1.2", "0.1.1", true},
		{"v0.2.0", "0.1.9", true},
		{"v1.0.0", "0.9.9", true},
		{"v0.1.1", "0.1.1", false},
		{"v0.1.0", "0.1.1", false},
		{"v0.1.2", "dev", false},
		{"v0.1.2", "source-main", false},
		{"v0.1.2", "0.0.1-snapshot+abc", false},
		{"", "0.1.1", false},
	} {
		if got := Newer(c.tag, c.cur); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.tag, c.cur, got)
		}
	}
}

func TestLatestCachesForADay(t *testing.T) {
	hits := 0
	tag := "v0.1.2"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, "https://github.com/leberman/Karvan/releases/tag/"+tag, http.StatusFound)
	}))
	defer srv.Close()
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	c := &Checker{StateDir: t.TempDir(), LatestURL: srv.URL, Now: func() time.Time { return now }}
	got, err := c.Latest(context.Background(), false)
	if err != nil || got != "v0.1.2" {
		t.Fatal(got, err)
	}
	tag = "v0.1.3"
	if got, _ := c.Latest(context.Background(), false); got != "v0.1.2" || hits != 1 {
		t.Fatalf("not cached: %s %d", got, hits)
	}
	if got, _ := c.Latest(context.Background(), true); got != "v0.1.3" {
		t.Fatal("force did not refresh")
	}
	now = now.Add(25 * time.Hour)
	tag = "v0.1.4"
	if got, _ := c.Latest(context.Background(), false); got != "v0.1.4" {
		t.Fatal("cache did not expire")
	}
	// Offline: the last known answer, with the error.
	srv.Close()
	now = now.Add(25 * time.Hour)
	if got, err := c.Latest(context.Background(), false); got != "v0.1.4" || err == nil {
		t.Fatal(got, err)
	}
}
