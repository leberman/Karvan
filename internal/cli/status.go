package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/spf13/cobra"

	"github.com/leberman/karvan/internal/config"
	"github.com/leberman/karvan/internal/hooks"
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
			fmt.Fprintln(w, "last sync:    never (the sync engine arrives in M0 phase 3)")
			return nil
		},
	}
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

	if cfg == nil {
		return
	}
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
