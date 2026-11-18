#!/bin/sh
# Karvan installer.
#
#   curl -fsSL https://raw.githubusercontent.com/leberman/Karvan/main/install.sh | sh
#
# What it does, in order:
#   1. Downloads a prebuilt karvan for your system from GitHub Releases and
#      checks its SHA-256. No Go, no git, no sudo needed.
#   2. If that is not possible (no release yet, blocked download), builds
#      karvan from source. If Go 1.26+ is missing it installs Go just for
#      Karvan in ~/.local/share/karvan/go (no sudo, nothing system-wide).
#   3. Installs to ~/.local/bin (or $KARVAN_INSTALL_DIR) and adds it to PATH.
#
# Settings (environment variables, all optional):
#   KARVAN_VERSION=v0.1.0       a specific release (default: the latest)
#   KARVAN_INSTALL_DIR=DIR      where the binary goes (default: ~/.local/bin)
#   KARVAN_FROM_SOURCE=1        skip the prebuilt binary, build from source
#   KARVAN_REF=main             git branch or tag to build from source
#   KARVAN_NO_MODIFY_PATH=1     do not touch shell startup files
#   KARVAN_RELEASE_URL=URL      a mirror holding the release files (checksums.txt...)
#   GOPROXY=...                 Go module proxies for the source build
#
# The whole script is inside main(), so a download cut off halfway runs nothing.

main() {
  set -eu

  REPO="leberman/Karvan"
  GO_MIN_MINOR=26
  GO_VERSION_FALLBACK="1.26.0"
  INSTALL_DIR="${KARVAN_INSTALL_DIR:-$HOME/.local/bin}"
  DATA_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/karvan"
  REF="${KARVAN_REF:-main}"

  setup_colors
  say "${BOLD}Installing Karvan${RESET} (Claude Code work on all your computers)"

  detect_platform
  detect_downloader

  TMP="$(mktemp -d 2>/dev/null || mktemp -d -t karvan)"
  BG_PID=""
  # Ctrl+C (or a closed terminal) stops everything: the background job, the
  # temporary files, and the script itself. Running it again starts clean.
  trap cleanup EXIT
  trap 'interrupted' INT TERM HUP

  installed=""
  if [ "${KARVAN_FROM_SOURCE:-0}" != "1" ]; then
    if install_release; then
      installed=1
    else
      warn "No prebuilt binary could be downloaded; building from source instead."
    fi
  fi
  if [ -z "$installed" ]; then
    install_from_source
  fi

  setup_path
  finish
}

# --------------------------------------------------------------- signals --

cleanup() {
  if [ -n "${BG_PID:-}" ]; then
    kill "$BG_PID" 2>/dev/null || true
  fi
  rm -rf "${TMP:-}" "$INSTALL_DIR/.karvan.new" "$DATA_DIR/go.new" 2>/dev/null || true
}

interrupted() {
  trap - EXIT INT TERM HUP
  printf '\n%sCancelled.%s Nothing was changed; run the same command again to start over.\n' "${YELLOW:-}" "${RESET:-}" >&2
  cleanup
  exit 130
}

# ---------------------------------------------------------------- output --

setup_colors() {
  TTY=0
  if [ -t 2 ]; then
    TTY=1
  fi
  if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
    BOLD="$(printf '\033[1m')"; DIM="$(printf '\033[2m')"; RED="$(printf '\033[31m')"
    GREEN="$(printf '\033[32m')"; YELLOW="$(printf '\033[33m')"; RESET="$(printf '\033[0m')"
  else
    BOLD=""; DIM=""; RED=""; GREEN=""; YELLOW=""; RESET=""
  fi
}

say()  { printf '%s\n' "$*"; }
step() { printf '%s==>%s %s\n' "$GREEN" "$RESET" "$*"; }
warn() { printf '%swarning:%s %s\n' "$YELLOW" "$RESET" "$*" >&2; }
die() {
  printf '%serror:%s %s\n' "$RED" "$RESET" "$*" >&2
  printf '%sIf this keeps happening, open an issue at https://github.com/%s/issues with the output above.%s\n' "$DIM" "$REPO" "$RESET" >&2
  exit 1
}

