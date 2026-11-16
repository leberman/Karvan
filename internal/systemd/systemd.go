// Package systemd installs Karvan's systemd user units on Linux (PRD 6-4,
// 10): a timer that syncs every few minutes while the machine is on, and a
// unit that syncs once more at logout and shutdown.
package systemd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/leberman/karvan/internal/fsutil"
)

// Unit names.
const (
	Service  = "karvan-sync.service"
	Timer    = "karvan-sync.timer"
	Shutdown = "karvan-shutdown.service"
)

// Units holds the rendered unit files.
type Units map[string]string

func quote(s string) string {
	if !strings.ContainsAny(s, " \t\"'\\") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// Render builds the units for exe with the given timer interval.
func Render(exe string, interval time.Duration, configPath string) Units {
	cmd := quote(exe)
	if configPath != "" {
		cmd += " --config " + quote(configPath)
	}
	return Units{
		Service: fmt.Sprintf(`# Written by karvan init. Remove with karvan uninstall.
[Unit]
Description=Karvan: sync Claude Code work between your computers
After=network-online.target

[Service]
Type=oneshot
ExecStart=%s sync --timer
Nice=10
`, cmd),
		Timer: fmt.Sprintf(`# Written by karvan init. Remove with karvan uninstall.
[Unit]
Description=Karvan: sync every %s

[Timer]
OnStartupSec=2min
OnUnitActiveSec=%s
RandomizedDelaySec=30s

[Install]
WantedBy=timers.target
`, interval, systemdDuration(interval)),
		Shutdown: fmt.Sprintf(`# Written by karvan init. Remove with karvan uninstall.
[Unit]
Description=Karvan: send everything before logout or shutdown

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/true
ExecStop=%s sync --background
TimeoutStopSec=90

[Install]
WantedBy=default.target
`, cmd),
	}
}

func systemdDuration(d time.Duration) string {
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dmin", int(d.Minutes()))
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

// Dir is the systemd user unit folder.
func Dir(configHome string) string { return filepath.Join(configHome, "systemd", "user") }

// Runner runs systemctl --user; tests replace it.
type Runner func(args ...string) ([]byte, error)

// Systemctl runs the real `systemctl --user`.
func Systemctl(args ...string) ([]byte, error) {
	cmd := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if errors.Is(err, exec.ErrNotFound) {
		return nil, errors.New("systemctl not found: this system does not use systemd; run `karvan sync` from your own scheduler")
	}
	if err != nil {
		return out.Bytes(), fmt.Errorf("systemctl --user %s: %s", strings.Join(args, " "), strings.TrimSpace(out.String()))
	}
	return out.Bytes(), nil
}

// Install writes the units and enables them.
func Install(dir string, units Units, run Runner) error {
	for name, content := range units {
		if err := fsutil.WriteAtomic(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			return err
		}
	}
	if _, err := run("daemon-reload"); err != nil {
		return err
	}
	if _, err := run("enable", "--now", Timer, Shutdown); err != nil {
		return err
	}
	return nil
}

// Remove disables and deletes the units. It is fine if they are not there.
func Remove(dir string, run Runner) (bool, error) {
	found := false
	for _, n := range []string{Timer, Service, Shutdown} {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			found = true
		}
	}
	if !found {
		return false, nil
	}
	_, _ = run("disable", "--now", Timer, Shutdown)
	for _, n := range []string{Timer, Service, Shutdown} {
		if err := os.Remove(filepath.Join(dir, n)); err != nil && !os.IsNotExist(err) {
			return true, err
		}
	}
	_, _ = run("daemon-reload")
	return true, nil
}

// Check reports problems with the installed units ("" = all good).
func Check(dir string, want Units, run Runner) string {
	for name, content := range want {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "timer not installed; run `karvan init`"
		}
		if string(b) != content {
			return "timer installed for another karvan binary or interval; run `karvan init` again"
		}
	}
	out, err := run("is-active", Timer)
	if err != nil || strings.TrimSpace(string(out)) != "active" {
		return "timer installed but not active: `systemctl --user enable --now " + Timer + "`"
	}
	return ""
}
