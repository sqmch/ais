package ais

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	apiURL               = "https://api.openai.com/v1/responses"
	defaultAPIModel      = "gpt-6-luna"
	defaultBackend       = "auto"
	defaultRenderMode    = "auto"
	defaultSystemPrompt  = "You are a practical terminal assistant. Answer the user's question or request directly and immediately; never ask what they want to work on or offer to start a task. Keep answers concise and clear. Use short sections and bullets where useful. Avoid markdown tables."
	defaultMaxInputChars = 120000
	defaultTruncateMode  = "head"
	defaultReasoning     = "low"
)

// version is overridden at release time with
// -ldflags "-X github.com/sqmch/ais/internal/ais.version=<tag>".
var version = "dev"

const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiCyan   = "\033[36m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
)

type Config struct {
	PromptFlag      string
	PromptWords     []string
	Backend         string
	Model           string
	LocalProvider   string
	System          string
	MaxOutputTokens int
	Temperature     float64
	RawJSON         bool
	Render          string
	NoSpinner       bool
	Stream          bool
	MaxInputChars   int
	TruncateMode    string
	ShowInputStats  bool
	ListModels      bool
	Configure       bool
	ShowVersion     bool
	Agent           bool
	AssumeYes       bool
	DryRun          bool
	ReasoningEffort string
}

