#!/usr/bin/env sh
#
# install.sh — Quick installer for the enju binary on Linux / macOS.
#
# Typical use:
#
#   curl -fsSL https://raw.githubusercontent.com/tamerh/enju/main/install.sh | sh
#
# Cautious use (recommended for anyone reading this who didn't write it):
#
#   curl -fsSL https://raw.githubusercontent.com/tamerh/enju/main/install.sh -o install.sh
#   less install.sh        # read it
#   sh install.sh          # then run it
#
# Installs to ~/.local/bin/enju. No sudo required. No system files
# touched. Pass --version vX.Y.Z to pin a specific release;
# omitted means "the latest tag on GitHub".
#
# Windows users: download the .zip directly from
# https://github.com/tamerh/enju/releases and extract enju.exe to
# a directory on PATH.

set -eu

# ---------------------------------------------------------------
# Config
# ---------------------------------------------------------------

REPO="tamerh/enju"
INSTALL_DIR="${HOME}/.local/bin"
BIN_NAME="enju"

# Pinned fallback version used only when the GitHub API call fails
# (rate-limit, offline, etc.). Bump this when you cut a new release
# so curl|sh users without API access still get something recent.
FALLBACK_VERSION="v0.1.0"

# ---------------------------------------------------------------
# Parse flags
# ---------------------------------------------------------------

VERSION=""
while [ $# -gt 0 ]; do
    case "$1" in
        --version)
            VERSION="${2:-}"
            shift 2
            ;;
        --version=*)
            VERSION="${1#*=}"
            shift
            ;;
        --help|-h)
            sed -n '2,20p' "$0"
            exit 0
            ;;
        *)
            printf 'Unknown flag: %s\n' "$1" >&2
            printf 'Run with --help for usage.\n' >&2
            exit 1
            ;;
    esac
done

# ---------------------------------------------------------------
# Detect OS / arch
# ---------------------------------------------------------------

detect_platform() {
    uname_s=$(uname -s 2>/dev/null || echo unknown)
    uname_m=$(uname -m 2>/dev/null || echo unknown)

    case "$uname_s" in
        Linux)  OS="linux" ;;
        Darwin) OS="darwin" ;;
        MINGW*|MSYS*|CYGWIN*)
            printf 'This installer is for Linux and macOS only.\n' >&2
            printf 'On Windows, download the .zip directly from:\n' >&2
            printf '  https://github.com/%s/releases\n' "$REPO" >&2
            exit 1
            ;;
        *)
            printf 'Unsupported OS: %s\n' "$uname_s" >&2
            exit 1
            ;;
    esac

    case "$uname_m" in
        x86_64|amd64)  ARCH="amd64" ;;
        aarch64|arm64) ARCH="arm64" ;;
        *)
            printf 'Unsupported architecture: %s\n' "$uname_m" >&2
            printf 'Pre-built binaries exist for amd64 and arm64.\n' >&2
            exit 1
            ;;
    esac
}

# ---------------------------------------------------------------
# Download helpers — try curl, fall back to wget
# ---------------------------------------------------------------

fetch_to_stdout() {
    # $1 = URL
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$1"
    elif command -v wget >/dev/null 2>&1; then
        wget -qO- "$1"
    else
        printf 'Neither curl nor wget is available.\n' >&2
        exit 1
    fi
}

fetch_to_file() {
    # $1 = URL, $2 = output path
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$1" -o "$2"
    elif command -v wget >/dev/null 2>&1; then
        wget -q "$1" -O "$2"
    else
        printf 'Neither curl nor wget is available.\n' >&2
        exit 1
    fi
}

# ---------------------------------------------------------------
# Resolve target version
# ---------------------------------------------------------------

resolve_version() {
    if [ -n "$VERSION" ]; then
        # User pinned. Normalize to v-prefixed.
        case "$VERSION" in
            v*) : ;;
            *)  VERSION="v${VERSION}" ;;
        esac
        return
    fi

    # Try the GitHub API for the latest release tag. The 60-req/hr
    # anon limit is the only realistic failure mode here; on miss
    # we fall back to FALLBACK_VERSION so curl|sh still ends in a
    # working binary.
    api_url="https://api.github.com/repos/${REPO}/releases/latest"
    json=$(fetch_to_stdout "$api_url" 2>/dev/null || true)
    if [ -n "$json" ]; then
        # Extract "tag_name": "vX.Y.Z" — POSIX sed only.
        tag=$(printf '%s\n' "$json" \
            | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
            | head -1)
        if [ -n "$tag" ]; then
            VERSION="$tag"
            return
        fi
    fi

    printf 'Could not reach GitHub API; falling back to %s.\n' "$FALLBACK_VERSION"
    VERSION="$FALLBACK_VERSION"
}

# ---------------------------------------------------------------
# SHA256 verify — uses sha256sum on Linux, shasum on macOS
# ---------------------------------------------------------------

sha256_of() {
    # $1 = file path; prints the hex digest only (no filename)
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
    else
        printf 'No sha256sum or shasum available — cannot verify download.\n' >&2
        exit 1
    fi
}

