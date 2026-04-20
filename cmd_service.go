// cmd_service.go — CLI commands for container service management.
//
// Commands:
//   cog service list              — list all declared services + runtime status
//   cog service status <name>     — detailed single-service view
//   cog service pull <name>       — pull/update image with streamed progress
//   cog service start <name>      — auto-pull, create, start, health check
//   cog service stop <name>       — graceful stop
//   cog service restart <name>    — stop + start
//   cog service logs <name> [-f] [-n N]  — tail/stream container logs

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"
)

func cmdService(args []string) int {
	if len(args) == 0 {
		cmdServiceHelp()
		return 0
	}

	switch args[0] {
	case "list", "ls":
		return cmdServiceList(args[1:])
	case "status":
		return cmdServiceStatus(args[1:])
	case "pull":
		return cmdServicePull(args[1:])
	case "start":
		return cmdServiceStart(args[1:])
	case "stop":
		return cmdServiceStop(args[1:])
	case "restart":
		return cmdServiceRestart(args[1:])
	case "logs":
		return cmdServiceLogs(args[1:])
	case "help", "-h", "--help":
		cmdServiceHelp()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "Unknown service command: %s\n", args[0])
		cmdServiceHelp()
		return 1
	}
}

// ─── List ───────────────────────────────────────────────────────────────────────

func cmdServiceList(args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: no workspace found: %v\n", err)
		return 1
	}

	crds, err := ListServiceCRDs(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	if len(crds) == 0 {
		fmt.Println("No services defined.")
		fmt.Println("  Create one at .cog/config/services/<name>.service.yaml")
		return 0
	}

	runtime := NewDockerClient("")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runtimeAvailable := runtime.Ping(ctx) == nil

	fmt.Printf("%-20s %-8s %-40s %-12s %-8s %s\n", "NAME", "MODE", "IMAGE / COMMAND", "STATUS", "HEALTH", "PORTS")
	fmt.Printf("%-20s %-8s %-40s %-12s %-8s %s\n",
		strings.Repeat("─", 20), strings.Repeat("─", 8), strings.Repeat("─", 40),
		strings.Repeat("─", 12), strings.Repeat("─", 8), strings.Repeat("─", 20))

	for _, crd := range crds {
		mode := serviceMode(&crd, runtimeAvailable)
		status := "not running"
		health := "—"
		ports := formatPorts(crd.Spec.Ports)
		var target string

		switch mode {
		case modeDocker:
			target = truncateString(crd.Spec.Image, 40)
			entry, _ := runtime.FindManagedContainer(ctx, crd.Metadata.Name)
			if entry != nil {
				status = entry.State
				if entry.State == "running" {
					health = checkServiceHealth(ctx, &crd)
				}
			}
		case modeLocal:
			target = truncateString(crd.Spec.Local.Command+" "+strings.Join(crd.Spec.Local.Args, " "), 40)
			proc, _ := LocalStatus(root, crd.Metadata.Name)
			if proc != nil && proc.Running {
				status = fmt.Sprintf("pid %d", proc.PID)
				health = checkServiceHealth(ctx, &crd)
			}
		case modeNone:
			target = truncateString(crd.Spec.Image, 40)
			status = "no executor"
		}

		fmt.Printf("%-20s %-8s %-40s %-12s %-8s %s\n",
			crd.Metadata.Name, mode, target, status, health, ports)
	}

	return 0
}

// ─── Status ─────────────────────────────────────────────────────────────────────

