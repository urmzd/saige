#!/usr/bin/env bash
# Install the latest saige release.
#
#   curl -fsSL https://raw.githubusercontent.com/urmzd/saige/main/install.sh | bash
#
# Environment:
#   BIN          binary to install: saige (default) or saige-mcp
#   INSTALL_DIR  destination directory (default: $HOME/.local/bin)
#   DRY_RUN=1    print the asset that would be installed, then exit
set -euo pipefail

REPO="urmzd/saige"
BIN="${BIN:-saige}"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"

case "$BIN" in
  saige|saige-mcp) ;;
  *)
    echo "Error: BIN must be 'saige' or 'saige-mcp', got '$BIN'." >&2
    exit 1
    ;;
esac

# Detect OS
OS="$(uname -s)"
case "$OS" in
  Darwin) GOOS="darwin" ;;
  Linux)  GOOS="linux" ;;
  *)
    echo "Error: Unsupported OS '$OS'. This script supports macOS and Linux." >&2
    exit 1
    ;;
esac

# Detect architecture
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64)  GOARCH="amd64" ;;
  aarch64|arm64) GOARCH="arm64" ;;
  *)
    echo "Error: Unsupported architecture '$ARCH'. Release binaries exist for amd64 and arm64." >&2
    exit 1
    ;;
esac

# Asset names follow <bin>-<goos>-<goarch>, matching the release workflow and
# `saige update`.
ASSET_NAME="${BIN}-${GOOS}-${GOARCH}"

if [ "${DRY_RUN:-}" = "1" ]; then
  echo "$ASSET_NAME"
  exit 0
fi

# Check dependencies
if ! command -v curl >/dev/null 2>&1; then
  echo "Error: curl is required but not installed." >&2
  exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
  SHA256="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
  SHA256="shasum -a 256"
else
  echo "Error: sha256sum or shasum is required to verify the download." >&2
  exit 1
fi

# Fetch latest release tag
echo "Fetching latest release..."
RELEASE_JSON="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest")"
TAG="$(echo "$RELEASE_JSON" | grep '"tag_name"' | head -1 | sed 's/.*: *"//;s/".*//')"

if [ -z "$TAG" ]; then
  echo "Error: Could not determine latest release tag." >&2
  exit 1
fi

echo "Latest release: $TAG"

# Download and verify
TMPDIR_INSTALL="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_INSTALL"' EXIT

BASE_URL="https://github.com/$REPO/releases/download/$TAG"
echo "Downloading $BASE_URL/$ASSET_NAME..."
curl -fsSL -o "$TMPDIR_INSTALL/$ASSET_NAME" "$BASE_URL/$ASSET_NAME"
curl -fsSL -o "$TMPDIR_INSTALL/SHA256SUMS" "$BASE_URL/SHA256SUMS"

EXPECTED="$(awk -v name="$ASSET_NAME" '$2 == name || $2 == "*" name { print $1; exit }' "$TMPDIR_INSTALL/SHA256SUMS")"
if [ -z "$EXPECTED" ]; then
  echo "Error: $ASSET_NAME is not listed in SHA256SUMS for $TAG." >&2
  exit 1
fi
ACTUAL="$(cd "$TMPDIR_INSTALL" && $SHA256 "$ASSET_NAME" | awk '{ print $1 }')"
if [ "$EXPECTED" != "$ACTUAL" ]; then
  echo "Error: checksum mismatch for $ASSET_NAME (expected $EXPECTED, got $ACTUAL)." >&2
  exit 1
fi
echo "Checksum verified."

# Install
mkdir -p "$INSTALL_DIR"
cp "$TMPDIR_INSTALL/$ASSET_NAME" "$INSTALL_DIR/$BIN"
chmod +x "$INSTALL_DIR/$BIN"

echo "Installed $BIN ($TAG) to $INSTALL_DIR/$BIN"

# Check PATH
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *)
    echo ""
    echo "WARNING: $INSTALL_DIR is not in your PATH."
    echo "Add it by appending this line to your shell profile (~/.bashrc, ~/.zshrc, etc.):"
    echo ""
    echo "  export PATH=\"$INSTALL_DIR:\$PATH\""
    echo ""
    ;;
esac
