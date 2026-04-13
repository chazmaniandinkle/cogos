// service_provider.go — Reconciliation provider for container services.
//
// Implements Reconcilable to manage container lifecycle through the standard
// plan/apply/state reconciliation loop. Compares declared ServiceCRDs against
// live Docker container state and produces create/update/skip actions.
//
// State file: .cog/config/services/.state.json

package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// ServiceProvider implements Reconcilable for container service management.
type ServiceProvider struct {
	mu      sync.Mutex
	root    string
	runtime *DockerClient
}

func init() {
	RegisterProvider("service", &ServiceProvider{})
}

func (s *ServiceProvider) Type() string { return "service" }

// ─── LoadConfig ─────────────────────────────────────────────────────────────────

// LoadConfig loads all service CRD definitions from .cog/config/services/.
func (s *ServiceProvider) LoadConfig(root string) (any, error) {
	s.mu.Lock()
	s.root = root
	s.mu.Unlock()
	crds, err := ListServiceCRDs(root)
	if err != nil {
		return nil, fmt.Errorf("service provider: load config: %w", err)
	}
	if crds == nil {
		crds = []ServiceCRD{}
	}
	return crds, nil
}

// ─── FetchLive ──────────────────────────────────────────────────────────────────

// ServiceLiveState holds the runtime state of managed containers.
type ServiceLiveState struct {
	Available  bool
	Containers map[string]*ContainerInfo // keyed by service name
}

// FetchLive queries the Docker daemon for all cogos-managed containers.
func (s *ServiceProvider) FetchLive(ctx context.Context, config any) (any, error) {
	s.mu.Lock()
	if s.runtime == nil {
		s.runtime = NewDockerClient("")
	}
	s.mu.Unlock()

	live := &ServiceLiveState{
		Containers: make(map[string]*ContainerInfo),
	}

	if err := s.runtime.Ping(ctx); err != nil {
		// Runtime not available — return empty live state
		live.Available = false
		return live, nil
	}
	live.Available = true

	entries, err := s.runtime.ListManagedContainers(ctx)
	if err != nil {
		return nil, fmt.Errorf("service provider: list containers: %w", err)
	}

	for _, entry := range entries {
		serviceName := entry.Labels[labelService]
		if serviceName == "" {
			continue
		}
		info, err := s.runtime.ContainerInspect(ctx, entry.ID)
		if err != nil {
			log.Printf("[service] warning: inspect %s failed: %v", entry.ID[:12], err)
			continue
		}
		live.Containers[serviceName] = info
	}

	return live, nil
}

// ─── ComputePlan ────────────────────────────────────────────────────────────────

// ComputePlan compares declared CRDs against live container state.
func (s *ServiceProvider) ComputePlan(config any, live any, state *ReconcileState) (*ReconcilePlan, error) {
	crds := config.([]ServiceCRD)
	liveState := live.(*ServiceLiveState)

	plan := &ReconcilePlan{
		ResourceType: "service",
		GeneratedAt:  nowISO(),
		ConfigPath:   ".cog/config/services/",
	}

	if !liveState.Available {
		plan.Warnings = append(plan.Warnings, "container runtime not available — skipping all services")
		for _, crd := range crds {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionSkip,
				ResourceType: "service",
				Name:         crd.Metadata.Name,
				Details:      map[string]any{"reason": "runtime not available"},
			})
			plan.Summary.Skipped++
		}
		return plan, nil
	}

	// Sort CRDs by name for deterministic output
	sort.Slice(crds, func(i, j int) bool {
		return crds[i].Metadata.Name < crds[j].Metadata.Name
	})

	seen := make(map[string]bool)

	for _, crd := range crds {
		name := crd.Metadata.Name
		seen[name] = true
		info, exists := liveState.Containers[name]

		if !exists {
			// Declared but no container
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionCreate,
				ResourceType: "service",
				Name:         name,
				Details: map[string]any{
					"reason": "no container found",
					"image":  crd.Spec.Image,
				},
			})
			plan.Summary.Creates++
			continue
		}

		// Container exists — check for drift
		drifted, reasons := detectServiceDrift(&crd, info)
		if drifted {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionUpdate,
				ResourceType: "service",
				Name:         name,
				Details: map[string]any{
					"reason":       "drift detected: " + joinReasons(reasons),
					"container_id": info.ID[:12],
				},
			})
			plan.Summary.Updates++
		} else {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionSkip,
				ResourceType: "service",
				Name:         name,
				Details: map[string]any{
					"reason":       "in sync",
					"container_id": info.ID[:12],
					"status":       info.State.Status,
				},
			})
			plan.Summary.Skipped++
		}
	}

	// Check for orphaned managed containers (not in any CRD)
	for name := range liveState.Containers {
		if !seen[name] {
			info := liveState.Containers[name]
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionDelete,
				ResourceType: "service",
				Name:         name,
				Details: map[string]any{
					"reason":       "no matching service CRD",
					"container_id": info.ID[:12],
				},
			})
			plan.Summary.Deletes++
		}
	}

	return plan, nil
}

