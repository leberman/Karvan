<p align="center">
  <img src="docs/assets/karvan.svg" width="220" alt="Karvan, a camel carrying your Claude Code work">
</p>

<h1 align="center">Karvan</h1>

Continue your Claude Code work on any of your computers, exactly where you left it: chat history,
the desktop app's session list, memory, skills and, optionally, unfinished project work.

**Status:** M0 (feature parity with the bash prototype) is code-complete and tested end to end; it is being
tried on real machines. Data is **not encrypted yet** (M1): use a private repository or a private folder.

- Syncs Claude Code conversations (line-level merge: nothing is ever dropped), the desktop app's session
  list, `CLAUDE.md`, skills, per-project memory, personal folders, and optionally unfinished git work.
- Projects can live at different paths on each machine; Karvan finds them for you.
- Store: a private git repository (built-in git, nothing to install; SSH on port 443 supported) or any
  folder a cloud service syncs.
- Automatic: Claude Code hooks plus a systemd timer.

## Install (Linux)

```sh
curl -fsSL https://raw.githubusercontent.com/leberman/Karvan/main/install.sh | sh
karvan init
```

The installer downloads a prebuilt binary and checks its SHA-256. If that is not possible it builds from
source, installing Go for Karvan only (no sudo) when needed. It retries on flaky networks, tries several
mirrors, and adds `~/.local/bin` to your PATH. Options are at the top of [install.sh](install.sh).
Update later with `karvan update`; `karvan status` tells you when a new release is out.

User guide (Persian): [docs/guide.md](docs/guide.md). Design: [docs/PRD.md](docs/PRD.md),
[docs/sync-engine.md](docs/sync-engine.md).

## Building

Requires Go 1.26 or newer.

```sh
go build ./cmd/karvan       # binary: ./karvan
go test ./...
golangci-lint run           # v2, configured in .golangci.yml
goreleaser release --snapshot --clean   # local .tar.gz and .deb in dist/
```

Configuration lives in `~/.config/karvan/config.toml` (per machine, never synced); `karvan doctor`
checks it and the Claude Code folders.

## License

Licensed under either of

- Apache License, Version 2.0 ([LICENSE-APACHE](LICENSE-APACHE))
- MIT license ([LICENSE-MIT](LICENSE-MIT))

at your option.