type SavedConfig struct {
	Backend         string `json:"backend,omitempty"`
	Model           string `json:"model,omitempty"`
	LocalProvider   string `json:"local_provider,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

type InputStats struct {
	PromptChars     int
	StdinPresent    bool
	StdinCharsOrig  int
	StdinCharsUsed  int
	StdinTruncated  bool
	TotalChars      int
	EstimatedTokens int
}

func Run(args []string, in io.Reader, out io.Writer, errOut io.Writer) int {
	cfg, err := parseArgs(args, errOut)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if cfg.ShowVersion {
		fmt.Fprintln(out, "ais", version)
		return 0
	}

	if cfg.Configure {
		if err := runConfigure(cfg, in, out, errOut); err != nil {
			if errors.Is(err, errSelectCancelled) {
				fmt.Fprintln(out, "Configuration cancelled.")
				return 0
			}
			fmt.Fprintln(errOut, err.Error())
			return 1
		}
		return 0
	}

	if cfg.ListModels {
		backend := resolveBackend(cfg.Backend)
		if backend == "" {
			backend = cfg.Backend
		}
		printModelCatalog(cfg, backend, out)
		return 0
	}

	// Routing: on the Codex backend, an interactive request (no piped stdin) goes
	// to the agent path, where one model call either answers it or proposes a
	// command to run. `-a` forces a command. Piped input (summaries) and
	// non-Codex backends use the classic Q&A path below.
	stdinPiped := !readerIsTTY(in)
	if cfg.Agent || cfg.DryRun || (!stdinPiped && resolveBackend(cfg.Backend) == "codex") {
		if resolveBackend(cfg.Backend) != "codex" {
			fmt.Fprintln(errOut, "Running commands (-a) requires the Codex backend.")
			return 2
		}
		if task := joinTask(cfg); task != "" || cfg.Agent {
			return runAgent(cfg, task, in, out, errOut)
		}
	}

	prompt, stats, err := buildPrompt(cfg, in)
	if err != nil {
		fmt.Fprintln(errOut, "Failed to read input:", err)
		return 1
	}
	if prompt == "" {
		fmt.Fprintln(errOut, "No prompt provided. Example: ais explain grep -R")
		fmt.Fprintln(errOut, "You can also pipe stdin: cat file.txt | ais -p 'summarize this'.")
		return 2
	}
	if stats.StdinTruncated {
		fmt.Fprintf(errOut, "Warning: stdin was truncated (%d -> %d chars). Tune with --max-input-chars/--truncate.\n", stats.StdinCharsOrig, stats.StdinCharsUsed)
	}
	if cfg.ShowInputStats {
		printInputStats(stats, errOut)
	}

	backend := resolveBackend(cfg.Backend)
	if backend == "" {
		fmt.Fprintln(errOut, "No usable backend found.")
		fmt.Fprintln(errOut, "Option 1 (subscription-style): run `codex login` first.")
		fmt.Fprintln(errOut, "Option 2 (API): set OPENAI_API_KEY.")
		fmt.Fprintln(errOut, "Option 3 (free/local): run a local Ollama or LM Studio model and use `--backend oss`.")
		return 2
	}
	spinnerEnabled := !cfg.NoSpinner && canUseColor(errOut)
	shouldStream := cfg.Stream && !cfg.RawJSON
	streamedAny := false
	activeModel := effectiveModel(cfg, backend)

	var result map[string]any
	if shouldStream {
		spinner := NewSpinner(fmt.Sprintf("Thinking via %s (%s)", backend, activeModel), spinnerEnabled, errOut)
		spinner.Start()
		defer spinner.Stop()

		onDelta := func(delta string) {
			if !streamedAny {
				spinner.Stop()
				printAnswerPreamble(backend, activeModel, out)
				streamedAny = true
			}
			_, _ = io.WriteString(out, delta)
		}

		if backend == "codex" || backend == "oss" {
			result, err = callCodex(cfg, backend, prompt)
		} else {
			result, err = callAPI(cfg, prompt, true, onDelta)
		}
	} else {
		spinner := NewSpinner(fmt.Sprintf("Thinking via %s (%s)", backend, activeModel), spinnerEnabled, errOut)
		spinner.Start()
		if backend == "codex" || backend == "oss" {
			result, err = callCodex(cfg, backend, prompt)
		} else {
			result, err = callAPI(cfg, prompt, false, nil)
		}
		spinner.Stop()
	}

	if err != nil {
		fmt.Fprintln(errOut, err.Error())
		return 1
	}

	if cfg.RawJSON {
		b, _ := json.MarshalIndent(result, "", "  ")
		fmt.Fprintln(out, string(b))
		return 0
	}

	text := extractOutputText(result)
	if strings.TrimSpace(text) == "" {
		fmt.Fprintln(errOut, "No text output returned by model.")
		return 1
	}

	if shouldStream && streamedAny {
		if !strings.HasSuffix(text, "\n") {
			fmt.Fprintln(out)
		}
		return 0
	}

	printAnswerPreamble(backend, activeModel, out)
	if resolveRenderMode(cfg.Render, out) == "ansi" {
		fmt.Fprintln(out, renderMarkdownANSI(text))
	} else {
		fmt.Fprintln(out, text)
	}
	return 0
}

func parseArgs(args []string, errOut io.Writer) (Config, error) {
	saved := loadSavedConfig()
	cfg := Config{
		Backend:         firstNonEmpty(os.Getenv("AIS_BACKEND"), firstNonEmpty(saved.Backend, defaultBackend)),
		Model:           firstNonEmpty(os.Getenv("AIS_MODEL"), saved.Model),
		LocalProvider:   firstNonEmpty(os.Getenv("AIS_LOCAL_PROVIDER"), saved.LocalProvider),
		System:          envOr("AIS_SYSTEM_PROMPT", defaultSystemPrompt),
		MaxOutputTokens: 2000,
		Temperature:     0.2,
		Render:          envOr("AIS_RENDER", defaultRenderMode),
		Stream:          envBool("AIS_STREAM", true),
		MaxInputChars:   envInt("AIS_MAX_INPUT_CHARS", defaultMaxInputChars),
		TruncateMode:    envOr("AIS_TRUNCATE", defaultTruncateMode),
		ReasoningEffort: firstNonEmpty(os.Getenv("AIS_REASONING_EFFORT"), firstNonEmpty(saved.ReasoningEffort, defaultReasoning)),
	}

	fs := flag.NewFlagSet("ais", flag.ContinueOnError)
	fs.SetOutput(errOut)

	fs.StringVar(&cfg.PromptFlag, "p", "", "Explicit prompt/instructions, useful with piped stdin.")
	fs.StringVar(&cfg.PromptFlag, "prompt", "", "Explicit prompt/instructions, useful with piped stdin.")
	fs.StringVar(&cfg.Backend, "b", cfg.Backend, "Backend to use (auto, codex, api, oss).")
	fs.StringVar(&cfg.Backend, "backend", cfg.Backend, "Backend to use (auto, codex, api, oss).")
	fs.StringVar(&cfg.Model, "m", cfg.Model, "Model name override.")
	fs.StringVar(&cfg.Model, "model", cfg.Model, "Model name override.")
	fs.StringVar(&cfg.LocalProvider, "local-provider", cfg.LocalProvider, "Local OSS provider for codex --oss (ollama or lmstudio).")
	fs.StringVar(&cfg.System, "s", cfg.System, "System instructions.")
	fs.StringVar(&cfg.System, "system", cfg.System, "System instructions.")
	fs.IntVar(&cfg.MaxOutputTokens, "max-output-tokens", cfg.MaxOutputTokens, "Maximum output tokens.")
	fs.Float64Var(&cfg.Temperature, "temperature", cfg.Temperature, "Sampling temperature.")
	fs.BoolVar(&cfg.RawJSON, "raw-json", false, "Print raw JSON response.")
	fs.StringVar(&cfg.Render, "render", cfg.Render, "Output rendering mode (auto, ansi, raw).")
	fs.BoolVar(&cfg.NoSpinner, "no-spinner", false, "Disable loading spinner.")
	fs.BoolVar(&cfg.Stream, "stream", cfg.Stream, "Enable token streaming where supported.")
	noStream := fs.Bool("no-stream", false, "Disable token streaming.")
	fs.IntVar(&cfg.MaxInputChars, "max-input-chars", cfg.MaxInputChars, "Maximum stdin chars to include (0 disables limit).")
	fs.StringVar(&cfg.TruncateMode, "truncate", cfg.TruncateMode, "If stdin exceeds limit, keep head, tail, or middle.")
	fs.BoolVar(&cfg.ShowInputStats, "show-input-stats", false, "Print input size stats to stderr.")
	fs.BoolVar(&cfg.ListModels, "list-models", false, "List practical model choices for the selected backend and exit.")
	fs.BoolVar(&cfg.Configure, "configure", false, "Open an interactive default backend/model picker and save the result.")
	fs.BoolVar(&cfg.ShowVersion, "version", false, "Show version and exit.")
	fs.BoolVar(&cfg.Agent, "a", false, "Always turn the request into a command to run (asks before running).")
	fs.BoolVar(&cfg.Agent, "agent", false, "Always turn the request into a command to run (asks before running).")
	fs.BoolVar(&cfg.AssumeYes, "y", false, "Run proposed commands without asking for confirmation.")
	fs.BoolVar(&cfg.AssumeYes, "yes", false, "Run proposed commands without asking for confirmation.")
	fs.BoolVar(&cfg.DryRun, "n", false, "Show the proposed command but do not run it.")
	fs.BoolVar(&cfg.DryRun, "dry-run", false, "Show the proposed command but do not run it.")
	fs.StringVar(&cfg.ReasoningEffort, "r", cfg.ReasoningEffort, "Reasoning effort (low, medium, high, xhigh, ...; see --list-models).")
	fs.StringVar(&cfg.ReasoningEffort, "reasoning", cfg.ReasoningEffort, "Reasoning effort (low, medium, high, xhigh, ...; see --list-models).")

	fs.Usage = func() {
		fmt.Fprintf(errOut, "Usage: ais [options] [prompt words...]\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if *noStream {
		cfg.Stream = false
	}
	if cfg.Backend != "auto" && cfg.Backend != "codex" && cfg.Backend != "api" && cfg.Backend != "oss" {
		return Config{}, fmt.Errorf("invalid backend: %s", cfg.Backend)
	}
	if cfg.LocalProvider != "" && cfg.LocalProvider != "ollama" && cfg.LocalProvider != "lmstudio" {
		return Config{}, fmt.Errorf("invalid local provider: %s", cfg.LocalProvider)
	}
	if cfg.Render != "auto" && cfg.Render != "ansi" && cfg.Render != "raw" {
		cfg.Render = defaultRenderMode
	}
	if cfg.TruncateMode != "head" && cfg.TruncateMode != "tail" && cfg.TruncateMode != "middle" {
		cfg.TruncateMode = defaultTruncateMode
	}
	if !validReasoningEffort(cfg.ReasoningEffort) {
		cfg.ReasoningEffort = defaultReasoning
	}

	cfg.PromptWords = fs.Args()
	return cfg, nil
}

// joinTask combines the -p prompt flag and positional words into a single task
// string (used by the agent path, which does not read piped stdin).
func joinTask(cfg Config) string {
	parts := make([]string, 0, 2)
	if strings.TrimSpace(cfg.PromptFlag) != "" {
		parts = append(parts, strings.TrimSpace(cfg.PromptFlag))
	}
	if len(cfg.PromptWords) > 0 {
		parts = append(parts, strings.Join(cfg.PromptWords, " "))
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

func buildPrompt(cfg Config, in io.Reader) (string, InputStats, error) {
	promptParts := make([]string, 0, 2)
	if strings.TrimSpace(cfg.PromptFlag) != "" {
		promptParts = append(promptParts, strings.TrimSpace(cfg.PromptFlag))
	}
	if len(cfg.PromptWords) > 0 {
		promptParts = append(promptParts, strings.TrimSpace(strings.Join(cfg.PromptWords, " ")))
	}
	promptText := strings.TrimSpace(strings.Join(promptParts, " "))

	stdinPresent := !readerIsTTY(in)
	stdinRaw := ""
	stdinUsed := ""
	stdinTruncated := false

	if stdinPresent {
		b, err := io.ReadAll(in)
		if err != nil {
			return "", InputStats{}, err
		}
		stdinRaw = string(b)
		stdinUsed, stdinTruncated = truncateText(stdinRaw, cfg.MaxInputChars, cfg.TruncateMode)
		stdinUsed = strings.TrimSpace(stdinUsed)
	}

	combined := ""
	switch {
	case promptText != "" && stdinUsed != "":
		combined = promptText + "\n\nInput:\n" + stdinUsed
	case promptText != "":
		combined = promptText
	default:
		combined = stdinUsed
	}
	combined = strings.TrimSpace(combined)

	stats := InputStats{
		PromptChars:     runeLen(promptText),
		StdinPresent:    stdinPresent,
		StdinCharsOrig:  runeLen(stdinRaw),
		StdinCharsUsed:  runeLen(stdinUsed),
		StdinTruncated:  stdinTruncated,
		TotalChars:      runeLen(combined),
		EstimatedTokens: int(math.Ceil(float64(runeLen(combined)) / 4.0)),
	}
	return combined, stats, nil
}

func truncateText(text string, maxChars int, mode string) (string, bool) {
	runes := []rune(text)
	if maxChars <= 0 || len(runes) <= maxChars {
		return text, false
	}

	switch mode {
	case "tail":
		return string(runes[len(runes)-maxChars:]), true
	case "middle":
		headLen := maxChars / 2
		tailLen := maxChars - headLen
		marker := []rune("\n...[TRUNCATED]...\n")
		if maxChars > len(marker)+20 {
			headLen = max((maxChars-len(marker))/2, 0)
			tailLen = max(maxChars-len(marker)-headLen, 0)
			return string(runes[:headLen]) + string(marker) + string(runes[len(runes)-tailLen:]), true
		}
		return string(runes[:headLen]) + string(runes[len(runes)-tailLen:]), true
	default:
		return string(runes[:maxChars]), true
	}
}

func printInputStats(stats InputStats, out io.Writer) {
	stdinNote := "stdin=none"
	if stats.StdinPresent {
		stdinNote = fmt.Sprintf("stdin=%d/%d chars", stats.StdinCharsUsed, stats.StdinCharsOrig)
	}
	truncated := "truncated=no"
	if stats.StdinTruncated {
		truncated = "truncated=yes"
	}
	fmt.Fprintf(out, "Input stats: prompt=%d chars, %s, %s, total=%d chars, est_tokens~=%d\n", stats.PromptChars, stdinNote, truncated, stats.TotalChars, stats.EstimatedTokens)
}

func resolveBackend(requested string) string {
	if requested == "codex" || requested == "api" || requested == "oss" {
		return requested
	}
	if codexBin := findCodexBinary(); codexBin != "" && isCodexLoggedIn(codexBin) {
		return "codex"
	}
	if os.Getenv("OPENAI_API_KEY") != "" {
		return "api"
	}
	return ""
}

func callAPI(cfg Config, prompt string, stream bool, onDelta func(string)) (map[string]any, error) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("API backend requested but OPENAI_API_KEY is not set.")
	}

	model := firstNonEmpty(cfg.Model, defaultAPIModel)
	payload := map[string]any{
		"model":             model,
		"instructions":      cfg.System,
		"input":             prompt,
		"max_output_tokens": cfg.MaxOutputTokens,
	}
	// Reasoning models (gpt-5 and later, o-series) reject temperature and take a
	// reasoning effort instead; older models are the reverse.
	if isReasoningModel(model) {
		if validReasoningEffort(cfg.ReasoningEffort) {
			payload["reasoning"] = map[string]any{"effort": strings.ToLower(cfg.ReasoningEffort)}
		}
	} else {
		payload["temperature"] = cfg.Temperature
	}
	if stream {
		payload["stream"] = true
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Network error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error (%d): %s", resp.StatusCode, extractErrorMessage(string(raw)))
	}

	if !stream {
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("Received non-JSON response from API")
		}
		return out, nil
	}

	reader := bufio.NewReader(resp.Body)
	dataLines := make([]string, 0, 4)
	deltas := make([]string, 0, 64)
	fallbackText := ""
	var responseObj map[string]any
	streamErr := ""

	handleEvent := func(payloadLine string) bool {
		if payloadLine == "[DONE]" {
			return true
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(payloadLine), &event); err != nil {
			return false
		}
		if msg := extractStreamError(event); msg != "" {
			streamErr = msg
			return false
		}

		etype := asString(event["type"])
		switch etype {
		case "response.output_text.delta":
			delta := asString(event["delta"])
			if delta != "" {
				deltas = append(deltas, delta)
				if onDelta != nil {
					onDelta(delta)
				}
			}
		case "response.output_text.done":
			if len(deltas) == 0 {
				if t := asString(event["text"]); t != "" {
					fallbackText = t
				}
			}
		case "response.completed", "response.done":
			if rm, ok := event["response"].(map[string]any); ok {
				responseObj = rm
			}
		}
		return false
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")

		if line == "" {
			if len(dataLines) > 0 {
				if done := handleEvent(strings.Join(dataLines, "\n")); done {
					break
				}
				dataLines = dataLines[:0]
			}
		} else if strings.HasPrefix(line, ":") {
			// Ignore comments.
		} else if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}

		if errors.Is(err, io.EOF) {
			if len(dataLines) > 0 {
				handleEvent(strings.Join(dataLines, "\n"))
			}
			break
		}
	}

	if streamErr != "" {
		return nil, fmt.Errorf("API stream error: %s", streamErr)
	}

	text := strings.Join(deltas, "")
	if text == "" {
		text = fallbackText
	}
	if responseObj == nil {
		responseObj = map[string]any{"output_text": text}
	} else if asString(responseObj["output_text"]) == "" && text != "" {
		responseObj["output_text"] = text
	}
	return responseObj, nil
}

// callCodex answers a free-form prompt (e.g. piped input to summarize) through
// Codex, with the system prompt as Codex's instructions. backend is "codex" or
// "oss".
func callCodex(cfg Config, backend string, prompt string) (map[string]any, error) {
	if backend == "codex" && cfg.Backend == "codex" {
		if bin := findCodexBinary(); bin != "" && !isCodexLoggedIn(bin) {
			return nil, fmt.Errorf("codex backend requested but not logged in. Run `codex login`.")
		}
	}
	if backend == "oss" && cfg.Model == "" {
		return nil, fmt.Errorf("oss backend requested but no model was set. Pass --model with a local Ollama or LM Studio model name.")
	}
	text, err := codexCall(cfg, backend, cfg.System, prompt, "")
	if err != nil {
		return nil, err
	}
	return map[string]any{"output_text": text, "backend": backend}, nil
}

func extractOutputText(response map[string]any) string {
	if s := asString(response["output_text"]); s != "" {
		return strings.TrimSpace(s)
	}

	output, ok := response["output"].([]any)
	if !ok {
		return ""
	}
	parts := make([]string, 0, 8)
	for _, item := range output {
		itemMap, ok := item.(map[string]any)
		if !ok {
			continue
		}
		content, ok := itemMap["content"].([]any)
		if !ok {
			continue
		}
		for _, c := range content {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			t := asString(cm["type"])
			if (t == "output_text" || t == "text") && asString(cm["text"]) != "" {
				parts = append(parts, asString(cm["text"]))
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func extractErrorMessage(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "Unknown API error"
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return raw
	}
	if errObj, ok := parsed["error"].(map[string]any); ok {
		if msg := asString(errObj["message"]); msg != "" {
			return msg
		}
		b, _ := json.Marshal(errObj)
		return string(b)
	}
	b, _ := json.Marshal(parsed)
	return string(b)
}

func extractStreamError(event map[string]any) string {
	t := asString(event["type"])
	switch t {
	case "error":
		if errObj, ok := event["error"].(map[string]any); ok {
			if msg := asString(errObj["message"]); msg != "" {
				return msg
			}
		}
		if msg := asString(event["message"]); msg != "" {
			return msg
		}
	case "response.failed":
		if resp, ok := event["response"].(map[string]any); ok {
			if errObj, ok := resp["error"].(map[string]any); ok {
				if msg := asString(errObj["message"]); msg != "" {
					return msg
				}
			}
		}
	case "response.error":
		if errObj, ok := event["error"].(map[string]any); ok {
			if msg := asString(errObj["message"]); msg != "" {
				return msg
			}
		}
	}
	return ""
}

func summarizeCodexFailure(stdout, stderr string) string {
	joined := stderr + "\n" + stdout
	linesRaw := strings.Split(joined, "\n")
	lines := make([]string, 0, len(linesRaw))
	for _, l := range linesRaw {
		l = strings.TrimSpace(l)
		if l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return "unknown codex error"
	}

	priority := []string{"not logged in", "authentication", "rate limit", "stream disconnected", "error sending request", "network"}
	for _, token := range priority {
		for i := len(lines) - 1; i >= 0; i-- {
			if strings.Contains(strings.ToLower(lines[i]), token) {
				return lines[i]
			}
		}
	}

	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(lines[i], "ERROR:") && !strings.Contains(lines[i], "shutdown rollout recorder") {
			return lines[i]
		}
	}
	return lines[len(lines)-1]
}

func findCodexBinary() string {
	if override := strings.TrimSpace(os.Getenv("AIS_CODEX_BIN")); override != "" {
		return override
	}
	path, err := exec.LookPath("codex")
	if err != nil {
		return ""
	}
	return path
}

func isCodexLoggedIn(codexBin string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, codexBin, "login", "status")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	low := strings.ToLower(string(out))
	return strings.Contains(low, "logged in")
}

var (
	headingRe    = regexp.MustCompile(`^(#{1,6})\s+(.+)$`)
	bulletRe     = regexp.MustCompile(`^(\s*)([-*+]|\d+\.)\s+(.*)$`)
	inlineCodeRe = regexp.MustCompile("`[^`\n]+`")
	boldStarRe   = regexp.MustCompile(`\*\*[^*\n]+\*\*`)
	boldUnderRe  = regexp.MustCompile(`__[^_\n]+__`)
	italicRe     = regexp.MustCompile(`\*[^*\n]+\*`)
)

func renderMarkdownANSI(text string) string {
	lines := strings.Split(text, "\n")
	rendered := make([]string, 0, len(lines))
	inCode := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inCode = !inCode
			rendered = append(rendered, ansi(line, ansiDim))
			continue
		}
		if inCode {
			rendered = append(rendered, ansi(line, ansiGreen))
			continue
		}

		if m := headingRe.FindStringSubmatch(line); len(m) == 3 {
			title := renderInlineMarkdown(m[2])
			rendered = append(rendered, ansi(title, ansiBold, ansiCyan))
			if len(m[1]) <= 2 {
				underlineChar := "-"
				if len(m[1]) == 1 {
					underlineChar = "="
				}
				rendered = append(rendered, ansi(strings.Repeat(underlineChar, max(runeLen(m[2]), 3)), ansiDim))
			}
			continue
		}

		if m := bulletRe.FindStringSubmatch(line); len(m) == 4 {
			style := ansiGreen
			if strings.HasSuffix(m[2], ".") {
				style = ansiCyan
			}
			rendered = append(rendered, fmt.Sprintf("%s%s %s", m[1], ansi(m[2], ansiBold, style), renderInlineMarkdown(m[3])))
			continue
		}

		if strings.HasPrefix(trimmed, ">") {
			rendered = append(rendered, ansi(renderInlineMarkdown(line), ansiDim))
			continue
		}
		rendered = append(rendered, renderInlineMarkdown(line))
	}
	return strings.Join(rendered, "\n")
}

