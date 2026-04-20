// reconcile_helpers.go — small text helpers shared by main-package providers.
//
// These helpers previously lived in component_provider.go. That file moved
// to internal/providers/component/ as part of ADR-085 Wave 1a, taking a
// local copy of the helpers with it. This file retains the copies still
// needed by providers that remain at the apps/cogos root (currently
// service_provider.go).

package main

// joinReasons joins multiple drift reasons into a semicolon-separated string.
func joinReasons(reasons []string) string {
	if len(reasons) == 0 {
		return "unknown"
	}
	result := reasons[0]
	for _, r := range reasons[1:] {
		result += "; " + r
	}
	return result
}
