# PRD: Karvan

Continue your Claude Code work on any of your computers, exactly where you left it.

- Status: draft 3, 2026-11-16
- Owner: the maintainer
- Language and stack: Rust
- License: MIT or Apache-2.0, at the user's option
- First public release: Linux only; Windows next, then macOS

> Unofficial tool. Not made or endorsed by Anthropic. It reads and writes files that Claude Code and
> the Claude desktop app keep on disk; some of those formats are undocumented and can change.

## 1. Problem

People use Claude Code on more than one computer (work and home, desktop and laptop). Everything that
makes a session useful stays on the machine where it happened:

- the chat history of Claude Code sessions (CLI and the desktop app's Code tab);
- the session list in the desktop app;
- personal instructions (`CLAUDE.md`), skills and per-project memory;
- unfinished work in the project itself: a branch, commits not pushed yet, staged and unstaged changes.

Moving to the other machine means starting cold: no history to resume, no memory, and code left half
done on the machine that is now off.

A personal prototype (bash scripts and a private git repository) ran between two Ubuntu machines in
November 2026 and proved the idea. Karvan turns it into a tool anyone can install.

## 2. Users

| User | Uses | Needs |
| ---- | ---- | ----- |
| Developer with two or more machines | Claude Code CLI and/or the desktop Code tab | History, memory, skills and unfinished git work on every machine |
| Non-programmer | The desktop app's Code tab (and Cowork, which already runs in the cloud) | History and session list on every machine, with no terminal, git or GitHub knowledge |

The first Linux release mostly targets developers; the design must not block the non-programmer path,
which matters for Windows and macOS.

## 3. Goals and non-goals

### Goals

1. A session started on one machine can be resumed on another, in the CLI (`claude --resume`) and in
   the desktop app's Code tab list.
2. `CLAUDE.md`, skills and per-project memory are the same on every machine.
3. For developers (optional): unfinished project work travels and comes back with one command.
4. Private by default: data is encrypted before it leaves the machine; secrets are caught before upload.
5. Nothing is lost silently: no force pushes, no overwrite of newer data; a conflict keeps both sides.
6. Hands-off once set up: syncs on its own (Claude Code hooks, a timer, at shutdown).

### Non-goals (v1)

- Real-time collaboration or syncing between different people.
- Syncing credentials, `.env` files, or Claude login state.
- Syncing claude.ai chats or Cowork sessions (already stored in the cloud).
- Windows and macOS builds (v1 code must stay portable; section 11).
- Being an official Anthropic product.

## 4. What syncs

| Item | Location on Linux | Default | Notes |
| ---- | ----------------- | ------- | ----- |
| Global instructions | `~/.claude/CLAUDE.md` | on | |
| Skills | `~/.claude/skills/<name>/` | on | Skip `skills/synced/` (the app syncs those itself) |
| Project memory | `~/.claude/projects/<dashed path>/memory/` | on | Per project; section 5 |
| Session transcripts | `~/.claude/projects/<dashed path>/<id>.jsonl` | on | Last N days; configurable, default 7 |
| Desktop app session list | `~/.config/Claude/claude-code-sessions/<account>/<org>/local_<id>.json` | on | Paths rewritten per machine; the app reads the list only at start |
| Unfinished project work | the project's git repository | off | Developers; branch, unpushed commits, staged, unstaged, untracked (not ignored) files |
| Personal folders | any folder the user picks | off | e.g. notes kept outside the project |
| `settings.json` | `~/.claude/settings.json` | never | Machine-specific; Karvan only adds its hooks |
| Machine-only instructions | `~/.claude/CLAUDE.machine.md` | never | Section 4.1 |

### 4.1 Per-machine settings

By default everything is shared between machines. Two simple ways to differ, with no merge logic:

- **Machine-only instructions:** `karvan init` adds one import line at the end of the shared `CLAUDE.md`
  (`@~/.claude/CLAUDE.machine.md`) and creates that file on each machine. Claude Code reads it; Karvan
  never syncs it. For example, at work: "you can reach the staging server"; at home: "the VPN is off".
- **Exclude on one machine:** a machine can opt out of a skill or a project
  (`karvan exclude skill <name>`). Other machines are not affected.

## 5. Projects across machines

The same project sits at different paths on different machines (`~/work/app` at work, `~/temp/app`
at home). Claude Code names a project's folder after its absolute path, and the desktop app stores
absolute paths in its session entries.

- A project is identified by its normalized git remote URL, or by a name the user gives when there is
  no remote.
- Each machine keeps its own map: project id → local path.
- Stored data uses a placeholder for the project path; each machine writes its own path back when it
  loads. Only whole path prefixes are rewritten (`/a/app` matches `/a/app/x`, not `/a/app-old`).
- A project not mapped on a machine is carried but not loaded there; `status` lists it.

## 6. Sync behavior

### When

- Claude Code `SessionStart` hook: pull and load.
- Claude Code `SessionEnd` hook: save and push.
- A timer, every 10 minutes (configurable), while the machine is on; it skips transcripts written in
  the last 15 minutes (probably still open).
- At logout and shutdown: save everything and push.
- By hand: `karvan sync`.

Runs are serialized with a lock. Output on `SessionStart` is shown to Claude, so a normal run prints
nothing; a run that needs the user prints one plain line.

### Conflict rules

From the trial: a session continued on both machines changed its transcript on both sides, and every
later sync stopped until it was fixed by hand.

| Data | Rule |
| ---- | ---- |
| Transcript | Append-only: if one copy is a prefix of the other, keep the longer. If they diverged (continued on both machines), keep both: the second becomes a new session titled "… (from <machine>)". Never drop lines. |
| Desktop session entry | The newer `lastActivityAt` wins. |
| A machine's own state (unfinished-work snapshot, held-back list) | Each machine owns its own; never merged. |
| Text (`CLAUDE.md`, memory, skills, personal folders) | Three-way merge. On a real conflict keep both versions side by side, notify, and keep syncing everything else. |
| Deletion vs. change | The change wins. |

### Unfinished project work (optional)

From the prototype's `wip.sh`:

- Save: branch, HEAD, a bundle of commits no remote has, staged and unstaged diffs, untracked files git
  does not ignore (limit 20 MB). An unchanged tree adds nothing. A merge or rebase in progress cannot
  travel; say so.
- Status on start: "unfinished work from <machine> (<time>, branch <b>) is waiting", only when
  something is actually unfinished.
- Restore (only when the user agrees): fetch the branch (fetch everything if HEAD is still missing: a
  new branch with no commits of its own is not on the remote under its name), switch, fast-forward,
  reapply the changes. Refuse rather than overwrite local changes no snapshot holds. Say what it is
  doing while it waits on the network.
- Never commit, push, stash or reset the user's project.

## 7. Transport

The store is where machines meet.

| Option | For | v1 |
| ---- | ---- | ---- |
| A private git repository (GitHub, GitLab, any SSH/HTTPS remote) | Developers | yes |
| A folder synced by a cloud drive (Google Drive, Dropbox, OneDrive, Syncthing) | Everyone | before the Windows release |

Requirements for the git transport, from the trial:

- **Nothing to install:** git is built in (the gitoxide library); the `git` command is not needed. The
  one exception is the optional unfinished-work feature, which works on the user's own repository;
  whoever turns it on already has git.
- Never force-push. Fetch, merge by the rules above, push; on failure keep local data and retry.
- A timeout on every network call, with a clear message ("push failed, will retry").
- Some networks stall SSH on port 22 while login and fetch still work. Detect a stalled push and offer
  GitHub's SSH over port 443 (`ssh.github.com:443`). Optional, never the default.
- Refuse a public repository (checked through the host's API where possible) unless the user overrides.

## 8. Security and privacy

Transcripts hold code, file contents, command output and sometimes secrets.

- **Encryption on by default.** Every file is encrypted (age) before it reaches the store. Keys:
  - The first `karvan init` creates a random **master key** (an age X25519 identity). All data is
    encrypted to it.
  - The master key is locked with the user's **passphrase** (scrypt), and the locked copy lives in the
    store itself. A second machine asks only for the passphrase; no file is moved by hand.
  - On each machine the unlocked key is kept in the OS keyring; the passphrase is needed once per machine.
  - **Recovery code:** `init` shows a recovery code (the master key in printable form) once, to keep
    somewhere safe. Losing both the passphrase and this code means the data cannot be recovered; Karvan
    says so plainly.
  - Changing the passphrase re-locks only the master key, not the data.
  - Advanced users may use a key file instead of a passphrase.
- **Secret scanning before upload,** with built-in gitleaks-style rules. A file with a finding stays on
  the machine and is listed in `status` with the rule that matched.
- **Allow rules** the user can turn on, e.g. database URLs on `localhost` (in the trial, 16 of 20
  sessions were held back only for a local development database URL).
- Never read or send `.env`, credentials, `~/.claude/.credentials.json`, `~/.claude.json`.
- Encrypted data still shows file sizes and times to the store; the docs say so.

## 9. User experience

### v1 (Linux): command line

| Command | Does |
| ---- | ---- |
| `karvan init` | First run: choose the store, set up encryption, map projects, install hooks and the timer |
| `karvan status` | Last sync, what is waiting, what was held back and why, problems |
| `karvan sync` | Sync now |
| `karvan restore [machine]` | Restore unfinished work |
| `karvan doctor` | Check `settings.json` is valid JSON, hooks, timer, store access, network (port 22/443), app format version |
| `karvan uninstall` | Remove hooks and timer, restore settings, keep the store |

- Messages in plain language; English first, Persian next.
- When desktop sessions arrived: "new sessions from <machine>: quit the Claude app with Ctrl+Q and open
  it again to see them" (closing the window is not enough; the app reads its list only at start).

### Later: tray app

Status icon, Sync now, Restore, settings. Tauri, sharing the Rust core.

## 10. Install (Linux v1)

- A `.deb` and a static tarball (AppImage optional); one binary, nothing else to install.
- `init` edits `~/.claude/settings.json` safely: refuses invalid JSON with a clear message, backs it up,
  adds its hooks once.
- systemd user units for the timer and the shutdown run.

## 11. Platforms

| | Linux (v1) | Windows (next) | macOS (later) |
| -- | -- | -- | -- |
| Desktop app data | `~/.config/Claude` | `%APPDATA%\Claude` | `~/Library/Application Support/Claude` |
| Timer and shutdown run | systemd user units | Task Scheduler | launchd |
| Default store | git | cloud-drive folder | cloud-drive folder |

v1 code keeps platform specifics behind one module and never shells out to `date`, `sed`, `tac` or
`flock` (the prototype's GNU-only tools).

## 12. Undocumented formats

The desktop app's session entries and the transcripts' layout are internal formats.

- Detect the format version; if it is unknown, stop syncing that part, keep the rest working, and say so.
- Write only fields known to exist; copy the rest through unchanged.
- Integration tests run against real files from current app versions.

## 13. Success measures

- No data lost across the trial scenarios (section 15) and a two-week beta.
- A session continued on one machine can be resumed on the other within 15 minutes of both being online.
- No manual merges in normal use.
- Install to first sync in under 5 minutes for a developer.

## 14. Milestones

1. **M0 – parity:** a Rust CLI with the prototype's behavior (git store, one project, no encryption),
   tested on the two trial machines. Built phase by phase (section 14.1).
2. **M1 – safe:** encryption, secret scanning, allow rules, multiple projects, the divergent-transcript rule.
3. **M2 – Linux public release:** packaging, `doctor`, `uninstall`, docs.
4. **M3:** cloud-drive folder store, tray app.
5. **M4:** Windows. **M5:** macOS.

### 14.1 M0 phases

Each phase ends with passing tests, a commit and a push, then stops for review before the next one.

1. **Skeleton:** Cargo project, CLI (`init`, `sync`, `status`, `restore`, `doctor` stubs), config file,
   paths module (Claude dirs, dashed project folder name), CI running `cargo test` and `cargo clippy`.
2. **Hooks:** `init` adds `SessionStart`/`SessionEnd` hooks to `settings.json` safely; `doctor` checks them.
3. **Store:** git store via gitoxide: clone, fetch, commit, merge with per-path conflict rules, push,
   timeouts, lock; never force.
4. **Shared files:** `CLAUDE.md`, skills (not `synced/`), project memory.
5. **Transcripts:** save and load with the history window, active-session skip, secret hold-back.
6. **Desktop entries:** save and load with the path placeholder and the newer-wins rule.
7. **Unfinished work:** save, status, restore.
8. **Timer and end-to-end:** systemd user units; an end-to-end test with two temporary home folders and
   a local bare repository.

## 15. Test scenarios (from the trial)

1. The same session continued on both machines → both kept, nothing stops.
2. A new branch with no commits of its own → restore fetches what it needs.
3. A push stalls on port 22 → detected, 443 offered.
4. `settings.json` with a trailing comma → clear error, nothing written.
5. Desktop entries arrive while the app is open → shown after Ctrl+Q and reopening.
6. A secret in a transcript → held back, listed, never uploaded.
7. One machine off for a day, then both online → they converge with no manual step.
8. A merge or rebase in progress in the project → not saved, explained.

## 16. Decisions

1. Name: Karvan.
2. License: MIT or Apache-2.0.
3. Encryption keys: a random master key locked with a passphrase, plus a recovery code; key file optional (section 8).
4. History window: configurable, default 7 days.
5. Git: built in (gitoxide); users install nothing (section 7).
6. Per-machine settings: shared by default; a machine-only `CLAUDE.machine.md` and per-machine excludes (section 4.1).
7. Docs, code and the development conversation are in English.
