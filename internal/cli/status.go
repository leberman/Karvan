package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/leberman/karvan/internal/config"
	"github.com/leberman/karvan/internal/hooks"
	"github.com/leberman/karvan/internal/systemd"
)

func newStatusCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the last sync, what is waiting or held back, conflicts and problems",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w := cmd.OutOrStdout()
			cfg, err := app.LoadConfig()
			if errors.Is(err, config.ErrNotInitialized) {
				fmt.Fprintf(w, "config:       %s (not found)\n", app.ConfigPath())
				fmt.Fprintln(w, "Karvan is not set up on this machine yet: run `karvan init`.")
				if n := app.updateNotice(); n != "" {
					fmt.Fprintf(w, "update:       %s\n", n)
				}
				return nil
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "config:       %s\n", app.ConfigPath())
			fmt.Fprintf(w, "machine:      %s\n", cfg.Machine)
			fmt.Fprintf(w, "store:        %s\n", describeStore(cfg.Store))
			fmt.Fprintf(w, "projects:     %d\n", len(cfg.Projects))
			fmt.Fprintf(w, "Claude Code:  %s\n", app.Env.ClaudeHome())
			fmt.Fprintf(w, "desktop app:  %s\n", app.Env.DesktopDataDir())
			printLastRun(w, app, cfg)
			if n := app.updateNotice(); n != "" {
				fmt.Fprintf(w, "update:       %s\n", n)
			}
			return nil
		},
	}
}

func ago(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

func printLastRun(w io.Writer, app *App, cfg *config.Config) {
	var last LastRun
	b, err := os.ReadFile(statusFile(app))
	if err != nil || json.Unmarshal(b, &last) != nil || last.Report == nil {
		fmt.Fprintln(w, "last sync:    never; run `karvan sync`")
		return
	}
	now := time.Now()
	rep := last.Report
	result := "ok"
	if last.Error != "" {
		result = "FAILED: " + last.Error
	}
	fmt.Fprintf(w, "last sync:    %s (%s, %s)\n", ago(last.When, now), last.Mode, result)
	if rep.Sent+rep.Received+rep.Merged+rep.Deleted > 0 {
		fmt.Fprintf(w, "              %d sent, %d received, %d merged, %d deleted\n", rep.Sent, rep.Received, rep.Merged, rep.Deleted)
	}
	if len(rep.Machines) > 0 {
		fmt.Fprintln(w, "other machines:")
		for _, name := range sortedNames(rep.Machines) {
			m := rep.Machines[name]
			fmt.Fprintf(w, "  %-12s last sync %s (Karvan %s)\n", name, ago(m.WrittenAt, now), m.Karvan)
		}
	}
	for _, c := range rep.Conflicts {
		fmt.Fprintf(w, "conflict:     %s: the other version is kept as %s; merge them by hand and delete the copy\n", c.Path, c.Copy)
	}
	for _, d := range rep.Deferred {
		fmt.Fprintf(w, "waiting:      %s (%s)\n", d.Path, d.Reason)
	}
	for _, wn := range rep.Warnings {
		fmt.Fprintf(w, "warning:      %s\n", wn)
	}
	if rep.Unplaced > 0 {
		fmt.Fprintf(w, "not here:     %d file(s) belong to projects not set up on this machine; add them with `karvan init`\n", rep.Unplaced)
	}
	_ = cfg
}

func sortedNames[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func describeStore(s config.Store) string {
	switch s.Kind {
	case config.StoreGit:
		return fmt.Sprintf("git %s (branch %s)", s.URL, s.Branch)
	case config.StoreFolder:
		return "folder " + s.Path
	}
	return s.Kind
}

func newDoctorCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check the setup: config, Claude folders, hooks, timer, store access, network",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := &doctor{w: cmd.OutOrStdout()}
			d.run(app)
			if d.failed > 0 {
				return fmt.Errorf("%d check(s) failed", d.failed)
			}
			return nil
		},
	}
}

// doctor prints one line per check. Later phases add hooks, timer, store,
// network (port 22/443), SSH host keys and desktop-app format checks.
type doctor struct {
	w      io.Writer
	failed int
}

func (d *doctor) ok(format string, a ...any)   { d.line("ok  ", format, a...) }
func (d *doctor) warn(format string, a ...any) { d.line("warn", format, a...) }
func (d *doctor) info(format string, a ...any) { d.line("info", format, a...) }
func (d *doctor) fail(format string, a ...any) {
	d.failed++
	d.line("FAIL", format, a...)
}

func (d *doctor) line(tag, format string, a ...any) {
	fmt.Fprintf(d.w, "%s  %s\n", tag, fmt.Sprintf(format, a...))
}