// detectServiceDrift compares a CRD against a live container.
func detectServiceDrift(crd *ServiceCRD, info *ContainerInfo) (bool, []string) {
	var reasons []string

	// Check if container is not running
	if !info.State.Running {
		reasons = append(reasons, fmt.Sprintf("container not running (state: %s)", info.State.Status))
	}

	// Check image drift
	liveImage := info.Config.Image
	if !strings.Contains(liveImage, crd.Spec.Image) && !strings.Contains(crd.Spec.Image, liveImage) {
		// Do a more lenient check — sometimes the tag resolves differently
		declaredBase := strings.Split(crd.Spec.Image, ":")[0]
		liveBase := strings.Split(liveImage, ":")[0]
		if declaredBase != liveBase {
			reasons = append(reasons, fmt.Sprintf("image: declared=%s live=%s", crd.Spec.Image, liveImage))
		}
	}

	// Check port bindings
	for _, port := range crd.Spec.Ports {
		proto := port.Protocol
		if proto == "" {
			proto = "tcp"
		}
		key := fmt.Sprintf("%d/%s", port.Container, proto)
		bindings, ok := info.HostConfig.PortBindings[key]
		if !ok || len(bindings) == 0 {
			reasons = append(reasons, fmt.Sprintf("port %d not bound", port.Container))
			continue
		}
		expectedHost := fmt.Sprintf("%d", port.Host)
		if bindings[0].HostPort != expectedHost {
			reasons = append(reasons, fmt.Sprintf("port %d: host=%s expected=%s",
				port.Container, bindings[0].HostPort, expectedHost))
		}
	}

	// Check environment variables
	liveEnv := make(map[string]string)
	for _, e := range info.Config.Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			liveEnv[parts[0]] = parts[1]
		}
	}
	for _, e := range crd.Spec.Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			if liveVal, ok := liveEnv[parts[0]]; !ok || liveVal != parts[1] {
				reasons = append(reasons, fmt.Sprintf("env %s changed", parts[0]))
			}
		}
	}

	return len(reasons) > 0, reasons
}

// ─── ApplyPlan ──────────────────────────────────────────────────────────────────

// ApplyPlan executes planned service changes.
func (s *ServiceProvider) ApplyPlan(ctx context.Context, plan *ReconcilePlan) ([]ReconcileResult, error) {
	s.mu.Lock()
	if s.runtime == nil {
		s.runtime = NewDockerClient("")
	}
	s.mu.Unlock()

	var results []ReconcileResult
	for _, action := range plan.Actions {
		if action.Action == ActionSkip {
			continue
		}

		switch action.Action {
		case ActionCreate:
			result := s.applyCreate(ctx, action.Name)
			results = append(results, result)
		case ActionUpdate:
			result := s.applyUpdate(ctx, action.Name)
			results = append(results, result)
		case ActionDelete:
			result := s.applyDelete(ctx, action.Name)
			results = append(results, result)
		}
	}
	return results, nil
}

func (s *ServiceProvider) applyCreate(ctx context.Context, name string) ReconcileResult {
	crd, err := LoadServiceCRD(s.root, name)
	if err != nil {
		return ReconcileResult{
			Phase: "service", Action: "create", Name: name,
			Status: ApplyFailed, Error: err.Error(),
		}
	}

	// Pull image
	if err := s.runtime.ImagePull(ctx, crd.Spec.Image, crd.Spec.Platform, nil); err != nil {
		return ReconcileResult{
			Phase: "service", Action: "create", Name: name,
			Status: ApplyFailed, Error: fmt.Sprintf("pull failed: %v", err),
		}
	}

	// Build config and create
	config, err := BuildContainerConfig(s.root, crd)
	if err != nil {
		return ReconcileResult{
			Phase: "service", Action: "create", Name: name,
			Status: ApplyFailed, Error: fmt.Sprintf("config: %v", err),
		}
	}

	containerName := ManagedContainerName(name)
	containerID, err := s.runtime.ContainerCreate(ctx, containerName, config)
	if err != nil {
		return ReconcileResult{
			Phase: "service", Action: "create", Name: name,
			Status: ApplyFailed, Error: fmt.Sprintf("create: %v", err),
		}
	}

	// Start
	if err := s.runtime.ContainerStart(ctx, containerID); err != nil {
		return ReconcileResult{
			Phase: "service", Action: "create", Name: name,
			Status: ApplyFailed, Error: fmt.Sprintf("start: %v", err),
		}
	}

	return ReconcileResult{
		Phase: "service", Action: "create", Name: name,
		Status: ApplySucceeded, CreatedID: containerID,
	}
}

