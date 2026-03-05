#!/usr/bin/env sh
set -eu

# Usage:
#   curl -fsSL https://raw.githubusercontent.com/sqmch/ais/main/scripts/uninstall.sh | sh
#
# Optional env:
#   AIS_INSTALL_DIR  Installation directory (default: ~/.local/bin, or /usr/local/bin for root)
#   AIS_BIN_NAME     Binary name (default: ais)

log() {
  printf "%s\n" "$*" >&2
}

main() {
  bin_name="${AIS_BIN_NAME:-ais}"

  if [ -n "${AIS_INSTALL_DIR:-}" ]; then
    install_dir="$AIS_INSTALL_DIR"
  elif [ "$(id -u)" = "0" ]; then
    install_dir="/usr/local/bin"
  else
    install_dir="$HOME/.local/bin"
  fi

  target="${install_dir}/${bin_name}"
  if [ -f "$target" ]; then
    rm -f "$target"
    log "Removed ${target}"
  else
    log "Nothing to remove at ${target}"
  fi
}

main "$@"
