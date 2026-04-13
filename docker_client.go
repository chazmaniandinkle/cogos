// docker_client.go — Docker Engine API client over unix socket.
//
// Talks directly to the Docker Engine REST API via unix socket transport.
// No Docker CLI shelling, no Docker SDK dependency. Works with Docker Desktop,
// Podman, or Colima — anything that exposes a Docker-compatible unix socket.
//
// Socket discovery order:
//   $DOCKER_HOST → ~/.colima/default/docker.sock → ~/.docker/run/docker.sock → /var/run/docker.sock

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ─── Error Types ────────────────────────────────────────────────────────────────

var (
	ErrRuntimeNotAvailable = fmt.Errorf("container runtime not available")
	ErrImageNotFound       = fmt.Errorf("image not found")
	ErrContainerNotFound   = fmt.Errorf("container not found")
	ErrDockerNotFound      = fmt.Errorf("not found") // generic 404 from Docker API
	ErrPortConflict        = fmt.Errorf("port already in use")
)

// ─── Labels ─────────────────────────────────────────────────────────────────────

const (
	labelManaged = "com.cogos.managed"
	labelService = "com.cogos.service"
	containerPfx = "cogos-"
	apiVersion   = "v1.45"
)

// ─── Client ─────────────────────────────────────────────────────────────────────

// DockerClient communicates with the Docker Engine API over a unix socket.
type DockerClient struct {
	client     *http.Client
	socketPath string
}

// NewDockerClient creates a Docker API client connected to the given unix socket.
// If socketPath is empty, it discovers the socket automatically.
func NewDockerClient(socketPath string) *DockerClient {
	if socketPath == "" {
		socketPath = discoverSocket()
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return &DockerClient{
		client: &http.Client{
			Transport: transport,
			Timeout:   0, // no global timeout — per-request via context
		},
		socketPath: socketPath,
	}
}

// SocketPath returns the resolved socket path.
func (d *DockerClient) SocketPath() string { return d.socketPath }

// discoverSocket finds the Docker-compatible unix socket.
func discoverSocket() string {
	// 1. $DOCKER_HOST (unix:///path/to/socket)
	if dh := os.Getenv("DOCKER_HOST"); dh != "" {
		if strings.HasPrefix(dh, "unix://") {
			return strings.TrimPrefix(dh, "unix://")
		}
	}

	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".colima", "default", "docker.sock"),
		filepath.Join(home, ".docker", "run", "docker.sock"),
		"/var/run/docker.sock",
	}
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	// Return the most common default even if not present —
	// Ping() will surface ErrRuntimeNotAvailable.
	return "/var/run/docker.sock"
}

// apiURL constructs the full URL for an API endpoint.
func apiURL(path string) string {
	return fmt.Sprintf("http://localhost/%s%s", apiVersion, path)
}

// ─── Raw Request Helpers ────────────────────────────────────────────────────────

// doRequest performs an HTTP request and returns the response.
// Caller is responsible for closing resp.Body.
func (d *DockerClient) doRequest(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, apiURL(path), body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client.Do(req)
	if err != nil {
		if isConnectionRefused(err) {
			return nil, ErrRuntimeNotAvailable
		}
		return nil, err
	}
	return resp, nil
}

// doJSON performs a request and decodes the JSON response into dst.
func (d *DockerClient) doJSON(ctx context.Context, method, path string, body io.Reader, dst any) error {
	resp, err := d.doRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return ErrDockerNotFound
	}
	if resp.StatusCode == http.StatusConflict {
		// Read error message for port conflict detection
		var errResp struct{ Message string }
		json.NewDecoder(resp.Body).Decode(&errResp)
		if strings.Contains(errResp.Message, "port is already allocated") ||
			strings.Contains(errResp.Message, "address already in use") {
			return ErrPortConflict
		}
		return fmt.Errorf("conflict: %s", errResp.Message)
	}
	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("docker API %s %s: %d %s", method, path, resp.StatusCode, string(bodyBytes))
	}
	if dst != nil {
		return json.NewDecoder(resp.Body).Decode(dst)
	}
	return nil
}

func isConnectionRefused(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "connection refused") ||
		strings.Contains(s, "no such file or directory") ||
		strings.Contains(s, "connect: no such file")
}

// ─── API Methods ────────────────────────────────────────────────────────────────

// Ping verifies the Docker daemon is reachable.
func (d *DockerClient) Ping(ctx context.Context) error {
	resp, err := d.doRequest(ctx, "GET", "/_ping", nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ping failed: %d", resp.StatusCode)
	}
	return nil
}

