package ais

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestParseAgentPlan(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantCmd string
		wantErr bool
	}{
		{
			name:    "bare command object",
			raw:     `{"command":"Get-ChildItem","explanation":"Lists files","danger":"low"}`,
			wantCmd: "Get-ChildItem",
		},
		{
			name:    "empty command",
			raw:     `{"command":"","explanation":"Nothing to do","danger":"low"}`,
			wantCmd: "",
		},
		{
			name:    "wrapped in prose",
			raw:     "Here you go:\n{\"command\":\"ls\",\"explanation\":\"x\",\"danger\":\"low\"}\nThanks!",
			wantCmd: "ls",
		},
		{
			name:    "not json",
			raw:     "I cannot do that",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := parseAgentPlan(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got plan %+v", plan)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if strings.TrimSpace(plan.Command) != tt.wantCmd {
				t.Errorf("command = %q, want %q", plan.Command, tt.wantCmd)
			}
		})
	}
}

func TestValidCommand(t *testing.T) {
	valid := []string{
		"Get-ChildItem",
		"Stop-Process -Id (Get-NetTCPConnection -LocalPort 8080).OwningProcess -Force",
		"netstat -ano | findstr :3000",
	}
	for _, c := range valid {
		if !validCommand(c) {
			t.Errorf("validCommand(%q) = false, want true", c)
		}
	}
	invalid := []string{
		"",
		"   ",
		"Get-Process -Id <OwningProcess>",
		"Get-Process -Id <pid>",
		"kill <PID>",
	}
	for _, c := range invalid {
		if validCommand(c) {
			t.Errorf("validCommand(%q) = true, want false", c)
		}
	}
}

