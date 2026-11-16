// Command sshtry runs the phase-0 git store scenario against a real remote
// over SSH: fetch, fast-forward push, a forced rejection (a second in-memory
// clone pushes first), fetch + rebuild + retry, and a final check. It never
// force-pushes. Use it ONLY with a throwaway test repository.
//
//	go run ./cmd/sshtry -url ssh://git@ssh.github.com:443/<owner>/karvan-test.git
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/memory"
	"golang.org/x/term"

	gitstore "github.com/leberman/karvan/spikes/gogit"
)

func main() {
	url := flag.String("url", "", "ssh:// URL of a THROWAWAY test repository, e.g. ssh://git@ssh.github.com:443/<owner>/karvan-test.git")
	key := flag.String("key", "", "private key file (default: ssh-agent if running, else ~/.ssh/id_ed25519, then ~/.ssh/id_rsa)")
	branch := flag.String("branch", "main", "branch to use")
	timeout := flag.Duration("timeout", 60*time.Second, "time limit for each network step")
	anyName := flag.Bool("any-name", false, "allow a repository whose name does not contain \"test\"")
	keep := flag.Bool("keep", false, "do not delete the spike's files from the repository at the end")
	flag.Parse()

	if *url == "" {
		flag.Usage()
		os.Exit(2)
	}
	repoName := strings.TrimSuffix(filepath.Base(*url), ".git")
	if !strings.Contains(strings.ToLower(repoName), "test") && !*anyName {
		fail("refusing: repository name %q does not contain \"test\". Use a throwaway test repository.", repoName)
	}

	var auth transport.AuthMethod
	how := "none (local file:// smoke test)"
	var err error
	if !strings.HasPrefix(*url, "file://") {
		auth, how, err = sshAuth(*key)
		if err == nil {
			var algos []string
			algos, err = pinHostKeys(auth, *url)
			how += fmt.Sprintf(", host key types from known_hosts: %v", algos)
		}
	}
	if err != nil {
		fail("ssh key: %v", err)
	}
	fmt.Printf("go-git %s, auth: %s\n", goGitVersion(), how)
	fmt.Printf("remote: %s (branch %s)\n\n", *url, *branch)

	run := randomID()
	dir := "karvan-spike/" + run
	open := func(machine string) *gitstore.Store {
		s, err := gitstore.Open(memory.NewStorage(), gitstore.Options{URL: *url, Branch: *branch, Auth: auth, Depth: 1, Machine: machine})
		if err != nil {
			fail("open: %v", err)
		}
		return s
	}
	a, b := open("spike-a"), open("spike-b")

	step := func(name string, fn func(ctx context.Context) error) {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()
		start := time.Now()
		err := fn(ctx)
		d := time.Since(start).Round(time.Millisecond)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
				fail("%s: no answer within %v. If port 22 is blocked on this network, try the :443 URL above. (%v)", name, *timeout, err)
			}
			fail("%s: %v", name, err)
		}
		fmt.Printf("ok   %-46s %v\n", name, d)
	}

	var headA, headB plumbing.Hash
	step("1. fetch (A)", func(ctx context.Context) (err error) { headA, err = a.Fetch(ctx); return })
	step("2. commit + fast-forward push (A)", func(ctx context.Context) error {
		c, err := a.Commit(headA, map[string][]byte{dir + "/a1.txt": []byte("from A\n")}, "karvan spike: A1")
		if err != nil {
			return err
		}
		return a.Push(ctx, c, headA)
	})
	step("3. fetch (A and B see the same head)", func(ctx context.Context) (err error) {
		if headA, err = a.Fetch(ctx); err != nil {
			return err
		}
		if headB, err = b.Fetch(ctx); err != nil {
			return err
		}
		if headA != headB {
			return fmt.Errorf("heads differ: %s vs %s", headA, headB)
		}
		return nil
	})
	step("4. B pushes first", func(ctx context.Context) error {
		c, err := b.Commit(headB, map[string][]byte{dir + "/b1.txt": []byte("from B\n")}, "karvan spike: B1")
		if err != nil {
			return err
		}
		return b.Push(ctx, c, headB)
	})
	step("5. A pushes on the old head: must be REJECTED", func(ctx context.Context) error {
		c, err := a.Commit(headA, map[string][]byte{dir + "/a2.txt": []byte("from A again\n")}, "karvan spike: A2")
		if err != nil {
			return err
		}
		err = a.Push(ctx, c, headA)
		if errors.Is(err, gitstore.ErrRejected) {
			fmt.Printf("     (rejected as expected: %v)\n", err)
			return nil
		}
		if err == nil {
			return errors.New("push was accepted but should have been rejected")
		}
		return err
	})
	var attempts int
	step("6. fetch + rebuild on top + retry (A)", func(ctx context.Context) (err error) {
		attempts, err = a.Update(ctx, "karvan spike: A2", 5, func(plumbing.Hash) (map[string][]byte, error) {
			return map[string][]byte{dir + "/a2.txt": []byte("from A again\n")}, nil
		})
		return err
	})
	fmt.Printf("     (%d attempt(s))\n", attempts)
	step("7. fresh clone sees a1, b1 and a2", func(ctx context.Context) error {
		c := open("spike-check")
		h, err := c.Fetch(ctx)
		if err != nil {
			return err
		}
		files, err := c.List(h, dir)
		if err != nil {
			return err
		}
		want := []string{dir + "/a1.txt", dir + "/a2.txt", dir + "/b1.txt"}
		if strings.Join(files, ",") != strings.Join(want, ",") {
			return fmt.Errorf("files = %v, want %v", files, want)
		}
		return nil
	})
	if !*keep {
		step("8. clean up (a normal commit deleting the files)", func(ctx context.Context) error {
			_, err := a.Update(ctx, "karvan spike: clean up", 5, func(h plumbing.Hash) (map[string][]byte, error) {
				files, err := a.List(h, dir)
				ch := map[string][]byte{}
				for _, f := range files {
					ch[f] = nil
				}
				return ch, err
			})
			return err
		})
	}
	fmt.Println("\nAll steps passed. Nothing was force-pushed.")
}