func (s *ServiceProvider) applyUpdate(ctx context.Context, name string) ReconcileResult {
	// Stop and remove existing container
	entry, err := s.runtime.FindManagedContainer(ctx, name)
	if err != nil {
		return ReconcileResult{
			Phase: "service", Action: "update", Name: name,
			Status: ApplyFailed, Error: fmt.Sprintf("find container: %v", err),
		}
	}
	if entry != nil {
		if err := s.runtime.ContainerStop(ctx, entry.ID, 10); err != nil {
			log.Printf("[service] warning: stop %s before update: %v", entry.ID[:12], err)
		}
		if err := s.runtime.ContainerRemove(ctx, entry.ID, true); err != nil {
			return ReconcileResult{
				Phase: "service", Action: "update", Name: name,
				Status: ApplyFailed, Error: fmt.Sprintf("remove old container: %v", err),
			}
		}
	}

	// Recreate
	result := s.applyCreate(ctx, name)
	result.Action = "update"
	return result
}

func (s *ServiceProvider) applyDelete(ctx context.Context, name string) ReconcileResult {
	entry, err := s.runtime.FindManagedContainer(ctx, name)
	if err != nil {
		return ReconcileResult{
			Phase: "service", Action: "delete", Name: name,
			Status: ApplyFailed, Error: fmt.Sprintf("find container: %v", err),
		}
	}
	if entry == nil {
		return ReconcileResult{
			Phase: "service", Action: "delete", Name: name,
			Status: ApplySucceeded,
		}
	}

	if entry.State == "running" {
		if err := s.runtime.ContainerStop(ctx, entry.ID, 10); err != nil {
			log.Printf("[service] warning: stop %s before delete: %v", entry.ID[:12], err)
		}
	}
	if err := s.runtime.ContainerRemove(ctx, entry.ID, true); err != nil {
		return ReconcileResult{
			Phase: "service", Action: "delete", Name: name,
			Status: ApplyFailed, Error: err.Error(),
		}
	}

	return ReconcileResult{
		Phase: "service", Action: "delete", Name: name,
		Status: ApplySucceeded,
	}
}

// ─── BuildState ─────────────────────────────────────────────────────────────────

// BuildState constructs reconcile state from live container data.
func (s *ServiceProvider) BuildState(config any, live any, existing *ReconcileState) (*ReconcileState, error) {
	liveState := live.(*ServiceLiveState)

	state := &ReconcileState{
		Version:      1,
		ResourceType: "service",
		GeneratedAt:  nowISO(),
	}

	if existing != nil {
		state.Lineage = existing.Lineage
		state.Serial = existing.Serial + 1
	} else {
		state.Lineage = "service-" + nowISO()
	}

	for name, info := range liveState.Containers {
		health := "unknown"
		if info.State.Health != nil {
			health = info.State.Health.Status
		}

		resource := ReconcileResource{
			Address:       "service." + name,
			Type:          "container",
			Mode:          ModeManaged,
			ExternalID:    info.ID,
			Name:          name,
			LastRefreshed: nowISO(),
			Attributes: map[string]any{
				"image":   info.Config.Image,
				"status":  info.State.Status,
				"running": info.State.Running,
				"health":  health,
			},
		}
		state.Resources = append(state.Resources, resource)
	}

	// Sort by address for deterministic output
	sort.Slice(state.Resources, func(i, j int) bool {
		return state.Resources[i].Address < state.Resources[j].Address
	})

	return state, nil
}

// ─── Health ─────────────────────────────────────────────────────────────────────

// Health returns the three-axis status of the service subsystem.
func (s *ServiceProvider) Health() ResourceStatus {
	s.mu.Lock()
	root := s.root
	s.mu.Unlock()
	if root == "" {
		var err error
		root, _, err = ResolveWorkspace()
		if err != nil {
			return ResourceStatus{
				Sync: SyncStatusUnknown, Health: HealthMissing, Operation: OperationIdle,
				Message: "workspace not found",
			}
		}
	}

	crds, err := ListServiceCRDs(root)
	if err != nil || len(crds) == 0 {
		return NewResourceStatus(SyncStatusSynced, HealthHealthy)
	}

	runtime := NewDockerClient("")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := runtime.Ping(ctx); err != nil {
		return ResourceStatus{
			Sync: SyncStatusUnknown, Health: HealthMissing, Operation: OperationIdle,
			Message: "container runtime not available",
		}
	}

	down := 0
	for _, crd := range crds {
		entry, _ := runtime.FindManagedContainer(ctx, crd.Metadata.Name)
		if entry == nil || entry.State != "running" {
			down++
		}
	}

	if down == len(crds) {
		return ResourceStatus{
			Sync: SyncStatusOutOfSync, Health: HealthDegraded, Operation: OperationIdle,
			Message: fmt.Sprintf("all %d services down", down),
		}
	}
	if down > 0 {
		return ResourceStatus{
			Sync: SyncStatusOutOfSync, Health: HealthDegraded, Operation: OperationIdle,
			Message: fmt.Sprintf("%d/%d services down", down, len(crds)),
		}
	}

	return NewResourceStatus(SyncStatusSynced, HealthHealthy)
}