// ─── Image Operations ───────────────────────────────────────────────────────────

// ImagePullProgress represents a single progress line during image pull.
type ImagePullProgress struct {
	Status   string `json:"status"`
	Progress string `json:"progress,omitempty"`
	ID       string `json:"id,omitempty"`
}

// ImagePull pulls an image with optional platform and progress callback.
func (d *DockerClient) ImagePull(ctx context.Context, ref, platform string, onProgress func(ImagePullProgress)) error {
	params := url.Values{"fromImage": {ref}}
	if platform != "" {
		params.Set("platform", platform)
	}
	path := "/images/create?" + params.Encode()

	resp, err := d.doRequest(ctx, "POST", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%w: %s", ErrImageNotFound, ref)
		}
		return fmt.Errorf("image pull %s: %d %s", ref, resp.StatusCode, string(bodyBytes))
	}

	// Stream progress
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if onProgress != nil {
			var p ImagePullProgress
			if json.Unmarshal(scanner.Bytes(), &p) == nil {
				onProgress(p)
			}
		}
	}
	return scanner.Err()
}

// ImageInspectResult holds selected fields from image inspect.
type ImageInspectResult struct {
	ID      string   `json:"Id"`
	Tags    []string `json:"RepoTags"`
	Size    int64    `json:"Size"`
	Created string   `json:"Created"`
}

// ImageInspect returns details about a local image.
func (d *DockerClient) ImageInspect(ctx context.Context, ref string) (*ImageInspectResult, error) {
	var result ImageInspectResult
	if err := d.doJSON(ctx, "GET", "/images/"+url.PathEscape(ref)+"/json", nil, &result); err != nil {
		if err == ErrDockerNotFound {
			return nil, fmt.Errorf("%w: %s", ErrImageNotFound, ref)
		}
		return nil, err
	}
	return &result, nil
}

// ─── Container Operations ───────────────────────────────────────────────────────

// ContainerConfig defines a container to create.
type ContainerConfig struct {
	Image        string
	Cmd          []string
	Env          []string
	Labels       map[string]string
	ExposedPorts map[string]struct{}
	HostConfig   HostConfig
	Platform     string
}

// HostConfig defines host-side container settings.
type HostConfig struct {
	PortBindings map[string][]PortBinding
	Binds        []string
	Memory       int64
	NanoCPUs     int64
	RestartPolicy RestartPolicy
}

// PortBinding maps a container port to a host port.
type PortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// RestartPolicy defines container restart behavior.
type RestartPolicy struct {
	Name              string `json:"Name"`
	MaximumRetryCount int    `json:"MaximumRetryCount"`
}

// ContainerCreate creates a container and returns its ID.
func (d *DockerClient) ContainerCreate(ctx context.Context, name string, config ContainerConfig) (string, error) {
	body := map[string]any{
		"Image":        config.Image,
		"Env":          config.Env,
		"Labels":       config.Labels,
		"ExposedPorts": config.ExposedPorts,
		"HostConfig": map[string]any{
			"PortBindings":  config.HostConfig.PortBindings,
			"Binds":         config.HostConfig.Binds,
			"Memory":        config.HostConfig.Memory,
			"NanoCPUs":      config.HostConfig.NanoCPUs,
			"RestartPolicy": config.HostConfig.RestartPolicy,
		},
	}
	if len(config.Cmd) > 0 {
		body["Cmd"] = config.Cmd
	}

	data, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	params := url.Values{}
	if name != "" {
		params.Set("name", name)
	}
	if config.Platform != "" {
		params.Set("platform", config.Platform)
	}
	path := "/containers/create"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}

	var result struct {
		ID       string   `json:"Id"`
		Warnings []string `json:"Warnings"`
	}
	if err := d.doJSON(ctx, "POST", path, strings.NewReader(string(data)), &result); err != nil {
		return "", err
	}
	for _, w := range result.Warnings {
		log.Printf("[docker] container create warning: %s", w)
	}
	return result.ID, nil
}

// ContainerStart starts a container by ID.
func (d *DockerClient) ContainerStart(ctx context.Context, id string) error {
	resp, err := d.doRequest(ctx, "POST", "/containers/"+id+"/start", nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrContainerNotFound
	}
	if resp.StatusCode == http.StatusConflict {
		return nil // already started
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("container start %s: %d", id, resp.StatusCode)
	}
	return nil
}

