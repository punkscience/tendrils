#!/bin/sh
# Tendrils installer for Linux and macOS. Builds the CLI from source.
#
#   curl -fsSL https://raw.githubusercontent.com/punkscience/tendrils/main/install.sh | sh
#
# Requirements: Go 1.26+ and git on PATH.
# Env: TENDRILS_BIN_DIR overrides the install directory (default ~/.local/bin).
set -eu

REPO="https://github.com/punkscience/tendrils.git"
BIN="tendrils"

info() { printf '\033[1;36m==>\033[0m %s\n' "$1"; }
die()  { printf '\033[1;31mERROR:\033[0m %s\n' "$1" >&2; exit 1; }

command -v go  >/dev/null 2>&1 || die "Go 1.26+ is required. Install from https://go.dev/dl/ and re-run."
command -v git >/dev/null 2>&1 || die "git is required."

info "Detected $(uname -s)/$(uname -m); $(go version | awk '{print $3}')"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

info "Fetching source"
git clone --depth 1 "$REPO" "$TMP/src" >/dev/null 2>&1 || die "git clone failed"

info "Building $BIN"
( cd "$TMP/src" && go build -o "$TMP/$BIN" ./cmd/tendrils ) || die "build failed"

DEST="${TENDRILS_BIN_DIR:-$HOME/.local/bin}"
mkdir -p "$DEST"
install -m 0755 "$TMP/$BIN" "$DEST/$BIN"
info "Installed $DEST/$BIN"

case ":$PATH:" in
  *":$DEST:"*) : ;;
  *) info "NOTE: $DEST is not on your PATH — add it:  export PATH=\"$DEST:\$PATH\"" ;;
esac

cat <<EOF

Tendrils installed. Next steps:
  1. $BIN keygen                            # create your master key — BACK UP the nsec
  2. $BIN enroll --key <nsec> --root <folder> \\
       --relay wss://<relay> --blossom http://<blossom>:8091
  3. $BIN daemon --interval 1m              # start syncing

You need a Nostr relay and a Blossom server (self-host the minimal one with
'go build ./cmd/blossomd'). Enroll every device with the SAME key to sync them.
See https://github.com/punkscience/tendrils for details.
EOF