func cmdServiceStatus(args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: cog service status <name>\n")
		return 1
	}

	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: no workspace found: %v\n", err)
		return 1
	}

	name := args[0]
	crd, err := LoadServiceCRD(root, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	fmt.Printf("Service: %s\n", crd.Metadata.Name)
	fmt.Printf("  Image:    %s\n", crd.Spec.Image)
	fmt.Printf("  Platform: %s\n", valueOr(crd.Spec.Platform, "default"))
	fmt.Printf("  Restart:  %s\n", crd.Spec.Restart)
	if crd.Spec.Resources.Memory != "" || crd.Spec.Resources.CPUs > 0 {
		fmt.Printf("  Memory:   %s\n", valueOr(crd.Spec.Resources.Memory, "unlimited"))
		if crd.Spec.Resources.CPUs > 0 {
			fmt.Printf("  CPUs:     %.1f\n", crd.Spec.Resources.CPUs)
		}
	}
	fmt.Printf("  Ports:\n")
	for _, p := range crd.Spec.Ports {
		fmt.Printf("    %d → %d/%s\n", p.Host, p.Container, p.Protocol)
	}
	if len(crd.Spec.Tools) > 0 {
		fmt.Printf("  Tools:\n")
		for _, t := range crd.Spec.Tools {
			fmt.Printf("    %s: %s %s\n", t.Name, t.Method, t.Endpoint)
		}
	}

	// Runtime info
	runtime := NewDockerClient("")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := runtime.Ping(ctx); err != nil {
		fmt.Printf("\n  Runtime: not available (%v)\n", err)
		return 0
	}

	entry, _ := runtime.FindManagedContainer(ctx, name)
	if entry == nil {
		fmt.Printf("\n  Container: not created\n")
		return 0
	}

	info, err := runtime.ContainerInspect(ctx, entry.ID)
	if err != nil {
		fmt.Printf("\n  Container: %s (inspect failed: %v)\n", entry.ID[:12], err)
		return 0
	}

	fmt.Printf("\n  Container: %s\n", info.ID[:12])
	fmt.Printf("  Status:    %s\n", info.State.Status)
	if info.State.Running {
		fmt.Printf("  Started:   %s\n", info.State.StartedAt)
		uptime := time.Since(parseTime(info.State.StartedAt)).Truncate(time.Second)
		fmt.Printf("  Uptime:    %s\n", uptime)
		health := checkServiceHealth(ctx, crd)
		fmt.Printf("  Health:    %s\n", health)
	} else {
		fmt.Printf("  Finished:  %s\n", info.State.FinishedAt)
		fmt.Printf("  Exit Code: %d\n", info.State.ExitCode)
	}

	return 0
}

// ─── Pull ───────────────────────────────────────────────────────────────────────

func cmdServicePull(args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: cog service pull <name>\n")
		return 1
	}

	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: no workspace found: %v\n", err)
		return 1
	}

	name := args[0]
	crd, err := LoadServiceCRD(root, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	runtime := NewDockerClient("")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if err := runtime.Ping(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error: container runtime not available: %v\n", err)
		return 1
	}

	fmt.Printf("Pulling %s", crd.Spec.Image)
	if crd.Spec.Platform != "" {
		fmt.Printf(" (platform: %s)", crd.Spec.Platform)
	}
	fmt.Println()

	lastStatus := ""
	err = runtime.ImagePull(ctx, crd.Spec.Image, crd.Spec.Platform, func(p ImagePullProgress) {
		line := p.Status
		if p.ID != "" {
			line = p.ID + ": " + line
		}
		if p.Progress != "" {
			line += " " + p.Progress
		}
		if line != lastStatus {
			fmt.Printf("  %s\n", line)
			lastStatus = line
		}
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	fmt.Println("Pull complete.")
	return 0
}

// ─── Start ──────────────────────────────────────────────────────────────────────

func cmdServiceStart(args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: cog service start <name>\n")
		return 1
	}

	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: no workspace found: %v\n", err)
		return 1
	}

	name := args[0]
	crd, err := LoadServiceCRD(root, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	runtime := NewDockerClient("")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	runtimeAvailable := runtime.Ping(ctx) == nil

	// Dispatch to local runner for local-mode services.
	if serviceMode(crd, runtimeAvailable) == modeLocal {
		if proc, _ := LocalStatus(root, name); proc != nil && proc.Running {
			fmt.Printf("Service %s is already running (pid %d)\n", name, proc.PID)
			return 0
		}
		proc, err := LocalStart(root, crd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error starting local service: %v\n", err)
			return 1
		}
		fmt.Printf("Started %s as pid %d (logs: %s)\n", name, proc.PID, proc.LogPath)
		if crd.Spec.Health.Endpoint != "" && crd.Spec.Health.Port > 0 {
			fmt.Printf("Waiting for health check (localhost:%d%s)...\n",
				crd.Spec.Health.Port, crd.Spec.Health.Endpoint)
			if err := waitForHealth(ctx, crd); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: health check did not pass: %v\n", err)
			} else {
				fmt.Println("Health check passed.")
			}
		}
		return 0
	}

	if !runtimeAvailable {
		fmt.Fprintf(os.Stderr, "Error: container runtime not available and no spec.local defined\n")
		return 1
	}

	// Check if already running
	existing, _ := runtime.FindManagedContainer(ctx, name)
	if existing != nil && existing.State == "running" {
		fmt.Printf("Service %s is already running (container %s)\n", name, existing.ID[:12])
		return 0
	}

	// Remove existing stopped container if any
	if existing != nil {
		fmt.Printf("Removing stopped container %s...\n", existing.ID[:12])
		runtime.ContainerRemove(ctx, existing.ID, true)
	}

	// Auto-pull image
	fmt.Printf("Pulling image %s...\n", crd.Spec.Image)
	err = runtime.ImagePull(ctx, crd.Spec.Image, crd.Spec.Platform, func(p ImagePullProgress) {
		if p.Status == "Downloading" || p.Status == "Extracting" {
			return // skip verbose layer progress
		}
		if p.ID != "" {
			fmt.Printf("  %s: %s\n", p.ID, p.Status)
		} else {
			fmt.Printf("  %s\n", p.Status)
		}
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error pulling image: %v\n", err)
		return 1
	}

	// Create container
	config, err := BuildContainerConfig(root, crd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error building config: %v\n", err)
		return 1
	}

	containerName := ManagedContainerName(name)
	fmt.Printf("Creating container %s...\n", containerName)
	containerID, err := runtime.ContainerCreate(ctx, containerName, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating container: %v\n", err)
		return 1
	}

	// Start
	fmt.Printf("Starting container %s...\n", containerID[:12])
	if err := runtime.ContainerStart(ctx, containerID); err != nil {
		fmt.Fprintf(os.Stderr, "Error starting container: %v\n", err)
		return 1
	}

	// Wait for health
	if crd.Spec.Health.Endpoint != "" && crd.Spec.Health.Port > 0 {
		fmt.Printf("Waiting for health check (%s:%d%s)...\n",
			"localhost", crd.Spec.Health.Port, crd.Spec.Health.Endpoint)
		if err := waitForHealth(ctx, crd); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: health check did not pass: %v\n", err)
			fmt.Println("Container is running but may still be starting up.")
		} else {
			fmt.Println("Health check passed.")
		}
	}

	fmt.Printf("Service %s started successfully.\n", name)
	return 0
}

