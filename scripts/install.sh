#!/usr/bin/env sh
set -eu

# Usage:
#   curl -fsSL https://raw.githubusercontent.com/sqmch/ais/main/scripts/install.sh | sh
#
# Optional env:
#   AIS_REPO         GitHub repo in "owner/name" format (default: sqmch/ais)
#   AIS_VERSION      Release tag (e.g. v0.2.0). If unset, uses latest release.
#   AIS_INSTALL_DIR  Installation directory (default: ~/.local/bin, or /usr/local/bin for root)
#   AIS_BIN_NAME     Installed binary name (default: ais)

AIS_DEFAULT_REPO="sqmch/ais"

log() {
  printf "%s\n" "$*" >&2
}

fail() {
  log "Error: $*"
  exit 1
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "Missing required command: $1"
}

detect_os() {
  case "$(uname -s | tr '[:upper:]' '[:lower:]')" in
    linux) echo "linux" ;;
    darwin) echo "darwin" ;;
    *) fail "Unsupported OS: $(uname -s). Supported: Linux, macOS." ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    *) fail "Unsupported architecture: $(uname -m). Supported: amd64, arm64." ;;
  esac
}

sha_cmd() {
  if command -v sha256sum >/dev/null 2>&1; then
    echo "sha256sum"
    return
  fi
  if command -v shasum >/dev/null 2>&1; then
    echo "shasum -a 256"
    return
  fi
  fail "No SHA256 tool found (need sha256sum or shasum)."
}

verify_checksum() {
  checksum_file="$1"
  artifact="$2"
  artifact_path="$3"

  expected="$(grep "  ${artifact}$" "$checksum_file" | awk '{print $1}' | head -n1 || true)"
  [ -n "$expected" ] || fail "Checksum entry for ${artifact} not found."

  sha="$(sha_cmd)"
  actual="$(sh -c "$sha \"$artifact_path\" | awk '{print \$1}'")"
  [ "$expected" = "$actual" ] || fail "Checksum mismatch for ${artifact}."
}

main() {
  need_cmd uname
  need_cmd mktemp
  need_cmd tar
  need_cmd curl

  repo="${AIS_REPO:-$AIS_DEFAULT_REPO}"
  [ -n "$repo" ] || fail "AIS_REPO cannot be empty."

  os="$(detect_os)"
  arch="$(detect_arch)"
  version="${AIS_VERSION:-latest}"
  bin_name="${AIS_BIN_NAME:-ais}"

  if [ -n "${AIS_INSTALL_DIR:-}" ]; then
    install_dir="$AIS_INSTALL_DIR"
  elif [ "$(id -u)" = "0" ]; then
    install_dir="/usr/local/bin"
  else
    install_dir="$HOME/.local/bin"
  fi

  artifact="ais_${os}_${arch}.tar.gz"
  checksums="checksums.txt"

  if [ "$version" = "latest" ]; then
    base_url="https://github.com/${repo}/releases/latest/download"
  else
    base_url="https://github.com/${repo}/releases/download/${version}"
  fi

  workdir="$(mktemp -d)"
  trap 'rm -rf "$workdir"' EXIT INT TERM

  artifact_path="${workdir}/${artifact}"
  checksums_path="${workdir}/${checksums}"

  log "Downloading ${artifact} from ${base_url}"
  curl -fsSL "${base_url}/${artifact}" -o "$artifact_path" || fail "Failed to download ${artifact}."
  curl -fsSL "${base_url}/${checksums}" -o "$checksums_path" || fail "Failed to download checksums.txt."

  verify_checksum "$checksums_path" "$artifact" "$artifact_path"
  log "Checksum verified."

  mkdir -p "$install_dir" || fail "Cannot create install directory: ${install_dir}"
  tar -xzf "$artifact_path" -C "$workdir" || fail "Failed to extract archive."

  extracted_bin="${workdir}/ais"
  [ -f "$extracted_bin" ] || fail "Archive did not contain expected binary 'ais'."
  chmod +x "$extracted_bin"

  target="${install_dir}/${bin_name}"
  cp "$extracted_bin" "$target" || fail "Failed to install binary to ${target}."

  log "Installed ${bin_name} to ${target}"
  log "Run: ${bin_name} --help"
  case ":$PATH:" in
    *":${install_dir}:"*) ;;
    *)
      log "Note: ${install_dir} is not on PATH in this shell."
      log "Add it, e.g.: export PATH=\"${install_dir}:\$PATH\""
      ;;
  esac
}

main "$@"
