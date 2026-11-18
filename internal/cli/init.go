package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/leberman/karvan/internal/config"
	"github.com/leberman/karvan/internal/engine"
	"github.com/leberman/karvan/internal/fsutil"
	"github.com/leberman/karvan/internal/hooks"
	"github.com/leberman/karvan/internal/project"
	"github.com/leberman/karvan/internal/sshsetup"
	"github.com/leberman/karvan/internal/systemd"
)

type initFlags struct {
	machine     string
	git         string
	folder      string
	branch      string
	sshKey      string
	yes         bool
	noHooks     bool
	noTimer     bool
	noSync      bool
	allowPublic bool
	projects    []string
	// skipAccessCheck is for tests that use fake git URLs.
	skipAccessCheck bool
}

func newInitCmd(app *App) *cobra.Command {
	var f initFlags
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Set up Karvan on this machine: store, projects, hooks and timer",
		Long: `Set up Karvan on this machine. Run it again any time to change settings
or add projects.

It asks where to keep your data (a private git repository, or a folder that a
cloud service syncs), finds the folders you have used Claude Code in, installs
two Claude Code hooks and a timer, and syncs once.

Without a terminal, pass everything as flags and --yes, for example:
  karvan init --machine work --git ssh://git@ssh.github.com:443/me/karvan-data.git --yes`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(cmd.Context(), app, cmd.InOrStdin(), cmd.OutOrStdout(), f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.machine, "machine", "", "name of this machine (e.g. work, home)")
	fl.StringVar(&f.git, "git", "", "private git repository for the data (ssh:// URL or git@host:owner/repo)")
	fl.StringVar(&f.folder, "folder", "", "folder for the data (e.g. inside Dropbox or Syncthing)")
	fl.StringVar(&f.branch, "branch", "", "git branch (default main)")
	fl.StringVar(&f.sshKey, "ssh-key", "", "SSH private key file (default: ssh-agent, then ~/.ssh/id_ed25519)")
	fl.StringSliceVar(&f.projects, "project", nil, "add this folder as a project (repeatable)")
	fl.BoolVarP(&f.yes, "yes", "y", false, "accept the defaults without asking")
	fl.BoolVar(&f.noHooks, "no-hooks", false, "do not add Claude Code hooks")
	fl.BoolVar(&f.noTimer, "no-timer", false, "do not install the systemd timer")
	fl.BoolVar(&f.noSync, "no-sync", false, "do not sync at the end")
	fl.BoolVar(&f.allowPublic, "allow-public", false, "allow a public git repository (your conversations would be public!)")
	return cmd
}

type prompter struct {
	in  *bufio.Reader
	out io.Writer
	yes bool
}

// errCancelled is raised (as a panic, caught in runInit) when the user ends
// the input with Ctrl+D: stop, change nothing, so `karvan init` can be run
// again from the start.
var errCancelled = errors.New("canceled; nothing was changed. Run `karvan init` again to start over")

func (p *prompter) ask(question, def string) string {
	if p.yes {
		return def
	}
	if def != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", question, def)
	} else {
		fmt.Fprintf(p.out, "%s: ", question)
	}
	line, err := p.in.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		fmt.Fprintln(p.out)
		panic(errCancelled)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

func (p *prompter) confirm(question string, def bool) bool {
	d := "y/N"
	if def {
		d = "Y/n"
	}
	a := strings.ToLower(p.ask(question+" ("+d+")", ""))
	if a == "" {
		return def
	}
	return a == "y" || a == "yes"
}