// ─── Stop ───────────────────────────────────────────────────────────────────────

func cmdServiceStop(args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: cog service stop <name>\n")
		return 1
	}

	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: no workspace found: %v\n", err)
		return 1
	}

	name := args[0]
	crd, err := LoadServiceCRD(root, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	runtime := NewDockerClient("")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	runtimeAvailable := runtime.Ping(ctx) == nil

	if serviceMode(crd, runtimeAvailable) == modeLocal {
		proc, _ := LocalStatus(root, name)
		if proc == nil || !proc.Running {
			fmt.Printf("Service %s is not running (local).\n", name)
			return 0
		}
		fmt.Printf("Stopping %s (pid %d)...\n", name, proc.PID)
		if err := LocalStop(root, name); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		fmt.Printf("Service %s stopped.\n", name)
		return 0
	}

	if !runtimeAvailable {
		fmt.Fprintf(os.Stderr, "Error: container runtime not available and no spec.local defined\n")
		return 1
	}

	entry, _ := runtime.FindManagedContainer(ctx, name)
	if entry == nil {
		fmt.Printf("Service %s has no container.\n", name)
		return 0
	}

	if entry.State != "running" {
		fmt.Printf("Service %s is not running (state: %s).\n", name, entry.State)
		return 0
	}

	fmt.Printf("Stopping service %s (container %s)...\n", name, entry.ID[:12])
	if err := runtime.ContainerStop(ctx, entry.ID, 10); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	fmt.Printf("Service %s stopped.\n", name)
	return 0
}

// ─── Restart ────────────────────────────────────────────────────────────────────

func cmdServiceRestart(args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: cog service restart <name>\n")
		return 1
	}

	// Stop then start
	if code := cmdServiceStop(args); code != 0 {
		return code
	}
	return cmdServiceStart(args)
}

// ─── Logs ───────────────────────────────────────────────────────────────────────

func cmdServiceLogs(args []string) int {
	fs := flag.NewFlagSet("service logs", flag.ExitOnError)
	follow := fs.Bool("f", false, "follow log output")
	tail := fs.String("n", "50", "number of lines to show")
	fs.Parse(args)

	remaining := fs.Args()
	if len(remaining) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: cog service logs <name> [-f] [-n N]\n")
		return 1
	}

	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: no workspace found: %v\n", err)
		return 1
	}

	name := remaining[0]
	crd, err := LoadServiceCRD(root, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	runtime := NewDockerClient("")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtimeAvailable := runtime.Ping(ctx) == nil

	if serviceMode(crd, runtimeAvailable) == modeLocal {
		return tailLocalLog(localLogPath(root, name), *follow, *tail)
	}

	// Handle Ctrl-C gracefully
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	if !runtimeAvailable {
		fmt.Fprintf(os.Stderr, "Error: container runtime not available\n")
		return 1
	}

	entry, _ := runtime.FindManagedContainer(ctx, name)
	if entry == nil {
		fmt.Fprintf(os.Stderr, "Error: no container found for service %s\n", name)
		return 1
	}

	reader, err := runtime.ContainerLogs(ctx, entry.ID, *follow, *tail)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer reader.Close()

	// Docker multiplexed stream: each frame has 8-byte header.
	// [stream_type(1)][0(3)][size(4)][payload(size)]
	// For simplicity with timestamps enabled, strip the header.
	buf := make([]byte, 32*1024)
	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			// Strip Docker multiplexed stream headers
			chunk = stripDockerLogHeaders(chunk)
			os.Stdout.Write(chunk)
		}
		if readErr != nil {
			if readErr == io.EOF || errors.Is(readErr, context.Canceled) {
				break
			}
			fmt.Fprintf(os.Stderr, "\nError reading logs: %v\n", readErr)
			return 1
		}
	}

	return 0
}