# ---------------------------------------------------------------
# Main
# ---------------------------------------------------------------

detect_platform
resolve_version

ARCHIVE="enju-${OS}-${ARCH}.tar.gz"
RELEASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"
ARCHIVE_URL="${RELEASE_URL}/${ARCHIVE}"
SUMS_URL="${RELEASE_URL}/SHA256SUMS"

printf 'Installing enju %s (%s/%s) to %s\n' "$VERSION" "$OS" "$ARCH" "$INSTALL_DIR"

# Tempdir for the download. Trap cleans it up on any exit path.
TMPDIR_INST=$(mktemp -d 2>/dev/null || mktemp -d -t enju-install)
trap 'rm -rf "$TMPDIR_INST"' EXIT

# Pull archive + SHA256SUMS.
printf '  Downloading %s ...\n' "$ARCHIVE"
fetch_to_file "$ARCHIVE_URL" "${TMPDIR_INST}/${ARCHIVE}"

# Checksum verification — best-effort. If SHA256SUMS isn't published
# yet (older release), warn but proceed. New releases (post the
# SHA256SUMS commit) will always have it.
printf '  Verifying checksum ...\n'
if fetch_to_file "$SUMS_URL" "${TMPDIR_INST}/SHA256SUMS" 2>/dev/null; then
    expected=$(grep "  ${ARCHIVE}\$" "${TMPDIR_INST}/SHA256SUMS" | awk '{print $1}')
    if [ -z "$expected" ]; then
        printf '  SHA256SUMS does not list %s — refusing to install.\n' "$ARCHIVE" >&2
        exit 1
    fi
    actual=$(sha256_of "${TMPDIR_INST}/${ARCHIVE}")
    if [ "$expected" != "$actual" ]; then
        printf '  Checksum mismatch!\n' >&2
        printf '    expected: %s\n' "$expected" >&2
        printf '    actual:   %s\n' "$actual" >&2
        exit 1
    fi
    printf '  ✓ checksum OK\n'
else
    printf '  ⚠ SHA256SUMS not published for this release — skipping verification.\n'
fi

# Extract.
printf '  Extracting ...\n'
tar -xzf "${TMPDIR_INST}/${ARCHIVE}" -C "$TMPDIR_INST"

# The archive layout is enju-<os>-<arch>/enju.
EXTRACTED_BIN="${TMPDIR_INST}/enju-${OS}-${ARCH}/${BIN_NAME}"
if [ ! -f "$EXTRACTED_BIN" ]; then
    printf 'Extracted layout unexpected — could not find %s\n' "$EXTRACTED_BIN" >&2
    exit 1
fi

# Install. mkdir -p is idempotent; the install dir often exists.
mkdir -p "$INSTALL_DIR"
install -m 0755 "$EXTRACTED_BIN" "${INSTALL_DIR}/${BIN_NAME}"

# Hardlink enju → enju-coord so `pkill -f enju-coord` targets only
# the coordinator process. Build.sh does the same locally; mirror
# that here so installed setups behave the same way operators see
# in dev. ln -f overwrites any stale link from a previous install.
ln -f "${INSTALL_DIR}/${BIN_NAME}" "${INSTALL_DIR}/${BIN_NAME}-coord"

# macOS quarantine attribute — fresh downloads carry it; first run
# would otherwise hit Gatekeeper. Strip silently (xattr exit is
# non-zero when the attr isn't set, which is fine).
if [ "$OS" = "darwin" ] && command -v xattr >/dev/null 2>&1; then
    xattr -d com.apple.quarantine "${INSTALL_DIR}/${BIN_NAME}" 2>/dev/null || true
    xattr -d com.apple.quarantine "${INSTALL_DIR}/${BIN_NAME}-coord" 2>/dev/null || true
fi

printf '\n'
printf '✓ Installed: %s\n' "${INSTALL_DIR}/${BIN_NAME}"

# PATH check — if ~/.local/bin isn't already on PATH, print the
# right export line for the user's shell. case-match on $SHELL so
# zsh users get ~/.zshrc, fish users get fish syntax, etc.
case ":${PATH}:" in
    *":${INSTALL_DIR}:"*)
        ;;
    *)
        printf '\n'
        printf '  %s is not on your PATH. Add it with:\n' "$INSTALL_DIR"
        case "${SHELL:-}" in
            */zsh)
                printf '    echo '\''export PATH="$HOME/.local/bin:$PATH"'\'' >> ~/.zshrc\n'
                ;;
            */fish)
                printf '    fish_add_path ~/.local/bin\n'
                ;;
            *)
                printf '    echo '\''export PATH="$HOME/.local/bin:$PATH"'\'' >> ~/.bashrc\n'
                ;;
        esac
        printf '  Then restart your shell or `source` the rc file.\n'
        ;;
esac

# Confirm by running the installed binary. Falls back to a generic
# "installed" line if version exits non-zero for any reason.
printf '\n'
if "${INSTALL_DIR}/${BIN_NAME}" version 2>/dev/null; then
    :
else
    printf 'enju installed (version output unavailable on this build).\n'
fi