// ContainerStop gracefully stops a container with the given timeout.
func (d *DockerClient) ContainerStop(ctx context.Context, id string, timeout int) error {
	params := url.Values{"t": {fmt.Sprintf("%d", timeout)}}
	resp, err := d.doRequest(ctx, "POST", "/containers/"+id+"/stop?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrContainerNotFound
	}
	if resp.StatusCode == http.StatusNotModified {
		return nil // already stopped
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("container stop %s: %d", id, resp.StatusCode)
	}
	return nil
}

// ContainerRemove removes a container by ID.
func (d *DockerClient) ContainerRemove(ctx context.Context, id string, force bool) error {
	params := url.Values{}
	if force {
		params.Set("force", "true")
	}
	params.Set("v", "true") // also remove anonymous volumes
	resp, err := d.doRequest(ctx, "DELETE", "/containers/"+id+"?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil // already gone
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("container remove %s: %d", id, resp.StatusCode)
	}
	return nil
}

// ContainerInfo holds selected fields from container inspect.
type ContainerInfo struct {
	ID      string `json:"Id"`
	Name    string `json:"Name"`
	Created string `json:"Created"`
	State   struct {
		Status     string `json:"Status"`
		Running    bool   `json:"Running"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
		ExitCode   int    `json:"ExitCode"`
		Health     *struct {
			Status string `json:"Status"`
		} `json:"Health,omitempty"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Env    []string          `json:"Env"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		PortBindings map[string][]PortBinding `json:"PortBindings"`
		Binds        []string                 `json:"Binds"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Ports map[string][]PortBinding `json:"Ports"`
	} `json:"NetworkSettings"`
}

// ContainerInspect returns full details about a container.
func (d *DockerClient) ContainerInspect(ctx context.Context, id string) (*ContainerInfo, error) {
	var info ContainerInfo
	if err := d.doJSON(ctx, "GET", "/containers/"+id+"/json", nil, &info); err != nil {
		if err == ErrDockerNotFound {
			return nil, ErrContainerNotFound
		}
		return nil, err
	}
	return &info, nil
}

// ContainerListEntry holds fields from the container list endpoint.
type ContainerListEntry struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Labels  map[string]string `json:"Labels"`
	Created int64             `json:"Created"`
	Ports   []struct {
		IP          string `json:"IP"`
		PrivatePort int    `json:"PrivatePort"`
		PublicPort  int    `json:"PublicPort"`
		Type        string `json:"Type"`
	} `json:"Ports"`
}

// ContainerList returns containers matching the given label filters.
func (d *DockerClient) ContainerList(ctx context.Context, labels map[string]string) ([]ContainerListEntry, error) {
	filters := map[string][]string{}
	for k, v := range labels {
		filters["label"] = append(filters["label"], k+"="+v)
	}
	filtersJSON, _ := json.Marshal(filters)
	params := url.Values{
		"all":     {"true"},
		"filters": {string(filtersJSON)},
	}

	var entries []ContainerListEntry
	if err := d.doJSON(ctx, "GET", "/containers/json?"+params.Encode(), nil, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// ContainerLogs streams container logs. If follow is true, it blocks until ctx
// is cancelled. Returns an io.ReadCloser — caller must close.
func (d *DockerClient) ContainerLogs(ctx context.Context, id string, follow bool, tail string) (io.ReadCloser, error) {
	params := url.Values{
		"stdout":     {"true"},
		"stderr":     {"true"},
		"timestamps": {"true"},
	}
	if follow {
		params.Set("follow", "true")
	}
	if tail != "" {
		params.Set("tail", tail)
	}

	resp, err := d.doRequest(ctx, "GET", "/containers/"+id+"/logs?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrContainerNotFound
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("container logs %s: %d %s", id, resp.StatusCode, string(body))
	}
	return resp.Body, nil
}

// ─── Managed Container Helpers ──────────────────────────────────────────────────

// ManagedContainerName returns the prefixed container name for a service.
func ManagedContainerName(serviceName string) string {
	return containerPfx + serviceName
}

// ManagedLabels returns the standard labels for a cogos-managed container.
func ManagedLabels(serviceName string) map[string]string {
	return map[string]string{
		labelManaged: "true",
		labelService: serviceName,
	}
}

// ListManagedContainers returns all cogos-managed containers.
func (d *DockerClient) ListManagedContainers(ctx context.Context) ([]ContainerListEntry, error) {
	return d.ContainerList(ctx, map[string]string{labelManaged: "true"})
}

// FindManagedContainer finds a managed container by service name.
func (d *DockerClient) FindManagedContainer(ctx context.Context, serviceName string) (*ContainerListEntry, error) {
	entries, err := d.ContainerList(ctx, ManagedLabels(serviceName))
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	return &entries[0], nil
}
