// bus_reader_http.go — HTTP-backed BusReader implementation.
//
// Reads events from the kernel's GET /v1/bus/{channel}/events endpoint.
// Used by FetchLive to pull tournament.trial.v1 events from bus_tournament.
//
// This is the Phase C concrete implementation of the BusReader interface
// defined in provider.go. The kernel URL is configurable; defaults to
// http://localhost:6931.
package eval

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

// BusEvent is a single event from the kernel bus, as returned by
// GET /v1/bus/{channel}/events. Matches cogfield.Block JSON shape.
type BusEvent struct {
	V       int                    `json:"v"`
	BusID   string                 `json:"bus_id,omitempty"`
	Seq     int                    `json:"seq,omitempty"`
	Ts      string                 `json:"ts"`
	From    string                 `json:"from"`
	Type    string                 `json:"type"`
	Payload map[string]interface{} `json:"payload,omitempty"`
	Hash    string                 `json:"hash"`
}

// BusReader reads events from a named bus channel.
//
// Implementations are separate from BusEmitter so mocks are simpler —
// see design memo Q7.
type BusReader interface {
	// ReadChannel reads events from the named channel. since is a hint
	// (hash or timestamp) for incremental reads; implementations may ignore it
	// and return all events (design memo Q2 recommends all-time reads).
	ReadChannel(ctx context.Context, channelName string, since string) ([]BusEvent, error)
}

// HTTPBusReader implements BusReader over the kernel HTTP API.
// Hits GET /v1/bus/{channel}/events with a large limit to capture all events.
type HTTPBusReader struct {
	kernelURL string
	client    *http.Client
}

// NewHTTPBusReader constructs a BusReader backed by the kernel HTTP API.
// kernelURL should be the base URL, e.g. "http://localhost:6931".
func NewHTTPBusReader(kernelURL string) *HTTPBusReader {
	return &HTTPBusReader{
		kernelURL: kernelURL,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// ReadChannel fetches all events from the named bus channel.
// since is ignored in this implementation (we read all-time per design memo Q2).
// The kernel's retention policy governs actual eviction.
func (r *HTTPBusReader) ReadChannel(ctx context.Context, channelName string, since string) ([]BusEvent, error) {
	endpoint := fmt.Sprintf("%s/v1/bus/%s/events", r.kernelURL, url.PathEscape(channelName))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("bus reader: build request: %w", err)
	}

	q := req.URL.Query()
	q.Set("limit", "1000") // fetch up to 1000 events all-time
	req.URL.RawQuery = q.Encode()

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bus reader: GET %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// Channel doesn't exist yet — return empty
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("bus reader: kernel returned %d: %s", resp.StatusCode, body)
	}

	var events []BusEvent
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		return nil, fmt.Errorf("bus reader: decode events: %w", err)
	}
	return events, nil
}

// FileBusReader reads events from a JSONL file on disk.
// Used as a fallback when the kernel is not running, or in tests.
type FileBusReader struct {
	eventsPath string
}

// NewFileBusReader constructs a BusReader backed by a JSONL events file.
// eventsPath is the absolute path to the events.jsonl file.
func NewFileBusReader(eventsPath string) *FileBusReader {
	return &FileBusReader{eventsPath: eventsPath}
}

// ReadChannel reads events from the JSONL file at r.eventsPath.
// channelName and since are ignored — the file contains one channel's events.
func (r *FileBusReader) ReadChannel(ctx context.Context, channelName string, since string) ([]BusEvent, error) {
	data, err := os.ReadFile(r.eventsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("bus reader: read file %s: %w", r.eventsPath, err)
	}

	var events []BusEvent
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var e BusEvent
		if err := json.Unmarshal(line, &e); err != nil {
			// Skip malformed lines
			continue
		}
		events = append(events, e)
	}
	return events, nil
}
