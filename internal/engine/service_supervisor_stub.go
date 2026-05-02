//go:build !darwin

// service_supervisor_stub.go — no-op stub for non-darwin platforms.
//
// On non-macOS platforms there is no launchctl(1). LaunchctlController is
// present as a type alias for ObserverSupervisor so the rest of the package
// compiles cleanly; every mutation returns ErrNotControllable.
//
// Phase 3 (#101) will add a SystemdSupervisor for Linux and a DirectSupervisor
// for dev-mode process management. Until then, non-darwin platforms are in
// observer-only mode for all services.
package engine

// LaunchctlController is a no-op alias for ObserverSupervisor on non-darwin
// platforms. It compiles cleanly but returns ErrNotControllable for all
// mutations.
type LaunchctlController = ObserverSupervisor

// NewLaunchctlController returns an ObserverSupervisor on non-darwin platforms.
func NewLaunchctlController() *LaunchctlController {
	return &ObserverSupervisor{}
}

// homeDir is a stub on non-darwin platforms — not called in practice.
func homeDir() (string, error) {
	return "", ErrNotControllable
}