func renderInlineMarkdown(text string) string {
	text = inlineCodeRe.ReplaceAllStringFunc(text, func(m string) string {
		if len(m) < 2 {
			return m
		}
		return ansi(m[1:len(m)-1], ansiYellow)
	})
	text = boldStarRe.ReplaceAllStringFunc(text, func(m string) string {
		if len(m) < 4 {
			return m
		}
		return ansi(m[2:len(m)-2], ansiBold)
	})
	text = boldUnderRe.ReplaceAllStringFunc(text, func(m string) string {
		if len(m) < 4 {
			return m
		}
		return ansi(m[2:len(m)-2], ansiBold)
	})
	text = italicRe.ReplaceAllStringFunc(text, func(m string) string {
		if len(m) < 3 || strings.HasPrefix(m, "**") || strings.HasSuffix(m, "**") {
			return m
		}
		return ansi(m[1:len(m)-1], ansiDim)
	})
	return text
}

func resolveRenderMode(mode string, out io.Writer) string {
	switch mode {
	case "ansi":
		return "ansi"
	case "raw":
		return "raw"
	default:
		if canUseColor(out) {
			return "ansi"
		}
		return "raw"
	}
}

func printAnswerPreamble(backend string, model string, out io.Writer) {
	if !writerIsTTY(out) {
		return
	}
	title := fmt.Sprintf("AI Response (%s, %s)", backend, model)
	divider := strings.Repeat("-", 28)
	if supportsUnicode() {
		divider = strings.Repeat("─", 28)
	}
	if canUseColor(out) {
		title = ansi(title, ansiBold, ansiCyan)
		divider = ansi(divider, ansiDim)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out)
	fmt.Fprintln(out, title)
	fmt.Fprintln(out, divider)
}