func (d *doctor) run(app *App) {
	env := app.Env

	cfg, err := app.LoadConfig()
	switch {
	case errors.Is(err, config.ErrNotInitialized):
		d.warn("config: %s not found; run `karvan init`", app.ConfigPath())
	case err != nil:
		d.fail("config: %v", err)
	default:
		d.ok("config: %s (machine %q)", app.ConfigPath(), cfg.Machine)
	}

	if err := isDir(env.ClaudeHome()); err != nil {
		d.fail("Claude Code folder %s: %v (has Claude Code run on this machine?)", env.ClaudeHome(), err)
	} else {
		d.ok("Claude Code folder: %s", env.ClaudeHome())
	}
	if err := isDir(env.ClaudeProjectsDir()); err != nil {
		d.warn("Claude Code projects folder %s: %v (no conversations yet?)", env.ClaudeProjectsDir(), err)
	} else {
		d.ok("Claude Code projects folder: %s", env.ClaudeProjectsDir())
	}
	if err := isDir(env.DesktopDataDir()); err != nil {
		d.info("Claude desktop app data not found at %s (fine if you only use the terminal)", env.DesktopDataDir())
	} else {
		d.ok("Claude desktop app data: %s", env.DesktopDataDir())
	}

	d.checkHooks(app)
	if n := app.updateNotice(); n != "" {
		d.info("%s", n)
	} else if !isDevBuild(app.Build.short()) {
		d.ok("version: %s is the latest", app.Build.short())
	}

	if cfg == nil {
		return
	}
	d.checkTimer(app, cfg)
	d.checkStore(app, cfg)
	for _, p := range cfg.Projects {
		if err := isDir(p.Path); err != nil {
			d.fail("project %s: %s: %v", p.ID, p.Path, err)
			continue
		}
		cdir := env.ClaudeProjectDir(p.Path)
		if err := isDir(cdir); err != nil {
			d.warn("project %s: no Claude Code folder %s yet (not used here, or the folder name rule changed)", p.ID, cdir)
			continue
		}
		d.ok("project %s: %s", p.ID, p.Path)
	}
}

func (d *doctor) checkHooks(app *App) {
	path := app.Env.ClaudeSettings()
	st, err := hooks.Check(path)
	switch {
	case errors.Is(err, hooks.ErrInvalidJSON):
		d.fail("Claude Code settings %s: %v (fix it by hand; Karvan will not change an invalid file)", path, err)
	case err != nil:
		d.fail("Claude Code settings %s: %v", path, err)
	case st.Installed(app.Executable()):
		d.ok("hooks: SessionStart and SessionEnd run %s", app.Executable())
	case st.Present["SessionStart"] || st.Present["SessionEnd"]:
		d.warn("hooks: present but out of date or incomplete; run `karvan init` again")
	default:
		d.warn("hooks: not installed; run `karvan init`")
	}
}

func (d *doctor) checkTimer(app *App, cfg *config.Config) {
	if runtime.GOOS != "linux" && app.Systemctl == nil {
		return
	}
	units := systemd.Render(app.Executable(), cfg.Sync.Interval.Duration, app.configPath)
	if msg := systemd.Check(systemd.Dir(app.configHome()), units, app.systemctl()); msg != "" {
		d.warn("timer: %s", msg)
		return
	}
	d.ok("timer: every %s, and at logout/shutdown", cfg.Sync.Interval.Duration)
}

func (d *doctor) checkStore(app *App, cfg *config.Config) {
	if cfg.Store.Kind == config.StoreGit && strings.Contains(cfg.Store.URL, "github.com") && app.DialCheck != nil {
		for _, hp := range []string{"github.com:22", "ssh.github.com:443"} {
			if err := app.DialCheck(hp); err != nil {
				d.warn("network: cannot reach %s (%v)", hp, err)
			} else {
				d.ok("network: %s reachable", hp)
			}
		}
	}
	eng, err := app.NewEngine(cfg)
	if err != nil {
		d.fail("store: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	snap, err := eng.Store.Snapshot(ctx)
	if err != nil {
		d.fail("store %s: %v", eng.Store.Describe(), err)
		return
	}
	if _, err := snap.Get("FORMAT"); err != nil {
		d.fail("store %s: not set up (run `karvan init`)", eng.Store.Describe())
		return
	}
	names, _ := snap.List("manifests/")
	d.ok("store: %s (%d machine(s))", eng.Store.Describe(), len(names))
}

func isDir(p string) error {
	st, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return errors.New("does not exist")
	}
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return errors.New("is not a folder")
	}
	return nil
}
