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
	defaultAPIModel      = "gpt-4.1-mini"
	defaultBackend       = "auto"
	defaultRenderMode    = "auto"
	defaultSystemPrompt  = "You are a practical terminal assistant. Keep answers concise and clear. Use short sections and bullets where useful. Avoid markdown tables."
	defaultMaxInputChars = 120000
	defaultTruncateMode  = "head"
	version              = "0.3.0"
)

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
}

type SavedConfig struct {
	Backend       string `json:"backend,omitempty"`
	Model         string `json:"model,omitempty"`
	LocalProvider string `json:"local_provider,omitempty"`
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
			result, err = callCodex(cfg, prompt, true, onDelta)
		} else {
			result, err = callAPI(cfg, prompt, true, onDelta)
		}
	} else {
		spinner := NewSpinner(fmt.Sprintf("Thinking via %s (%s)", backend, activeModel), spinnerEnabled, errOut)
		spinner.Start()
		if backend == "codex" || backend == "oss" {
			result, err = callCodex(cfg, prompt, false, nil)
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
		MaxOutputTokens: 700,
		Temperature:     0.2,
		Render:          envOr("AIS_RENDER", defaultRenderMode),
		Stream:          envBool("AIS_STREAM", true),
		MaxInputChars:   envInt("AIS_MAX_INPUT_CHARS", defaultMaxInputChars),
		TruncateMode:    envOr("AIS_TRUNCATE", defaultTruncateMode),
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

	cfg.PromptWords = fs.Args()
	return cfg, nil
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

	payload := map[string]any{
		"model":             firstNonEmpty(cfg.Model, defaultAPIModel),
		"instructions":      cfg.System,
		"input":             prompt,
		"max_output_tokens": cfg.MaxOutputTokens,
		"temperature":       cfg.Temperature,
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

func callCodex(cfg Config, prompt string, stream bool, onDelta func(string)) (map[string]any, error) {
	codexBin := findCodexBinary()
	if codexBin == "" {
		return nil, fmt.Errorf("codex backend requested but `codex` is not installed. Install Codex CLI or use --backend api.")
	}
	if cfg.Backend == "codex" && !isCodexLoggedIn(codexBin) {
		return nil, fmt.Errorf("codex backend requested but not logged in. Run `codex login`.")
	}
	if cfg.Backend == "oss" && cfg.Model == "" {
		return nil, fmt.Errorf("oss backend requested but no model was set. Pass --model with a local Ollama or LM Studio model name.")
	}

	tmpFile, err := os.CreateTemp("", "ais-codex-*.txt")
	if err != nil {
		return nil, err
	}
	outputPath := tmpFile.Name()
	_ = tmpFile.Close()
	defer os.Remove(outputPath)

	combinedPrompt := "System instructions:\n" + cfg.System + "\n\nUser request:\n" + prompt

	if !stream {
		args := []string{"exec", "--skip-git-repo-check", "-o", outputPath}
		if cfg.Backend == "oss" {
			args = append(args, "--oss")
			if cfg.LocalProvider != "" {
				args = append(args, "--local-provider", cfg.LocalProvider)
			}
		}
		if cfg.Model != "" {
			args = append(args, "-m", cfg.Model)
		}
		args = append(args, combinedPrompt)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, codexBin, args...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()

		text := readFileTrim(outputPath)
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("codex backend timed out.")
		}
		if err != nil {
			return nil, fmt.Errorf("codex backend failed: %s", summarizeCodexFailure(stdout.String(), stderr.String()))
		}
		if text == "" {
			return nil, fmt.Errorf("codex backend returned empty output.")
		}
		return map[string]any{"output_text": text, "backend": "codex"}, nil
	}

	args := []string{"exec", "--skip-git-repo-check", "--json", "-o", outputPath}
	if cfg.Backend == "oss" {
		args = append(args, "--oss")
		if cfg.LocalProvider != "" {
			args = append(args, "--local-provider", cfg.LocalProvider)
		}
	}
	if cfg.Model != "" {
		args = append(args, "-m", cfg.Model)
	}
	args = append(args, combinedPrompt)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, codexBin, args...)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	stderrLines := make([]string, 0, 32)
	var stderrMu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s := bufio.NewScanner(stderrPipe)
		for s.Scan() {
			stderrMu.Lock()
			stderrLines = append(stderrLines, strings.TrimSpace(s.Text()))
			stderrMu.Unlock()
		}
	}()

	deltas := make([]string, 0, 64)
	codexErrors := make([]string, 0, 8)
	sawTurnFailed := false

	s := bufio.NewScanner(stdoutPipe)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}

		if msg := extractCodexEventError(event); msg != "" {
			codexErrors = append(codexErrors, msg)
			if asString(event["type"]) == "turn.failed" {
				sawTurnFailed = true
			}
		}

		if delta := extractCodexTextDelta(event); delta != "" {
			deltas = append(deltas, delta)
			if onDelta != nil {
				onDelta(delta)
			}
		}
	}

	runErr := cmd.Wait()
	wg.Wait()

	text := readFileTrim(outputPath)
	if text == "" && len(deltas) > 0 {
		text = strings.TrimSpace(strings.Join(deltas, ""))
	}

	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("codex backend timed out.")
	}

	stderrMu.Lock()
	stderrJoined := strings.Join(stderrLines, "\n")
	stderrMu.Unlock()

	if sawTurnFailed || (runErr != nil && text == "") {
		details := chooseCodexError(codexErrors)
		if details == "" {
			details = summarizeCodexFailure("", stderrJoined)
		}
		if details == "" {
			details = "unknown codex error"
		}
		return nil, fmt.Errorf("codex backend failed: %s", details)
	}

	if text == "" {
		if details := chooseCodexError(codexErrors); details != "" {
			return nil, fmt.Errorf("codex backend failed: %s", details)
		}
		return nil, fmt.Errorf("codex backend returned empty output.")
	}

	return map[string]any{"output_text": text, "backend": "codex"}, nil
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

