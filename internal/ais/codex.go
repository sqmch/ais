package ais

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// codexDisabledFeatures are Codex features ais turns off for its own calls. ais
// only needs a single model reply, so loading the user's plugins, apps, skills,
// memories, and agent tools just adds startup time and ~10k prompt tokens per
// call. They are passed as `-c features.<name>=false` rather than `--disable`,
// because Codex rejects unknown `--disable` names but ignores unknown config
// keys — so this list stays safe across Codex versions.
var codexDisabledFeatures = []string{
	"apps", "plugins", "remote_plugin", "memories", "multi_agent", "goals",
	"hooks", "image_generation", "browser_use", "browser_use_external",
	"computer_use", "in_app_browser", "skill_search", "tool_suggest",
	"unified_exec", "shell_tool", "shell_snapshot", "view_image", "sleep_tool",
	"personality", "collaboration_modes", "workspace_dependencies",
}

// codexExecArgs builds the `codex exec` arguments shared by every ais call.
//
// Isolation: all overrides are per-invocation flags, so ais never changes the
// user's own Codex setup. `--ephemeral` keeps ais calls out of Codex's session
// history, and on the codex backend `--ignore-user-config` stops the user's
// config.toml (default model, notify hooks, MCP servers, personality) from
// leaking into ais. The oss backend keeps user config, since local provider
// settings (e.g. a custom Ollama URL) live there.
//
// lean=false drops the newer flags, as a fallback for older Codex versions.
func codexExecArgs(cfg Config, backend string, instructionsPath string, lean bool) []string {
	args := []string{"exec", "--skip-git-repo-check", "--sandbox", "read-only", "--color", "never"}
	if lean {
		args = append(args, "--ephemeral", "--ignore-rules")
		if backend != "oss" {
			args = append(args, "--ignore-user-config")
		}
		for _, f := range codexDisabledFeatures {
			args = append(args, "-c", "features."+f+"=false")
		}
		// Do not send the current repo's AGENTS.md along with every request.
		args = append(args, "-c", "project_doc_max_bytes=0")
		if instructionsPath != "" {
			// Replace Codex's long coding-agent system prompt with ais's own.
			args = append(args, "-c", fmt.Sprintf("model_instructions_file=%q", instructionsPath))
		}
	} else {
		args = append(args, "--disable", "shell_tool")
	}
	args = append(args, codexReasoningArgs(cfg.ReasoningEffort)...)
	if backend == "oss" {
		args = append(args, "--oss")
		if cfg.LocalProvider != "" {
			args = append(args, "--local-provider", cfg.LocalProvider)
		}
	}
	if model := effectiveModel(cfg, backend); model != "" {
		args = append(args, "-m", model)
	}
	return args
}

// writeTempFile writes content to a new temp file and returns its path plus a
// cleanup func. An empty content returns "" and a no-op cleanup.
func writeTempFile(pattern string, content string) (string, func(), error) {
	if content == "" {
		return "", func() {}, nil
	}
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", func() {}, err
	}
	path := f.Name()
	_, werr := f.WriteString(content)
	cerr := f.Close()
	cleanup := func() { _ = os.Remove(path) }
	if werr != nil || cerr != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("could not write temp file: %v", firstErr(werr, cerr))
	}
	return path, cleanup, nil
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// isOldCodexArgError reports whether codex failed because it does not know one
// of the newer flags ais passes, so the caller can retry with lean=false.
func isOldCodexArgError(stderr string) bool {
	low := strings.ToLower(stderr)
	return strings.Contains(low, "unexpected argument") || strings.Contains(low, "unknown feature")
}

// codexCall runs one non-interactive Codex call and returns the model's final
// message. instructions replace Codex's own system prompt; schemaJSON, if set,
// constrains the reply to that JSON schema. Codex's shell tool is disabled, so
// the model can only return text. backend is "codex" or "oss".
func codexCall(cfg Config, backend string, instructions string, prompt string, schemaJSON string) (string, error) {
	codexBin := findCodexBinary()
	if codexBin == "" {
		return "", fmt.Errorf("this needs the Codex CLI. Install it, then run `codex login` (or use --backend api)")
	}
	schemaPath, cleanSchema, err := writeTempFile("ais-schema-*.json", schemaJSON)
	if err != nil {
		return "", err
	}
	defer cleanSchema()
	instructionsPath, cleanInstructions, err := writeTempFile("ais-instructions-*.md", instructions)
	if err != nil {
		return "", err
	}
	defer cleanInstructions()
	outPath, cleanOut, err := writeTempFile("ais-out-*.txt", " ")
	if err != nil {
		return "", err
	}
	defer cleanOut()

	run := func(lean bool) (string, string, error) {
		_ = os.WriteFile(outPath, nil, 0o600)
		args := codexExecArgs(cfg, backend, instructionsPath, lean)
		if schemaPath != "" {
			args = append(args, "--output-schema", schemaPath)
		}
		fullPrompt := prompt
		if !lean && instructions != "" {
			// Older Codex: no instructions override, so prepend them instead.
			fullPrompt = instructions + "\n\n" + prompt
		}
		args = append(args, "-o", outPath, fullPrompt)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, codexBin, args...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		runErr := cmd.Run()
		if ctx.Err() == context.DeadlineExceeded {
			return "", "", fmt.Errorf("codex timed out")
		}
		raw := readFileTrim(outPath)
		if raw == "" {
			if runErr != nil {
				return "", stderr.String(), fmt.Errorf("codex failed: %s", summarizeCodexFailure(stdout.String(), stderr.String()))
			}
			return "", stderr.String(), fmt.Errorf("codex returned an empty response")
		}
		return raw, "", nil
	}

	raw, stderr, err := run(true)
	if err != nil && isOldCodexArgError(stderr) {
		raw, _, err = run(false)
	}
	return raw, err
}
