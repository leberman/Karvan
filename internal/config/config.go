// Package config reads, validates and writes Karvan's per-machine
// configuration file (config.toml). The file belongs to one machine and is
// never synced (PRD 4-1).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// FormatVersion is the version of the config file layout this build writes.
const FormatVersion = 1

// ErrNotInitialized means there is no config file yet: run `karvan init`.
var ErrNotInitialized = errors.New("karvan is not set up on this machine yet: run `karvan init`")

// Store kinds (PRD 7).
const (
	StoreGit    = "git"
	StoreFolder = "folder"
)

// Config is the whole file.
type Config struct {
	Version  int       `toml:"version"`
	Machine  string    `toml:"machine"`
	Store    Store     `toml:"store"`
	Sync     Sync      `toml:"sync"`
	Items    Items     `toml:"items"`
	Projects []Project `toml:"projects"`
	Exclude  Exclude   `toml:"exclude"`
}

// Store says where the shared data lives.
type Store struct {
	Kind   string `toml:"kind"`              // "git" or "folder"
	URL    string `toml:"url,omitempty"`     // git: remote URL (ssh:// or https://)
	Branch string `toml:"branch,omitempty"`  // git: default "main"
	SSHKey string `toml:"ssh_key,omitempty"` // git: private key file; empty = ssh-agent, then ~/.ssh/id_ed25519
	Path   string `toml:"path,omitempty"`    // folder: absolute path
}

// Sync holds timings and limits (PRD 6-3, 6-4; docs/sync-engine.md).
type Sync struct {
	HistoryDays int      `toml:"history_days"` // send transcripts changed in the last N days
	Interval    Duration `toml:"interval"`     // timer period
	SkipRecent  Duration `toml:"skip_recent"`  // timer runs skip transcripts written this recently
	Timeout     Duration `toml:"timeout"`      // limit for one whole run
}

// Items switches each kind of data on or off (PRD 4).
type Items struct {
	ClaudeMD    bool     `toml:"claude_md"`
	Skills      bool     `toml:"skills"`
	Memory      bool     `toml:"memory"`
	Transcripts bool     `toml:"transcripts"`
	Desktop     bool     `toml:"desktop"`
	Folders     []Folder `toml:"folders"`
}

// Folder is a personal folder the user chose to sync.
type Folder struct {
	Name string `toml:"name"` // logical name, same on every machine
	Path string `toml:"path"` // this machine's path
}

// Project maps a project's identity to its path on this machine (PRD 5).
type Project struct {
	ID   string `toml:"id"`             // normalized git remote, or "name:<name>"
	Name string `toml:"name,omitempty"` // shown to the user
	Path string `toml:"path"`           // absolute path on this machine
	WIP  bool   `toml:"wip"`            // save unfinished git work (PRD 6-5); off by default
}

// Exclude lists things this machine does not want (PRD 4-1).
type Exclude struct {
	Skills   []string `toml:"skills"`
	Projects []string `toml:"projects"` // project IDs
}

// Duration is a time.Duration written as "10m" in the file.
type Duration struct{ time.Duration }

// MarshalText writes the shortest form ("10m", "1h30m", "15s").
func (d Duration) MarshalText() ([]byte, error) {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return []byte(s), nil
}

// UnmarshalText parses Go duration syntax.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("invalid duration %q (use e.g. \"10m\" or \"1h\")", b)
	}
	d.Duration = v
	return nil
}

// Default returns the settings a fresh install starts with.
func Default() Config {
	return Config{
		Version: FormatVersion,
		Store:   Store{Branch: "main"},
		Sync: Sync{
			HistoryDays: 7,
			Interval:    Duration{10 * time.Minute},
			SkipRecent:  Duration{15 * time.Minute},
			Timeout:     Duration{5 * time.Minute},
		},
		Items: Items{ClaudeMD: true, Skills: true, Memory: true, Transcripts: true, Desktop: true},
	}
}

