#!/usr/bin/env bash
# install.sh — build and install whisper-stt
#
# Usage:
#   ./install.sh           — install
#   ./install.sh uninstall — remove everything installed by this script

set -euo pipefail

BINARY="whisper-stt"
BIN_DIR="${HOME}/.local/bin"
CONFIG_DIR="${XDG_CONFIG_HOME:-${HOME}/.config}/whisper-stt"
SERVICE_DIR="${XDG_CONFIG_HOME:-${HOME}/.config}/systemd/user"
SERVICE_FILE="whisper-stt.service"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

info()  { echo "  [+] $*"; }
warn()  { echo "  [!] $*" >&2; }
die()   { echo "  [✗] $*" >&2; exit 1; }

require_cmd() {
    command -v "$1" &>/dev/null || die "'$1' not found — $2"
}

# ---------------------------------------------------------------------------
# Uninstall
# ---------------------------------------------------------------------------

if [[ "${1:-}" == "uninstall" ]]; then
    echo "Uninstalling $BINARY…"

    if systemctl --user is-active --quiet "$SERVICE_FILE" 2>/dev/null; then
        systemctl --user stop "$SERVICE_FILE"
        info "Stopped service"
    fi
    if systemctl --user is-enabled --quiet "$SERVICE_FILE" 2>/dev/null; then
        systemctl --user disable "$SERVICE_FILE"
        info "Disabled service"
    fi

    rm -f "${SERVICE_DIR}/${SERVICE_FILE}" && info "Removed ${SERVICE_DIR}/${SERVICE_FILE}"
    rm -f "${BIN_DIR}/${BINARY}"           && info "Removed ${BIN_DIR}/${BINARY}"

    systemctl --user daemon-reload 2>/dev/null || true

    echo ""
    warn "Config at ${CONFIG_DIR}/ was NOT removed — delete it manually if you wish."
    echo "Done."
    exit 0
fi

# ---------------------------------------------------------------------------
# Install system dependencies
# ---------------------------------------------------------------------------

install_dependencies() {
    info "Installing system dependencies…"
    sudo apt install -y \
        libx11-xcb1-dev \
        libxkbcommon-dev \
        libxkbcommon-x11-dev \
        libxext-dev \
        libxtst-dev \
        portaudio19-dev \
        xdotool \
        ffmpeg
}

# ---------------------------------------------------------------------------
# Install
# ---------------------------------------------------------------------------

echo "Installing $BINARY…"
echo ""

# Check prerequisites
require_cmd go "install Go from https://go.dev/dl/"

# Install system dependencies (best-effort — may require sudo password)
install_dependencies || true

# Must run from the project root (where go.mod lives)
[[ -f go.mod ]] || die "Run this script from the project root (where go.mod is)"
[[ -f "$SERVICE_FILE" ]] || die "$SERVICE_FILE not found in current directory"
[[ -f config.example.toml ]] || die "config.example.toml not found in current directory"

# Build
info "Building binary…"
go build -o "$BINARY" .

# Install binary
mkdir -p "$BIN_DIR"
install -m 755 "$BINARY" "${BIN_DIR}/${BINARY}"
rm -f "$BINARY"   # remove local build artifact
info "Binary installed to ${BIN_DIR}/${BINARY}"

# Ensure ~/.local/bin is on PATH
if [[ ":${PATH}:" != *":${BIN_DIR}:"* ]]; then
    warn "${BIN_DIR} is not in \$PATH."
    warn "Add the following to your shell profile (~/.bashrc, ~/.zshrc, etc.):"
    warn "  export PATH=\"\${HOME}/.local/bin:\${PATH}\""
fi

# Config
mkdir -p "$CONFIG_DIR"
if [[ -f "${CONFIG_DIR}/config.toml" ]]; then
    info "Config already exists at ${CONFIG_DIR}/config.toml — not overwritten"
else
    cp config.example.toml "${CONFIG_DIR}/config.toml"
    info "Default config written to ${CONFIG_DIR}/config.toml"
    echo ""
    echo "  *** Edit ${CONFIG_DIR}/config.toml before starting the service. ***"
    echo ""
fi

# Systemd user service
mkdir -p "$SERVICE_DIR"
cp "$SERVICE_FILE" "${SERVICE_DIR}/${SERVICE_FILE}"
info "Service file installed to ${SERVICE_DIR}/${SERVICE_FILE}"

# Reload systemd
if command -v systemctl &>/dev/null; then
    systemctl --user daemon-reload
    info "systemd user daemon reloaded"
fi

echo ""
echo "Installation complete."
echo ""
echo "Next steps:"
echo "  1. Edit your config:    \$EDITOR ${CONFIG_DIR}/config.toml"
echo "  2. Enable the service:  systemctl --user enable --now ${SERVICE_FILE}"
echo "  3. Check its status:    systemctl --user status  ${SERVICE_FILE}"
echo "  4. View logs:           journalctl --user -u ${SERVICE_FILE} -f"