func effectiveModel(cfg Config, backend string) string {
	if strings.TrimSpace(cfg.Model) != "" {
		return strings.TrimSpace(cfg.Model)
	}
	switch backend {
	case "api":
		return defaultAPIModel
	case "codex":
		// Deliberately not the model from the user's Codex config: ais runs with
		// --ignore-user-config and defaults to a fast model of its own.
		return defaultCodexModel
	default:
		return ""
	}
}

func printModelCatalog(cfg Config, backend string, out io.Writer) {
	activeModel := effectiveModel(cfg, backend)

	fmt.Fprintf(out, "Backend: %s\n", backend)
	fmt.Fprintf(out, "Active default: %s\n", activeModel)

	switch backend {
	case "codex":
		models, live := loadCodexModels(findCodexBinary())
		fmt.Fprintln(out, "")
		if live {
			fmt.Fprintln(out, "Models available to your Codex account:")
		} else {
			fmt.Fprintln(out, "Codex models (built-in list; could not read `codex debug models`):")
		}
		for _, m := range models {
			marker := "  "
			if m.Slug == activeModel {
				marker = "* "
			}
			fmt.Fprintf(out, "  %s%-16s %s\n", marker, m.Slug, m.Description)
			if len(m.Efforts) > 0 {
				fmt.Fprintf(out, "      reasoning: %s\n", strings.Join(m.Efforts, ", "))
			}
			if m.Notice != "" {
				fmt.Fprintf(out, "      note: %s\n", m.Notice)
			}
		}
		if _, ok := findModel(models, activeModel); !ok && live {
			fmt.Fprintf(out, "\nWarning: the active model %q is not in your account's model list.\n", activeModel)
		}
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "Use one with -m, or save a default with ais --configure.")
	case "api":
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "API model picker:")
		fmt.Fprintln(out, "  ais --backend api --model gpt-6-luna \"fast and affordable\"")
		fmt.Fprintln(out, "  ais --backend api --model gpt-6-sol \"everyday workhorse\"")
		fmt.Fprintln(out, "  ais --backend api --model gpt-6-astra \"strongest\"")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "Notes:")
		fmt.Fprintln(out, "  - API backend requires OPENAI_API_KEY.")
		fmt.Fprintln(out, "  - API model names change over time; verify against current OpenAI docs if a name fails.")
	case "oss":
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "Local/free model picker:")
		fmt.Fprintln(out, "  ais --backend oss --local-provider ollama --model qwen2.5-coder:7b \"quick question\"")
		fmt.Fprintln(out, "  ais --backend oss --local-provider ollama --model llama3.1:8b \"summarize this\"")
		fmt.Fprintln(out, "  ais --backend oss --local-provider lmstudio --model <your-local-model> \"explain this\"")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "Notes:")
		fmt.Fprintln(out, "  - OSS backend runs through `codex --oss` and requires a local Ollama or LM Studio server.")
		fmt.Fprintln(out, "  - Model names come from your local provider, not from OpenAI.")
	default:
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "Auto resolution order:")
		fmt.Fprintln(out, "  1. Logged-in Codex")
		fmt.Fprintln(out, "  2. OPENAI_API_KEY")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "Use one of these to inspect choices:")
		fmt.Fprintln(out, "  ais --backend codex --list-models")
		fmt.Fprintln(out, "  ais --backend api --list-models")
		fmt.Fprintln(out, "  ais --backend oss --list-models")
	}
}

