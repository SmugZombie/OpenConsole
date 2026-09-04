#!/bin/sh
# OpenConsole client installer.
#
#   curl -fsSL https://openconsole.dev/install.sh | sh
#
# Installs the `openconsole` client, which shares a terminal or joins one.
# It does not install or configure a relay.
#
# You do not need this to *join* a shared terminal: a browser works with
# nothing installed, and so does `ssh <session>@<relay>`. This is for sharing
# your own terminal, or for joining from a terminal.

set -eu

REPO="SmugZombie/OpenConsole"
BIN="openconsole"

# Where to install. A directory the user owns is preferred over /usr/local/bin
# so the common case needs no sudo.
PREFIX="${OPENCONSOLE_PREFIX:-}"
VERSION="${OPENCONSOLE_VERSION:-latest}"

# Where releases are fetched from. Overridable so this can be pointed at a
# mirror on a network that cannot reach GitHub — and so the script itself can
# be tested against a local server rather than only in production.
BASE_URL="${OPENCONSOLE_BASE_URL:-https://github.com/$REPO/releases}"

say()  { printf '%s\n' "$*"; }
warn() { printf '%s\n' "$*" >&2; }
die()  { printf 'install: %s\n' "$*" >&2; exit 1; }

need() {
    command -v "$1" >/dev/null 2>&1 || die "this installer needs $1"
}

detect_platform() {
    os=$(uname -s | tr '[:upper:]' '[:lower:]')
    arch=$(uname -m)

    case "$os" in
        linux|darwin) ;;
        *) die "no prebuilt binary for $os. Build from source: https://github.com/$REPO" ;;
    esac

    case "$arch" in
        x86_64|amd64) arch=amd64 ;;
        aarch64|arm64) arch=arm64 ;;
        *) die "no prebuilt binary for $arch. Build from source: https://github.com/$REPO" ;;
    esac

    printf '%s_%s' "$os" "$arch"
}

# choose_prefix picks somewhere writable, preferring a per-user directory so
# the common path does not need root.
choose_prefix() {
    if [ -n "$PREFIX" ]; then
        printf '%s' "$PREFIX"
        return
    fi
    for candidate in "$HOME/.local/bin" "$HOME/bin"; do
        if [ -d "$candidate" ] && [ -w "$candidate" ]; then
            printf '%s' "$candidate"
            return
        fi
    done
    if [ -w /usr/local/bin ]; then
        printf '%s' /usr/local/bin
        return
    fi
    # Nothing existing is writable: make the conventional user directory.
    printf '%s' "$HOME/.local/bin"
}

# fallback_to_source builds with Go when no release matches, so the installer
# is still useful on a platform we do not ship binaries for.
#
# The exit status of `go install` is passed on. Returning success regardless —
# which this did — made the installer announce a binary it had not produced,
# and `set -e` does not catch it because errexit is suspended inside a function
# called as an `if` condition.
fallback_to_source() {
    command -v go >/dev/null 2>&1 || return 1
    warn "No release binary available; building from source with Go…"
    GOBIN="$1" go install "github.com/$REPO/cmd/$BIN@latest" || return 1
    return 0
}

# verify_installed refuses to report success without a binary to show for it.
# Every path to "Installed" goes through here.
verify_installed() {
    [ -x "$1/$BIN" ] || die "installation did not produce $1/$BIN"
}

main() {
    need uname
    platform=$(detect_platform)
    prefix=$(choose_prefix)
    mkdir -p "$prefix"

    if [ "$VERSION" = "latest" ]; then
        url="$BASE_URL/latest/download/${BIN}_${platform}.tar.gz"
    else
        url="$BASE_URL/download/${VERSION}/${BIN}_${platform}.tar.gz"
    fi

    tmp=$(mktemp -d)
    # shellcheck disable=SC2064
    trap "rm -rf '$tmp'" EXIT INT TERM

    say "Downloading $BIN ($platform)…"
    if command -v curl >/dev/null 2>&1; then
        fetch="curl -fsSL -o $tmp/$BIN.tar.gz"
    elif command -v wget >/dev/null 2>&1; then
        fetch="wget -qO $tmp/$BIN.tar.gz"
    else
        die "this installer needs curl or wget"
    fi

    if ! $fetch "$url" 2>/dev/null; then
        warn "Could not download $url"
        if fallback_to_source "$prefix"; then
            verify_installed "$prefix"
            say "Installed $prefix/$BIN"
            "$prefix/$BIN" version 2>/dev/null || true
            check_path "$prefix"
            return
        fi
        die "no release binary for this platform, and building from source did not work.

This usually means one of:
  * no release has been published yet;
  * the repository is private, so neither the release download nor
    \`go install\` can reach it without credentials;
  * this machine cannot reach $BASE_URL.

Ask whoever runs the relay for a binary, or build one from a checkout:
  go build -o $BIN ./cmd/$BIN"
    fi

    tar -xzf "$tmp/$BIN.tar.gz" -C "$tmp"
    [ -f "$tmp/$BIN" ] || die "the downloaded archive did not contain $BIN"

    chmod +x "$tmp/$BIN"
    if ! mv "$tmp/$BIN" "$prefix/$BIN" 2>/dev/null; then
        die "could not write to $prefix. Set OPENCONSOLE_PREFIX to somewhere you can write."
    fi

    verify_installed "$prefix"
    say "Installed $prefix/$BIN"
    "$prefix/$BIN" version 2>/dev/null || true
    check_path "$prefix"
}

# check_path warns when the install directory is not on PATH, which is the
# single most common reason a fresh install "does not work".
check_path() {
    case ":$PATH:" in
        *":$1:"*) ;;
        *)
            warn ""
            warn "$1 is not on your PATH. Add it:"
            warn "    export PATH=\"$1:\$PATH\""
            ;;
    esac
}

main "$@"
