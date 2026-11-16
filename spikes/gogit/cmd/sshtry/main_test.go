package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/skeema/knownhosts"
	"golang.org/x/crypto/ssh"
	xkh "golang.org/x/crypto/ssh/knownhosts"
)

// pinHostKeys must take the key types from known_hosts (plain or hashed
// entries) for [host]:443 and install them on the auth method.
func TestPinHostKeys(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	hostKey, _ := ssh.NewPublicKey(pub)
	signer, _ := ssh.NewSignerFromKey(priv)
	host := knownhosts.Normalize("ssh.github.com:443") // "[ssh.github.com]:443"
	for name, line := range map[string]string{
		"plain":  knownhosts.Line([]string{host}, hostKey),
		"hashed": xkh.HashHostname(host) + " " + string(ssh.MarshalAuthorizedKey(hostKey)),
	} {
		t.Run(name, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), "known_hosts")
			if err := os.WriteFile(f, []byte(line+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("SSH_KNOWN_HOSTS", f)
			auth := &gitssh.PublicKeys{User: "git", Signer: signer}
			algos, err := pinHostKeys(auth, "ssh://git@ssh.github.com:443/o/karvan-test.git")
			if err != nil {
				t.Fatal(err)
			}
			if len(algos) != 1 || algos[0] != ssh.KeyAlgoED25519 {
				t.Fatalf("algos = %v", algos)
			}
			cfg, err := auth.ClientConfig()
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.HostKeyAlgorithms) != 1 || cfg.HostKeyCallback == nil {
				t.Fatalf("config not pinned: %v", cfg.HostKeyAlgorithms)
			}
			// Port 22 has no entry: must fail clearly, not fall back.
			if _, err := pinHostKeys(auth, "ssh://git@ssh.github.com/o/karvan-test.git"); err == nil {
				t.Fatal("expected error for a host:port missing from known_hosts")
			}
		})
	}
}
