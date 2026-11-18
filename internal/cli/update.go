package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/leberman/karvan/internal/update"
)

// latestRelease returns the newest release tag ("" if unknown).
func (a *App) latestRelease(force bool) string {
	if a.UpdateCheck != nil {
		tag, _ := a.UpdateCheck(force)
		return tag
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tag, _ := (&update.Checker{StateDir: a.Env.StateDir()}).Latest(ctx, force)
	return tag
}

// updateNotice is one line about a newer release, or "".
func (a *App) updateNotice() string {
	tag := a.latestRelease(false)
	if update.Newer(tag, a.Build.short()) {
		return fmt.Sprintf("Karvan %s is available (you have %s): run `karvan update`", tag, a.Build.short())
	}
	return ""
}

func newUpdateCmd(app *App) *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update Karvan to the latest release",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpdate(cmd.Context(), app, cmd.OutOrStdout(), check)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "only say whether an update is available")
	return cmd
}

func runUpdate(ctx context.Context, app *App, w io.Writer, check bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	cur := app.Build.short()
	tag := app.latestRelease(true)
	switch {
	case tag == "":
		return fmt.Errorf("could not find the latest release (no network?); try again later")
	case !update.Newer(tag, cur) && !isDevBuild(cur):
		fmt.Fprintf(w, "Karvan %s is the latest version.\n", cur)
		return nil
	case check:
		fmt.Fprintf(w, "Karvan %s is available (you have %s). Run `karvan update`.\n", tag, cur)
		return nil
	}
	fmt.Fprintf(w, "Updating Karvan %s -> %s...\n", cur, tag)
	if app.Installer != nil {
		return app.Installer(ctx, tag, app.Executable(), w)
	}
	if err := update.Install(ctx, tag, app.Executable(), w); err != nil {
		return err
	}
	fmt.Fprintln(w, "Your settings, hooks and timer stay as they are; the new version is used from the next run.")
	return nil
}

// isDevBuild reports a build that is not a plain release (source, snapshot,
// dev): `karvan update` then installs the latest release.
func isDevBuild(v string) bool { return !update.Newer("v999.0.0", v) }