func runInit(ctx context.Context, app *App, in io.Reader, w io.Writer, f initFlags) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if r != errCancelled { //nolint:errorlint // sentinel identity
				panic(r)
			}
			err = errCancelled
		}
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	p := &prompter{in: bufio.NewReader(in), out: w, yes: f.yes}
	cfg, err := app.LoadConfig()
	fresh := errors.Is(err, config.ErrNotInitialized)
	switch {
	case fresh:
		d := config.Default()
		cfg = &d
		fmt.Fprintln(w, "Setting up Karvan on this machine.")
	case err != nil:
		return fmt.Errorf("the existing config has problems; fix or delete it first: %w", err)
	default:
		fmt.Fprintf(w, "Updating Karvan's setup (%s).\n", app.ConfigPath())
	}

	// 1. Machine name.
	if f.machine != "" {
		cfg.Machine = cleanName(f.machine)
	}
	if cfg.Machine == "" {
		host, _ := os.Hostname()
		def := config.MachineNameFrom(host)
		for {
			cfg.Machine = cleanName(p.ask("Name for this machine (lowercase, e.g. work or home)", def))
			if config.ValidMachineName(cfg.Machine) {
				break
			}
			if p.yes {
				return usageError{errors.New("pass a valid --machine name (lowercase letters, digits and -)")}
			}
			fmt.Fprintln(w, "  Use lowercase letters, digits and -, at most 32.")
		}
	}

	// 2. Store.
	switch {
	case f.git != "":
		cfg.Store = config.Store{Kind: config.StoreGit, URL: f.git, Branch: "main"}
	case f.folder != "":
		abs, err := filepath.Abs(expandHome(f.folder, app.Env.Home))
		if err != nil {
			return err
		}
		cfg.Store = config.Store{Kind: config.StoreFolder, Path: abs, Branch: "main"}
	case cfg.Store.Kind == "":
		if p.yes {
			return usageError{errors.New("pass --git <private repository> or --folder <path>")}
		}
		fmt.Fprintln(w, "\nWhere should Karvan keep your data? Every machine uses the same place.")
		fmt.Fprintln(w, "  1) a private git repository (e.g. on GitHub), over SSH")
		fmt.Fprintln(w, "  2) a folder that Dropbox, Google Drive, OneDrive or Syncthing keeps in sync")
		if p.ask("Choose 1 or 2", "1") == "2" {
			fmt.Fprintln(w, "  The folder must be inside a folder your cloud service syncs; every machine then")
			fmt.Fprintln(w, "  sees the same files. Karvan itself does not copy anything between machines here.")
			def := ""
			for _, c := range cloudFolders(app.Env.Home) {
				fmt.Fprintf(w, "  found: %s\n", tildePath(c, app.Env.Home))
				if def == "" {
					def = tildePath(filepath.Join(c, "karvan"), app.Env.Home)
				}
			}
			var abs string
			for {
				path := strings.TrimSpace(p.ask("Folder (full path, e.g. ~/Dropbox/karvan)", def))
				if strings.HasPrefix(path, "~") || filepath.IsAbs(path) {
					abs = filepath.Clean(expandHome(path, app.Env.Home))
					break
				}
				if p.yes {
					return usageError{errors.New("the folder must be a full path")}
				}
				fmt.Fprintln(w, "  Please give a full path (starting with / or ~/).")
			}
			if !insideCloud(abs, app.Env.Home) {
				fmt.Fprintf(w, "  Note: %s is not inside a Dropbox, Google Drive, OneDrive or Syncthing folder Karvan knows.\n", tildePath(abs, app.Env.Home))
				fmt.Fprintln(w, "  Your other machines will only see it if something syncs this folder.")
			}
			cfg.Store = config.Store{Kind: config.StoreFolder, Path: abs, Branch: "main"}
		} else {
			fmt.Fprintln(w, "  Create an empty PRIVATE repository first (for example karvan-data).")
			fmt.Fprintln(w, "  If SSH on port 22 is blocked on your network, GitHub also listens on 443:")
			fmt.Fprintln(w, "  ssh://git@ssh.github.com:443/<you>/karvan-data.git")
			url := p.ask("Repository URL", "")
			if url == "" {
				return usageError{errors.New("a repository URL is required")}
			}
			cfg.Store = config.Store{Kind: config.StoreGit, URL: url, Branch: "main"}
		}
	}
	if f.branch != "" {
		cfg.Store.Branch = f.branch
	}
	if f.sshKey != "" {
		cfg.Store.SSHKey = expandHome(f.sshKey, app.Env.Home)
	}
	if cfg.Store.Kind == config.StoreFolder {
		if err := os.MkdirAll(cfg.Store.Path, 0o700); err != nil {
			return err
		}
	}
	if cfg.Store.Kind == config.StoreGit {
		// Karvan talks to git over SSH; a GitHub https:// or git@ address
		// becomes SSH on port 443, which also works where port 22 is blocked.
		if u, ok := sshsetup.GitHubSSH(cfg.Store.URL); ok && u != cfg.Store.URL {
			fmt.Fprintf(w, "  Using %s (SSH on port 443).\n", u)
			cfg.Store.URL = u
		}
	}
	if cfg.Store.Kind == config.StoreGit && !f.allowPublic {
		if public, known := app.isPublicRepo(cfg.Store.URL); known && public {
			return errors.New("that repository is PUBLIC: your conversations would be visible to everyone. Make it private, or pass --allow-public if you really mean it")
		}
	}
	if cfg.Store.Kind == config.StoreGit && !f.skipAccessCheck {
		if err := prepareGitAccess(ctx, app, p, w, cfg); err != nil {
			return err
		}
	}

	// 3. Projects.
	have := map[string]bool{}
	for _, pr := range cfg.Projects {
		have[pr.Path] = true
	}
	add := func(path string, wip bool) {
		id, _ := project.IDFor(path)
		wip = wip && project.IsRepo(path)
		for _, pr := range cfg.Projects {
			if pr.ID == id {
				fmt.Fprintf(w, "  %s is already set up (at %s)\n", id, pr.Path)
				return
			}
		}
		cfg.Projects = append(cfg.Projects, config.Project{ID: id, Name: filepath.Base(path), Path: path, WIP: wip})
		have[path] = true
		fmt.Fprintf(w, "  + %s  (%s)\n", id, tildePath(path, app.Env.Home))
	}
	for _, pth := range f.projects {
		abs, err := filepath.Abs(expandHome(pth, app.Env.Home))
		if err != nil {
			return err
		}
		if st, err := os.Stat(abs); err != nil || !st.IsDir() {
			return fmt.Errorf("project folder %s does not exist", abs)
		}
		add(abs, false)
	}
	// The desktop app's scratch workspaces are not projects.
	found, _ := project.Discover(app.Env.ClaudeProjectsDir(), app.Env.DesktopDataDir())
	var news []project.Found
	for _, fd := range found {
		if !have[fd.Path] {
			news = append(news, fd)
		}
	}
	if len(news) > 0 && len(f.projects) == 0 {
		fmt.Fprintf(w, "\nFound %d folder(s) where you used Claude Code:\n", len(news))
		for _, fd := range news {
			fmt.Fprintf(w, "  %-40s %3d conversation(s), last %s\n", tildePath(fd.Path, app.Env.Home), fd.Sessions, ago(fd.LastUsed, time.Now()))
		}
		if p.confirm("Sync all of them?", true) {
			for _, fd := range news {
				add(fd.Path, false)
			}
		} else if !p.yes {
			for _, fd := range news {
				if p.confirm("  "+tildePath(fd.Path, app.Env.Home)+"?", false) {
					add(fd.Path, false)
				}
			}
		}
	}
	repos := 0
	for _, pr := range cfg.Projects {
		if project.IsRepo(pr.Path) {
			repos++
		}
	}
	if fresh && repos > 0 && !p.yes {
		if p.confirm("Also carry unfinished git work (uncommitted changes, unpushed commits) between machines?", false) {
			for i := range cfg.Projects {
				cfg.Projects[i].WIP = project.IsRepo(cfg.Projects[i].Path)
			}
		}
	}

	if err := config.Save(app.ConfigPath(), cfg); err != nil {
		return err
	}
	fmt.Fprintf(w, "\nSaved %s\n", app.ConfigPath())

	// 4. CLAUDE.machine.md (PRD 4-1).
	if err := setupMachineFile(app); err != nil {
		fmt.Fprintf(w, "  note: could not set up CLAUDE.machine.md: %v\n", err)
	}

	// 5. Hooks.
	if !f.noHooks {
		changed, backup, err := hooks.Install(app.Env.ClaudeSettings(), app.Executable())
		switch {
		case errors.Is(err, hooks.ErrInvalidJSON):
			return fmt.Errorf("%s is not valid JSON, so Karvan did not touch it. Fix it and run `karvan init` again: %w", app.Env.ClaudeSettings(), err)
		case err != nil:
			return err
		case changed:
			fmt.Fprintln(w, "Added Karvan's SessionStart and SessionEnd hooks to Claude Code.")
			if backup != "" {
				fmt.Fprintf(w, "  (previous settings kept as %s)\n", backup)
			}
		}
	}

	// 6. Store and first sync.
	eng, err := app.NewEngine(cfg)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Connecting to %s...\n", eng.Store.Describe())
	ictx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	created, err := engine.InitStore(ictx, eng.Store, app.Build.Version, time.Now())
	cancel()
	if err != nil {
		return fmt.Errorf("could not reach the store: %w", err)
	}
	if created {
		fmt.Fprintln(w, "Prepared the new, empty store.")
	}
	if !f.noSync {
		if err := runSync(ctx, app, w, syncFlags{}); err != nil {
			return err
		}
	}

	// 7. Timer.
	if !f.noTimer && runtime.GOOS == "linux" {
		units := systemd.Render(app.Executable(), cfg.Sync.Interval.Duration, app.configPath)
		if err := systemd.Install(systemd.Dir(app.configHome()), units, app.systemctl()); err != nil {
			fmt.Fprintf(w, "Could not install the timer (%v). Karvan still syncs from the hooks; run `karvan doctor` for details.\n", err)
		} else {
			fmt.Fprintf(w, "Installed a timer: syncs every %s and once more at logout and shutdown.\n", strings.TrimSuffix(cfg.Sync.Interval.String(), "0s"))
		}
	}
	printSetupSummary(w, app, cfg, !f.noHooks, !f.noTimer && runtime.GOOS == "linux")
	return nil
}