func runConfigure(cfg Config, in io.Reader, out io.Writer, errOut io.Writer) error {
	if !readerIsTTY(in) || !writerIsTTY(out) {
		return fmt.Errorf("--configure requires an interactive terminal")
	}

	reader := bufio.NewReader(in)
	inFile, _ := in.(*os.File)
	outFile, _ := out.(*os.File)
	fmt.Fprintln(out, "ais configuration")
	fmt.Fprintln(out, "")

	backend, err := chooseOption(reader, inFile, outFile, out, "Default backend", []string{
		"codex",
		"api",
		"oss",
	}, cfg.Backend)
	if err != nil {
		return err
	}

	saved := SavedConfig{Backend: backend}
	switch backend {
	case "codex":
		currentModel := ""
		if cfg.Backend == "codex" {
			currentModel = cfg.Model
		}
		models, _ := loadCodexModels(findCodexBinary())
		options := make([]string, 0, len(models)+1)
		for _, m := range models {
			options = append(options, m.Slug)
		}
		options = append(options, "custom")
		if _, ok := findModel(models, currentModel); !ok {
			if currentModel != "" {
				fmt.Fprintf(out, "Your saved model %q is no longer offered; pick a new one.\n\n", currentModel)
			}
			currentModel = defaultCodexModel
		}
		model, err := chooseOption(reader, inFile, outFile, out, "Default model (gpt-6-luna is fastest)", options, currentModel)
		if err != nil {
			return err
		}
		if model == "custom" {
			model, err = promptLine(reader, out, "Enter Codex model name", "")
			if err != nil {
				return err
			}
		}
		saved.Model = model

		efforts := []string{"low", "medium", "high", "xhigh"}
		if m, ok := findModel(models, model); ok && len(m.Efforts) > 0 {
			efforts = m.Efforts
		}
		currentEffort := defaultReasoning
		for _, e := range efforts {
			if e == cfg.ReasoningEffort {
				currentEffort = e
			}
		}
		effort, err := chooseOption(reader, inFile, outFile, out, "Reasoning effort (lower = faster, higher = more thorough)", efforts, currentEffort)
		if err != nil {
			return err
		}
		saved.ReasoningEffort = effort
	case "api":
		currentModel := ""
		if cfg.Backend == "api" {
			currentModel = cfg.Model
		}
		model, err := chooseOption(reader, inFile, outFile, out, "Default API model", []string{
			"gpt-6-luna",
			"gpt-6-sol",
			"gpt-6-astra",
			"custom",
		}, defaultChoice(currentModel, defaultAPIModel))
		if err != nil {
			return err
		}
		if model == "custom" {
			model, err = promptLine(reader, out, "Enter API model name", currentModel)
			if err != nil {
				return err
			}
		}
		saved.Model = model
	case "oss":
		currentProvider := ""
		currentModel := ""
		if cfg.Backend == "oss" {
			currentProvider = cfg.LocalProvider
			currentModel = cfg.Model
		}
		provider, err := chooseOption(reader, inFile, outFile, out, "Local provider", []string{
			"ollama",
			"lmstudio",
		}, defaultChoice(currentProvider, "ollama"))
		if err != nil {
			return err
		}
		model, err := promptLine(reader, out, "Enter local model name", defaultChoice(currentModel, "qwen2.5-coder:7b"))
		if err != nil {
			return err
		}
		saved.LocalProvider = provider
		saved.Model = model
	}

	if err := saveConfig(saved); err != nil {
		return err
	}

	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "Saved to %s\n", configPath())
	fmt.Fprintf(out, "Default backend: %s\n", saved.Backend)
	if saved.LocalProvider != "" {
		fmt.Fprintf(out, "Default local provider: %s\n", saved.LocalProvider)
	}
	if saved.Model != "" {
		fmt.Fprintf(out, "Default model: %s\n", saved.Model)
	}
	if saved.ReasoningEffort != "" {
		fmt.Fprintf(out, "Reasoning effort: %s\n", saved.ReasoningEffort)
	}
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Flags still override saved defaults for one-off runs.")
	fmt.Fprintln(out, "Example: ais what does awk do?")
	return nil
}

