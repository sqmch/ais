# ais

Tiny terminal AI helper for quick command-line questions and summaries.

It exists for the small stuff: quick shell questions, pasted output, diff summaries, and "what is this error?" moments without opening a browser or a full chat app.

<img width="1011" height="276" alt="image" src="https://github.com/user-attachments/assets/39638a46-058a-40f3-a420-3f9bbbf0e147" />


## Install

### Linux / macOS

```bash
curl -fsSL https://raw.githubusercontent.com/sqmch/ais/main/scripts/install.sh | sh
```

Update (re-run the installer) / Uninstall:

```bash
curl -fsSL https://raw.githubusercontent.com/sqmch/ais/main/scripts/install.sh | sh
curl -fsSL https://raw.githubusercontent.com/sqmch/ais/main/scripts/uninstall.sh | sh
```

### Windows (PowerShell)

```powershell
irm https://raw.githubusercontent.com/sqmch/ais/main/scripts/install.ps1 | iex
```

Update (re-run the installer) / Uninstall:

```powershell
irm https://raw.githubusercontent.com/sqmch/ais/main/scripts/install.ps1 | iex
irm https://raw.githubusercontent.com/sqmch/ais/main/scripts/uninstall.ps1 | iex
```

`ais` is a single self-contained executable — no Go, Node, or other runtime is
needed to run it. The installer drops `ais.exe` in `%LOCALAPPDATA%\Programs\ais`
and adds it to your user PATH; open a new terminal afterwards.

## Quick Use

```
ais how do I split windows in nvim?
git diff | ais -p "summarize risk and test impact"
cat file.txt | ais -p "summarize"
ls -la 2>&1 | ais -p "explain what's wrong"
curl -Ls https://example.com | ais -p "extract the key points"
```

## Configure

Set a remembered default backend/model:

```bash
ais --configure
```

The picker saves to `~/.config/ais/config.json`.

Useful flags:

- `-p, --prompt`
- `-b, --backend` = `auto|codex|api|oss`
- `-m, --model`
- `--local-provider` = `ollama|lmstudio`
- `--configure`
- `--list-models`
- `--stream` / `--no-stream`
- `--version`

Saved defaults are used automatically. Flags and env vars still override them for one-off runs.

Quick examples:

```bash
ais --configure
ais --backend codex --list-models
ais --backend oss --local-provider ollama --model qwen2.5-coder:7b "explain this error"
```

## Backends

- `codex`: uses your local `codex login`
- `api`: uses `OPENAI_API_KEY`
- `oss`: uses `codex --oss` with Ollama or LM Studio
- `auto`: prefers logged-in Codex, then API

First-time Codex setup:

```bash
codex login
```

## Build

```bash
go build -o ais ./cmd/ais
./ais --help
```

On Windows (PowerShell):

```powershell
go build -o ais.exe ./cmd/ais
.\ais.exe --help
```

## Install Notes

The installer:

- downloads the latest GitHub release by default
- verifies SHA256 checksums
- installs to `~/.local/bin` by default, or `/usr/local/bin` as root (Linux/macOS),
  or `%LOCALAPPDATA%\Programs\ais` on Windows

Optional env vars (both installers):

- `AIS_VERSION`
- `AIS_INSTALL_DIR`
- `AIS_BIN_NAME`
