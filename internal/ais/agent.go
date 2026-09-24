package ais

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

// maxFixAttempts caps how many times ais will re-plan after a failed command.
const maxFixAttempts = 2

// agentPlan is the structured response Codex returns, constrained by
// agentSchemaJSON. The model itself decides whether the request is something
// to run on this machine (kind "command") or a question to answer (kind
// "answer"), in the same call — so routing costs no extra latency.
//
// A command is a SINGLE self-contained string: this structurally prevents the
// model from splitting a task into steps where a later step has a placeholder
// (e.g. "<pid>") that depends on an earlier step's output.
type agentPlan struct {
	Kind        string `json:"kind"`
	Command     string `json:"command"`
	Explanation string `json:"explanation"`
	Danger      string `json:"danger"`
	Answer      string `json:"answer"`
}

// agentSchemaJSON uses strict structured-output rules (every property required,
// no extras) so parsing is reliable; unused fields come back as "".
const agentSchemaJSON = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["kind", "command", "explanation", "danger", "answer"],
  "properties": {
    "kind": { "type": "string", "enum": ["command", "answer"] },
    "command": { "type": "string" },
    "explanation": { "type": "string" },
    "danger": { "type": "string", "enum": ["low", "medium", "high"] },
    "answer": { "type": "string" }
  }
}`

// agentInstructions replaces Codex's own coding-agent system prompt for ais
// calls (see codexExecArgs). Keeping it short is most of the latency win.
const agentInstructions = "You are ais, a terminal assistant. You never execute anything yourself: you only return JSON matching the provided schema, and the ais program acts on it. Follow the request's rules exactly."

// planRequest is one planning call: the user's task plus, when re-planning, the
// command that just failed.
type planRequest struct {
	task         string
	forceCommand bool
	strict       bool
	failed       *failedRun
}

type failedRun struct {
	command  string
	exitCode int
	stderr   string
}

// runAgent handles an interactive request on the codex backend: one model call
// decides whether to answer it or to propose a command, which ais runs in the
// user's shell after confirmation. If the command fails, ais offers to ask for
// a corrected one (up to maxFixAttempts).
func runAgent(cfg Config, task string, in io.Reader, out io.Writer, errOut io.Writer) int {
	task = strings.TrimSpace(task)
	if task == "" {
		fmt.Fprintln(errOut, "No task provided. Example: ais kill the process on port 8080")
		return 2
	}

	codexBin := findCodexBinary()
	if codexBin == "" {
		fmt.Fprintln(errOut, "This needs the Codex CLI. Install it, then run `codex login`.")
		return 2
	}
	if !isCodexLoggedIn(codexBin) {
		fmt.Fprintln(errOut, "This needs a logged-in Codex. Run `codex login`.")
		return 2
	}

	canPrompt := readerIsTTY(in)
	reader := bufio.NewReader(in)
	shell := shellName()
	req := planRequest{task: task, forceCommand: cfg.Agent}

	for attempt := 0; ; attempt++ {
		plan, err := planWithSpinner(cfg, req, shell, errOut)
		if err != nil {
			fmt.Fprintln(errOut, err.Error())
			return 1
		}

		command := strings.TrimSpace(plan.Command)
		if plan.Kind == "answer" && !req.forceCommand {
			answer := strings.TrimSpace(plan.Answer)
			if answer == "" {
				answer = sanitizeAgentNote(plan.Explanation)
			}
			if answer == "" {
				fmt.Fprintln(errOut, "No answer returned by codex.")
				return 1
			}
			printAnswerPreamble("codex", effectiveModel(cfg, "codex"), out)
			printAgentAnswer(cfg, answer, out)
			return 0
		}

		if !validCommand(command) {
			// No runnable command — surface any (sanitized) note Codex gave.
			if text := sanitizeAgentNote(firstNonEmpty(plan.Answer, plan.Explanation)); text != "" {
				printAgentAnswer(cfg, text, out)
			} else {
				fmt.Fprintln(errOut, "Codex did not return a runnable command for that request. Try rephrasing it.")
			}
			return 1
		}

		if note := sanitizeAgentNote(plan.Explanation); note != "" {
			fmt.Fprintln(out, style(note, out, ansiCyan))
		}
		printProposedCommand(command, plan.Danger, shell, out)

		if cfg.DryRun {
			return 0
		}
		if !cfg.AssumeYes {
			if !canPrompt {
				fmt.Fprintln(errOut, "Running commands needs an interactive terminal to confirm, or pass --yes to auto-approve.")
				return 2
			}
			var ok bool
			command, ok, err = confirmCommand(reader, out, command)
			if err != nil {
				fmt.Fprintln(errOut, err.Error())
				return 1
			}
			if !ok {
				fmt.Fprintln(out, style("Skipped.", out, ansiDim))
				return 0
			}
		}

		code, stderrTail := runShellCommand(command, shell, in, out, errOut)
		if code == 0 || !canPrompt || cfg.AssumeYes || attempt >= maxFixAttempts {
			return exitCode(code)
		}
		fix, err := confirm(reader, out, "Ask ais for a fix?")
		if err != nil || !fix {
			return exitCode(code)
		}
		req = planRequest{
			task:         task,
			forceCommand: true,
			failed:       &failedRun{command: command, exitCode: code, stderr: stderrTail},
		}
	}
}

func exitCode(code int) int {
	if code < 0 {
		return 1
	}
	return code
}

// planWithSpinner plans once, and if the result should be a command but is not
// runnable (empty or with a placeholder), retries once with stricter wording.
func planWithSpinner(cfg Config, req planRequest, shell shellInfo, errOut io.Writer) (agentPlan, error) {
	label := fmt.Sprintf("Thinking via codex (%s)", effectiveModel(cfg, "codex"))
	if req.failed != nil {
		label = fmt.Sprintf("Looking for a fix via codex (%s)", effectiveModel(cfg, "codex"))
	}
	spinner := NewSpinner(label, !cfg.NoSpinner && canUseColor(errOut), errOut)
	spinner.Start()
	defer spinner.Stop()

	plan, err := planAgentStep(cfg, req, shell)
	if err != nil {
		return plan, err
	}
	wantsCommand := req.forceCommand || plan.Kind != "answer"
	if wantsCommand && !validCommand(plan.Command) {
		req.strict = true
		if retry, retryErr := planAgentStep(cfg, req, shell); retryErr == nil {
			plan = retry
		}
	}
	return plan, nil
}

// planAgentStep asks Codex to answer the request or translate it into one
// command, returning the parsed plan.
func planAgentStep(cfg Config, req planRequest, shell shellInfo) (agentPlan, error) {
	raw, err := codexCall(cfg, "codex", agentInstructions, buildAgentPrompt(cfg, req, shell), agentSchemaJSON)
	if err != nil {
		return agentPlan{}, err
	}
	plan, err := parseAgentPlan(raw)
	if err != nil {
		return agentPlan{}, fmt.Errorf("could not parse plan from codex: %v", err)
	}
	plan.Kind = strings.ToLower(strings.TrimSpace(plan.Kind))
	return plan, nil
}

// placeholderRe matches angle-bracket placeholders like <pid> or <OwningProcess>
// that a model leaves when it expects to fill them in from a previous step's
// output — which never happens in a single-shot run.
var placeholderRe = regexp.MustCompile(`<[A-Za-z_][A-Za-z0-9_ ]*>`)

// validCommand reports whether a proposed command is runnable as-is: non-empty
// and free of unfilled placeholders.
func validCommand(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	return !placeholderRe.MatchString(command)
}

// parseAgentPlan tolerates either a bare JSON object or one wrapped in prose /
// fenced code, since models occasionally wrap structured output.
func parseAgentPlan(raw string) (agentPlan, error) {
	var plan agentPlan
	if err := json.Unmarshal([]byte(raw), &plan); err == nil {
		return plan, nil
	}
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start >= 0 && end > start {
		if err := json.Unmarshal([]byte(raw[start:end+1]), &plan); err == nil {
			return plan, nil
		}
	}
	return agentPlan{}, fmt.Errorf("no JSON object found")
}

func buildAgentPrompt(cfg Config, req planRequest, shell shellInfo) string {
	cwd, _ := os.Getwd()
	var b strings.Builder
	// Lead with the request itself; models respond most reliably when the
	// request comes first rather than after a long preamble.
	b.WriteString(strings.TrimSpace(req.task) + "\n\n")
	b.WriteString("That request was typed into ais, a terminal assistant, in a " + shell.label + " terminal on " + runtime.GOOS + ". Working directory: " + cwd + "\n")
	b.WriteString("ais can run one shell command for the user (after they confirm) or answer a question. You do not execute anything: ais runs whatever you put in `command`, in the user's real terminal, so never refuse or talk about your own tools, sandbox, or access.\n\n")

	if req.forceCommand {
		b.WriteString("The user wants a command run. Set `kind` to \"command\".\n")
	} else {
		b.WriteString("Choose `kind`:\n")
		b.WriteString("- \"command\" if the request is something to do on this machine, or needs this machine's live state to answer (files, git repo, processes, ports, installed software, system info, network, etc.). Example: \"what is my current branch\" is a command (`git branch --show-current`).\n")
		b.WriteString("- \"answer\" if it is general knowledge, an explanation, or a how-to that does not depend on this machine. Example: \"how do I undo a git commit\" is an answer. Put the complete answer in `answer`, and leave `command` and `explanation` empty.\n")
		b.WriteString("Answer style: " + firstNonEmpty(strings.TrimSpace(cfg.System), defaultSystemPrompt) + "\n")
	}

	b.WriteString("\nFor a command:\n")
	b.WriteString("- Exactly ONE self-contained " + shell.label + " command, run verbatim. No placeholders (never <pid>, <name>, etc.): combine steps with pipes or sub-expressions such as $(...) so nothing needs filling in by hand.\n")
	b.WriteString("- Prefer the simplest command that does the job. Do not add paths or flags the user did not need.\n")
	b.WriteString("- `explanation`: one short sentence saying what the command WILL do, in present tense (e.g. \"Lists the files in the current folder.\"). Never describe results.\n")
	b.WriteString("- `danger`: \"low\" = read-only, \"medium\" = creates or changes state, \"high\" = destructive/irreversible, kills processes, or changes system/network settings.\n")

	if req.failed != nil {
		b.WriteString("\nThe previous command for this request failed. Propose a corrected command that addresses the error.\n")
		b.WriteString("Failed command: " + req.failed.command + "\n")
		b.WriteString(fmt.Sprintf("Exit code: %d\n", req.failed.exitCode))
		if tail := strings.TrimSpace(req.failed.stderr); tail != "" {
			b.WriteString("Error output (last part):\n" + tail + "\n")
		}
	}
	if req.strict {
		b.WriteString("\nIMPORTANT: `command` MUST be a single, complete, runnable " + shell.label + " command with no placeholders and no prose. Do not leave it empty.\n")
	}
	return b.String()
}

// runShellCommand runs one command in the user's shell with the terminal's
// stdin/stdout attached, so interactive commands (sudo prompts, editors, pagers)
// work. stderr is shown live and its tail is also returned, so a failure can be
// sent back to the model for a fix. Returns the exit code (-1 if it could not
// start).
func runShellCommand(command string, shell shellInfo, in io.Reader, out io.Writer, errOut io.Writer) (int, string) {
	args := append(append([]string{}, shell.args...), command)
	cmd := exec.Command(shell.bin, args...)
	if readerIsTTY(in) {
		cmd.Stdin = in
	}
	cmd.Stdout = out
	tail := &tailBuffer{max: 4000}
	cmd.Stderr = io.MultiWriter(errOut, tail)

	// Ctrl+C goes to the child (it shares the terminal); ais ignores it while the
	// command runs so it can still report the result.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)

	shellBase := strings.TrimSuffix(filepath.Base(shell.bin), ".exe")
	fmt.Fprintln(out, style("running… ("+shellBase+")", out, ansiDim))
	err := cmd.Run()
	fmt.Fprintln(out)

	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			fmt.Fprintln(out, agentStatus(false, "failed to start: "+err.Error(), out))
			return -1, err.Error()
		}
	}
	if code == 0 {
		fmt.Fprintln(out, agentStatus(true, "done (exit 0)", out))
	} else {
		fmt.Fprintln(out, agentStatus(false, fmt.Sprintf("exit %d", code), out))
	}
	return code, tail.String()
}

// tailBuffer keeps only the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// shellInfo describes how to invoke a command string on the current OS.
type shellInfo struct {
	bin   string
	args  []string
	label string
}

func shellName() shellInfo {
	if runtime.GOOS == "windows" {
		bin := "powershell"
		if p, err := exec.LookPath("pwsh"); err == nil {
			bin = p
		}
		return shellInfo{bin: bin, args: []string{"-NoProfile", "-Command"}, label: "PowerShell"}
	}
	bin := "/bin/sh"
	if sh := strings.TrimSpace(os.Getenv("SHELL")); sh != "" {
		bin = sh
	}
	return shellInfo{bin: bin, args: []string{"-c"}, label: "sh"}
}

func confirm(reader *bufio.Reader, out io.Writer, question string) (bool, error) {
	fmt.Fprintf(out, "%s [y/N] ", question)
	line, err := reader.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		// Treat EOF as "no" rather than crashing.
		return false, nil
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

// confirmCommand asks "Run this command? [y/N/e]". "e" lets the user type a
// replacement command (empty keeps the current one) and asks again. Returns the
// command to run and whether to run it.
func confirmCommand(reader *bufio.Reader, out io.Writer, command string) (string, bool, error) {
	for {
		fmt.Fprint(out, "Run this command? [y/N/e=edit] ")
		line, err := reader.ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			return command, false, nil
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return command, true, nil
		case "e", "edit":
			fmt.Fprintln(out, style("Type the command to run (Enter keeps the current one):", out, ansiDim))
			fmt.Fprint(out, "> ")
			edited, err := reader.ReadString('\n')
			if err != nil && strings.TrimSpace(edited) == "" {
				return command, false, nil
			}
			if edited = strings.TrimSpace(edited); edited != "" {
				command = edited
			}
			fmt.Fprintln(out, "  "+style(command, out, ansiYellow))
		default:
			return command, false, nil
		}
	}
}

// agentNoteDisclaimers are fragments that mark a sentence as Codex leaking its
// own sandbox/tool limitations (e.g. "I can't execute shell commands in this
// read-only tool context"). Such sentences are stripped from the note.
var agentNoteDisclaimers = []string{
	"can't execute", "cannot execute", "can't run", "cannot run",
	"can't inspect", "cannot inspect", "don't have", "do not have",
	"not able to", "unable to", "read-only", "read only", "tool context",
	"shell execution", "terminal execution", "execution tool", "this session",
	"sandbox", "no shell", "no terminal", "tools available", "in this chat",
	"from the tools",
}

// sanitizeAgentNote drops sentences where Codex talks about its own inability to
// run things, which it leaks despite instructions. What remains is the useful,
// present-tense description of what the command does.
func sanitizeAgentNote(note string) string {
	note = strings.TrimSpace(note)
	if note == "" {
		return ""
	}
	// Split into sentences on '.', keeping it simple; good enough for one-liners.
	parts := strings.Split(note, ".")
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		s := strings.TrimSpace(p)
		if s == "" {
			continue
		}
		// Normalize smart apostrophes/quotes so markers using a straight ' match
		// the model's curly ' (the reason "I can't inspect…" slipped through).
		low := normalizeQuotes(strings.ToLower(s))
		drop := false
		for _, marker := range agentNoteDisclaimers {
			if strings.Contains(low, marker) {
				drop = true
				break
			}
		}
		if !drop {
			kept = append(kept, s)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, ". ") + "."
}

// normalizeQuotes replaces Unicode smart quotes with their ASCII equivalents so
// text matching does not miss curly apostrophes the model emits.
func normalizeQuotes(s string) string {
	r := strings.NewReplacer(
		"’", "'", // right single quote
		"‘", "'", // left single quote
		"“", "\"", // left double quote
		"”", "\"", // right double quote
	)
	return r.Replace(s)
}

// printAgentAnswer prints a plain answer with the same markdown rendering the
// regular Q&A output uses.
func printAgentAnswer(cfg Config, text string, out io.Writer) {
	text = strings.TrimSpace(text)
	if resolveRenderMode(cfg.Render, out) == "ansi" {
		fmt.Fprintln(out, renderMarkdownANSI(text))
	} else {
		fmt.Fprintln(out, text)
	}
}

func printProposedCommand(command string, danger string, shell shellInfo, out io.Writer) {
	fmt.Fprintln(out)
	suffix := ""
	switch strings.ToLower(danger) {
	case "high":
		suffix = " (destructive)"
	case "medium":
		suffix = " (changes state)"
	}
	cmdStyle := ansiYellow
	if strings.EqualFold(danger, "high") {
		cmdStyle = ansiBold + ansiYellow
	}
	fmt.Fprintln(out, style(fmt.Sprintf("%s will run%s:", shell.label, suffix), out, ansiDim))
	fmt.Fprintln(out, "  "+style(command, out, cmdStyle))
}

// agentStatus formats a one-line execution result, e.g. "✓ done (exit 0)".
func agentStatus(ok bool, msg string, out io.Writer) string {
	mark, color := "x", ansiYellow
	if ok {
		mark, color = "+", ansiGreen
	}
	if supportsUnicode() {
		if ok {
			mark = "✓"
		} else {
			mark = "✗"
		}
	}
	return style(mark+" "+msg, out, color)
}

// style applies ANSI styles only when out supports color.
func style(text string, out io.Writer, styles ...string) string {
	if canUseColor(out) {
		return ansi(text, styles...)
	}
	return text
}
