package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/leberman/karvan/internal/config"
	"github.com/leberman/karvan/internal/engine"
	"github.com/leberman/karvan/internal/fsutil"
	"github.com/leberman/karvan/internal/lock"
	"github.com/leberman/karvan/internal/sources"
	"github.com/leberman/karvan/internal/store"
)

// sessionStartBudget is how long SessionStart may hold up Claude Code; the
// rest continues in the background (docs/sync-engine.md, open question 10).
const sessionStartBudget = 8 * time.Second

type syncFlags struct {
	hook       string
	timer      bool
	background bool
}

func newSyncCmd(app *App) *cobra.Command {
	var f syncFlags
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Sync now",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch f.hook {
			case "", "session-start", "session-end":
			default:
				return usageError{fmt.Errorf("--hook %q: must be session-start or session-end", f.hook)}
			}
			return runSync(cmd.Context(), app, cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.hook, "hook", "", "called from a Claude Code hook (session-start or session-end)")
	cmd.Flags().BoolVar(&f.timer, "timer", false, "called by the timer: quiet, skips sessions written in the last minutes")
	cmd.Flags().BoolVar(&f.background, "background", false, "quiet run started by Karvan itself")
	for _, n := range []string{"hook", "timer", "background"} {
		_ = cmd.Flags().MarkHidden(n)
	}
	return cmd
}

// LastRun is what `status` shows about the most recent run.
type LastRun struct {
	Report *engine.Report `json:"report"`
	Error  string         `json:"error,omitempty"`
	Mode   string         `json:"mode"`
	When   time.Time      `json:"when"`
}

func statusFile(app *App) string { return filepath.Join(app.Env.StateDir(), "last-run.json") }

func hookInput() (sessionID string) {
	st, err := os.Stdin.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice != 0 {
		return ""
	}
	var in struct {
		SessionID string `json:"session_id"`
	}
	b, _ := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	_ = json.Unmarshal(b, &in)
	return in.SessionID
}

// NewEngine builds the engine for a config (exported for tests and init).
func (a *App) NewEngine(cfg *config.Config) (*engine.Engine, error) {
	st, err := a.newStore(cfg)
	if err != nil {
		return nil, err
	}
	srcs, projects := sources.Build(cfg, a.Env)
	if a.DesktopAppRunning != nil {
		for _, s := range srcs {
			if d, ok := s.(*sources.Desktop); ok {
				d.AppRunning = a.DesktopAppRunning
			}
		}
	}
	return &engine.Engine{
		Store:    st,
		Sources:  srcs,
		Machine:  cfg.Machine,
		Version:  a.Build.Version,
		StateDir: a.Env.StateDir(),
		Projects: projects,
	}, nil
}

func (a *App) newStore(cfg *config.Config) (store.Store, error) {
	switch cfg.Store.Kind {
	case config.StoreGit:
		return &store.Git{URL: cfg.Store.URL, Branch: cfg.Store.Branch, SSHKey: cfg.Store.SSHKey,
			Dir: a.Env.Join(a.Env.CacheDir(), "store.git"), Machine: cfg.Machine}, nil
	case config.StoreFolder:
		return &store.Folder{Root: cfg.Store.Path}, nil
	}
	return nil, fmt.Errorf("unknown store kind %q", cfg.Store.Kind)
}

