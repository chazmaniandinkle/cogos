// providers_register.go — registration hook for Reconcilable providers.
//
// RegisterProviders is a function variable that, when non-nil, is called once
// at daemon boot (inside runServe) before the HTTP server starts. The daemon
// binary (cmd/cogos) sets this variable from its own init() via a blank import
// of internal/providers/daemon, which triggers that package's init() and
// registers all 8 production providers with pkg/reconcile.
//
// This hook is intentionally nil in the engine package itself so that test
// binaries (which register stub providers directly) are not affected.
package engine

// RegisterProviders is called once at daemon boot to populate the
// pkg/reconcile provider registry. Set by cmd/cogos/providers_wire.go.
// Nil means "no additional providers to register" (e.g. in tests).
var RegisterProviders func()