# -------------------------------------------------------------- platform --

detect_platform() {
  os="$(uname -s)"
  case "$os" in
    Linux) OS=linux ;;
    Darwin)
      OS=darwin
      warn "macOS support is not released yet; trying anyway (see docs/PRD.md)." ;;
    MINGW*|MSYS*|CYGWIN*) die "Windows is not supported yet (planned). Use WSL for now." ;;
    *) die "unsupported system: $os" ;;
  esac
  arch="$(uname -m)"
  case "$arch" in
    x86_64|amd64) ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) die "unsupported processor: $arch (amd64 and arm64 are supported)" ;;
  esac
  step "System: $OS/$ARCH"
}

detect_downloader() {
  if command -v curl >/dev/null 2>&1; then
    DL=curl
  elif command -v wget >/dev/null 2>&1; then
    DL=wget
  else
    die "neither curl nor wget is installed. Install one (e.g. sudo apt install curl) and run this again."
  fi
}

# download URL FILE: retries, resumes partial downloads, fails on HTTP errors.
download() {
  url="$1"; out="$2"; tries=0
  while [ "$tries" -lt 5 ]; do
    tries=$((tries + 1))
    rc=0
    if [ "$DL" = curl ]; then
      if [ "$TTY" = 1 ]; then
        curl -fL --retry 3 --retry-delay 2 --connect-timeout 20 -C - --progress-bar -o "$out" "$url" || rc=$?
      else
        curl -fL --retry 3 --retry-delay 2 --connect-timeout 20 -C - -sS -o "$out" "$url" || rc=$?
      fi
      # 22 = HTTP error (404...): retrying will not help.
      [ "$rc" -eq 0 ] && return 0
      [ "$rc" -eq 22 ] && return 1
    else
      if [ "$TTY" = 1 ]; then
        wget -q --show-progress -c -T 20 -t 3 -O "$out" "$url" || rc=$?
      else
        wget -q -c -T 20 -t 3 -O "$out" "$url" || rc=$?
      fi
      # 8 = the server answered with an error.
      [ "$rc" -eq 0 ] && return 0
      [ "$rc" -eq 8 ] && return 1
    fi
    say "    download interrupted, retrying ($tries/5)..."
    sleep "$tries"
  done
  return 1
}

# probe URL: true if the file is there (quiet, first byte only).
probe() {
  if [ "$DL" = curl ]; then
    curl -fsSL --connect-timeout 15 -r 0-0 -o /dev/null "$1" 2>/dev/null
  else
    wget -q -T 15 -t 1 --spider "$1" 2>/dev/null
  fi
}

# with_spinner MESSAGE COMMAND...: runs a long step, showing that it is alive
# and how long it has taken. Output goes to $TMP/step.log (shown on failure).
with_spinner() {
  msg="$1"; shift
  "$@" >"$TMP/step.log" 2>&1 &
  BG_PID=$!
  start=$(date +%s)
  if [ "$TTY" = 1 ]; then
    i=0
    while kill -0 "$BG_PID" 2>/dev/null; do
      i=$(( (i + 1) % 4 ))
      case $i in 0) c='.   ' ;; 1) c='..  ' ;; 2) c='... ' ;; *) c='....' ;; esac
      printf '\r%s==>%s %s %s (%ss)  ' "$GREEN" "$RESET" "$msg" "$c" "$(( $(date +%s) - start ))" >&2
      sleep 1
    done
  fi
  rc=0
  wait "$BG_PID" || rc=$?
  BG_PID=""
  result="done"
  [ "$rc" -eq 0 ] || result="${RED}failed${RESET}"
  if [ "$TTY" = 1 ]; then
    printf '\r%s==>%s %s %s (%ss)   \n' "$GREEN" "$RESET" "$msg" "$result" "$(( $(date +%s) - start ))" >&2
  else
    step "$msg $result"
  fi
  return "$rc"
}

# fetch URL: prints the body (small files).
fetch() {
  if [ "$DL" = curl ]; then
    curl -fsSL --retry 3 --connect-timeout 20 "$1"
  else
    wget -q -T 20 -t 3 -O - "$1"
  fi
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | cut -d' ' -f1
  else
    echo ""
  fi
}