var (
	machineRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	nameRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// ValidMachineName reports whether s can be a machine name: lowercase ASCII
// letters, digits and "-", at most 32 characters, not starting with "-".
func ValidMachineName(s string) bool { return machineRE.MatchString(s) }

// MachineNameFrom turns a hostname into a valid machine name, or "" if
// nothing usable is left.
func MachineNameFrom(host string) string {
	host, _, _ = strings.Cut(host, ".")
	var b strings.Builder
	for _, r := range strings.ToLower(host) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 32 {
		s = strings.TrimRight(s[:32], "-")
	}
	if !ValidMachineName(s) {
		return ""
	}
	return s
}

// Validate checks the whole configuration and reports every problem at once.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.Version != FormatVersion {
		add("version = %d: this Karvan understands version %d only", c.Version, FormatVersion)
	}
	if !ValidMachineName(c.Machine) {
		add("machine = %q: use lowercase letters, digits and \"-\" (at most 32)", c.Machine)
	}

	switch c.Store.Kind {
	case StoreGit:
		if c.Store.URL == "" {
			add("store.url is required for a git store")
		} else if !strings.HasPrefix(c.Store.URL, "ssh://") && !strings.HasPrefix(c.Store.URL, "https://") &&
			!strings.HasPrefix(c.Store.URL, "file://") && !strings.Contains(c.Store.URL, "@") {
			add("store.url = %q: use ssh://, https:// or git@host:owner/repo", c.Store.URL)
		}
		if c.Store.Branch == "" {
			add("store.branch must not be empty")
		}
		if c.Store.SSHKey != "" && !filepath.IsAbs(c.Store.SSHKey) {
			add("store.ssh_key = %q: must be an absolute path (\"~/\" is allowed)", c.Store.SSHKey)
		}
	case StoreFolder:
		if !filepath.IsAbs(c.Store.Path) {
			add("store.path = %q: must be an absolute path (\"~/\" is allowed)", c.Store.Path)
		}
	default:
		add("store.kind = %q: must be %q or %q", c.Store.Kind, StoreGit, StoreFolder)
	}

	if c.Sync.HistoryDays < 1 || c.Sync.HistoryDays > 365 {
		add("sync.history_days = %d: must be between 1 and 365", c.Sync.HistoryDays)
	}
	if c.Sync.Interval.Duration < time.Minute {
		add("sync.interval = %s: must be at least 1m", c.Sync.Interval.Duration)
	}
	if c.Sync.SkipRecent.Duration < 0 {
		add("sync.skip_recent must not be negative")
	}
	if c.Sync.Timeout.Duration < 10*time.Second {
		add("sync.timeout = %s: must be at least 10s", c.Sync.Timeout.Duration)
	}

	seenFolder := map[string]bool{}
	for i, f := range c.Items.Folders {
		if !nameRE.MatchString(f.Name) {
			add("items.folders[%d].name = %q: use letters, digits, \".\", \"_\" and \"-\"", i, f.Name)
		}
		if seenFolder[f.Name] {
			add("items.folders[%d].name = %q is used twice", i, f.Name)
		}
		seenFolder[f.Name] = true
		if !filepath.IsAbs(f.Path) {
			add("items.folders[%d].path = %q: must be an absolute path", i, f.Path)
		}
	}

	seenID, seenPath := map[string]bool{}, map[string]bool{}
	for i, p := range c.Projects {
		if p.ID == "" || strings.ContainsAny(p.ID, " \t\n") {
			add("projects[%d].id = %q: must be a git remote or \"name:<name>\" without spaces", i, p.ID)
		}
		if seenID[p.ID] {
			add("projects[%d].id = %q is used twice", i, p.ID)
		}
		seenID[p.ID] = true
		if !filepath.IsAbs(p.Path) {
			add("projects[%d].path = %q: must be an absolute path", i, p.Path)
		}
		if seenPath[p.Path] {
			add("projects[%d].path = %q is used twice", i, p.Path)
		}
		seenPath[p.Path] = true
	}
	return errors.Join(errs...)
}

// Load reads and validates the file at path. A missing file gives
// ErrNotInitialized. "~/" at the start of a path setting is expanded with
// home.
func Load(path, home string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotInitialized
	}
	if err != nil {
		return nil, err
	}
	c, err := Parse(data, home)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Parse decodes and validates a config file's contents. Settings missing from
// the file keep their defaults; unknown settings are an error (most likely a
// typo).
func Parse(data []byte, home string) (*Config, error) {
	c := Default()
	md, err := toml.Decode(string(data), &c)
	if err != nil {
		return nil, err
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, len(und))
		for i, k := range und {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("unknown setting(s): %s", strings.Join(keys, ", "))
	}
	c.expandHome(home)
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) expandHome(home string) {
	exp := func(p *string) {
		if home == "" {
			return
		}
		if *p == "~" {
			*p = home
		} else if strings.HasPrefix(*p, "~/") {
			*p = filepath.Join(home, (*p)[2:])
		}
	}
	exp(&c.Store.Path)
	exp(&c.Store.SSHKey)
	for i := range c.Items.Folders {
		exp(&c.Items.Folders[i].Path)
	}
	for i := range c.Projects {
		exp(&c.Projects[i].Path)
	}
}

const header = `# Karvan configuration for this machine. It is never synced.
# Documentation: https://github.com/leberman/karvan/blob/main/docs/PRD.md
`

// Encode renders c as TOML with a short header comment.
func (c *Config) Encode() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(header + "\n")
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Save validates c and writes it to path atomically (temporary file in the
// same directory, then rename) with mode 0600.
func Save(path string, c *Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	data, err := c.Encode()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
