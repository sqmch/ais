package ais

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// agentPlannerModel is the model used to translate a request into a command when
// the user has not pinned one with -m. Command planning needs a capable model;
// the user's saved chat model may be a cheaper one that is unreliable here.
const agentPlannerModel = "gpt-5.5"

// agentPlan is the structured response Codex returns, constrained by the JSON
// schema passed via `codex exec --output-schema`. It is a SINGLE self-contained
// command: a one-string schema structurally prevents the model from splitting a
// task into multiple steps where a later step contains a placeholder (e.g.
// "<OwningProcess>") that depends on an earlier step's output — which breaks in a
// single-shot run that never feeds output back.
type agentPlan struct {
	Command     string `json:"command"`
	Explanation string `json:"explanation"`
	Danger      string `json:"danger"`
}

// runAgent turns a request that the caller already decided is an action into
// shell command(s) and runs them in the user's terminal (confirming each unless
// --yes). It is single-shot: Codex plans once, ais runs the proposed command(s),
// and it stops. Re-planning after each command was removed because Codex — whose
// own sandbox is read-only — does not trust that the command ran and keeps
// re-proposing variants forever instead of setting done.
func runAgent(cfg Config, task string, in io.Reader, out io.Writer, errOut io.Writer) int {
	task = strings.TrimSpace(task)
	if task == "" {
		fmt.Fprintln(errOut, "No task provided. Example: ais kill the process on port 8080")
		return 2
	}

	codexBin := findCodexBinary()
	if codexBin == "" {
		fmt.Fprintln(errOut, "This needs the Codex CLI. Install it or run `codex login` first.")
		return 2
	}
	if !isCodexLoggedIn(codexBin) {
		fmt.Fprintln(errOut, "This needs a logged-in Codex. Run `codex login`.")
		return 2
	}

	canPrompt := readerIsTTY(in)
	if !cfg.AssumeYes && !canPrompt {
		fmt.Fprintln(errOut, "Running commands needs an interactive terminal to confirm, or pass --yes to auto-approve.")
		return 2
	}

	// Command planning is the hard part and small models (e.g. gpt-5.4-mini) are
	// unreliable at it — they return prose or junk instead of a real command. So
	// unless the user explicitly pinned a model with -m, plan with a strong model
	// regardless of the (possibly cheaper) chat model saved in config. cfg is a
	// value copy, so overriding Model here is local to this run.
	if !cfg.ModelExplicit {
		cfg.Model = agentPlannerModel
	}

	reader := bufio.NewReader(in)
	shell := shellName()

	spinner := NewSpinner(fmt.Sprintf("Thinking via codex (%s)", effectiveModel(cfg, "codex")), !cfg.NoSpinner && canUseColor(errOut), errOut)
	spinner.Start()
	plan, err := planAgentStep(cfg, codexBin, task, shell, false)
	command := strings.TrimSpace(plan.Command)
	// Weaker models sometimes return prose with no command, or a command with a
	// placeholder like "<OwningProcess>". Retry once, strictly, before giving up.
	if err == nil && !validCommand(command) {
		var retryErr error
		plan, retryErr = planAgentStep(cfg, codexBin, task, shell, true)
		if retryErr == nil {
			command = strings.TrimSpace(plan.Command)
		}
	}
	spinner.Stop()
	if err != nil {
		fmt.Fprintln(errOut, err.Error())
		return 1
	}

	if !validCommand(command) {
		// No runnable command — surface any (sanitized) note Codex gave.
		if text := sanitizeAgentNote(plan.Explanation); text != "" {
			printAgentAnswer(cfg, text, out)
		} else {
			fmt.Fprintln(errOut, "Codex did not return a runnable command for that request. Try rephrasing, or ask it as a question.")
		}
		return 1
	}

	if note := sanitizeAgentNote(plan.Explanation); note != "" {
		printAgentNote(note, out)
	}

	printProposedCommand(command, plan.Danger, shell, out)
	if !cfg.AssumeYes {
		ok, err := confirm(reader, out, "Run this command?")
		if err != nil {
			fmt.Fprintln(errOut, err.Error())
			return 1
		}
		if !ok {
			fmt.Fprintln(out, dim("Skipped.", out))
			return 0
		}
	}
	runShellCommand(command, shell, out)
	return 0
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

// runCodexQA answers a plain question through Codex using a forced {answer}
// schema. Free-form prompting of codex exec makes gpt-5.5 act as an agent and
// reply "Understood." or ask what to do; constraining the output to an `answer`
// field compels a real answer. Returns a process exit code.
func runCodexQA(cfg Config, question string, out io.Writer, errOut io.Writer) int {
	codexBin := findCodexBinary()
	if codexBin == "" {
		fmt.Fprintln(errOut, "This needs the Codex CLI. Install it or run `codex login` first.")
		return 2
	}
	if !isCodexLoggedIn(codexBin) {
		fmt.Fprintln(errOut, "This needs a logged-in Codex. Run `codex login`.")
		return 2
	}

	spinner := NewSpinner(fmt.Sprintf("Thinking via codex (%s)", effectiveModel(cfg, "codex")), !cfg.NoSpinner && canUseColor(errOut), errOut)
	spinner.Start()
	raw, err := codexStructuredCall(cfg, codexBin, buildQAPrompt(cfg, question), qaSchemaJSON)
	spinner.Stop()
	if err != nil {
		fmt.Fprintln(errOut, err.Error())
		return 1
	}

	var parsed struct {
		Answer string `json:"answer"`
	}
	answer := ""
	if jsonErr := json.Unmarshal([]byte(raw), &parsed); jsonErr == nil {
		answer = strings.TrimSpace(parsed.Answer)
	}
	if answer == "" {
		// Fall back to any JSON object embedded in prose, then to the raw text.
		if start, end := strings.Index(raw, "{"), strings.LastIndex(raw, "}"); start >= 0 && end > start {
			if jsonErr := json.Unmarshal([]byte(raw[start:end+1]), &parsed); jsonErr == nil {
				answer = strings.TrimSpace(parsed.Answer)
			}
		}
	}
	if answer == "" {
		answer = strings.TrimSpace(raw)
	}
	if answer == "" {
		fmt.Fprintln(errOut, "No answer returned by codex.")
		return 1
	}

	printAnswerPreamble("codex", effectiveModel(cfg, "codex"), out)
	printAgentAnswer(cfg, answer, out)
	return 0
}

func buildQAPrompt(cfg Config, question string) string {
	var b strings.Builder
	// Lead with the question (consistent with what reliably gets a response).
	b.WriteString(strings.TrimSpace(question) + "\n\n")
	system := strings.TrimSpace(cfg.System)
	if system == "" {
		system = defaultSystemPrompt
	}
	b.WriteString(system + "\n")
	b.WriteString("Answer the question above for someone working in a terminal. Put the complete answer in the `answer` field as plain text or simple markdown (short sections or bullets are fine; avoid tables). Do not just acknowledge, do not ask what they want, and do not mention your own tools or access — just answer.\n")
	return b.String()
}

// qaSchemaJSON forces Codex to return an answer string.
const qaSchemaJSON = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["answer"],
  "properties": {
    "answer": { "type": "string" }
  }
}`

// looksLikeCommand decides — locally, without any model call — whether a request
// is an action to run on this computer (true) versus a knowledge question to
// answer (false). It is deliberately biased: it only returns true on a clear
// action signal, and the confirmation gate makes false positives harmless (the
// user just declines). Asking Codex itself to classify proved unreliable because
// its read-only sandbox context makes it refuse and answer in prose instead.
func looksLikeCommand(task string) bool {
	t := strings.ToLower(strings.TrimSpace(task))
	if t == "" {
		return false
	}

	// Knowledge/how-to phrasings win outright — these want an explanation, even
	// if other words look action-like ("show me how to…", "how do I kill…").
	for _, marker := range knowledgeMarkers {
		if strings.Contains(t, marker) {
			return false
		}
	}

	fields := strings.Fields(t)
	first := strings.Trim(fields[0], ",.!?:;\"'`")
	if hardActionVerbs[first] {
		return true
	}
	if softActionVerbs[first] {
		return true
	}

	// State words/phrases anywhere imply this is about the local machine.
	for _, sig := range commandSignals {
		if strings.Contains(t, sig) {
			return true
		}
	}
	return false
}

