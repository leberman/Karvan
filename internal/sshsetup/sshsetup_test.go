package sshsetup

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestGitHubSSH(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/alihabibidev/my-claude.git": "ssh://git@ssh.github.com:443/alihabibidev/my-claude.git",
		"https://github.com/o/r":                        "ssh://git@ssh.github.com:443/o/r.git",
		"git@github.com:o/r.git":                        "ssh://git@ssh.github.com:443/o/r.git",
		"ssh://git@ssh.github.com:443/o/r.git":          "ssh://git@ssh.github.com:443/o/r.git",
	} {
		if got, ok := GitHubSSH(in); !ok || got != want {
			t.Errorf("GitHubSSH(%q) = %q %v", in, got, ok)
		}
	}
	for _, bad := range []string{"https://gitlab.com/o/r", "https://github.com/o", "/local"} {
		if _, ok := GitHubSSH(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if RepoPath("ssh://git@ssh.github.com:443/o/r.git") != "o/r" {
		t.Fatal("RepoPath")
	}
}

// The embedded host key really is the one GitHub publishes.
func TestHostKeyFingerprint(t *testing.T) {
	k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(GitHubHostKey))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(k.Marshal())
	if got := "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]); got != GitHubHostFingerprint {
		t.Fatalf("fingerprint %s", got)
	}
}

func TestKnownHostsAndKeys(t *testing.T) {
	home := t.TempDir()
	if added, err := EnsureGitHubHostKey(home); err != nil || !added {
		t.Fatal(added, err)
	}
	if added, _ := EnsureGitHubHostKey(home); added {
		t.Fatal("added twice")
	}
	b, _ := os.ReadFile(filepath.Join(home, ".ssh", "known_hosts"))
	if strings.Count(string(b), "ssh-ed25519") != 2 || !strings.Contains(string(b), "[ssh.github.com]:443 ") {
		t.Fatalf("%s", b)
	}
	if FindKey(home) != "" {
		t.Fatal("no key yet")
	}
	path, pub, err := CreateKey(home, "karvan@test")
	if err != nil || !strings.HasPrefix(pub, "ssh-ed25519 ") || FindKey(home) != path {
		t.Fatal(path, pub, err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatal("private key mode")
	}
	raw, _ := os.ReadFile(path)
	if _, err := ssh.ParsePrivateKey(raw); err != nil {
		t.Fatalf("key not usable: %v", err)
	}
	if _, _, err := CreateKey(home, "x"); err == nil {
		t.Fatal("overwrote a key")
	}
	msg := Explain(errors.New("authentication required: Repository not found."), "o/r", path)
	if !strings.Contains(msg, "https://github.com/settings/ssh/new") || !strings.Contains(msg, pub) {
		t.Fatalf("%s", msg)
	}
}