func runSync(ctx context.Context, app *App, w io.Writer, f syncFlags) error {
	quiet := f.hook != "" || f.timer || f.background
	cfg, err := app.LoadConfig()
	if errors.Is(err, config.ErrNotInitialized) && quiet {
		return nil // hooks and timers stay silent until `karvan init`
	}
	if err != nil {
		return err
	}
	if f.hook == "session-end" {
		// Claude Code is closing: never make it wait. Sync in the background.
		return app.spawnBackground()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	wait := time.Minute
	switch {
	case f.timer, f.background:
		wait = 0
	case f.hook == "session-start":
		wait = 2 * time.Second
	}
	lk, err := lock.Acquire(ctx, app.Env.StateDir(), wait)
	if errors.Is(err, lock.ErrBusy) && quiet {
		return nil // another run is already doing the work
	}
	if err != nil {
		return err
	}
	defer func() { _ = lk.Release() }()
	fsutil.CleanTemp(app.Env.StateDir())

	eng, err := app.NewEngine(cfg)
	if err != nil {
		return err
	}
	budget := cfg.Sync.Timeout.Duration
	if f.hook == "session-start" {
		budget = sessionStartBudget
	}
	rctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	opts := engine.Options{Timer: f.timer}
	if f.hook == "session-start" {
		opts.SessionID = hookInput()
	}
	rep, err := eng.Run(rctx, opts)

	mode := "manual"
	switch {
	case f.hook != "":
		mode = "hook " + f.hook
	case f.timer:
		mode = "timer"
	case f.background:
		mode = "background"
	}
	last := LastRun{Report: rep, Mode: mode, When: time.Now()}
	if err != nil {
		last.Error = err.Error()
	}
	if b, e := json.MarshalIndent(last, "", " "); e == nil {
		_ = fsutil.WriteAtomic(statusFile(app), b, 0o600)
	}

	if f.hook == "session-start" && errors.Is(err, context.DeadlineExceeded) {
		_ = lk.Release()
		return app.spawnBackground() // finish without holding up Claude Code
	}
	if quiet {
		// Hook output is shown to Claude, so speak only when the user must act.
		if f.hook == "session-start" {
			if line := attentionLine(rep, err); line != "" {
				fmt.Fprintln(w, line)
			}
			if err == nil {
				for _, wt := range findWaiting(app, cfg, eng) {
					fmt.Fprintf(w, "Karvan: %s is waiting for %s. Run `karvan restore` in the project to bring it here.\n", wt.describe(time.Now()), wt.Project.ID)
				}
			}
		}
		if err != nil && f.hook == "" {
			return err
		}
		return nil
	}
	printReport(w, rep, err)
	for _, s := range eng.Sources {
		if wp, ok := s.(*sources.WIP); ok {
			for _, p := range wp.Problems {
				fmt.Fprintf(w, "  unfinished work not saved: %s\n", p)
			}
		}
	}
	if err == nil {
		for _, wt := range findWaiting(app, cfg, eng) {
			fmt.Fprintf(w, "  %s is waiting for %s (`karvan restore`)\n", wt.describe(time.Now()), wt.Project.ID)
		}
	}
	return err
}

// attentionLine is the single line a SessionStart hook prints, or "".
func attentionLine(rep *engine.Report, err error) string {
	switch {
	case errors.Is(err, store.ErrRewound), errors.Is(err, engine.ErrIdentity), errors.Is(err, engine.ErrFormat):
		return "Karvan: sync stopped: " + err.Error() + " (see `karvan status`)."
	case rep == nil:
		return ""
	case len(rep.Conflicts) > 0:
		return fmt.Sprintf("Karvan: %d file(s) were changed on two machines in the same place; both versions are kept (see `karvan status`).", len(rep.Conflicts))
	}
	for _, d := range rep.Deferred {
		if strings.Contains(d.Reason, "Ctrl+Q") {
			return "Karvan: new sessions arrived for the Claude desktop app. Quit it with Ctrl+Q and open it again to see them."
		}
	}
	return ""
}

func printReport(w io.Writer, rep *engine.Report, err error) {
	if rep != nil {
		var parts []string
		add := func(n int, what string) {
			if n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n, what))
			}
		}
		add(rep.Sent, "local change(s) sent")
		add(rep.Received, "received")
		add(rep.Merged, "merged")
		add(rep.Deleted, "deleted")
		if len(parts) == 0 {
			parts = append(parts, "everything is up to date")
		}
		fmt.Fprintf(w, "Synced in %s: %s.\n", rep.Finished.Sub(rep.Started).Round(100*time.Millisecond), strings.Join(parts, ", "))
		for _, c := range rep.Conflicts {
			fmt.Fprintf(w, "  conflict: %s changed on two machines; the other version is kept as %s\n", c.Path, c.Copy)
		}
		for _, d := range rep.Deferred {
			fmt.Fprintf(w, "  waiting:  %s (%s)\n", d.Path, d.Reason)
		}
		for _, wn := range rep.Warnings {
			fmt.Fprintf(w, "  warning:  %s\n", wn)
		}
		if rep.Unplaced > 0 {
			fmt.Fprintf(w, "  %d file(s) belong to projects not set up on this machine (see `karvan status`).\n", rep.Unplaced)
		}
	}
	if err != nil && errors.Is(err, store.ErrConflict) {
		fmt.Fprintln(w, "Could not send this time; will try again.")
	}
}

// spawnBackground starts `karvan sync --background` detached from this
// process, logging to the state folder.
func (a *App) spawnBackground() error {
	if a.NoSpawn {
		return nil
	}
	logDir := a.Env.Join(a.Env.StateDir(), "log")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(logDir, "background.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	args := []string{"sync", "--background"}
	if a.configPath != "" {
		args = append([]string{"--config", a.configPath}, args...)
	}
	cmd := exec.Command(a.Executable(), args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