func extractCodexEventError(event map[string]any) string {
	t := asString(event["type"])
	switch t {
	case "error":
		msg := asString(event["message"])
		if strings.Contains(msg, "Failed to shutdown rollout recorder") {
			return ""
		}
		return msg
	case "turn.failed":
		if errObj, ok := event["error"].(map[string]any); ok {
			if msg := asString(errObj["message"]); msg != "" {
				return msg
			}
		}
	}
	return ""
}

func extractCodexTextDelta(event map[string]any) string {
	t := asString(event["type"])
	switch t {
	case "agent_message_delta", "agent_message.delta", "assistant_message.delta", "response.output_text.delta":
		if s := asString(event["delta"]); s != "" {
			return s
		}
		if dm, ok := event["delta"].(map[string]any); ok {
			if s := asString(dm["text"]); s != "" {
				return s
			}
		}
		if s := asString(event["text_delta"]); s != "" {
			return s
		}
		return asString(event["text"])
	case "item.updated":
		item, ok := event["item"].(map[string]any)
		if !ok {
			return ""
		}
		if s := asString(item["delta"]); s != "" {
			return s
		}
		if dm, ok := item["delta"].(map[string]any); ok {
			if s := asString(dm["text"]); s != "" {
				return s
			}
		}
		return asString(item["text_delta"])
	}
	return ""
}

