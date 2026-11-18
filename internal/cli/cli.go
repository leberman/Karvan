// Package cli is Karvan's command line. Each command is a thin layer over the
// packages that do the work; in phase 1 most of them are stubs.
//
// Exit codes: 0 success, 1 error, 2 usage error, 3 not implemented yet.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"

	"github.com/leberman/karvan/internal/config"
	"github.com/leberman/karvan/internal/paths"
)

// Exit codes.
const (
	ExitOK             = 0
	ExitError          = 1
	ExitUsage          = 2
	ExitNotImplemented = 3
)

// ErrNotImplemented is returned by commands that later phases fill in.
var ErrNotImplemented = errors.New("not implemented yet")

// Build describes this binary; goreleaser sets the fields with -ldflags.
type Build struct {
	Version string
	Commit  string
	Date    string
}

// App holds everything the commands need, so tests can replace it.
type App struct {
	Env    paths.Env
	Stdout io.Writer
	Stderr io.Writer
	Build  Build

	// Exe is the absolute path hooks and timers should run; empty means
	// os.Executable().
	Exe string
	// NoSpawn stops background runs from being started (tests).
	NoSpawn bool
	// DesktopAppRunning replaces the process check (tests).
	DesktopAppRunning func() (bool, error)
	// PublicRepoCheck replaces the GitHub visibility check (tests).
	PublicRepoCheck func(url string) (public, known bool)
	// Systemctl replaces `systemctl --user` (tests).
	Systemctl func(args ...string) ([]byte, error)
	// UpdateCheck replaces the release lookup (tests).
	UpdateCheck func(force bool) (string, error)
	// Installer replaces running install.sh (tests).
	Installer func(ctx context.Context, tag, exe string, w io.Writer) error
	// DialCheck tests a TCP connection to host:port (doctor's network
	// check); nil skips it.
	DialCheck func(hostPort string) error

	configPath string // --config; empty = Env.ConfigFile()
}

// Executable is the karvan binary that hooks and timers run.
func (a *App) Executable() string {
	if a.Exe != "" {
		return a.Exe
	}
	exe, err := os.Executable()
	if err != nil {
		return "karvan"
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return exe
}

// ConfigPath is the config file this run uses.
func (a *App) ConfigPath() string {
	if a.configPath != "" {
		return a.configPath
	}
	return a.Env.ConfigFile()
}

// LoadConfig reads the config file (config.ErrNotInitialized if missing).
func (a *App) LoadConfig() (*config.Config, error) {
	return config.Load(a.ConfigPath(), a.Env.Home)
}

type usageError struct{ error }

// Run executes the command line args (without the program name) and returns
// the process exit code.
func Run(app *App, args []string) int {
	root := newRoot(app)
	root.SetArgs(args)
	err := root.Execute()
	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, ErrNotImplemented):
		fmt.Fprintf(app.Stderr, "karvan: %v\n", err)
		return ExitNotImplemented
	case errors.As(err, new(usageError)):
		fmt.Fprintf(app.Stderr, "karvan: %v\nRun 'karvan --help' for usage.\n", err)
		return ExitUsage
	default:
		fmt.Fprintf(app.Stderr, "karvan: %v\n", err)
		return ExitError
	}
}

func newRoot(app *App) *cobra.Command {
	root := &cobra.Command{
		Use:   "karvan",
		Short: "Continue your Claude Code work on any of your computers",
		Long: `Karvan syncs Claude Code chat history, the desktop app's session list,
CLAUDE.md, skills, memory and (optionally) unfinished git work between your
computers, through a private git repository or a synced folder.

Unofficial tool, not made or endorsed by Anthropic.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       app.Build.String(),
	}
	root.SetOut(app.Stdout)
	root.SetErr(app.Stderr)
	root.SetVersionTemplate("{{.Version}}\n")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	root.PersistentFlags().StringVar(&app.configPath, "config", "", "config file (default "+app.Env.ConfigFile()+")")

	root.AddCommand(
		newInitCmd(app),
		newSyncCmd(app),
		newStatusCmd(app),
		newRestoreCmd(app),
		newDoctorCmd(app),
		newUninstallCmd(app),
		newUpdateCmd(app),
		newVersionCmd(app),
	)
	// Unknown subcommands and wrong argument counts are usage errors.
	for _, c := range root.Commands() {
		if c.Args != nil {
			check := c.Args
			c.Args = func(cmd *cobra.Command, args []string) error {
				if err := check(cmd, args); err != nil {
					return usageError{err}
				}
				return nil
			}
		}
	}
	root.RunE = func(cmd *cobra.Command, _ []string) error { return cmd.Help() }
	root.Args = func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			return usageError{fmt.Errorf("unknown command %q", args[0])}
		}
		return nil
	}
	return root
}

func newVersionCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), app.Build.String())
			return nil
		},
	}
}

// short is the bare version recorded in manifests ("dev" for local builds).
func (b Build) short() string {
	if b.Version != "" {
		return b.Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// String is the one-line version, e.g.
// "karvan 0.1.0 (commit 1a2b3c4, 2026-11-20) linux/amd64 go1.26.0".
func (b Build) String() string {
	v, commit, date := b.Version, b.Commit, b.Date
	if v == "" {
		v = "dev"
		// `go install` builds carry module and VCS data instead of ldflags.
		if bi, ok := debug.ReadBuildInfo(); ok {
			if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
				v = bi.Main.Version
			}
			for _, s := range bi.Settings {
				switch s.Key {
				case "vcs.revision":
					if commit == "" {
						commit = s.Value
					}
				case "vcs.time":
					if date == "" {
						date = s.Value
					}
				}
			}
		}
	}
	if len(commit) > 7 {
		commit = commit[:7]
	}
	out := "karvan " + v
	switch {
	case commit != "" && date != "":
		out += fmt.Sprintf(" (commit %s, %s)", commit, date)
	case commit != "":
		out += fmt.Sprintf(" (commit %s)", commit)
	}
	return out + fmt.Sprintf(" %s/%s %s", runtime.GOOS, runtime.GOARCH, runtime.Version())
}
