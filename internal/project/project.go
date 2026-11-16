// Package project identifies a project the same way on every machine
// (PRD 5): by its normalized git remote, or by a name the user gives.
package project

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"

	git "github.com/go-git/go-git/v5"
)

// PID is the short, path-safe form of a project ID used in logical paths:
// "p-" + 16 hex characters of SHA-256 (docs/sync-engine.md, 4).
func PID(id string) string {
	s := sha256.Sum256([]byte(id))
	return "p-" + hex.EncodeToString(s[:8])
}

// NormalizeRemote turns any form of a git remote URL into host/path:
//
//	git@github.com:Owner/App.git            -> github.com/owner/app
//	https://github.com/owner/app/           -> github.com/owner/app
//	ssh://git@ssh.github.com:443/owner/app  -> github.com/owner/app
//
// Hosts are lowercased; for GitHub, GitLab and Bitbucket (case-insensitive
// hosts) the path is lowercased too. ok is false if u is not a remote URL.
func NormalizeRemote(u string) (string, bool) {
	u = strings.TrimSpace(u)
	if u == "" {
		return "", false
	}
	var host, path string
	switch {
	case strings.Contains(u, "://"):
		p, err := url.Parse(u)
		if err != nil || p.Host == "" {
			return "", false
		}
		if p.Scheme == "file" {
			return "", false
		}
		host, path = p.Hostname(), p.Path
	case strings.Contains(u, ":") && !strings.HasPrefix(u, "/"):
		// scp-like: user@host:path
		hp, rest, _ := strings.Cut(u, ":")
		if i := strings.LastIndex(hp, "@"); i >= 0 {
			hp = hp[i+1:]
		}
		host, path = hp, rest
	default:
		return "", false
	}
	host = strings.ToLower(host)
	host = strings.TrimPrefix(host, "www.")
	if host == "ssh.github.com" {
		host = "github.com"
	}
	host = strings.TrimPrefix(host, "altssh.")
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	if host == "" || path == "" {
		return "", false
	}
	switch host {
	case "github.com", "gitlab.com", "bitbucket.org":
		path = strings.ToLower(path)
	}
	return host + "/" + path, true
}

// ErrNoRemote means the folder is not a git repository with a remote.
var ErrNoRemote = errors.New("no git remote")

// Detect returns the project ID for a folder: its normalized "origin" (or
// first) remote. No git binary is used.
func Detect(dir string) (string, error) {
	repo, err := git.PlainOpenWithOptions(dir, &git.PlainOpenOptions{DetectDotGit: true, EnableDotGitCommonDir: true})
	if err != nil {
		return "", ErrNoRemote
	}
	remotes, err := repo.Remotes()
	if err != nil || len(remotes) == 0 {
		return "", ErrNoRemote
	}
	pick := remotes[0]
	for _, r := range remotes {
		if r.Config().Name == "origin" {
			pick = r
		}
	}
	for _, u := range pick.Config().URLs {
		if id, ok := NormalizeRemote(u); ok {
			return id, nil
		}
	}
	return "", ErrNoRemote
}

// NameID is the ID of a project without a remote.
func NameID(name string) string { return "name:" + name }
