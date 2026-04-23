// identity_crd.go
// Loads Identity CRD definitions from .cog/config/identities/*.yaml.
//
// Identity is a CRD managed by the CogOS reconciliation loop (pkg/reconcile).
// The spec on disk is one leg of the (Spec, Projections, Reconciler) triple
// that defines an identity at runtime — see
// project_cogos_identity_as_crd.md in user memory for the architecture.
//
// Shape is OIDC-compatible: each identity has a stable (iss, sub) anchor and
// a list of expressions keyed by audience (workspace, channel, resource).
// The reconciler picks the expression matching the current audience when
// projecting — that's the "collapse operation" that maps global identity
// down to its contextual local expression.

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ─── CRD Types ──────────────────────────────────────────────────────────────────

// IdentityCRD is the top-level Kubernetes-style identity definition.
type IdentityCRD struct {
	APIVersion string          `yaml:"apiVersion"`
	Kind       string          `yaml:"kind"`
	Metadata   IdentityCRDMeta `yaml:"metadata"`
	Spec       IdentityCRDSpec `yaml:"spec"`
}

// IdentityCRDMeta matches the standard CRD metadata shape.
type IdentityCRDMeta struct {
	Name        string            `yaml:"name"`
	Namespace   string            `yaml:"namespace,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
}

// IdentityCRDSpec holds the OIDC-shaped identity body.
//
//	Issuer   — OIDC `iss`; who minted this identity (e.g., "cogos-dev",
//	           "https://accounts.google.com", "node:<hostname>", or a
//	           federation URL). Required.
//	Subject  — OIDC `sub`; globally stable identifier for the principal.
//	           Required. Conventionally matches metadata.name for
//	           self-minted identities, but may differ for federated ones.
//	Type     — agent | human | service. Governs downstream semantics
//	           (e.g., humans don't need presence expectation heartbeats).
//	PublicKey — optional PEM-encoded public key. When present, the
//	            reconciler records it for signature-verified attestations.
//	            Layer-1 node keys live separately in the constellation
//	            repo — this is Layer-2 principal identity.
//	Expressions — how this identity is projected into audiences. The
//	              reconciler's "collapse operation" picks the entry whose
//	              aud matches the current context.
type IdentityCRDSpec struct {
	Issuer      string               `yaml:"iss"`
	Subject     string               `yaml:"sub"`
	Type        string               `yaml:"type"`
	PublicKey   string               `yaml:"public_key,omitempty"`
	Expressions []IdentityExpression `yaml:"expressions"`
}

// IdentityExpression is one projection of an identity into a specific audience.
// At least one expression per Identity. The reconciler matches by `aud`:
//
//	workspace:<name>     — project into a workspace's view
//	channel:<channel_id> — project into a specific channel (mod3 room, etc.)
//	resource:<uri>       — project against an arbitrary cog:// URI
//	*                    — catch-all (used as fallback when no audience matches)
type IdentityExpression struct {
	Audience    string   `yaml:"aud"`
	DisplayName string   `yaml:"display_name,omitempty"`
	Role        string   `yaml:"role,omitempty"`
	Skills      []string `yaml:"skills,omitempty"`
	Voice       string   `yaml:"voice,omitempty"`
	Sudo        bool     `yaml:"sudo,omitempty"`
	// MemoryNamespace optionally scopes this expression's memory reads/writes
	// (e.g., "cog://" for root, "cog://agents/sandy/" for a scoped agent).
	MemoryNamespace string `yaml:"memory_namespace,omitempty"`
	// Claims is an OIDC-style free-form claim bag. The reconciler preserves
	// unknown claims verbatim — future tooling can read them without the
	// spec needing a schema bump.
	Claims map[string]any `yaml:"claims,omitempty"`
}

// Valid identity types accepted by the loader.
var validIdentityTypes = map[string]struct{}{
	"agent":   {},
	"human":   {},
	"service": {},
}

// Valid apiVersion/kind — validated on load to catch typos early.
const (
	IdentityAPIVersion = "cog.os/v1alpha1"
	IdentityKind       = "Identity"
)

// ─── Loader ─────────────────────────────────────────────────────────────────────

// identityCRDDir returns the path to the identity definitions directory.
// Matches the `.cog/config/<provider>/` convention used by services.
func identityCRDDir(root string) string {
	return filepath.Join(root, ".cog", "config", "identities")
}

// LoadIdentityCRD loads a single identity CRD by subject slug.
// Looks for {root}/.cog/config/identities/{sub}.yaml.
func LoadIdentityCRD(root, sub string) (*IdentityCRD, error) {
	if sub == "" {
		return nil, errors.New("load identity CRD: empty subject")
	}
	path := filepath.Join(identityCRDDir(root), sub+".yaml")
	return loadIdentityCRDFile(path)
}

// LoadIdentityCRDs loads every identity CRD under
// {root}/.cog/config/identities/*.yaml. Returns an empty slice (not an
// error) when the directory does not exist — identity is optional.
//
// The returned slice is deterministically ordered by filename so plan
// diffs stay stable.
func LoadIdentityCRDs(root string) ([]*IdentityCRD, error) {
	dir := identityCRDDir(root)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read identities dir %q: %w", dir, err)
	}

	var out []*IdentityCRD
	var loadErrs []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		crd, err := loadIdentityCRDFile(filepath.Join(dir, name))
		if err != nil {
			loadErrs = append(loadErrs, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		out = append(out, crd)
	}

	if len(loadErrs) > 0 {
		return out, fmt.Errorf("load identities: %d error(s): %s",
			len(loadErrs), strings.Join(loadErrs, "; "))
	}
	return out, nil
}

// loadIdentityCRDFile reads + parses + validates one CRD file.
func loadIdentityCRDFile(path string) (*IdentityCRD, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}

	var crd IdentityCRD
	if err := yaml.Unmarshal(data, &crd); err != nil {
		return nil, fmt.Errorf("parse %q: %w", path, err)
	}

	if err := validateIdentityCRD(&crd); err != nil {
		return nil, fmt.Errorf("validate %q: %w", path, err)
	}
	return &crd, nil
}

// validateIdentityCRD enforces the invariants the reconciler depends on.
// Fail fast at load time so the plan/apply loop never sees malformed specs.
func validateIdentityCRD(crd *IdentityCRD) error {
	if crd == nil {
		return errors.New("nil CRD")
	}
	if crd.APIVersion != IdentityAPIVersion {
		return fmt.Errorf("apiVersion = %q, want %q", crd.APIVersion, IdentityAPIVersion)
	}
	if crd.Kind != IdentityKind {
		return fmt.Errorf("kind = %q, want %q", crd.Kind, IdentityKind)
	}
	if strings.TrimSpace(crd.Metadata.Name) == "" {
		return errors.New("metadata.name is required")
	}
	if strings.TrimSpace(crd.Spec.Issuer) == "" {
		return errors.New("spec.iss is required")
	}
	if strings.TrimSpace(crd.Spec.Subject) == "" {
		return errors.New("spec.sub is required")
	}
	if _, ok := validIdentityTypes[crd.Spec.Type]; !ok {
		return fmt.Errorf("spec.type = %q, want one of {agent, human, service}", crd.Spec.Type)
	}
	if len(crd.Spec.Expressions) == 0 {
		return errors.New("spec.expressions must contain at least one entry")
	}
	seenAud := make(map[string]struct{}, len(crd.Spec.Expressions))
	for i, exp := range crd.Spec.Expressions {
		if strings.TrimSpace(exp.Audience) == "" {
			return fmt.Errorf("spec.expressions[%d].aud is required", i)
		}
		if _, dup := seenAud[exp.Audience]; dup {
			return fmt.Errorf("spec.expressions[%d].aud = %q is duplicated; each audience must appear at most once", i, exp.Audience)
		}
		seenAud[exp.Audience] = struct{}{}
	}
	return nil
}

// ─── Collapse Operation ─────────────────────────────────────────────────────────

// ExpressionFor returns the expression matching `aud`, or the catch-all `*`
// expression when no exact match exists. Returns nil when neither is present.
// This is the "collapse operation" — global identity collapsing down to a
// local expression based on the caller's context. Pure function; the
// reconciler calls it both at ComputePlan (to decide what to project) and
// at runtime (to resolve an identity's contextual view for a tool call).
func (s *IdentityCRDSpec) ExpressionFor(aud string) *IdentityExpression {
	if s == nil {
		return nil
	}
	var wildcard *IdentityExpression
	for i := range s.Expressions {
		exp := &s.Expressions[i]
		if exp.Audience == aud {
			return exp
		}
		if exp.Audience == "*" && wildcard == nil {
			wildcard = exp
		}
	}
	return wildcard
}
