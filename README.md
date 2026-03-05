# ais

Fast terminal AI search for command-line workflows.

Install (Linux/macOS):

```bash
curl -fsSL https://raw.githubusercontent.com/sqmch/ais/main/scripts/install.sh | sh
```

Uninstall:

```bash
curl -fsSL https://raw.githubusercontent.com/sqmch/ais/main/scripts/uninstall.sh | sh
```

## What it does

The main idea is to avoid needing to switch over to a browser or a heavier CLI tool to prompt AI with some questions or data processing needs.

- Ask directly from terminal: `ais explain awk vs sed`
- Pipe command output: `ls -la 2>&1 | ais -p "explain issues"`
- Pipe files/web content: `cat file.txt | ais -p "summarize"`
- Uses existing `codex login` (default path) or API key fallback.

## Auth

Backends:

- `codex` (recommended for least friction): uses your local Codex login session
- `api`: uses `OPENAI_API_KEY`
- `auto` (default): prefers logged-in codex, then API

First-time codex setup:

```bash
codex login
```

## Usage

```bash
ais [options] [prompt words...]
```

Common examples:

```bash
ais how do I split windows in nvim?

cat myfile.txt | ais -p "summarize this"

curl -Ls https://example.com | ais -p "extract key points"

git diff | ais -p "summarize risk and test impact"
```

Useful options:

- `-p, --prompt`: explicit instruction text (great with pipes)
- `-b, --backend`: `auto`, `codex`, `api`
- `--stream` / `--no-stream`
- `--max-input-chars`
- `--truncate head|tail|middle`
- `--show-input-stats`
- `--no-spinner`
- `--render auto|ansi|raw`

## Install details

The installer script:

- detects OS/arch
- downloads matching release artifact from GitHub Releases
- verifies SHA256 checksums
- installs `ais` into:
  - `~/.local/bin` (default user install)
  - `/usr/local/bin` (if run as root)

Optional install env vars:

- `AIS_VERSION` (example: `v0.1.0`)
- `AIS_INSTALL_DIR`
- `AIS_BIN_NAME`

## Build from source

```bash
go build -o ais ./cmd/ais
./ais --help
```