// hardActionVerbs, as the first word, are almost always imperative actions.
var hardActionVerbs = wordSet(
	"kill", "stop", "start", "restart", "reboot", "shutdown", "run", "exec",
	"execute", "launch", "delete", "remove", "rm", "del", "uninstall", "install",
	"reinstall", "update", "upgrade", "downgrade", "free", "mkdir", "touch",
	"rename", "ren", "move", "mv", "copy", "cp", "chmod", "chown", "enable",
	"disable", "mount", "unmount", "clone", "commit", "push", "pull", "checkout",
	"merge", "rebase", "deploy", "build", "compile", "clean", "set", "unset",
	"export", "git", "npm", "yarn", "pnpm", "docker", "kubectl", "pip", "cargo",
	"curl", "wget", "ssh", "scp", "ping", "netstat", "tar", "zip", "unzip",
)

// softActionVerbs, as the first word, usually request a local operation but can
// start a how-to ("show me how…"); knowledgeMarkers are checked first to catch
// that case.
var softActionVerbs = wordSet(
	"list", "show", "find", "open", "check", "get", "display", "print", "count",
	"create", "make", "add", "edit", "cat", "grep", "search", "cd", "ls",
)

// commandSignals are phrases that indicate the request is about this machine's
// live state, even when phrased as a question.
var commandSignals = []string{
	"on port", "port ", "listening", " running", "running on", "which process",
	"what process", "processes", " pid ", "git status", "disk space",
	"disk usage", "free up", "using port", "what's using", "whats using",
	"my ip", "ip address", "is installed", "do i have", "currently running",
	"in this folder", "in this directory", "in the current", "this repo",
	"environment variable", "env var",
}