func chooseOption(reader *bufio.Reader, inFile *os.File, outFile *os.File, out io.Writer, label string, options []string, current string) (string, error) {
	if interactiveSelectSupported && inFile != nil && outFile != nil && readerIsTTY(inFile) && writerIsTTY(outFile) {
		return chooseOptionInteractive(inFile, outFile, label, options, current)
	}
	fmt.Fprintf(out, "%s:\n", label)
	for i, option := range options {
		marker := ""
		if option == current {
			marker = " [current]"
		}
		fmt.Fprintf(out, "  %d. %s%s\n", i+1, option, marker)
	}
	fmt.Fprint(out, "> ")

	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" && current != "" {
		return current, nil
	}

	idx, err := strconv.Atoi(line)
	if err != nil || idx < 1 || idx > len(options) {
		return "", fmt.Errorf("invalid selection for %s", strings.ToLower(label))
	}
	fmt.Fprintln(out, "")
	return options[idx-1], nil
}

// selectKeyKind is the platform-neutral result of one keypress while the
// arrow-key picker is active. Each OS reads raw input differently (stty + ANSI
// escapes on Unix, ReadConsoleInput on Windows) but funnels into these kinds.
type selectKeyKind int

const (
	selectKeyNone selectKeyKind = iota
	selectKeyUp
	selectKeyDown
	selectKeyEnter
	selectKeyCancel
	selectKeyDigit
)

