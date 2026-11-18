// Package sshsetup gets a person from "I have a GitHub repository" to a
// working SSH connection: GitHub's host key, an SSH key for this computer,
// and clear instructions for the one step only they can do (adding the key
// to their GitHub account).
package sshsetup

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// GitHub's ed25519 host key, as published by GitHub (docs.github.com, "GitHub's
// SSH key fingerprints"): SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU.
// The same key serves github.com:22 and ssh.github.com:443.
const (
	GitHubHostKey         = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
	GitHubHostFingerprint = "SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU"
)

// GitHubSSH turns a GitHub URL in any form (https://github.com/o/r,
// git@github.com:o/r.git, ...) into the SSH-over-443 form Karvan uses, which
// works on networks that block port 22. ok is false for other hosts.
func GitHubSSH(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	var host, path string
	switch {
	case strings.Contains(raw, "://"):
		u, err := url.Parse(raw)
		if err != nil {
			return "", false
		}
		host, path = strings.ToLower(u.Hostname()), u.Path
	case strings.Contains(raw, ":"):
		hp, p, _ := strings.Cut(raw, ":")
		if i := strings.LastIndex(hp, "@"); i >= 0 {
			hp = hp[i+1:]
		}
		host, path = strings.ToLower(hp), p
	default:
		return "", false
	}
	if host != "github.com" && host != "ssh.github.com" && host != "www.github.com" {
		return "", false
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if strings.Count(path, "/") != 1 {
		return "", false
	}
	return "ssh://git@ssh.github.com:443/" + path + ".git", true
}

// RepoPath returns "owner/repo" of a GitHub URL.
func RepoPath(sshURL string) string {
	u, err := url.Parse(sshURL)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
}

// EnsureGitHubHostKey adds GitHub's host key for [ssh.github.com]:443 (and
// github.com) to ~/.ssh/known_hosts if it is missing. A different key
// already recorded there is left alone (that is for the person to check).
func EnsureGitHubHostKey(home string) (added bool, err error) {
	dir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	kh := filepath.Join(dir, "known_hosts")
	b, err := os.ReadFile(kh)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	have := string(b)
	var lines []string
	for _, h := range []string{"[ssh.github.com]:443", "github.com"} {
		if !hasHost(have, h) {
			lines = append(lines, h+" "+GitHubHostKey)
		}
	}
	if len(lines) == 0 {
		return false, nil
	}
	f, err := os.OpenFile(kh, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	defer f.Close()
	prefix := ""
	if len(have) > 0 && !strings.HasSuffix(have, "\n") {
		prefix = "\n"
	}
	_, err = f.WriteString(prefix + strings.Join(lines, "\n") + "\n")
	return err == nil, err
}

// hasHost reports a plain (not hashed) known_hosts entry with an ed25519 key
// for host. Hashed entries are not checked: adding a plain one next to them
// is harmless.
func hasHost(knownHosts, host string) bool {
	sc := bufio.NewScanner(strings.NewReader(knownHosts))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 || f[1] != "ssh-ed25519" {
			continue
		}
		for _, h := range strings.Split(f[0], ",") {
			if h == host {
				return true
			}
		}
	}
	return false
}

// FindKey returns the first usable private key file in ~/.ssh ("" if none).
func FindKey(home string) string {
	for _, n := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
		p := filepath.Join(home, ".ssh", n)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// CreateKey makes a new ed25519 key pair at ~/.ssh/id_ed25519 (without a
// passphrase, so Karvan's background runs can use it) and returns the
// public key line.
func CreateKey(home, comment string) (path, public string, err error) {
	dir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	path = filepath.Join(dir, "id_ed25519")
	if _, err := os.Stat(path); err == nil {
		return "", "", errors.New(path + " already exists")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return "", "", err
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", "", err
	}
	public = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))) + " " + comment
	if err := os.WriteFile(path+".pub", []byte(public+"\n"), 0o644); err != nil {
		return "", "", err
	}
	return path, public, nil
}

// PublicKey reads the .pub file next to a private key.
func PublicKey(private string) string {
	b, err := os.ReadFile(private + ".pub")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Explain turns a store connection error into what the person should do.
func Explain(err error, repo, keyFile string) string {
	msg := strings.ToLower(err.Error())
	var b strings.Builder
	switch {
	case strings.Contains(msg, "not found"), strings.Contains(msg, "authentication"),
		strings.Contains(msg, "unable to authenticate"), strings.Contains(msg, "permission denied"),
		strings.Contains(msg, "no supported methods"):
		fmt.Fprintf(&b, "GitHub did not let this computer into %s. Either the repository does not exist yet, or\n", repo)
		b.WriteString("this computer's SSH key is not added to your GitHub account. Please:\n")
		fmt.Fprintf(&b, "  1. Check the repository exists and is PRIVATE: https://github.com/%s\n", repo)
		b.WriteString("     (to create it: https://github.com/new, no README, Private)\n")
		b.WriteString("  2. Add this computer's key at https://github.com/settings/ssh/new\n")
		b.WriteString("     (Title: anything, e.g. this computer's name; Key: the line below)\n")
		if pub := PublicKey(keyFile); pub != "" {
			b.WriteString("\n" + pub + "\n")
		}
	case strings.Contains(msg, "in time"), strings.Contains(msg, "timeout"):
		b.WriteString("The connection to GitHub timed out. Check the internet connection (a VPN may help).")
	default:
		b.WriteString(err.Error())
	}
	return b.String()
}
