package engine

import (
	"context"
	"os"
	"os/exec"
	"strings"
)

// EnvPolicy controls what environment a provider child process inherits.
type EnvPolicy string

const (
	// EnvPolicyProviderChild starts from os.Environ() and strips known
	// ingress/proxy variables that can redirect model clients back into CogOS.
	EnvPolicyProviderChild EnvPolicy = "provider_child"

	// EnvPolicyInherit passes the current process environment unchanged.
	EnvPolicyInherit EnvPolicy = "inherit"
)

// ingressVars are environment keys stripped under EnvPolicyProviderChild.
// These are routing/proxy variables that can cause a kernel-spawned provider
// subprocess to route its own model calls back through the kernel.
var ingressVars = []string{
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_AUTH_TOKEN",
	"OPENAI_BASE_URL",
	"OPENAI_API_BASE",
	"OPENAI_COMPAT_BASE_URL",
}

// ManagedCommandOpts configures environment and working directory for a
// provider child process created via NewProviderCommandContext.
type ManagedCommandOpts struct {
	EnvPolicy EnvPolicy
	ExtraEnv  []string
	Dir       string
}

// NewProviderCommandContext creates an exec.Cmd for a provider child process,
// applying the env policy before the process starts.
func NewProviderCommandContext(ctx context.Context, opts ManagedCommandOpts, binary string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binary, args...)

	switch opts.EnvPolicy {
	case EnvPolicyInherit:
		// cmd.Env == nil means inherit; nothing to do.
	default:
		cmd.Env = providerChildEnv(opts.ExtraEnv)
	}

	if opts.Dir != "" {
		cmd.Dir = opts.Dir
	}

	return cmd
}

// providerChildEnv builds the sanitized environment for provider child
// processes: os.Environ() minus ingressVars, plus any extras.
func providerChildEnv(extra []string) []string {
	base := os.Environ()
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		if !isIngressVar(kv) {
			out = append(out, kv)
		}
	}
	out = append(out, extra...)
	return out
}

func isIngressVar(kv string) bool {
	for _, key := range ingressVars {
		if strings.HasPrefix(kv, key+"=") {
			return true
		}
	}
	return false
}