func chooseCodexError(errors []string) string {
	if len(errors) == 0 {
		return ""
	}
	for i := len(errors) - 1; i >= 0; i-- {
		if strings.Contains(errors[i], "Failed to shutdown rollout recorder") {
			continue
		}
		if strings.TrimSpace(errors[i]) != "" {
			return errors[i]
		}
	}
	return errors[len(errors)-1]
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

func renderMarkdownANSI(text string) string {
	lines := strings.Split(text, "\n")
	rendered := make([]string, 0, len(lines))
	inCode := false
	headingRe := regexp.MustCompile(`^(#{1,6})\s+(.+)$`)
	bulletRe := regexp.MustCompile(`^(\s*)([-*+]|\d+\.)\s+(.*)$`)

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
	codeRe := regexp.MustCompile("`[^`\n]+`")
	boldA := regexp.MustCompile(`\*\*[^*\n]+\*\*`)
	boldB := regexp.MustCompile(`__[^_\n]+__`)
	italic := regexp.MustCompile(`\*[^*\n]+\*`)

	text = codeRe.ReplaceAllStringFunc(text, func(m string) string {
		if len(m) < 2 {
			return m
		}
		return ansi(m[1:len(m)-1], ansiYellow)
	})
	text = boldA.ReplaceAllStringFunc(text, func(m string) string {
		if len(m) < 4 {
			return m
		}
		return ansi(m[2:len(m)-2], ansiBold)
	})
	text = boldB.ReplaceAllStringFunc(text, func(m string) string {
		if len(m) < 4 {
			return m
		}
		return ansi(m[2:len(m)-2], ansiBold)
	})
	text = italic.ReplaceAllStringFunc(text, func(m string) string {
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
	case "codex", "oss":
		if model := readCodexConfigModel(); model != "" {
			return model
		}
		return "codex-default"
	default:
		return "default"
	}
}

func printModelCatalog(cfg Config, backend string, out io.Writer) {
	activeModel := effectiveModel(cfg, backend)

	fmt.Fprintf(out, "Backend: %s\n", backend)
	fmt.Fprintf(out, "Active default: %s\n", activeModel)

	switch backend {
	case "codex":
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "Codex model picker:")
		fmt.Fprintln(out, "  ais --backend codex --model gpt-5.1-codex-mini \"quick question\"")
		fmt.Fprintln(out, "  ais --backend codex --model gpt-5.1-codex \"normal coding task\"")
		fmt.Fprintln(out, "  ais --backend codex --model gpt-5.2-codex \"stronger latest codex model\"")
		fmt.Fprintln(out, "  ais --backend codex --model gpt-5.1-codex-max \"hard repo change\"")
		fmt.Fprintln(out, "  ais --backend codex --model gpt-5-codex \"older codex model\"")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "Notes:")
		fmt.Fprintln(out, "  - If --model is omitted, ais uses its saved default first, then Codex config when present.")
		fmt.Fprintln(out, "  - Codex CLI accepts arbitrary model strings, but the server decides whether your account can actually run them.")
	case "api":
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "API model picker:")
		fmt.Fprintln(out, "  ais --backend api --model gpt-4.1-mini \"quick question\"")
		fmt.Fprintln(out, "  ais --backend api --model gpt-5-mini \"better quality, still fast\"")
		fmt.Fprintln(out, "  ais --backend api --model gpt-5 \"stronger general model\"")
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

func readCodexConfigModel() string {
	home := homeDir()
	if home == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			break
		}
		if strings.HasPrefix(trimmed, "model") {
			parts := strings.SplitN(trimmed, "=", 2)
			if len(parts) != 2 {
				continue
			}
			return strings.Trim(strings.TrimSpace(parts[1]), "\"'")
		}
	}
	return ""
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
		model, err := chooseOption(reader, inFile, outFile, out, "Default Codex model", []string{
			"gpt-5.2-codex",
			"gpt-5.1-codex-mini",
			"gpt-5.1-codex",
			"gpt-5.1-codex-max",
			"gpt-5-codex",
			"custom",
		}, defaultChoice(currentModel, "gpt-5.1-codex-mini"))
		if err != nil {
			return err
		}
		if model == "custom" {
			model, err = promptLine(reader, out, "Enter Codex model name", currentModel)
			if err != nil {
				return err
			}
		}
		saved.Model = model
	case "api":
		currentModel := ""
		if cfg.Backend == "api" {
			currentModel = cfg.Model
		}
		model, err := chooseOption(reader, inFile, outFile, out, "Default API model", []string{
			"gpt-4.1-mini",
			"gpt-5-mini",
			"gpt-5",
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

	fmt.Fprint(outFile, "\033[s")
	render := func() {
		fmt.Fprint(outFile, "\033[u\r\033[J")
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
	}

	render()
	buf := make([]byte, 3)
	for {
		n, err := inFile.Read(buf[:1])
		if err != nil {
			return "", err
		}
		if n == 0 {
			continue
		}
		b := buf[0]
		switch b {
		case '\r', '\n':
			fmt.Fprintln(outFile)
			return options[selected], nil
		case 'k':
			if selected > 0 {
				selected--
				render()
			}
		case 'j':
			if selected < len(options)-1 {
				selected++
				render()
			}
		case 27:
			_, err := inFile.Read(buf[1:3])
			if err != nil {
				return "", err
			}
			if buf[1] == '[' {
				switch buf[2] {
				case 'A':
					if selected > 0 {
						selected--
						render()
					}
				case 'B':
					if selected < len(options)-1 {
						selected++
						render()
					}
				}
			}
		default:
			if b >= '1' && b <= '9' {
				idx := int(b - '1')
				if idx >= 0 && idx < len(options) {
					fmt.Fprintln(outFile)
					return options[idx], nil
				}
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

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