# ------------------------------------------------------- prebuilt binary --

install_release() {
  if [ -n "${KARVAN_RELEASE_URL:-}" ]; then
    base="${KARVAN_RELEASE_URL%/}"
  elif [ -n "${KARVAN_VERSION:-}" ]; then
    base="https://github.com/$REPO/releases/download/$KARVAN_VERSION"
  else
    base="https://github.com/$REPO/releases/latest/download"
  fi
  archive="karvan_${OS}_${ARCH}.tar.gz"
  step "Downloading $archive"
  download "$base/$archive" "$TMP/$archive" || return 1
  download "$base/checksums.txt" "$TMP/checksums.txt" || return 1

  want="$(grep " $archive\$" "$TMP/checksums.txt" | cut -d' ' -f1)"
  got="$(sha256_of "$TMP/$archive")"
  if [ -z "$want" ]; then
    warn "checksums.txt has no entry for $archive"
    return 1
  fi
  if [ -z "$got" ]; then
    warn "no sha256sum or shasum to verify the download; refusing to install an unverified binary"
    return 1
  fi
  if [ "$want" != "$got" ]; then
    warn "checksum mismatch for $archive (download damaged or tampered with)"
    return 1
  fi
  step "Checksum OK"
  mkdir -p "$TMP/extract"
  tar -xzf "$TMP/$archive" -C "$TMP/extract" || return 1
  [ -f "$TMP/extract/karvan" ] || return 1
  place_binary "$TMP/extract/karvan"
}

place_binary() {
  mkdir -p "$INSTALL_DIR" || die "cannot create $INSTALL_DIR"
  # Write next to the target, then rename: a running karvan is never
  # replaced by a half-written file.
  cp "$1" "$INSTALL_DIR/.karvan.new"
  chmod 755 "$INSTALL_DIR/.karvan.new"
  if ! "$INSTALL_DIR/.karvan.new" version >/dev/null 2>&1; then
    rm -f "$INSTALL_DIR/.karvan.new"
    warn "the downloaded karvan does not run on this system"
    return 1
  fi
  mv -f "$INSTALL_DIR/.karvan.new" "$INSTALL_DIR/karvan"
  step "Installed $INSTALL_DIR/karvan"
}

# ---------------------------------------------------------- from source --

# go_ok GOBIN: true if that go is 1.$GO_MIN_MINOR or newer.
go_ok() {
  # GOTOOLCHAIN=local: an old go with automatic toolchain switching reports
  # the newer toolchain's version here, but the build below runs the old one.
  v="$(GOTOOLCHAIN=local "$1" env GOVERSION 2>/dev/null || true)"
  v="${v#go}"
  major="${v%%.*}"; rest="${v#*.}"; minor="${rest%%.*}"
  minor="${minor%%[!0-9]*}"
  [ -n "$major" ] && [ -n "$minor" ] || return 1
  [ "$major" -gt 1 ] || { [ "$major" -eq 1 ] && [ "$minor" -ge "$GO_MIN_MINOR" ]; }
}

find_go() {
  for g in "$(command -v go 2>/dev/null || true)" /usr/local/go/bin/go /snap/bin/go "$DATA_DIR/go/bin/go"; do
    if [ -n "$g" ] && [ -x "$g" ] && go_ok "$g"; then
      GO="$g"
      return 0
    fi
  done
  return 1
}