type selectKey struct {
	kind  selectKeyKind
	digit int // 1-9, valid only when kind == selectKeyDigit
}

// errSelectCancelled is returned when the user aborts the picker (Esc) so the
// caller can exit cleanly instead of treating it as a hard error.
var errSelectCancelled = errors.New("selection cancelled")

func chooseOptionInteractive(inFile *os.File, outFile *os.File, label string, options []string, current string) (string, error) {
	selected := 0
	for i, option := range options {
		if option == current {
			selected = i
			break
		}
	}

	restore, err := enterSelectMode(inFile)
	if err != nil {
		return "", err
	}
	defer restore()

	// Redraw in place by moving the cursor up over the previously drawn lines and
	// clearing downward, rather than ANSI save/restore cursor (\033[s / \033[u),
	// which is unreliable on Windows consoles and caused each keypress to print a
	// fresh stacked copy of the menu.
	prevLines := 0
	render := func() {
		if prevLines > 0 {
			fmt.Fprintf(outFile, "\033[%dA", prevLines)
		}
		fmt.Fprint(outFile, "\r\033[J")
		lines := make([]string, 0, len(options)+2)
		lines = append(lines, label+":")
		lines = append(lines, "  arrows/jk move, Enter confirms, 1-9 picks")
		for i, option := range options {
			line := fmt.Sprintf("  %d. %s", i+1, option)
			if option == current {
				line += " [current]"
			}
			if i == selected {
				if canUseColor(outFile) {
					line = "  " + ansi("› "+strings.TrimSpace(line), ansiBold, ansiCyan)
				} else {
					line = "  > " + strings.TrimSpace(line)
				}
			}
			lines = append(lines, line)
		}
		fmt.Fprint(outFile, strings.Join(lines, "\r\n"))
		fmt.Fprint(outFile, "\r\n")
		prevLines = len(lines)
	}

	render()
	for {
		key, err := readSelectKey(inFile)
		if err != nil {
			return "", err
		}
		switch key.kind {
		case selectKeyUp:
			if selected > 0 {
				selected--
				render()
			}
		case selectKeyDown:
			if selected < len(options)-1 {
				selected++
				render()
			}
		case selectKeyEnter:
			fmt.Fprintln(outFile)
			return options[selected], nil
		case selectKeyCancel:
			fmt.Fprintln(outFile)
			return "", errSelectCancelled
		case selectKeyDigit:
			idx := key.digit - 1
			if idx >= 0 && idx < len(options) {
				fmt.Fprintln(outFile)
				return options[idx], nil
			}
		}
	}
}