func TestValidReasoningEffort(t *testing.T) {
	for _, ok := range []string{"minimal", "low", "Medium", "HIGH", " low ", "xhigh", "max", "ultra", "none"} {
		if !validReasoningEffort(ok) {
			t.Errorf("validReasoningEffort(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "lowish", "extreme"} {
		if validReasoningEffort(bad) {
			t.Errorf("validReasoningEffort(%q) = true, want false", bad)
		}
	}
}

func TestCodexReasoningArgs(t *testing.T) {
	got := codexReasoningArgs("low")
	if len(got) != 2 || got[0] != "-c" || got[1] != `model_reasoning_effort="low"` {
		t.Errorf("codexReasoningArgs(low) = %v", got)
	}
	if codexReasoningArgs("bogus") != nil {
		t.Errorf("codexReasoningArgs(bogus) should be nil")
	}
}

func TestIsReasoningModel(t *testing.T) {
	for _, m := range []string{"gpt-6-luna", "gpt-5", "gpt-5.5", "gpt-5-mini", "GPT-6-Sol", "o3", "o4-mini"} {
		if !isReasoningModel(m) {
			t.Errorf("isReasoningModel(%q) = false, want true", m)
		}
	}
	for _, m := range []string{"gpt-4.1-mini", "gpt-4o", "", "llama3.1:8b", "omni"} {
		if isReasoningModel(m) {
			t.Errorf("isReasoningModel(%q) = true, want false", m)
		}
	}
}

func hasArgPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func TestCodexExecArgsIsolation(t *testing.T) {
	cfg := Config{ReasoningEffort: "low"}
	args := codexExecArgs(cfg, "codex", "/tmp/instr.md", true)
	joined := strings.Join(args, " ")
	for _, want := range []string{"--ephemeral", "--ignore-user-config", "--ignore-rules"} {
		if !strings.Contains(joined, want) {
			t.Errorf("lean codex args missing %s: %v", want, args)
		}
	}
	if !hasArgPair(args, "-c", "features.shell_tool=false") {
		t.Errorf("lean args must disable the shell tool: %v", args)
	}
	if !hasArgPair(args, "-m", defaultCodexModel) {
		t.Errorf("codex args should default to %s: %v", defaultCodexModel, args)
	}
	if !strings.Contains(joined, "model_instructions_file=") {
		t.Errorf("lean args should override instructions: %v", args)
	}
	// Overrides must be per-invocation only: never --disable (which errors on
	// unknown names in other Codex versions) in lean mode.
	for _, a := range args {
		if a == "--disable" {
			t.Errorf("lean args should use -c features.x=false, not --disable: %v", args)
		}
	}

	// oss keeps the user's config (local provider settings live there).
	oss := codexExecArgs(Config{Model: "qwen", LocalProvider: "ollama"}, "oss", "", true)
	if strings.Contains(strings.Join(oss, " "), "--ignore-user-config") {
		t.Errorf("oss args should not ignore user config: %v", oss)
	}
	if !hasArgPair(oss, "-m", "qwen") || !hasArgPair(oss, "--local-provider", "ollama") {
		t.Errorf("oss args missing model/provider: %v", oss)
	}

	// Fallback for older Codex keeps only long-standing flags.
	old := strings.Join(codexExecArgs(cfg, "codex", "/tmp/instr.md", false), " ")
	for _, notWant := range []string{"--ephemeral", "--ignore-user-config", "model_instructions_file"} {
		if strings.Contains(old, notWant) {
			t.Errorf("non-lean args should not contain %s: %s", notWant, old)
		}
	}
}

func TestParseCodexModels(t *testing.T) {
	raw := []byte(`{"models":[
	  {"slug":"gpt-5.5","description":"Legacy.","visibility":"list","priority":12,
	   "supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}],
	   "upgrade":{"model":"gpt-5.6-sol","migration_markdown":"GPT-5.5 retires soon."}},
	  {"slug":"codex-auto-review","visibility":"hide","priority":43},
	  {"slug":"gpt-6-luna","description":"Fast.","visibility":"list","priority":3,
	   "default_reasoning_level":"medium","supported_reasoning_levels":[{"effort":"low"},{"effort":"max"}]}
	]}`)
	models := parseCodexModels(raw)
	if len(models) != 2 {
		t.Fatalf("want 2 visible models, got %d: %+v", len(models), models)
	}
	if models[0].Slug != "gpt-6-luna" || models[1].Slug != "gpt-5.5" {
		t.Errorf("models should be sorted by priority: %+v", models)
	}
	if strings.Join(models[0].Efforts, ",") != "low,max" {
		t.Errorf("efforts = %v", models[0].Efforts)
	}
	if models[1].Notice != "GPT-5.5 retires soon." {
		t.Errorf("notice = %q", models[1].Notice)
	}
	if parseCodexModels([]byte("not json")) != nil {
		t.Errorf("bad JSON should yield nil")
	}
}

func TestTailBuffer(t *testing.T) {
	tb := &tailBuffer{max: 5}
	_, _ = tb.Write([]byte("abc"))
	_, _ = tb.Write([]byte("defgh"))
	if got := tb.String(); got != "defgh" {
		t.Errorf("tail = %q, want %q", got, "defgh")
	}
}

func TestBuildAgentPrompt(t *testing.T) {
	shell := shellInfo{bin: "powershell", args: nil, label: "PowerShell"}
	cfg := Config{}

	// Leads with the request, lets the model choose command vs answer.
	p := buildAgentPrompt(cfg, planRequest{task: "kill the process on port 8080"}, shell)
	if !strings.HasPrefix(p, "kill the process on port 8080") {
		t.Errorf("prompt should lead with the request, got: %q", p[:min(40, len(p))])
	}
	for _, want := range []string{"You do not execute", "`command`", "PowerShell", "Exactly ONE", "Choose `kind`"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}

	// -a forces a command.
	pf := buildAgentPrompt(cfg, planRequest{task: "x", forceCommand: true}, shell)
	if strings.Contains(pf, "Choose `kind`") || !strings.Contains(pf, `Set `+"`kind`"+` to "command"`) {
		t.Errorf("forced prompt should require kind=command")
	}

	// Strict mode adds the mandatory-command pressure.
	ps := buildAgentPrompt(cfg, planRequest{task: "x", strict: true}, shell)
	if !strings.Contains(ps, "MUST be a single") {
		t.Errorf("strict prompt should add the MUST-be-single instruction")
	}

	// A fix request carries the failed command and its error output.
	pfix := buildAgentPrompt(cfg, planRequest{task: "x", forceCommand: true, failed: &failedRun{command: "gti status", exitCode: 1, stderr: "gti: not recognized"}}, shell)
	for _, want := range []string{"gti status", "Exit code: 1", "gti: not recognized"} {
		if !strings.Contains(pfix, want) {
			t.Errorf("fix prompt missing %q", want)
		}
	}
}

func TestSanitizeAgentNote(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{
			in:   "I can't execute shell commands in this read-only tool context. Lists the process on port 3000.",
			want: "Lists the process on port 3000.",
		},
		{
			in:   "Lists the files in the current folder.",
			want: "Lists the files in the current folder.",
		},
		{
			in:   "I don't have a shell execution tool available in this session.",
			want: "",
		},
		{
			// Curly apostrophe (’) must still match the disclaimer markers.
			in:   "I can’t inspect the local machine directly from the tools available in this chat. Lists the listener on port 3000.",
			want: "Lists the listener on port 3000.",
		},
		{in: "", want: ""},
	}
	for _, tt := range tests {
		got := sanitizeAgentNote(tt.in)
		if got != tt.want {
			t.Errorf("sanitizeAgentNote(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestConfirmCommand(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantCmd string
		wantRun bool
	}{
		{"yes", "y\n", "git log -3", true},
		{"default no", "\n", "git log -3", false},
		{"eof is no", "", "git log -3", false},
		{"edit then run", "e\ngit log -5 --oneline\ny\n", "git log -5 --oneline", true},
		{"edit keep current", "e\n\nyes\n", "git log -3", true},
		{"edit then decline", "e\ngit log -5\nn\n", "git log -5", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			cmd, run, err := confirmCommand(bufio.NewReader(strings.NewReader(tt.input)), &out, "git log -3")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cmd != tt.wantCmd || run != tt.wantRun {
				t.Errorf("got (%q, %v), want (%q, %v)", cmd, run, tt.wantCmd, tt.wantRun)
			}
		})
	}
}