install_go() {
  step "Go 1.$GO_MIN_MINOR+ not found; installing Go for Karvan only (in $DATA_DIR/go, no sudo)"
  ver=""
  if json="$(fetch "https://go.dev/dl/?mode=json" 2>/dev/null)"; then
    ver="$(printf '%s' "$json" | grep -o '"version": *"go[0-9.]*"' | head -n1 | sed 's/.*"go\([0-9.]*\)"/\1/')"
  fi
  if [ -z "$ver" ]; then
    # go.dev unreachable: Go module proxies list Go releases too.
    for proxy in https://proxy.golang.org https://goproxy.cn https://goproxy.io; do
      list="$(fetch "$proxy/golang.org/toolchain/@v/list" 2>/dev/null || true)"
      ver="$(printf '%s\n' "$list" | sed -n "s/^v0\.0\.1-go\([0-9]*\.[0-9]*\.[0-9]*\)\.$OS-$ARCH\$/\1/p" |
        sort -t. -k1,1n -k2,2n -k3,3n | tail -n1)"
      [ -n "$ver" ] && break
    done
  fi
  [ -n "$ver" ] || ver="$GO_VERSION_FALLBACK"
  rm -rf "$DATA_DIR/go.new"
  mkdir -p "$DATA_DIR/go.new"

  file="go${ver}.${OS}-${ARCH}.tar.gz"
  ok=""
  # Several mirrors: go.dev is blocked or slow in some countries.
  for mirror in "https://go.dev/dl" "https://dl.google.com/go" "https://golang.google.cn/dl" "https://mirrors.aliyun.com/golang" "https://mirrors.ustc.edu.cn/golang"; do
    rm -f "$TMP/$file"
    if ! probe "$mirror/$file"; then
      say "    $mirror: not reachable from here, trying the next one"
      continue
    fi
    say "    from $mirror"
    if download "$mirror/$file" "$TMP/$file" && with_spinner "Unpacking Go" tar -xzf "$TMP/$file" -C "$DATA_DIR/go.new" --strip-components=1; then
      ok=1
      break
    fi
  done
  if [ -z "$ok" ]; then
    # Last resort: the same release as a Go module zip from a module proxy.
    zip="v0.0.1-go${ver}.${OS}-${ARCH}.zip"
    for proxy in https://proxy.golang.org https://goproxy.cn https://goproxy.io; do
      rm -f "$TMP/$zip"
      if ! probe "$proxy/golang.org/toolchain/@v/$zip"; then
        say "    $proxy: not reachable from here, trying the next one"
        continue
      fi
      say "    from $proxy (Go module mirror)"
      if download "$proxy/golang.org/toolchain/@v/$zip" "$TMP/$zip" && with_spinner "Unpacking Go" unzip_to "$TMP/$zip" "$TMP/gozip"; then
        src="$(find "$TMP/gozip" -maxdepth 2 -type d -name "toolchain@*" | head -n1)"
        if [ -n "$src" ] && [ -f "$src/bin/go" ]; then
          rm -rf "$DATA_DIR/go.new"
          mv "$src" "$DATA_DIR/go.new"
          # Module zips do not keep file modes.
          chmod -R a+x "$DATA_DIR/go.new/bin" "$DATA_DIR/go.new/pkg/tool" 2>/dev/null || true
          ok=1
          break
        fi
      fi
    done
  fi
  [ -n "$ok" ] || die "could not download Go $ver from any mirror. Install Go 1.$GO_MIN_MINOR+ yourself (e.g. sudo snap install go --classic) and run this again."
  rm -rf "$DATA_DIR/go"
  mv "$DATA_DIR/go.new" "$DATA_DIR/go"
  GO="$DATA_DIR/go/bin/go"
  go_ok "$GO" || die "the installed Go does not work"
  step "Go $ver ready"
}

# unzip_to ZIP DIR with whatever is available.
unzip_to() {
  rm -rf "$2"
  mkdir -p "$2"
  if command -v unzip >/dev/null 2>&1; then
    unzip -q "$1" -d "$2"
  elif command -v python3 >/dev/null 2>&1; then
    python3 -m zipfile -e "$1" "$2"
  elif command -v busybox >/dev/null 2>&1; then
    busybox unzip -q "$1" -d "$2"
  elif command -v bsdtar >/dev/null 2>&1; then
    bsdtar -xf "$1" -C "$2"
  else
    warn "need unzip (or python3) to unpack Go from a module mirror"
    return 1
  fi
}