// knowledgeMarkers indicate a request for explanation/how-to that should be
// answered, not executed.
var knowledgeMarkers = []string{
	"how do i", "how do you", "how to", "how can i", "how should i", "how does",
	"what does", "what is the difference", "what's the difference",
	"difference between", "explain", "why ", "meaning of", "what do you",
	"should i", "can you explain", "tell me about", "what are the", "examples of",
	"what is a", "what's a", "pros and cons",
}

func wordSet(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

// planAgentStep asks Codex to translate the task into one command, returning the
// parsed plan. strict adds extra pressure to return a runnable command (used on
// retry when the first attempt came back empty or with a placeholder).
func planAgentStep(cfg Config, codexBin string, task string, shell shellInfo, strict bool) (agentPlan, error) {
	raw, err := codexStructuredCall(cfg, codexBin, buildAgentPrompt(task, shell, strict), agentSchemaJSON)
	if err != nil {
		return agentPlan{}, err
	}
	plan, err := parseAgentPlan(raw)
	if err != nil {
		return agentPlan{}, fmt.Errorf("could not parse plan from codex: %v", err)
	}
	return plan, nil
}

// codexStructuredCall runs one non-interactive Codex call constrained to a
// read-only sandbox with `shell_tool` disabled and the given JSON schema, and
// returns the raw JSON string Codex wrote. Forcing structured output is what
// makes gpt-5.5 reliably produce content (commands or an answer) instead of
// acting as an agent ("Understood.", "what would you like me to do?").
func codexStructuredCall(cfg Config, codexBin string, prompt string, schemaJSON string) (string, error) {
	schemaFile, err := os.CreateTemp("", "ais-schema-*.json")
	if err != nil {
		return "", err
	}
	schemaPath := schemaFile.Name()
	_, _ = schemaFile.WriteString(schemaJSON)
	_ = schemaFile.Close()
	defer os.Remove(schemaPath)

	outFile, err := os.CreateTemp("", "ais-out-*.json")
	if err != nil {
		return "", err
	}
	outPath := outFile.Name()
	_ = outFile.Close()
	defer os.Remove(outPath)

	args := []string{
		"exec", "--skip-git-repo-check",
		"--sandbox", "read-only",
		"--color", "never",
		// Disable Codex's own shell tool so it cannot run commands during this
		// call — it must only return text. For the agent path this also prevents
		// the "already ran it and narrates results in past tense" behavior.
		"--disable", "shell_tool",
		"--output-schema", schemaPath,
		"-o", outPath,
	}
	args = append(args, codexReasoningArgs(cfg.ReasoningEffort)...)
	if strings.TrimSpace(cfg.Model) != "" {
		args = append(args, "-m", cfg.Model)
	}
	args = append(args, prompt)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, codexBin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("codex timed out")
	}
	raw := readFileTrim(outPath)
	if raw == "" {
		if runErr != nil {
			return "", fmt.Errorf("codex failed: %s", summarizeCodexFailure(stdout.String(), stderr.String()))
		}
		return "", fmt.Errorf("codex returned an empty response")
	}
	return raw, nil
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

func buildAgentPrompt(task string, shell shellInfo, strict bool) string {
	cwd, _ := os.Getwd()
	var b strings.Builder
	// Lead with the request itself. Empirically, gpt-5.5 responds far more
	// reliably when the request comes first; a "you are an agent" preamble makes
	// it reply "send me the request", and phrases like "make reasonable
	// assumptions" make it assume the safest action is to do nothing.
	b.WriteString(task + "\n\n")

	// Critical framing: the model is NOT the executor. A separate program (ais)
	// runs whatever goes in `command`, in the user's real terminal. Without this,
	// codex (whose own sandbox is read-only here) decides it "cannot run commands"
	// and writes copy-paste prose instead of filling `command`.
	b.WriteString("You are the planner for a tool called ais. You do NOT execute anything yourself. A separate program runs whatever you put in `command`, in the user's real " + shell.label + " terminal on " + runtime.GOOS + ". Returning command text is the whole job — your own inability to run things is irrelevant, so never refuse, never give copy-paste instructions in prose, and never mention tools, sandboxes, or access.\n\n")
	b.WriteString("Turn the request above into exactly ONE self-contained " + shell.label + " command and put it in `command`.\n")
	b.WriteString("\nRules:\n")
	b.WriteString("- Exactly one command. It must run as-is with NO placeholders (never write <pid>, <OwningProcess>, <name>, etc.). Combine steps with pipelines or sub-expressions (e.g. $(...) or (...).Property) so nothing needs to be filled in by hand and no intermediate output is needed.\n")
	b.WriteString("- The `command` string is run verbatim by " + shell.label + " on " + runtime.GOOS + ". Only return the command; do not narrate results.\n")
	b.WriteString("- In `explanation`, say what the command WILL do, in present tense (e.g. \"Lists the files in the current folder\"). Never describe results or speak as if it already ran. Do not mention your own tools or access.\n")
	b.WriteString("- `danger`: \"low\" = read-only, \"medium\" = creates or changes state, \"high\" = destructive/irreversible, kills processes, or changes system/network.\n")
	if strict {
		b.WriteString("\nIMPORTANT: `command` MUST be a single, complete, runnable " + shell.label + " command with no placeholders and no prose. Do not leave it empty. If the request needs data from another command, fetch it inline with a sub-expression in the same command.\n")
	}
	b.WriteString("\nWorking directory: " + cwd + "\n")
	return b.String()
}

// runShellCommand runs one command in the user's shell, streaming its output
// straight to the terminal, and prints a one-line status. Returns the exit code.
func runShellCommand(command string, shell shellInfo, out io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	args := append(append([]string{}, shell.args...), command)
	cmd := exec.CommandContext(ctx, shell.bin, args...)
	cmd.Stdout = out
	cmd.Stderr = out

	fmt.Fprintln(out, dim("running… ("+shell.bin+")", out))
	err := cmd.Run()
	fmt.Fprintln(out)

	if ctx.Err() == context.DeadlineExceeded {
		fmt.Fprintln(out, agentStatus(false, "timed out after 5m", out))
		return -1
	}
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			fmt.Fprintln(out, agentStatus(false, "failed to start: "+err.Error(), out))
			return -1
		}
	}
	if code == 0 {
		fmt.Fprintln(out, agentStatus(true, "done (exit 0)", out))
	} else {
		fmt.Fprintln(out, agentStatus(false, fmt.Sprintf("exit %d", code), out))
	}
	return code
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
		return shellInfo{bin: bin, args: []string{"-NoProfile", "-NonInteractive", "-Command"}, label: "PowerShell"}
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
	if err != nil {
		// Treat EOF as "no" rather than crashing.
		if strings.TrimSpace(line) == "" {
			return false, nil
		}
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

func printAgentNote(text string, out io.Writer) {
	text = strings.TrimSpace(text)
	if canUseColor(out) {
		fmt.Fprintln(out, ansi(text, ansiCyan))
	} else {
		fmt.Fprintln(out, text)
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

// printAgentAnswer prints a plain answer (the question path), with the same
// markdown rendering the regular Q&A output uses.
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
	tag := fmt.Sprintf("%s will run%s:", shell.label, suffix)
	if canUseColor(out) {
		style := ansiYellow
		if strings.EqualFold(danger, "high") {
			style = ansiBold + ansiYellow
		}
		fmt.Fprintln(out, ansi(tag, ansiDim))
		fmt.Fprintln(out, "  "+ansi(command, style))
	} else {
		fmt.Fprintln(out, tag)
		fmt.Fprintln(out, "  "+command)
	}
}

// agentStatus formats a one-line execution result, e.g. "✓ done (exit 0)".
func agentStatus(ok bool, msg string, out io.Writer) string {
	mark, style := "x", ansiYellow
	if ok {
		mark, style = "+", ansiGreen
	}
	if supportsUnicode() {
		if ok {
			mark = "✓"
		} else {
			mark = "✗"
		}
	}
	line := mark + " " + msg
	if canUseColor(out) {
		return ansi(line, style)
	}
	return line
}

func dim(text string, out io.Writer) string {
	if canUseColor(out) {
		return ansi(text, ansiDim)
	}
	return text
}

// agentSchemaJSON constrains Codex's per-step output. It uses strict structured
// output rules (every property required, no extras) so parsing is reliable.
const agentSchemaJSON = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["command", "explanation", "danger"],
  "properties": {
    "command": { "type": "string" },
    "explanation": { "type": "string" },
    "danger": { "type": "string", "enum": ["low", "medium", "high"] }
  }
}`