// ─── Helpers ────────────────────────────────────────────────────────────────────

func cmdServiceHelp() {
	fmt.Println("Usage: cog service <command> [args]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  list              List all declared services and their runtime status")
	fmt.Println("  status <name>     Show detailed status for a service")
	fmt.Println("  pull <name>       Pull/update the service image")
	fmt.Println("  start <name>      Start a service (auto-pulls image)")
	fmt.Println("  stop <name>       Stop a running service")
	fmt.Println("  restart <name>    Restart a service (stop + start)")
	fmt.Println("  logs <name> [-f] [-n N]  View service logs")
	fmt.Println("  help              Show this help")
	fmt.Println()
	fmt.Println("Services are defined in .cog/config/services/<name>.service.yaml")
}

// formatPorts renders port mappings as a compact string.
func formatPorts(ports []ServicePort) string {
	if len(ports) == 0 {
		return "—"
	}
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = fmt.Sprintf("%d→%d", p.Host, p.Container)
	}
	return strings.Join(parts, ", ")
}

// valueOr returns s if non-empty, otherwise fallback.
func valueOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// parseTime parses an ISO time string, returning zero time on failure.
func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// healthCheckClient is a dedicated HTTP client for health checks.
var healthCheckClient = &http.Client{Timeout: 15 * time.Second}

// checkServiceHealth performs an HTTP health check against the service.
func checkServiceHealth(ctx context.Context, crd *ServiceCRD) string {
	if crd.Spec.Health.Endpoint == "" || crd.Spec.Health.Port == 0 {
		return "—"
	}

	timeout, _ := ParseServiceDuration(crd.Spec.Health.Timeout)
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	healthCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	url := fmt.Sprintf("http://localhost:%d%s", crd.Spec.Health.Port, crd.Spec.Health.Endpoint)
	req, err := http.NewRequestWithContext(healthCtx, "GET", url, nil)
	if err != nil {
		return "error"
	}

	resp, err := healthCheckClient.Do(req)
	if err != nil {
		return "unhealthy"
	}
	resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return "healthy"
	}
	return "unhealthy"
}

// waitForHealth polls the health endpoint until it passes or context expires.
func waitForHealth(ctx context.Context, crd *ServiceCRD) error {
	startPeriod, _ := ParseServiceDuration(crd.Spec.Health.StartPeriod)
	if startPeriod == 0 {
		startPeriod = 60 * time.Second
	}
	interval, _ := ParseServiceDuration(crd.Spec.Health.Interval)
	if interval == 0 {
		interval = 5 * time.Second
	}

	deadline := time.Now().Add(startPeriod)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("health check timeout after %s", startPeriod)
			}
			result := checkServiceHealth(ctx, crd)
			if result == "healthy" {
				return nil
			}
		}
	}
}

// tailLocalLog shells out to `tail` for the local-service log file. Keeps
// follow/tail semantics consistent with the docker path without reimplementing
// them. Returns 0 on clean exit or 1 on unexpected error.
func tailLocalLog(path string, follow bool, n string) int {
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintf(os.Stderr, "Error: no log file at %s\n", path)
		return 1
	}
	args := []string{"-n", n}
	if follow {
		args = append(args, "-F")
	}
	args = append(args, path)
	cmd := exec.Command("tail", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// `tail -F` is cancelled by Ctrl-C via exit(130); don't surface as failure.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 130 {
			return 0
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	return 0
}

// stripDockerLogHeaders removes the 8-byte Docker multiplexed stream headers
// from log output. Each frame: [type(1)][0(3)][size_be32(4)][payload].
func stripDockerLogHeaders(data []byte) []byte {
	var result []byte
	for len(data) >= 8 {
		// Header: stream type (1 byte), 3 zero bytes, payload size (4 bytes big-endian)
		streamType := data[0]
		if streamType > 2 || data[1] != 0 || data[2] != 0 || data[3] != 0 {
			// Not a valid Docker stream header — return raw data
			return append(result, data...)
		}
		size := int(data[4])<<24 | int(data[5])<<16 | int(data[6])<<8 | int(data[7])
		data = data[8:]
		if size > len(data) {
			size = len(data)
		}
		result = append(result, data[:size]...)
		data = data[size:]
	}
	// Any remaining bytes that don't form a full header
	result = append(result, data...)
	return result
}
