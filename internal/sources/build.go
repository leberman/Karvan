package sources

import (
	"path/filepath"
	"strings"

	"github.com/leberman/karvan/internal/config"
	"github.com/leberman/karvan/internal/engine"
	"github.com/leberman/karvan/internal/manifest"
	"github.com/leberman/karvan/internal/paths"
	"github.com/leberman/karvan/internal/project"
)

// Build creates the sources a config asks for, and the project table for
// the manifest.
func Build(cfg *config.Config, env paths.Env) ([]engine.Source, map[string]manifest.Project) {
	var wips []WIPProject
	var out []engine.Source
	projects := map[string]manifest.Project{}
	excludedSkill := map[string]bool{}
	for _, s := range cfg.Exclude.Skills {
		excludedSkill[s] = true
	}
	excludedProject := map[string]bool{}
	for _, p := range cfg.Exclude.Projects {
		excludedProject[p] = true
	}

	if cfg.Items.ClaudeMD {
		out = append(out, &Dir{Label: "CLAUDE.md", Prefix: "global/CLAUDE.md", Root: env.Join(env.ClaudeHome(), "CLAUDE.md"), File: true})
	}
	if cfg.Items.Skills {
		out = append(out, &Dir{Label: "skills", Prefix: "global/skills/", Root: env.Join(env.ClaudeHome(), "skills"),
			Skip: func(rel string) bool {
				first, _, _ := strings.Cut(rel, "/")
				// skills/synced is managed by the desktop app itself (PRD 4).
				return first == "synced" || excludedSkill[first]
			}})
	}
	for _, f := range cfg.Items.Folders {
		out = append(out, &Dir{Label: "folder " + f.Name, Prefix: "folder:" + f.Name + "/", Root: filepath.Clean(f.Path)})
	}
	var tps []TranscriptProject
	for _, p := range cfg.Projects {
		if excludedProject[p.ID] {
			continue
		}
		pid := project.PID(p.ID)
		projects[pid] = manifest.Project{ID: p.ID, Name: p.Name}
		cdir := env.ClaudeProjectDir(p.Path)
		if cfg.Items.Memory {
			out = append(out, &Dir{Label: "memory " + nameOf(p), Prefix: "project:" + pid + "/memory/", Root: env.Join(cdir, "memory")})
		}
		tps = append(tps, TranscriptProject{PID: pid, Path: p.Path, Dir: cdir})
		if p.WIP && project.IsRepo(p.Path) {
			wips = append(wips, WIPProject{PID: pid, ID: p.ID, Path: p.Path})
		}
	}
	if cfg.Items.Transcripts && len(tps) > 0 {
		out = append(out, &Transcripts{Projects: tps, HistoryDays: cfg.Sync.HistoryDays, SkipRecent: cfg.Sync.SkipRecent.Duration, GOOS: env.GOOS})
	}
	if cfg.Items.Desktop && len(tps) > 0 {
		out = append(out, &Desktop{Projects: tps, Dir: env.DesktopSessionsDir(), GOOS: env.GOOS,
			TranscriptsOn: cfg.Items.Transcripts, HistoryDays: cfg.Sync.HistoryDays})
	}
	if len(wips) > 0 {
		out = append(out, &WIP{Machine: cfg.Machine, Projects: wips, Stage: env.Join(env.StateDir(), "wip")})
	}
	return out, projects
}

func nameOf(p config.Project) string {
	if p.Name != "" {
		return p.Name
	}
	return p.ID
}
