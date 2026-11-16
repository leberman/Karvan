package systemd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstallCheckRemove(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "systemd", "user")
	var calls []string
	active := false
	run := func(args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "enable" {
			active = true
		}
		if args[0] == "is-active" && active {
			return []byte("active\n"), nil
		}
		return []byte("inactive\n"), nil
	}
	u := Render("/opt/my apps/karvan", 10*time.Minute, "")
	if !strings.Contains(u[Service], `ExecStart="/opt/my apps/karvan" sync --timer`) || !strings.Contains(u[Timer], "OnUnitActiveSec=10min") ||
		!strings.Contains(u[Shutdown], "ExecStop=\"/opt/my apps/karvan\" sync --background") {
		t.Fatalf("%v", u)
	}
	if msg := Check(dir, u, run); !strings.Contains(msg, "not installed") {
		t.Fatal(msg)
	}
	if err := Install(dir, u, run); err != nil {
		t.Fatal(err)
	}
	if msg := Check(dir, u, run); msg != "" {
		t.Fatal(msg)
	}
	if msg := Check(dir, Render("/usr/bin/karvan", 10*time.Minute, ""), run); !strings.Contains(msg, "another karvan binary") {
		t.Fatal(msg)
	}
	removed, err := Remove(dir, run)
	if err != nil || !removed {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, Timer)); !os.IsNotExist(err) {
		t.Fatal("unit left")
	}
	if removed, _ := Remove(dir, run); removed {
		t.Fatal("second remove")
	}
	if !strings.Contains(strings.Join(calls, ";"), "enable --now karvan-sync.timer karvan-shutdown.service") {
		t.Fatal(calls)
	}
}
