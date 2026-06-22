package ais

import (
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

func TestLooksLikeCommand(t *testing.T) {
	// Requests that should run as commands (inspect or change this computer).
	commands := []string{
		"what is listening on port 3000",
		"kill the process listening on port 8080",
		"kill the server running on port 8080",
		"free port 3000",
		"is nginx running",
		"whats running on port 5173",
		"which process uses the most memory",
		"list the files in this folder",
		"show me the current date and time",
		"delete node_modules",
		"git status",
		"restart the docker daemon",
		"what's my ip address",
		"install ripgrep",
		"find all .log files here",
		"do i have python installed",
		"remove the dist directory",
	}
	for _, c := range commands {
		if !looksLikeCommand(c) {
			t.Errorf("looksLikeCommand(%q) = false, want true", c)
		}
	}

	// Requests that should be answered as questions (general knowledge / how-to).
	questions := []string{
		"how do I split a window in nvim",
		"what does chmod 755 mean",
		"explain this error: permission denied",
		"why is my build slow",
		"what is the difference between tcp and udp",
		"how to write a for loop in bash",
		"show me how to use grep",
		"tell me about rust ownership",
		"what are the benefits of docker",
		"hello",
	}
	for _, q := range questions {
		if looksLikeCommand(q) {
			t.Errorf("looksLikeCommand(%q) = true, want false", q)
		}
	}
}

func TestValidReasoningEffort(t *testing.T) {
	for _, ok := range []string{"minimal", "low", "Medium", "HIGH", " low "} {
		if !validReasoningEffort(ok) {
			t.Errorf("validReasoningEffort(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "ultra", "none", "lowish"} {
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

func TestBuildAgentPrompt(t *testing.T) {
	shell := shellInfo{bin: "powershell", args: nil, label: "PowerShell"}

	// Leads with the request and decouples planning from execution.
	p := buildAgentPrompt("kill the process on port 8080", shell, false)
	if !strings.HasPrefix(p, "kill the process on port 8080") {
		t.Errorf("prompt should lead with the request, got: %q", p[:min(40, len(p))])
	}
	for _, want := range []string{"do NOT execute", "`command`", "PowerShell", "exactly ONE"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}

	// Strict mode adds the mandatory-command pressure.
	ps := buildAgentPrompt("kill the process on port 8080", shell, true)
	if !strings.Contains(ps, "MUST be a single") {
		t.Errorf("strict prompt should add the MUST-be-single instruction")
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