// ─── Health Monitor ─────────────────────────────────────────────────────────────

const (
	BlockServiceHealth       = "service.health"
	BlockServiceCapabilities = "service.capabilities"
	serviceHealthBusID       = "bus_chat_system_capabilities"
)

// ServiceHealthMonitor periodically polls service health and emits bus events.
type ServiceHealthMonitor struct {
	root    string
	mgr     *busSessionManager
	runtime *DockerClient
	stopCh  chan struct{}
	done   chan struct{}
}

// NewServiceHealthMonitor creates a new health monitor.
func NewServiceHealthMonitor(root string, mgr *busSessionManager) *ServiceHealthMonitor {
	return &ServiceHealthMonitor{
		root:    root,
		mgr:     mgr,
		runtime: NewDockerClient(""),
		stopCh:  make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// Start begins the health monitor polling loop.
func (m *ServiceHealthMonitor) Start() {
	go m.run()
}

// Stop halts the health monitor.
func (m *ServiceHealthMonitor) Stop() {
	close(m.stopCh)
	<-m.done
}

func (m *ServiceHealthMonitor) run() {
	defer close(m.done)

	// Initial check after 15s delay
	select {
	case <-time.After(15 * time.Second):
	case <-m.stopCh:
		return
	}

	m.checkAndEmit()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.checkAndEmit()
		case <-m.stopCh:
			return
		}
	}
}

func (m *ServiceHealthMonitor) checkAndEmit() {
	crds, err := ListServiceCRDs(m.root)
	if err != nil || len(crds) == 0 {
		return
	}

	// Use a short context just for the ping check
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pingCancel()

	if err := m.runtime.Ping(pingCtx); err != nil {
		return
	}

	for _, crd := range crds {
		// Per-service timeout so one slow service doesn't starve the rest
		svcCtx, svcCancel := context.WithTimeout(context.Background(), 10*time.Second)

		entry, _ := m.runtime.FindManagedContainer(svcCtx, crd.Metadata.Name)
		status := "not_running"
		health := "unknown"

		if entry != nil {
			status = entry.State
			if entry.State == "running" {
				health = checkServiceHealth(svcCtx, &crd)
			}
		}
		svcCancel()

		if m.mgr != nil {
			payload := map[string]interface{}{
				"service": crd.Metadata.Name,
				"status":  status,
				"health":  health,
				"image":   crd.Spec.Image,
			}
			if _, err := m.mgr.appendBusEvent(serviceHealthBusID, BlockServiceHealth, "kernel:cogos", payload); err != nil {
				log.Printf("[svc-health] emit event for %s: %v", crd.Metadata.Name, err)
			}
		}
	}
}

// ─── Service Capability Advertiser ──────────────────────────────────────────────

// AdvertiseServiceCapabilities posts service.capabilities events on the bus
// for services with spec.bus.advertise == true.
func AdvertiseServiceCapabilities(root string, mgr *busSessionManager) error {
	crds, err := ListServiceCRDs(root)
	if err != nil {
		return fmt.Errorf("advertise service capabilities: %w", err)
	}

	for _, crd := range crds {
		if !crd.Spec.Bus.Advertise || len(crd.Spec.Tools) == 0 {
			continue
		}

		tools := make([]map[string]interface{}, len(crd.Spec.Tools))
		for i, t := range crd.Spec.Tools {
			tools[i] = map[string]interface{}{
				"name":        t.Name,
				"description": t.Description,
				"endpoint":    t.Endpoint,
				"method":      t.Method,
			}
		}

		ports := make([]map[string]interface{}, len(crd.Spec.Ports))
		for i, p := range crd.Spec.Ports {
			ports[i] = map[string]interface{}{
				"host":      p.Host,
				"container": p.Container,
				"protocol":  p.Protocol,
			}
		}

		payload := map[string]interface{}{
			"service":      crd.Metadata.Name,
			"image":        crd.Spec.Image,
			"tools":        tools,
			"ports":        ports,
			"advertisedAt": time.Now().UTC().Format(time.RFC3339Nano),
		}

		mgr.appendBusEvent(serviceHealthBusID, BlockServiceCapabilities, "kernel:cogos", payload)
		log.Printf("[svc-cap] advertised service=%s tools=%d", crd.Metadata.Name, len(crd.Spec.Tools))
	}
	return nil
}
