// Package update finds out whether a newer Karvan release exists, at most
// once a day, and runs the installer to move to it.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Repo is where releases are published.
const Repo = "leberman/Karvan"

// Checker looks up the latest release.
type Checker struct {
	StateDir string
	Client   *http.Client
	Now      func() time.Time
	// LatestURL redirects to the latest release page (tests replace it).
	LatestURL string
}

type cache struct {
	Checked time.Time `json:"checked"`
	Latest  string    `json:"latest"`
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Checker) path() string { return filepath.Join(c.StateDir, "update-check.json") }

// Latest returns the newest release tag (e.g. "v0.1.2"). It asks GitHub at
// most once a day unless force; network problems give "" without an error
// worth showing.
func (c *Checker) Latest(ctx context.Context, force bool) (string, error) {
	var cc cache
	if b, err := os.ReadFile(c.path()); err == nil {
		_ = json.Unmarshal(b, &cc)
	}
	if !force && cc.Latest != "" && c.now().Sub(cc.Checked) < 24*time.Hour {
		return cc.Latest, nil
	}
	tag, err := c.fetch(ctx)
	if err != nil {
		return cc.Latest, err
	}
	cc = cache{Checked: c.now(), Latest: tag}
	if b, err := json.Marshal(cc); err == nil {
		_ = os.MkdirAll(c.StateDir, 0o700)
		_ = os.WriteFile(c.path(), b, 0o600)
	}
	return tag, nil
}

// fetch follows github.com/<repo>/releases/latest, which redirects to
// .../releases/tag/<tag>. This needs no API token and has no rate limit.
func (c *Checker) fetch(ctx context.Context) (string, error) {
	url := c.LatestURL
	if url == "" {
		url = "https://github.com/" + Repo + "/releases/latest"
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	nc := *client
	nc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := nc.Do(req)
	if err != nil {
		return "", err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	i := strings.LastIndex(loc, "/tag/")
	if i < 0 {
		return "", fmt.Errorf("unexpected answer from GitHub (%s)", resp.Status)
	}
	return loc[i+len("/tag/"):], nil
}

// Newer reports whether release tag is newer than the running version.
// Development builds (no plain x.y.z version) never ask to update.
func Newer(tag, current string) bool {
	a, ok1 := parse(tag)
	b, ok2 := parse(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

func parse(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(v, "v")
	if strings.ContainsAny(v, "-+") {
		return out, false // snapshots, source builds
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// ErrNotWritable means the binary's folder cannot be written by this user.
var ErrNotWritable = errors.New("cannot write to the folder karvan is installed in")

// Install runs the release's install.sh for tag, installing next to exe.
func Install(ctx context.Context, tag, exe string, out io.Writer) error {
	dir := filepath.Dir(exe)
	probe := filepath.Join(dir, ".karvan-write-test")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		return fmt.Errorf("%w (%s); run the install command with the rights for that folder", ErrNotWritable, dir)
	}
	_ = os.Remove(probe)
	url := "https://github.com/" + Repo + "/releases/download/" + tag + "/install.sh"
	script := fmt.Sprintf(`set -e
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
if command -v curl >/dev/null 2>&1; then curl -fsSL --retry 3 -o "$tmp" %q; else wget -q -O "$tmp" %q; fi
sh "$tmp"`, url, url)
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Env = append(os.Environ(), "KARVAN_VERSION="+tag, "KARVAN_INSTALL_DIR="+dir, "KARVAN_NO_MODIFY_PATH=1")
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}