// printSetupSummary is the last thing `karvan init` prints: what is set up,
// what now happens by itself, the everyday commands, and the next step.
func printSetupSummary(w io.Writer, app *App, cfg *config.Config, hooksOn, timerOn bool) {
	store := cfg.Store.URL
	if cfg.Store.Kind == config.StoreFolder {
		store = tildePath(cfg.Store.Path, app.Env.Home)
	}
	var names []string
	for _, pr := range cfg.Projects {
		n := pr.Name
		if n == "" {
			n = pr.ID
		}
		names = append(names, n)
	}
	projects := fmt.Sprintf("%d", len(names))
	if len(names) > 0 {
		projects += " (" + strings.Join(names, ", ") + ")"
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Karvan is set up on this computer (%s). It is ready to use.\n\n", cfg.Machine)
	fmt.Fprintf(w, "  Store:     %s\n", store)
	fmt.Fprintf(w, "  Projects:  %s\n", projects)
	var when []string
	if hooksOn {
		when = append(when, "when a Claude Code session starts and ends")
	}
	if timerOn {
		when = append(when, "every "+strings.TrimSuffix(cfg.Sync.Interval.String(), "0s"), "at logout and shutdown")
	}
	if len(when) > 0 {
		fmt.Fprintf(w, "  Syncs by itself: %s.\n", strings.Join(when, ", "))
	} else {
		fmt.Fprintln(w, "  Syncs only when you run `karvan sync` (no hooks or timer installed).")
	}
	fmt.Fprintln(w, "\nCommands you can use now:")
	fmt.Fprintln(w, "  karvan status    what was synced, your other computers, anything waiting")
	fmt.Fprintln(w, "  karvan doctor    check that everything works")
	fmt.Fprintln(w, "  karvan sync      sync right now")
	fmt.Fprintln(w, "  karvan restore   bring unfinished git work from another computer")
	fmt.Fprintln(w, "  karvan update    update Karvan")
	fmt.Fprintln(w, "  karvan init      change settings or add projects (any time)")
	fmt.Fprintln(w, "\nNext: on your other computer, run the same install line, then `karvan init`")
	fmt.Fprintf(w, "with a different machine name and this store:\n  %s\n", store)
}

// prepareGitAccess makes sure this computer can reach the repository before
// anything is saved: GitHub's host key, an SSH key (created if there is
// none), and step-by-step help until GitHub lets the key in.
func prepareGitAccess(ctx context.Context, app *App, p *prompter, w io.Writer, cfg *config.Config) error {
	if !strings.HasPrefix(cfg.Store.URL, "ssh://") && !strings.Contains(cfg.Store.URL, "@") {
		return nil // file:// and other local repositories need no SSH
	}
	github := strings.Contains(cfg.Store.URL, "github.com")
	if github {
		if added, err := sshsetup.EnsureGitHubHostKey(app.Env.Home); err != nil {
			fmt.Fprintf(w, "  note: could not record GitHub's host key: %v\n", err)
		} else if added {
			fmt.Fprintf(w, "  Added GitHub's SSH host key to ~/.ssh/known_hosts (%s).\n", sshsetup.GitHubHostFingerprint)
		}
	}
	keyFile := cfg.Store.SSHKey
	if keyFile == "" {
		keyFile = sshsetup.FindKey(app.Env.Home)
	}
	if keyFile == "" && os.Getenv("SSH_AUTH_SOCK") == "" {
		fmt.Fprintln(w, "  This computer has no SSH key yet; GitHub needs one to let Karvan in.")
		if !p.confirm("  Create one now (~/.ssh/id_ed25519)?", true) {
			return errors.New("an SSH key is needed: create one with ssh-keygen -t ed25519, add it to GitHub, and run `karvan init` again")
		}
		path, _, err := sshsetup.CreateKey(app.Env.Home, "karvan@"+cfg.Machine)
		if err != nil {
			return err
		}
		keyFile = path
		fmt.Fprintf(w, "  Created %s.\n", path)
	}
	repo := sshsetup.RepoPath(cfg.Store.URL)
	for attempt := 1; ; attempt++ {
		st, err := app.newStore(cfg)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "Checking access to %s...\n", st.Describe())
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		_, err = st.Snapshot(cctx)
		cancel()
		if err == nil {
			fmt.Fprintln(w, "  OK: this computer can read and write it.")
			return nil
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, sshsetup.Explain(err, repo, keyFile))
		if p.yes {
			return fmt.Errorf("could not reach the store: %w", err)
		}
		fmt.Fprintln(w)
		if a := strings.ToLower(p.ask("When that is done, press Enter to try again (or q to stop)", "")); a == "q" {
			return errors.New("stopped; nothing was changed. Run `karvan init` again when the repository is ready")
		}
	}
}

