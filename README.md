# ais

Tiny terminal AI helper. Ask it questions, pipe it text to summarize, or tell it
to do something and it runs the command for you — all without leaving the shell.

<img width="1011" height="276" alt="image" src="https://github.com/user-attachments/assets/39638a46-058a-40f3-a420-3f9bbbf0e147" />

## Install

**Linux / macOS**

```bash
curl -fsSL https://raw.githubusercontent.com/sqmch/ais/main/scripts/install.sh | sh
```

**Windows (PowerShell)**

```powershell
irm https://raw.githubusercontent.com/sqmch/ais/main/scripts/install.ps1 | iex
```

Re-run the installer to update. To uninstall, run the same URL with
`uninstall.sh` / `uninstall.ps1`.

`ais` is a single self-contained binary — no runtime needed. Open a new terminal
after installing so the PATH change takes effect.

## Use

**Ask a question** — get a direct answer:

```
ais how do I split windows in nvim?
ais what does chmod 755 mean?
```

**Pipe text** — summarize or explain piped input with `-p`:

```
git diff | ais -p "summarize risk and test impact"
ls -la 2>&1 | ais -p "explain what's wrong"
```

**Run commands** — describe a task and ais turns it into a shell command, shows
it, and runs it after you confirm:

```
ais what is listening on port 3000
ais kill the process on port 8080
ais delete all .tmp files in this folder
```

```
$ ais kill the process on port 8080
Stops the process currently listening on TCP port 8080.

PowerShell will run:
  Stop-Process -Id (Get-NetTCPConnection -LocalPort 8080).OwningProcess -Force
Run this command? [y/N]
```

ais decides whether a request is a question or an action automatically. Force the
action path with `-a`, skip the confirmation prompt with `-y`, or force a plain
answer by phrasing it as a question. Commands run in your shell (PowerShell on
Windows, `$SHELL`/`sh` elsewhere). This feature uses the `codex` backend.

## Configure

```bash
ais --configure
```

An interactive picker (arrow keys / Enter) sets your default backend, model, and
reasoning effort, saved to `~/.config/ais/config.json`. Flags and env vars
override saved defaults per run.

To keep costs down, ask questions with whatever model you pick, but ais always
plans **run-command** actions with a strong model (gpt-5.5) for reliability —
unless you pin one with `-m`.

## Flags

| Flag | Meaning |
| --- | --- |
| `-p, --prompt` | Explicit prompt (useful with piped stdin) |
| `-a, --agent` | Force command-running mode |
| `-y, --yes` | Run proposed commands without confirming |
| `-b, --backend` | `auto` \| `codex` \| `api` \| `oss` |
| `-m, --model` | Model override |
| `-r, --reasoning` | `minimal` \| `low` \| `medium` \| `high` |
| `--local-provider` | `ollama` \| `lmstudio` (for `oss`) |
| `--configure` | Open the settings picker |
| `--list-models` | Show model choices for the backend |
| `--stream` / `--no-stream` | Toggle token streaming |
| `--version` | Print version |

## Backends

- `codex` — your local `codex login` (run `codex login` once to set up)
- `api` — `OPENAI_API_KEY`
- `oss` — `codex --oss` with Ollama or LM Studio
- `auto` — prefers logged-in Codex, then API

```bash
ais --backend oss --local-provider ollama --model qwen2.5-coder:7b "explain this error"
```

## Build

```bash
go build -o ais ./cmd/ais     # ais.exe on Windows
./ais --help
```

The installer downloads the latest GitHub release, verifies SHA256 checksums, and
installs to `~/.local/bin` (Linux/macOS) or `%LOCALAPPDATA%\Programs\ais`
(Windows). Override with `AIS_VERSION`, `AIS_INSTALL_DIR`, or `AIS_BIN_NAME`.
