# ais

**Ask your terminal in plain English.** ais turns requests into shell commands
and runs them after you confirm. It also answers questions and explains
anything you pipe into it.

<p align="center">
  <img src="docs/demo.png" alt="ais finding what is listening on port 3000, then diagnosing a server log" width="860">
</p>

```sh
# Run things: ais proposes a command and runs it when you press y
ais what is listening on port 3000
ais which files in this repo change the most
ais undo my last commit but keep the changes

# Ask things: ais just answers
ais how do I split windows in nvim

# Pipe things: ais reads the input and answers your prompt
git diff | ais -p "any bugs in this change?"
npm test 2>&1 | ais -p "why is this failing?"
```

## Install

```sh
# Linux / macOS
curl -fsSL https://raw.githubusercontent.com/sqmch/ais/main/scripts/install.sh | sh
```

```powershell
# Windows
irm https://raw.githubusercontent.com/sqmch/ais/main/scripts/install.ps1 | iex
```

ais is a single binary. Run the installer again to update, or use
`uninstall.sh` / `uninstall.ps1` to remove it. It uses your
[Codex CLI](https://github.com/openai/codex) login, so run `codex login` once.

## How it works

- **Run or answer.** Requests about your machine ("is nginx running?") become
  commands. General questions get a direct answer.
- **You approve every command.** ais shows the command first and flags it if it
  changes state or is destructive. Press `y` to run, `e` to edit, anything else
  to skip.
- **Failed commands get a fix.** If a command fails, ais can send the error back
  to the model and propose a corrected one.
- **Runs in your shell.** PowerShell on Windows, `$SHELL` elsewhere, with your
  terminal attached, so password prompts like `sudo` work.

## Models

The default is `gpt-6-luna` with low reasoning. It is fast, cheap, and good
enough for one-line commands.

```sh
ais --list-models                     # models your account can use
ais --configure                       # save a default model and reasoning level
ais -m gpt-6-sol -r medium <request>  # override for one run
```

ais leaves your Codex setup alone. It passes its settings as per-run flags,
stays out of your Codex session history, and skips your plugins and MCP
servers, which also makes it faster.

## Flags

| Flag | Description |
| --- | --- |
| `-p, --prompt` | Prompt to go with piped input |
| `-a, --agent` | Always turn the request into a command |
| `-y, --yes` | Run commands without confirming |
| `-n, --dry-run` | Show the command without running it |
| `-m, --model` | Model for this run |
| `-r, --reasoning` | Reasoning effort for this run |
| `-b, --backend` | `auto`, `codex`, `api` (uses `OPENAI_API_KEY`), or `oss` (Ollama / LM Studio via `--local-provider`) |
| `--configure` | Pick and save defaults |
| `--list-models` | List available models |

## Build

```sh
go build -o ais ./cmd/ais
go test ./...
```
