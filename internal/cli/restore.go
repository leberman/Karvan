package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/leberman/karvan/internal/config"
	"github.com/leberman/karvan/internal/engine"
	"github.com/leberman/karvan/internal/lock"
	"github.com/leberman/karvan/internal/paths"
	"github.com/leberman/karvan/internal/project"
	"github.com/leberman/karvan/internal/wip"
)

// waiting is a snapshot of unfinished work from another machine.
type waiting struct {
	Machine string
	Project config.Project
	PID     string
	Meta    wip.Meta
	files   map[string]string // file name -> logical path
}

func restoredFile(app *App) string { return filepath.Join(app.Env.StateDir(), "restored.json") }

func loadRestored(app *App) map[string]time.Time {
	m := map[string]time.Time{}
	if b, err := os.ReadFile(restoredFile(app)); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// findWaiting lists other machines' snapshots for projects set up here,
// newest first, leaving out ones already restored.
func findWaiting(app *App, cfg *config.Config, eng *engine.Engine) []waiting {
	if eng.LastSnapshot == nil {
		return nil
	}
	restored := loadRestored(app)
	var out []waiting
	for name, m := range eng.LastOthers {
		byPID := map[string]map[string]string{}
		for p, en := range m.Entries {
			rest, ok := strings.CutPrefix(p, "machine:"+name+"/wip/")
			if !ok || !en.Active() || en.Deleted {
				continue
			}
			pid, file, ok := strings.Cut(rest, "/")
			if !ok {
				continue
			}
			if byPID[pid] == nil {
				byPID[pid] = map[string]string{}
			}
			byPID[pid][file] = p
		}
		for _, proj := range cfg.Projects {
			pid := project.PID(proj.ID)
			files := byPID[pid]
			if files[wip.FileMeta] == "" {
				continue
			}
			b, err := engine.Content(eng.LastSnapshot, m.Entries[files[wip.FileMeta]])
			if err != nil {
				continue
			}
			meta, err := wip.ParseMeta(b)
			if err != nil {
				continue
			}
			if t, ok := restored[name+"/"+pid]; ok && !meta.Taken.After(t) {
				continue
			}
			out = append(out, waiting{Machine: name, Project: proj, PID: pid, Meta: meta, files: files})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Taken.After(out[j].Meta.Taken) })
	return out
}

func (w waiting) describe(now time.Time) string {
	branch := w.Meta.Branch
	if branch == "" {
		branch = "detached " + w.Meta.Head[:8]
	}
	var parts []string
	if w.Meta.Unpushed > 0 {
		parts = append(parts, fmt.Sprintf("%d commit(s) not pushed", w.Meta.Unpushed))
	}
	if w.Meta.Staged {
		parts = append(parts, "staged changes")
	}
	if w.Meta.Unstaged {
		parts = append(parts, "unstaged changes")
	}
	if n := len(w.Meta.Untracked); n > 0 {
		parts = append(parts, fmt.Sprintf("%d new file(s)", n))
	}
	return fmt.Sprintf("unfinished work from %s (%s, branch %s): %s", w.Machine, ago(w.Meta.Taken, now), branch, strings.Join(parts, ", "))
}

func newRestoreCmd(app *App) *cobra.Command {
	var yes bool
	var projectFlag string
	cmd := &cobra.Command{
		Use:   "restore [machine]",
		Short: "Bring back unfinished project work saved on another machine",
		Long: `Bring unfinished work (branch, commits not pushed yet, staged and unstaged
changes, new files) from another machine into the project you are in.

Karvan never commits, pushes, stashes or resets in your project. It refuses
to run if the project has uncommitted changes here.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			from := ""
			if len(args) == 1 {
				from = args[0]
			}
			return runRestore(cmd.Context(), app, cmd.InOrStdin(), cmd.OutOrStdout(), from, projectFlag, yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	cmd.Flags().StringVar(&projectFlag, "project", "", "project ID (default: the project you are in)")
	return cmd
}

func runRestore(ctx context.Context, app *App, in io.Reader, w io.Writer, from, projectFlag string, yes bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg, err := app.LoadConfig()
	if err != nil {
		return err
	}
	lk, err := lock.Acquire(ctx, app.Env.StateDir(), time.Minute)
	if err != nil {
		return err
	}
	defer func() { _ = lk.Release() }()
	eng, err := app.NewEngine(cfg)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "Syncing first...")
	rctx, cancel := context.WithTimeout(ctx, cfg.Sync.Timeout.Duration)
	_, err = eng.Run(rctx, engine.Options{})
	cancel()
	if err != nil {
		return err
	}
	all := findWaiting(app, cfg, eng)

	// Which project?
	var proj *config.Project
	if projectFlag != "" {
		for i := range cfg.Projects {
			if cfg.Projects[i].ID == projectFlag || cfg.Projects[i].Name == projectFlag {
				proj = &cfg.Projects[i]
			}
		}
		if proj == nil {
			return fmt.Errorf("no project %q in the config", projectFlag)
		}
	} else if wd, err := os.Getwd(); err == nil {
		for i := range cfg.Projects {
			if paths.HasPathPrefix(app.Env.GOOS, wd, cfg.Projects[i].Path) {
				proj = &cfg.Projects[i]
			}
		}
	}
	var cands []waiting
	for _, wt := range all {
		if (proj == nil || wt.Project.ID == proj.ID) && (from == "" || wt.Machine == from) {
			cands = append(cands, wt)
		}
	}
	if len(cands) == 0 {
		fmt.Fprintln(w, "No unfinished work is waiting from other machines.")
		return nil
	}
	if proj == nil {
		ids := map[string]bool{}
		for _, c := range cands {
			ids[c.Project.ID] = true
		}
		if len(ids) > 1 {
			fmt.Fprintln(w, "Unfinished work is waiting in several projects:")
			for _, c := range cands {
				fmt.Fprintf(w, "  %s: %s\n", c.Project.ID, c.describe(time.Now()))
			}
			return usageError{errors.New("run `karvan restore` inside the project folder, or pass --project")}
		}
	}
	pick := cands[0]
	fmt.Fprintf(w, "Found %s.\nProject: %s (%s)\n", pick.describe(time.Now()), pick.Project.ID, pick.Project.Path)
	if !yes {
		fmt.Fprint(w, "Bring it here? [y/N] ")
		line, _ := bufio.NewReader(in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintln(w, "Nothing changed.")
			return nil
		}
	}
	snap := &wip.Snapshot{Meta: pick.Meta, Files: map[string][]byte{}}
	other := eng.LastOthers[pick.Machine]
	for file, logical := range pick.files {
		b, err := engine.Content(eng.LastSnapshot, other.Entries[logical])
		if err != nil {
			return fmt.Errorf("reading the saved work: %w", err)
		}
		snap.Files[file] = b
	}
	if err := wip.Restore(ctx, pick.Project.Path, snap, func(s string) { fmt.Fprintln(w, s) }); err != nil {
		return err
	}
	restored := loadRestored(app)
	restored[pick.Machine+"/"+pick.PID] = pick.Meta.Taken
	if b, err := json.Marshal(restored); err == nil {
		_ = os.WriteFile(restoredFile(app), b, 0o600)
	}
	fmt.Fprintf(w, "Done: you are on %s with the work from %s.\n", orDetached(pick.Meta.Branch), pick.Machine)
	return nil
}

func orDetached(b string) string {
	if b == "" {
		return "a detached HEAD"
	}
	return "branch " + b
}