install_from_source() {
  if ! find_go; then
    install_go
  fi
  step "Using $("$GO" version)"

  # The source as a tarball: no git needed.
  src="https://github.com/$REPO/archive/refs/heads/$REF.tar.gz"
  case "$REF" in v[0-9]*) src="https://github.com/$REPO/archive/refs/tags/$REF.tar.gz" ;; esac
  step "Downloading the source ($REF)"
  download "$src" "$TMP/src.tar.gz" || die "could not download $src"
  mkdir -p "$TMP/src"
  tar -xzf "$TMP/src.tar.gz" -C "$TMP/src" --strip-components=1 || die "could not unpack the source"

  # Module downloads break easily on some networks: several proxies, retries.
  export GOPROXY="${GOPROXY:-https://proxy.golang.org,https://goproxy.io,https://goproxy.cn,direct}"
  export GOFLAGS="-mod=readonly"
  export GOTOOLCHAIN=local
  n=0
  # shellcheck disable=SC2016 # expanded by the inner sh, on purpose
  until with_spinner "Downloading Go modules (first time can take a minute)" sh -c 'cd "$1" && "$2" mod download' _ "$TMP/src" "$GO"; do
    n=$((n + 1))
    # Only network trouble is worth retrying.
    if ! grep -qiE "EOF|timeout|connection|reset|refused|TLS|unreachable|no such host|i/o|temporary|50[0-9]" "$TMP/step.log"; then
      cat "$TMP/step.log" >&2
      die "could not download Go modules (see above)."
    fi
    if [ "$n" -ge 6 ]; then
      cat "$TMP/step.log" >&2
      die "could not download Go modules (network). Try again later, or set GOPROXY to a proxy that works for you."
    fi
    say "    network hiccup, retrying ($n/5)..."
    sleep $((n * 2))
  done
  rev="$REF"
  # shellcheck disable=SC2016 # expanded by the inner sh, on purpose
  if ! with_spinner "Building karvan" sh -c 'cd "$1" && CGO_ENABLED=0 "$2" build -trimpath -ldflags "-s -w -X main.version=source-$3" -o "$4" ./cmd/karvan' _ "$TMP/src" "$GO" "$rev" "$TMP/karvan"; then
    cat "$TMP/step.log" >&2
    die "the build failed (see above)"
  fi
  place_binary "$TMP/karvan" || die "the built karvan does not run"
}

# ------------------------------------------------------------------ PATH --

setup_path() {
  case ":$PATH:" in
    *":$INSTALL_DIR:"*) PATH_OK=1; return ;;
  esac
  PATH_OK=""
  if [ "${KARVAN_NO_MODIFY_PATH:-0}" = "1" ]; then
    return
  fi
  line="export PATH=\"$INSTALL_DIR:\$PATH\"  # added by the Karvan installer"
  changed=""
  for rc in "$HOME/.profile" "$HOME/.bashrc" "$HOME/.zshrc"; do
    case "$rc" in
      */.zshrc) [ -f "$rc" ] || command -v zsh >/dev/null 2>&1 || continue ;;
      */.bashrc) [ -f "$rc" ] || continue ;;
    esac
    if [ -f "$rc" ] && grep -qs "$INSTALL_DIR" "$rc"; then
      continue
    fi
    printf '\n%s\n' "$line" >>"$rc" && changed="$changed $rc"
  done
  if [ -n "$changed" ]; then
    step "Added $INSTALL_DIR to PATH in:$changed"
  fi
}

finish() {
  say ""
  say "${GREEN}${BOLD}Karvan is installed:${RESET} $("$INSTALL_DIR/karvan" version)"
  say ""
  if [ -z "$PATH_OK" ]; then
    say "${YELLOW}${BOLD}One more step:${RESET} this terminal does not know the ${BOLD}karvan${RESET} command yet."
    say "Copy and run this line (or simply open a new terminal):"
    say ""
    say "    ${BOLD}export PATH=\"$INSTALL_DIR:\$PATH\"${RESET}"
    say ""
    say "Then:"
  else
    say "Next:"
  fi
  say "  ${BOLD}karvan init${RESET}     set up this computer (asks a few questions)"
  say "  ${BOLD}karvan doctor${RESET}   check that everything works"
  say ""
  say "Guide: https://github.com/$REPO/blob/main/docs/guide.md"
}

main "$@"
