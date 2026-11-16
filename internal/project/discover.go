package project

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
)

// Found is a project discovered from Claude Code's own folders.
type Found struct {
	Path     string // the folder Claude Code ran in
	ID       string
	Name     string
	Sessions int
	LastUsed time.Time
	Remote   bool // ID comes from a git remote
}

// Discover lists the folders Claude Code has been used in, from
// projectsDir (~/.claude/projects): each conversation records the folder it
// ran in ("cwd"). Folders that no longer exist are left out. Newest first.
func Discover(projectsDir string) ([]Found, error) {
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Found
	seen := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(projectsDir, e.Name())
		files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
		if len(files) == 0 {
			continue
		}
		var newest time.Time
		cwd := ""
		sort.Slice(files, func(i, j int) bool { return modTime(files[i]).After(modTime(files[j])) })
		newest = modTime(files[0])
		for _, f := range files {
			if cwd = cwdOf(f); cwd != "" {
				break
			}
		}
		if cwd == "" || seen[cwd] {
			continue
		}
		if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
			continue
		}
		seen[cwd] = true
		f := Found{Path: cwd, Sessions: len(files), LastUsed: newest, Name: filepath.Base(cwd)}
		f.ID, f.Remote = IDFor(cwd)
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastUsed.After(out[j].LastUsed) })
	return out, nil
}

// IDFor gives a folder's project ID: its git remote (plus the sub-folder
// inside the repository, if any), else "name:<folder name>".
func IDFor(dir string) (string, bool) {
	id, err := Detect(dir)
	if err != nil {
		return NameID(filepath.Base(dir)), false
	}
	repo, err := git.PlainOpenWithOptions(dir, &git.PlainOpenOptions{DetectDotGit: true, EnableDotGitCommonDir: true})
	if err == nil {
		if wt, err := repo.Worktree(); err == nil {
			root := wt.Filesystem.Root()
			if rel, err := filepath.Rel(root, dir); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
				id += "/" + filepath.ToSlash(rel)
			}
		}
	}
	return id, true
}

func modTime(p string) time.Time {
	st, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

// cwdOf returns the first top-level "cwd" in a transcript.
func cwdOf(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for i := 0; sc.Scan() && i < 200; i++ {
		var v struct {
			CWD string `json:"cwd"`
		}
		if json.Unmarshal(sc.Bytes(), &v) == nil && v.CWD != "" {
			return v.CWD
		}
	}
	return ""
}