func promptLine(reader *bufio.Reader, out io.Writer, label string, current string) (string, error) {
	if current != "" {
		fmt.Fprintf(out, "%s [%s]: ", label, current)
	} else {
		fmt.Fprintf(out, "%s: ", label)
	}

	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		if current == "" {
			return "", fmt.Errorf("%s cannot be empty", strings.ToLower(label))
		}
		return current, nil
	}
	fmt.Fprintln(out, "")
	return line, nil
}

func defaultChoice(current string, fallback string) string {
	if strings.TrimSpace(current) != "" {
		return strings.TrimSpace(current)
	}
	return fallback
}

// validReasoningEffort accepts every effort level current OpenAI models use.
// Which levels a given model supports varies (see --list-models); an
// unsupported one is reported by the model service.
func validReasoningEffort(effort string) bool {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
		return true
	}
	return false
}

// isReasoningModel reports whether an API model takes a reasoning effort (and
// rejects temperature): the gpt-5+ and o-series families.
func isReasoningModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if strings.HasPrefix(m, "o") && len(m) > 1 && m[1] >= '0' && m[1] <= '9' {
		return true
	}
	if rest, ok := strings.CutPrefix(m, "gpt-"); ok {
		major := rest
		if i := strings.IndexAny(rest, ".-"); i >= 0 {
			major = rest[:i]
		}
		n, err := strconv.Atoi(major)
		return err == nil && n >= 5
	}
	return false
}

// codexReasoningArgs returns the `-c model_reasoning_effort="..."` override for a
// codex invocation, or nil if the effort is unset/invalid (leaving Codex's own
// config default in place).
func codexReasoningArgs(effort string) []string {
	if !validReasoningEffort(effort) {
		return nil
	}
	return []string{"-c", fmt.Sprintf("model_reasoning_effort=%q", strings.ToLower(strings.TrimSpace(effort)))}
}

func loadSavedConfig() SavedConfig {
	path := configPath()
	if path == "" {
		return SavedConfig{}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return SavedConfig{}
	}
	var cfg SavedConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return SavedConfig{}
	}
	return cfg
}

func saveConfig(cfg SavedConfig) error {
	path := configPath()
	if path == "" {
		return fmt.Errorf("could not determine config path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

func configPath() string {
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "ais", "config.json")
	}
	home := homeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "ais", "config.json")
}

// homeDir returns the current user's home directory in an OS-appropriate way
// ($HOME on Unix, %USERPROFILE% on Windows), or "" if it cannot be resolved.
func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(home)
}

func ansi(text string, styles ...string) string {
	if len(styles) == 0 {
		return text
	}
	return strings.Join(styles, "") + text + ansiReset
}

func canUseColor(out io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("CLICOLOR_FORCE") == "1" {
		return true
	}
	if !writerIsTTY(out) {
		return false
	}
	return strings.ToLower(os.Getenv("TERM")) != "dumb"
}

func supportsUnicode() bool {
	if runtime.GOOS == "windows" {
		return true
	}
	lang := strings.ToUpper(os.Getenv("LC_ALL") + " " + os.Getenv("LANG"))
	return strings.Contains(lang, "UTF-8") || strings.Contains(lang, "UTF8")
}

type Spinner struct {
	label   string
	enabled bool
	out     io.Writer

	mu      sync.Mutex
	stopCh  chan struct{}
	doneCh  chan struct{}
	started bool
	lastLen int
}

func NewSpinner(label string, enabled bool, out io.Writer) *Spinner {
	return &Spinner{label: label, enabled: enabled, out: out}
}

func (s *Spinner) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled || s.started {
		return
	}
	s.started = true
	s.stopCh = make(chan struct{})
	s.doneCh = make(chan struct{})

	frames := []string{"|", "/", "-", "\\"}
	if supportsUnicode() {
		frames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	}
	start := time.Now()

	go func() {
		defer close(s.doneCh)
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()
		i := 0
		for {
			select {
			case <-s.stopCh:
				return
			case <-ticker.C:
				elapsed := time.Since(start).Seconds()
				msg := fmt.Sprintf("\r%s %s %4.1fs", frames[i%len(frames)], s.label, elapsed)
				i++
				s.mu.Lock()
				s.lastLen = max(s.lastLen, runeLen(stripANSI(msg)))
				s.mu.Unlock()
				_, _ = io.WriteString(s.out, msg)
			}
		}
	}()
}

func (s *Spinner) Stop() {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	close(s.stopCh)
	done := s.doneCh
	s.started = false
	last := s.lastLen
	s.lastLen = 0
	s.mu.Unlock()

	<-done
	clear := "\r" + strings.Repeat(" ", last) + "\r"
	_, _ = io.WriteString(s.out, clear)
}

var ansiStripRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(text string) string {
	return ansiStripRe.ReplaceAllString(text, "")
}

func readerIsTTY(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func writerIsTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func envOr(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}

func envInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func envBool(key string, fallback bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if v == "" {
		return fallback
	}
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func readFileTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func firstNonEmpty(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func runeLen(s string) int {
	return len([]rune(s))
}