// cleanName lowercases a typed name and drops invisible characters (e.g.
// direction marks a Persian keyboard can insert).
func cleanName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if r > ' ' && r < 0x7f {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// cloudFolders lists the usual cloud-synced folders that exist in home.
func cloudFolders(home string) []string {
	var out []string
	for _, n := range []string{"Dropbox", "Google Drive", "GoogleDrive", "OneDrive", "Sync", "Syncthing", "Nextcloud", "pCloudDrive", "MEGA"} {
		p := filepath.Join(home, n)
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			out = append(out, p)
		}
	}
	return out
}

func insideCloud(p, home string) bool {
	for _, c := range cloudFolders(home) {
		if p == c || strings.HasPrefix(p, c+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func expandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

func tildePath(p, home string) string {
	if home != "" && (p == home || strings.HasPrefix(p, home+string(filepath.Separator))) {
		return "~" + p[len(home):]
	}
	return p
}

const machineHeader = `<!-- Instructions for this machine only. Karvan never copies this file to your
other computers. Claude Code reads it through the import line in CLAUDE.md. -->
`

const importLine = "@~/.claude/CLAUDE.machine.md"

// setupMachineFile creates CLAUDE.machine.md and imports it from CLAUDE.md.
func setupMachineFile(app *App) error {
	home := app.Env.ClaudeHome()
	mf := filepath.Join(home, "CLAUDE.machine.md")
	if _, err := os.Stat(mf); os.IsNotExist(err) {
		if err := fsutil.WriteAtomic(mf, []byte(machineHeader), 0o600); err != nil {
			return err
		}
	}
	cf := filepath.Join(home, "CLAUDE.md")
	b, err := os.ReadFile(cf)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if strings.Contains(string(b), "CLAUDE.machine.md") {
		return nil
	}
	out := string(b)
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	if out != "" {
		out += "\n"
	}
	out += importLine + "\n"
	return fsutil.WriteAtomic(cf, []byte(out), 0o644)
}

// isPublicRepo asks GitHub whether a repository is public. known is false
// for other hosts or when the answer is unclear (no network...).
func (a *App) isPublicRepo(url string) (public, known bool) {
	if a.PublicRepoCheck != nil {
		return a.PublicRepoCheck(url)
	}
	id, ok := project.NormalizeRemote(url)
	if !ok || !strings.HasPrefix(id, "github.com/") {
		return false, false
	}
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get("https://api.github.com/repos/" + strings.TrimPrefix(id, "github.com/"))
	if err != nil {
		return false, false
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, true // visible without logging in: public
	case http.StatusNotFound:
		return false, true // private (or missing)
	}
	return false, false
}

func (a *App) configHome() string {
	if v := a.Env.Getenv; v != nil {
		if x := v("XDG_CONFIG_HOME"); x != "" && filepath.IsAbs(x) {
			return x
		}
	}
	return filepath.Join(a.Env.Home, ".config")
}

func (a *App) systemctl() systemd.Runner {
	if a.Systemctl != nil {
		return a.Systemctl
	}
	return systemd.Systemctl
}

func newUninstallCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove Karvan's hooks and timer from this machine (your data and the store are kept)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w := cmd.OutOrStdout()
			changed, backup, err := hooks.Remove(app.Env.ClaudeSettings())
			if err != nil {
				return err
			}
			if changed {
				fmt.Fprintf(w, "Removed Karvan's hooks from %s.\n", app.Env.ClaudeSettings())
				if backup != "" {
					fmt.Fprintf(w, "The previous file is kept as %s.\n", backup)
				}
			} else {
				fmt.Fprintln(w, "No Karvan hooks to remove.")
			}
			removed, err := systemd.Remove(systemd.Dir(app.configHome()), app.systemctl())
			if err != nil {
				return err
			}
			if removed {
				fmt.Fprintln(w, "Removed the timer.")
			}
			fmt.Fprintf(w, "Your config (%s), local state and the store are kept.\n", app.ConfigPath())
			return nil
		},
	}
}
