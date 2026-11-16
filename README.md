# Karvan

Continue your Claude Code work on any of your computers, exactly where you left it: chat history,
the desktop app's session list, memory, skills and, optionally, unfinished project work.

> Unofficial tool, not made or endorsed by Anthropic.

**Status:** early development (M0 phase 1: command-line skeleton; the commands do not sync yet).
See [docs/PRD.md](docs/PRD.md) and [docs/sync-engine.md](docs/sync-engine.md) (in Persian). Written in Go.
First release: Linux; Windows and macOS later.

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
