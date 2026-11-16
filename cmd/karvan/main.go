// Command karvan continues your Claude Code work on any of your computers.
package main

import (
	"fmt"
	"os"

	"github.com/leberman/karvan/internal/cli"
	"github.com/leberman/karvan/internal/paths"
)

// Set by goreleaser: -X main.version=... -X main.commit=... -X main.date=...
var (
	version string
	commit  string
	date    string
)

func main() {
	env, err := paths.FromOS()
	if err != nil {
		fmt.Fprintf(os.Stderr, "karvan: %v\n", err)
		os.Exit(cli.ExitError)
	}
	app := &cli.App{
		Env:    env,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Build:  cli.Build{Version: version, Commit: commit, Date: date},
	}
	os.Exit(cli.Run(app, os.Args[1:]))
}
