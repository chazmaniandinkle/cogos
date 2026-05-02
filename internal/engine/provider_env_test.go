package engine

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestProviderChildEnv_StripsIngressVars(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "http://localhost:6931")

	env := providerChildEnv(nil)

	for _, kv := range env {
		if strings.HasPrefix(kv, "ANTHROPIC_BASE_URL=") {
			t.Errorf("ANTHROPIC_BASE_URL must be stripped from provider child env, got %q", kv)
		}
	}
}

func TestProviderChildEnv_PreservesUnrelatedVars(t *testing.T) {
	t.Parallel()

	path := os.Getenv("PATH")
	if path == "" {
		t.Skip("PATH not set in test environment")
	}

	env := providerChildEnv(nil)

	found := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			found = true
			break
		}
	}
	if !found {
		t.Error("PATH must be preserved in provider child env")
	}
}

func TestProviderChildEnv_ExtraEnvAppendsAfterSanitization(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "http://localhost:6931")

	extra := []string{"ANTHROPIC_BASE_URL=https://api.anthropic.com"}
	env := providerChildEnv(extra)

	var last string
	for _, kv := range env {
		if strings.HasPrefix(kv, "ANTHROPIC_BASE_URL=") {
			last = kv
		}
	}
	if last != "ANTHROPIC_BASE_URL=https://api.anthropic.com" {
		t.Errorf("ExtraEnv should win; got last ANTHROPIC_BASE_URL=%q", last)
	}
}

func TestEnvPolicyInherit_PreservesEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "http://localhost:6931")

	cmd := NewProviderCommandContext(context.Background(),
		ManagedCommandOpts{EnvPolicy: EnvPolicyInherit},
		"true",
	)

	// nil means inherit — the variable will be present when the process runs.
	if cmd.Env != nil {
		t.Errorf("EnvPolicyInherit should leave cmd.Env nil, got %v", cmd.Env)
	}
}

func TestEnvPolicyProviderChild_StripsAllIngressKeys(t *testing.T) {
	for _, key := range ingressVars {
		t.Setenv(key, "http://should-be-stripped")
	}

	env := providerChildEnv(nil)

	for _, kv := range env {
		for _, key := range ingressVars {
			if strings.HasPrefix(kv, key+"=") {
				t.Errorf("ingress var %s must be stripped, found %q", key, kv)
			}
		}
	}
}

// TestClaudeCodeComplete_DoesNotInheritBaseURL is the regression test for #59:
// a kernel-spawned claude subprocess must not receive ANTHROPIC_BASE_URL even
// when the parent process (the kernel) has it set.
func TestClaudeCodeComplete_DoesNotInheritBaseURL(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "http://localhost:6931")

	// envfile is a temp file where the fake claude writes its environment.
	envFile, err := os.CreateTemp(t.TempDir(), "env-*.txt")
	if err != nil {
		t.Fatalf("create env file: %v", err)
	}
	envFile.Close()

	// Fake claude: record env to envfile, then emit valid provider stream-json output.
	script, err := os.CreateTemp(t.TempDir(), "fake-claude-*.sh")
	if err != nil {
		t.Fatalf("create fake claude: %v", err)
	}
	_, _ = script.WriteString("#!/bin/sh\n")
	_, _ = script.WriteString("env > " + envFile.Name() + "\n")
	// Emit minimal valid stream-json: a result message.
	_, _ = script.WriteString(`echo '{"type":"result","subtype":"success","result":"ok","is_error":false,"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}'` + "\n")
	script.Close()
	if err := os.Chmod(script.Name(), 0o755); err != nil {
		t.Fatalf("chmod fake claude: %v", err)
	}

	procMgr := NewProcessManager(ProcessManagerConfig{})
	p := &ClaudeCodeProvider{
		name:      "test",
		model:     "sonnet",
		cliBinary: script.Name(),
		procMgr:   procMgr,
	}

	req := &CompletionRequest{
		Messages: []ProviderMessage{
			{Role: "user", Content: "hello"},
		},
	}

	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete returned unexpected error: %v", err)
	}

	data, err := os.ReadFile(envFile.Name())
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}

	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "ANTHROPIC_BASE_URL=") {
			t.Errorf("child process received ANTHROPIC_BASE_URL: %q", line)
		}
	}
}