func sshAuth(keyFile string) (transport.AuthMethod, string, error) {
	if keyFile == "" && os.Getenv("SSH_AUTH_SOCK") != "" {
		a, err := gitssh.NewSSHAgentAuth("git")
		if err == nil {
			return a, "ssh-agent", nil
		}
	}
	if keyFile == "" {
		home, _ := os.UserHomeDir()
		for _, k := range []string{"id_ed25519", "id_rsa"} {
			p := filepath.Join(home, ".ssh", k)
			if _, err := os.Stat(p); err == nil {
				keyFile = p
				break
			}
		}
		if keyFile == "" {
			return nil, "", errors.New("no ssh-agent and no ~/.ssh/id_ed25519 or ~/.ssh/id_rsa; pass -key")
		}
	}
	a, err := gitssh.NewPublicKeysFromFile("git", keyFile, "")
	if err != nil && strings.Contains(err.Error(), "passphrase") {
		fmt.Fprintf(os.Stderr, "passphrase for %s: ", keyFile)
		pw, rerr := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if rerr != nil {
			return nil, "", rerr
		}
		a, err = gitssh.NewPublicKeysFromFile("git", keyFile, string(pw))
	}
	if err != nil {
		return nil, "", err
	}
	// go-git checks ~/.ssh/known_hosts by default; say so if it fails.
	return a, "key file " + keyFile, nil
}

// pinHostKeys works around a go-git v5.19.3 bug: when the auth method is
// ssh-agent or a key file, go-git installs a known_hosts callback but never
// sets HostKeyAlgorithms from known_hosts (that code path only runs when the
// callback is nil, and it never is). The server may then offer a key type
// that is not in known_hosts, and the check fails with "knownhosts: key
// mismatch" although `ssh` itself works. So we open the known_hosts database
// ourselves and set both the callback and the key types it has for host:port.
func pinHostKeys(auth transport.AuthMethod, url string) ([]string, error) {
	ep, err := transport.NewEndpoint(url)
	if err != nil {
		return nil, err
	}
	port := ep.Port
	if port == 0 {
		port = 22
	}
	hostPort := net.JoinHostPort(ep.Host, strconv.Itoa(port))
	db, err := gitssh.NewKnownHostsDb()
	if err != nil {
		return nil, err
	}
	algos := db.HostKeyAlgorithms(hostPort)
	if len(algos) == 0 {
		return nil, fmt.Errorf("no key for %s in ~/.ssh/known_hosts (knownhosts: key is unknown)", hostPort)
	}
	var h *gitssh.HostKeyCallbackHelper
	switch a := auth.(type) {
	case *gitssh.PublicKeysCallback:
		h = &a.HostKeyCallbackHelper
	case *gitssh.PublicKeys:
		h = &a.HostKeyCallbackHelper
	default:
		return nil, fmt.Errorf("unexpected auth type %T", auth)
	}
	h.HostKeyCallback = db.HostKeyCallback()
	h.HostKeyAlgorithms = algos
	return algos, nil
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func goGitVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, d := range bi.Deps {
			if d.Path == "github.com/go-git/go-git/v5" {
				return d.Version
			}
		}
	}
	return "(unknown)"
}

func randomID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

func fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintln(os.Stderr, "FAIL "+msg)
	if strings.Contains(msg, "key mismatch") {
		fmt.Fprintln(os.Stderr, "     The server's host key differs from the one in ~/.ssh/known_hosts. Check with:")
		fmt.Fprintln(os.Stderr, "       ssh-keygen -F '[ssh.github.com]:443' -l")
		fmt.Fprintln(os.Stderr, "     and compare the fingerprints with GitHub's published ones.")
	} else if strings.Contains(msg, "knownhosts") || strings.Contains(msg, "key is unknown") || strings.Contains(msg, "known_hosts") {
		fmt.Fprintln(os.Stderr, "     The host key is not in ~/.ssh/known_hosts. Run once:  ssh -T -p 443 git@ssh.github.com")
		fmt.Fprintln(os.Stderr, "     and accept the key only if its fingerprint matches GitHub's published one.")
	}
	os.Exit(1)
}
