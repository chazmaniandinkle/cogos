// serve_services.go — read-only /v1/services API.
//
// Routes:
//
//	GET /v1/services         — list all declared services with live health
//	GET /v1/services/{name}  — single service projection
//
// Design:
//   - Projects from two read sources: the parsed *NodeManifest (static
//     declaration) and *NodeHealth (last probe snapshot). Both are held on
//     *Process and accessed through NodeManifest() / NodeHealth() accessors.
//   - Read-only. Mutations (start / stop / restart) are Phase 2.
//   - "kind" defaults to "managed" when omitted from the manifest.
//   - "controllable" is false for observed and external services.
//
// Registered via s.route() so the routes auto-appear in GET /v1/manifest.
package engine

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
)

// serviceView is the JSON projection returned by /v1/services.
type serviceView struct {
	Name            string          `json:"name"`
	Kind            ServiceKind     `json:"kind"`
	Port            int             `json:"port"`
	Supervisor      string          `json:"supervisor"`
	SupervisorLabel string          `json:"supervisor_label,omitempty"`
	Controllable    bool            `json:"controllable"`
	Running         bool            `json:"running"`
	PID             *int            `json:"pid"`
	ExitCode        int             `json:"exit_code"`
	Health          *serviceHealthV `json:"health,omitempty"`
	DependsOn       []string        `json:"depends_on"`
	Command         string          `json:"command,omitempty"`
	RestartPolicy   string          `json:"restart_policy,omitempty"`
}

// serviceHealthV is the health sub-object embedded in serviceView.
type serviceHealthV struct {
	Status   string    `json:"status"`
	Endpoint string    `json:"endpoint"`
	ProbedAt time.Time `json:"probed_at"`
}

// registerServiceRoutes wires GET /v1/services and GET /v1/services/{name}.
func (s *Server) registerServiceRoutes(mux *http.ServeMux) {
	s.route(mux, "GET /v1/services", s.handleServicesList)
	s.route(mux, "GET /v1/services/{name}", s.handleServicesGet)
}

// handleServicesList returns all declared services sorted by name.
//
//	GET /v1/services
//	200 → { services: [...] }
func (s *Server) handleServicesList(w http.ResponseWriter, r *http.Request) {
	manifest := s.process.NodeManifest()
	if manifest == nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"services": []serviceView{},
		})
		return
	}

	healthSnap := s.process.NodeHealth().Snapshot()

	names := make([]string, 0, len(manifest.Services))
	for name := range manifest.Services {
		names = append(names, name)
	}
	sort.Strings(names)

	views := make([]serviceView, 0, len(names))
	for _, name := range names {
		def := manifest.Services[name]
		views = append(views, projectService(name, def, healthSnap[name]))
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"services": views,
	})
}

// handleServicesGet returns a single service by name.
//
//	GET /v1/services/{name}
//	200 → serviceView
//	404 → { error: "..." }
func (s *Server) handleServicesGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	manifest := s.process.NodeManifest()
	if manifest == nil {
		http.Error(w, `{"error":"manifest not loaded"}`, http.StatusNotFound)
		return
	}

	def, ok := manifest.Services[name]
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "service not found: " + name})
		return
	}

	healthSnap := s.process.NodeHealth().Snapshot()
	view := projectService(name, def, healthSnap[name])

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(view)
}

// projectService converts a ServiceDef + live ServiceHealth into a serviceView.
// All fields not yet tracked in the manifest (pid, exit_code) are defaulted;
// Phase 2 will wire live process state.
func projectService(name string, def ServiceDef, h ServiceHealth) serviceView {
	kind := def.Kind.EffectiveKind()
	controllable := kind == ServiceKindManaged

	// Supervisor classification: launchctl if a launchd label is present;
	// "observer" for observed/external (kernel doesn't supervise them);
	// "direct" for managed services launched without launchd.
	var supervisor string
	switch kind {
	case ServiceKindObserved, ServiceKindExternal:
		supervisor = "observer"
	default:
		if def.Launchd != "" {
			supervisor = "launchctl"
		} else {
			supervisor = "direct"
		}
	}

	// running is inferred from the last health probe. A zero-value
	// ServiceHealth (no probe yet) reports status "" which is treated as not
	// running. Phase 2 will wire live pid/exit_code from the supervisor.
	running := h.Status == "healthy" || h.Status == "degraded"

	var healthV *serviceHealthV
	if h.Status != "" {
		healthV = &serviceHealthV{
			Status:   h.Status,
			Endpoint: def.Health,
			ProbedAt: h.At,
		}
	}

	dependsOn := def.DependsOn
	if dependsOn == nil {
		dependsOn = []string{}
	}

	// Resolve command template placeholder: {{venv}} → def.Venv path.
	cmd := def.Command
	if def.Venv != "" {
		cmd = strings.ReplaceAll(cmd, "{{venv}}", def.Venv)
	}

	return serviceView{
		Name:            name,
		Kind:            kind,
		Port:            def.Port,
		Supervisor:      supervisor,
		SupervisorLabel: def.Launchd,
		Controllable:    controllable,
		Running:         running,
		PID:             nil, // Phase 2: live process state
		ExitCode:        0,   // Phase 2: live process state
		Health:          healthV,
		DependsOn:       dependsOn,
		Command:         cmd,
		RestartPolicy:   def.Restart,
	}
}
