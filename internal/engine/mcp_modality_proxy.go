// mcp_modality_proxy.go — kernel-side MCP proxy for mod3 voice tools.
//
// Wave 3 of the mod3-kernel integration (ADR-082 + channel-provider RFC).
// The kernel becomes the MCP front door for mod3; the previous OpenClaw
// gateway pattern in the installed binary read metrics but discarded audio
// bytes. This proxy fixes that: it forwards HTTP calls to mod3, captures the
// audio/wav payload, plays it locally via afplay/aplay (fire-and-forget by
// default), and returns mod3's metric headers (X-Mod3-*) to the MCP caller.
//
// Design locks:
//
//  1. MCP transport = HTTP proxy. Every tool handler here POSTs/GETs against
//     cfg.Mod3URL + "/v1/*". Mod3 is NOT an MCP server to the kernel. The
//     installed binary's OpenClaw gateway is a separate concern — we are the
//     next kernel build and will supersede it when deployed.
//  2. Session authority = kernel-owned. session_id is threaded through every
//     proxied call. Callers pass it as an optional field; absent → proxy
//     omits it and mod3 routes to its default session. Present → proxy
//     includes it in the request body (synthesize) or query string (stop).
//  3. Playback strategy = Option (A), server-side. Kernel receives audio/wav,
//     writes to a tempfile, execs `afplay` (macOS) or `aplay` (Linux),
//     fire-and-forget. Callers can opt in to blocking with blocking=true.
//     Forward-compatible with Option (B) session-routed playback once the
//     Wave 4 dashboard WebSocket lands — a future session-router check can
//     gate this path when a browser subscriber exists.
//
// Tools registered (prefix `mod3_` to namespace against cog_* kernel tools):
//
//   - mod3_speak                — synthesize + (optionally) play
//   - mod3_stop                 — cancel current/queued speech
//   - mod3_voices               — list available voices
//   - mod3_status               — mod3 /health probe + build info
//   - mod3_register_session     — proxy to mod3 session register (future)
//   - mod3_deregister_session   — proxy to mod3 session deregister
//   - mod3_list_sessions        — proxy to mod3 session list
//
// Note: the session-registry family forwards to mod3's /v1/sessions/* routes
// which are not yet live on every mod3 instance (see openapi.json). They
// return a clean 502 "mod3_unreachable" in that case; once mod3 implements
// the routes (ADR-082 Wave 2 target), these tools become useful.
package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── proxy wiring on MCPServer ───────────────────────────────────────────────

// modalityProxy holds the HTTP client and playback helper used by the mod3_*
// MCP tools. Fields are exported-by-convention (capitalized where needed for
// tests) so test code can override the HTTP client and the player command.
type modalityProxy struct {
	// client is the HTTP client used for all mod3 forwards. Nil falls back
	// to defaultMod3ProxyClient.
	client *http.Client

	// player is the OS command executed for server-side audio playback.
	// Overridable in tests to a stub binary / /usr/bin/true. Empty means
	// "autodetect via runtime.GOOS" (afplay on darwin, aplay elsewhere).
	player string

	// playerArgs, when non-nil, are passed as additional command args
	// before the tempfile path. Useful for tests to pipe the wav through
	// a counting script. Nil means no extra args.
	playerArgs []string

	// disablePlayback short-circuits the player exec entirely. Tests set
	// this when they want to assert "we got the bytes" without spawning a
	// real player. Production code leaves it false.
	disablePlayback bool
}

// defaultMod3ProxyTimeout is the per-request timeout for mod3 forwards. 30s
// covers the longest-plausible synthesis on the current Kokoro voice stack
// (~5-10s for multi-sentence input, with headroom for cold starts).
const defaultMod3ProxyTimeout = 30 * time.Second

// defaultMod3ProxyClient is the shared http.Client used when modalityProxy.client
// is nil. Lazily initialised; safe for concurrent use.
var defaultMod3ProxyClient = &http.Client{Timeout: defaultMod3ProxyTimeout}

// getModalityProxy returns the MCPServer's modality proxy, lazily creating
// one with sane defaults on first access. Tests can pre-seed m.mod3Proxy with
// their own instance before calling this.
func (m *MCPServer) getModalityProxy() *modalityProxy {
	if m.mod3Proxy == nil {
		m.mod3Proxy = &modalityProxy{}
	}
	return m.mod3Proxy
}

// ─── tool registration ───────────────────────────────────────────────────────

// registerMod3Tools installs the 7 mod3_* MCP tools. Called from
// MCPServer.registerTools after the cog_* tools so the tool index stays
// stable at the front.
func (m *MCPServer) registerMod3Tools() {
	mcp.AddTool(m.server, &mcp.Tool{
		Name: "mod3_speak",
		Description: "Synthesize text to speech via mod3 and play the audio " +
			"locally. Required: text. Optional: session_id, voice, speed, " +
			"emotion, blocking (wait for playback to finish). Returns mod3 " +
			"metrics (job_id, duration_sec, rtf, voice) and a playback_status " +
			"flag. Fallback: curl -X POST http://localhost:7860/v1/synthesize " +
			"-d '{\"text\":\"...\"}' -o out.wav && afplay out.wav",
	}, withToolObserver(m, "mod3_speak", m.toolMod3Speak))

	mcp.AddTool(m.server, &mcp.Tool{
		Name: "mod3_stop",
		Description: "Stop current mod3 speech and/or cancel queued jobs. " +
			"Optional: session_id, job_id (cancel one specific job). Empty " +
			"cancels current playback and clears the queue. Returns mod3's " +
			"barge-in interruption context. Fallback: curl -X POST " +
			"http://localhost:7860/v1/stop",
	}, withToolObserver(m, "mod3_stop", m.toolMod3Stop))

	mcp.AddTool(m.server, &mcp.Tool{
		Name: "mod3_voices",
		Description: "List available mod3 voices, optionally scoped to a " +
			"session. Optional: session_id. Returns the voice catalogue mod3 " +
			"exposes (id, name, language, gender metadata per voice). " +
			"Fallback: curl http://localhost:7860/v1/voices",
	}, withToolObserver(m, "mod3_voices", m.toolMod3Voices))

	mcp.AddTool(m.server, &mcp.Tool{
		Name: "mod3_status",
		Description: "Probe mod3's /health endpoint. Returns the raw health " +
			"payload (model_loaded, engine info, queue_depth, etc). 502 if " +
			"mod3 is unreachable. Fallback: curl http://localhost:7860/health",
	}, withToolObserver(m, "mod3_status", m.toolMod3Status))

	mcp.AddTool(m.server, &mcp.Tool{
		Name: "mod3_register_session",
		Description: "Proxy to mod3's POST /v1/sessions/register. Required: " +
			"participant_id. Optional: session_id (caller-supplied; kernel " +
			"does NOT mint here — use /v1/channel-sessions/register for that), " +
			"participant_type, preferred_voice, preferred_output_device, " +
			"priority. Returns mod3's full SessionRegisterResponse (assigned_" +
			"voice, voice_conflict, output_device, queue_depth).",
	}, withToolObserver(m, "mod3_register_session", m.toolMod3RegisterSession))

	mcp.AddTool(m.server, &mcp.Tool{
		Name: "mod3_deregister_session",
		Description: "Proxy to mod3's POST /v1/sessions/{session_id}/deregister. " +
			"Required: session_id. Returns mod3's deregister acknowledgment " +
			"(released_voice, dropped_jobs).",
	}, withToolObserver(m, "mod3_deregister_session", m.toolMod3DeregisterSession))

	mcp.AddTool(m.server, &mcp.Tool{
		Name: "mod3_list_sessions",
		Description: "Proxy to mod3's GET /v1/sessions. Returns the live " +
			"mod3 session roster (sessions, voice_pool, voice_holders, " +
			"serializer policy). No kernel-side filtering — mod3 is source " +
			"of truth for per-channel state.",
	}, withToolObserver(m, "mod3_list_sessions", m.toolMod3ListSessions))
}

// ─── input / output types ────────────────────────────────────────────────────

type mod3SpeakInput struct {
	Text      string  `json:"text"`
	SessionID string  `json:"session_id,omitempty"`
	Voice     string  `json:"voice,omitempty"`
	Speed     float64 `json:"speed,omitempty"`
	Emotion   float64 `json:"emotion,omitempty"`
	// Blocking waits for the spawned player to exit before returning the
	// tool result. Default false — fire-and-forget so multi-second audio
	// doesn't block the MCP call.
	Blocking bool `json:"blocking,omitempty"`
	// SkipPlayback returns the wav bytes (base64) without attempting local
	// playback. Useful for callers routing audio elsewhere (dashboard WS,
	// file write, etc). Default false.
	SkipPlayback bool `json:"skip_playback,omitempty"`
}

type mod3StopInput struct {
	SessionID string `json:"session_id,omitempty"`
	JobID     string `json:"job_id,omitempty"`
}

type mod3VoicesInput struct {
	SessionID string `json:"session_id,omitempty"`
}

type mod3StatusInput struct{}

type mod3RegisterSessionInput struct {
	SessionID             string `json:"session_id,omitempty"`
	ParticipantID         string `json:"participant_id"`
	ParticipantType       string `json:"participant_type,omitempty"`
	PreferredVoice        string `json:"preferred_voice,omitempty"`
	PreferredOutputDevice string `json:"preferred_output_device,omitempty"`
	Priority              int    `json:"priority,omitempty"`
}

type mod3DeregisterSessionInput struct {
	SessionID string `json:"session_id"`
}

type mod3ListSessionsInput struct{}

// ─── handlers ────────────────────────────────────────────────────────────────

func (m *MCPServer) toolMod3Speak(ctx context.Context, req *mcp.CallToolRequest, in mod3SpeakInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.Text) == "" {
		return textResult("text is required")
	}
	body := map[string]any{"text": in.Text}
	if in.Voice != "" {
		body["voice"] = in.Voice
	}
	if in.Speed > 0 {
		body["speed"] = in.Speed
	}
	if in.Emotion > 0 {
		body["emotion"] = in.Emotion
	}
	if in.SessionID != "" {
		// Session threading: mod3 ignores unknown fields on SynthesizeRequest
		// today; once multi-session synthesis lands, this is the channel.
		body["session_id"] = in.SessionID
	}
	raw, _ := json.Marshal(body)

	audio, headers, status, err := m.proxyMod3Bytes(ctx, http.MethodPost,
		"/v1/synthesize", bytes.NewReader(raw), "application/json")
	if err != nil {
		return mod3ErrorResult(fmt.Sprintf("mod3 unreachable: %v", err))
	}
	if status < 200 || status >= 300 {
		return mod3ErrorResult(fmt.Sprintf("mod3 returned %d: %s", status, truncate(string(audio), 400)))
	}

	metrics := extractMod3Metrics(headers)
	result := map[string]any{
		"ok":         true,
		"bytes":      len(audio),
		"metrics":    metrics,
		"session_id": in.SessionID, // may be empty; echoed for observability
	}

	// If the caller asked for raw bytes (no server-side playback), base64-
	// encode and return. Forward-compatible with session-routed playback.
	if in.SkipPlayback {
		result["audio_base64"] = base64.StdEncoding.EncodeToString(audio)
		result["playback_status"] = "skipped"
		return marshalResult(result)
	}

	p := m.getModalityProxy()
	if p.disablePlayback {
		result["playback_status"] = "disabled"
		return marshalResult(result)
	}

	playErr := p.playAudio(audio, in.Blocking)
	switch {
	case playErr == nil && in.Blocking:
		result["playback_status"] = "played"
	case playErr == nil:
		result["playback_status"] = "spawned"
	default:
		result["playback_status"] = "error"
		result["playback_error"] = playErr.Error()
	}
	return marshalResult(result)
}

func (m *MCPServer) toolMod3Stop(ctx context.Context, req *mcp.CallToolRequest, in mod3StopInput) (*mcp.CallToolResult, any, error) {
	path := "/v1/stop"
	q := url.Values{}
	if in.JobID != "" {
		q.Set("job_id", in.JobID)
	}
	if in.SessionID != "" {
		q.Set("session_id", in.SessionID)
	}
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return m.proxyMod3JSONAsMCP(ctx, http.MethodPost, path, nil)
}

func (m *MCPServer) toolMod3Voices(ctx context.Context, req *mcp.CallToolRequest, in mod3VoicesInput) (*mcp.CallToolResult, any, error) {
	path := "/v1/voices"
	if in.SessionID != "" {
		path += "?session_id=" + url.QueryEscape(in.SessionID)
	}
	return m.proxyMod3JSONAsMCP(ctx, http.MethodGet, path, nil)
}

func (m *MCPServer) toolMod3Status(ctx context.Context, req *mcp.CallToolRequest, in mod3StatusInput) (*mcp.CallToolResult, any, error) {
	return m.proxyMod3JSONAsMCP(ctx, http.MethodGet, "/health", nil)
}

func (m *MCPServer) toolMod3RegisterSession(ctx context.Context, req *mcp.CallToolRequest, in mod3RegisterSessionInput) (*mcp.CallToolResult, any, error) {
	if in.ParticipantID == "" {
		return textResult("participant_id is required")
	}
	body := map[string]any{
		"participant_id": in.ParticipantID,
	}
	if in.SessionID != "" {
		body["session_id"] = in.SessionID
	}
	if in.ParticipantType != "" {
		body["participant_type"] = in.ParticipantType
	}
	if in.PreferredVoice != "" {
		body["preferred_voice"] = in.PreferredVoice
	}
	if in.PreferredOutputDevice != "" {
		body["preferred_output_device"] = in.PreferredOutputDevice
	}
	if in.Priority != 0 {
		body["priority"] = in.Priority
	}
	raw, _ := json.Marshal(body)
	return m.proxyMod3JSONAsMCP(ctx, http.MethodPost, "/v1/sessions/register", bytes.NewReader(raw))
}

func (m *MCPServer) toolMod3DeregisterSession(ctx context.Context, req *mcp.CallToolRequest, in mod3DeregisterSessionInput) (*mcp.CallToolResult, any, error) {
	if in.SessionID == "" {
		return textResult("session_id is required")
	}
	return m.proxyMod3JSONAsMCP(ctx, http.MethodPost,
		"/v1/sessions/"+url.PathEscape(in.SessionID)+"/deregister", nil)
}

func (m *MCPServer) toolMod3ListSessions(ctx context.Context, req *mcp.CallToolRequest, in mod3ListSessionsInput) (*mcp.CallToolResult, any, error) {
	return m.proxyMod3JSONAsMCP(ctx, http.MethodGet, "/v1/sessions", nil)
}

// ─── HTTP forwarder primitives ───────────────────────────────────────────────

// proxyMod3Bytes issues an HTTP request to mod3 and returns the raw body,
// response headers, HTTP status, and a transport error. Caller owns the body
// bytes; they may be audio/wav (mod3_speak) or JSON (everything else).
func (m *MCPServer) proxyMod3Bytes(ctx context.Context, method, path string, body io.Reader, contentType string) ([]byte, http.Header, int, error) {
	if m.cfg == nil {
		return nil, nil, 0, errors.New("Mod3URL not configured (cfg nil)")
	}
	base := strings.TrimRight(m.cfg.Mod3URL, "/")
	if base == "" {
		return nil, nil, 0, errors.New("Mod3URL not configured")
	}

	reqCtx, cancel := context.WithTimeout(ctx, defaultMod3ProxyTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, method, base+path, body)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("build request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	// Accept both audio and JSON so a single client path covers synthesize
	// (audio/wav) and the rest (application/json).
	req.Header.Set("Accept", "audio/wav, application/json")

	client := m.getModalityProxy().client
	if client == nil {
		client = defaultMod3ProxyClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, 0, err
	}
	defer resp.Body.Close()

	// 16 MB cap — more than enough for multi-minute Kokoro wav at 24kHz
	// (~2 MB per 30s), safety net against an upstream that never closes.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, resp.Header, resp.StatusCode, fmt.Errorf("read response body: %w", err)
	}
	return raw, resp.Header, resp.StatusCode, nil
}

// proxyMod3JSONAsMCP is a convenience wrapper for tools whose response is
// JSON (everything except mod3_speak). Reads the body, parses it as JSON if
// possible, and returns an mcp.CallToolResult; on non-2xx status returns a
// mod3-error marshalled result so the caller sees the mod3 body intact.
func (m *MCPServer) proxyMod3JSONAsMCP(ctx context.Context, method, path string, body io.Reader) (*mcp.CallToolResult, any, error) {
	contentType := ""
	if body != nil {
		contentType = "application/json"
	}
	raw, _, status, err := m.proxyMod3Bytes(ctx, method, path, body, contentType)
	if err != nil {
		return mod3ErrorResult(fmt.Sprintf("mod3 unreachable: %v", err))
	}
	// Try to parse as JSON; if parse fails, surface the body as text so the
	// caller at least sees what mod3 said.
	var parsed any
	if len(raw) > 0 {
		if jsonErr := json.Unmarshal(raw, &parsed); jsonErr != nil {
			parsed = map[string]any{"raw": string(raw)}
		}
	}
	if status < 200 || status >= 300 {
		return mod3ErrorResult(fmt.Sprintf("mod3 returned %d: %v", status, parsed))
	}
	return marshalResult(parsed)
}

// mod3ErrorResult returns an IsError=true CallToolResult so the observer
// wrapper records the tool invocation as a failure in the ledger.
func mod3ErrorResult(msg string) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
		IsError: true,
	}, nil, nil
}

// extractMod3Metrics pulls the X-Mod3-* headers into a metrics map. Numeric
// fields are parsed when possible; unknown headers pass through as strings
// so future mod3 headers surface without code changes.
func extractMod3Metrics(h http.Header) map[string]any {
	out := map[string]any{}
	for key, values := range h {
		lk := strings.ToLower(key)
		if !strings.HasPrefix(lk, "x-mod3-") || len(values) == 0 {
			continue
		}
		short := strings.TrimPrefix(lk, "x-mod3-")
		v := values[0]
		// Try numeric parse for the common metric headers.
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			// Preserve integers as int for nicer JSON output.
			if i, ierr := strconv.ParseInt(v, 10, 64); ierr == nil {
				out[short] = i
			} else {
				out[short] = f
			}
			continue
		}
		out[short] = v
	}
	return out
}

// ─── playback helper ─────────────────────────────────────────────────────────

// playAudio writes the wav bytes to a tempfile and spawns the platform's
// default player. When blocking==false the function returns immediately
// after the process starts; a goroutine waits for exit so the tempfile can
// be cleaned up. When blocking==true the function waits for exit and
// surfaces any non-zero return as an error.
//
// In tests, set modalityProxy.player to "/usr/bin/true" (or similar) to
// avoid actually playing audio.
func (p *modalityProxy) playAudio(wav []byte, blocking bool) error {
	if p.disablePlayback {
		return nil
	}
	f, err := os.CreateTemp("", "mod3-speak-*.wav")
	if err != nil {
		return fmt.Errorf("tempfile: %w", err)
	}
	path := f.Name()
	if _, err := f.Write(wav); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("write tempfile: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return fmt.Errorf("close tempfile: %w", err)
	}

	player := p.player
	if player == "" {
		player = defaultPlayerCommand()
	}
	if player == "" {
		os.Remove(path)
		return fmt.Errorf("no audio player available for GOOS=%s", runtime.GOOS)
	}

	args := append([]string{}, p.playerArgs...)
	args = append(args, path)
	cmd := exec.Command(player, args...)

	if err := cmd.Start(); err != nil {
		os.Remove(path)
		return fmt.Errorf("start %s: %w", player, err)
	}

	if blocking {
		err := cmd.Wait()
		_ = os.Remove(path)
		if err != nil {
			return fmt.Errorf("player %s exited: %w", player, err)
		}
		return nil
	}
	// Fire-and-forget: reap the child so the tempfile gets cleaned and the
	// process isn't a zombie. Log errors; don't propagate (the MCP call
	// already returned successfully).
	go func() {
		if werr := cmd.Wait(); werr != nil {
			slog.Debug("mod3 proxy: player exited non-zero",
				"player", player, "path", path, "err", werr)
		}
		_ = os.Remove(path)
	}()
	return nil
}

// defaultPlayerCommand returns the preferred platform player, or "" when
// none is available in PATH. Resolved lazily per call so tests that change
// PATH take effect.
func defaultPlayerCommand() string {
	candidates := map[string][]string{
		"darwin":  {"afplay"},
		"linux":   {"aplay", "paplay"},
		"freebsd": {"aplay"},
	}
	for _, name := range candidates[runtime.GOOS] {
		if _, err := exec.LookPath(name); err == nil {
			return name
		}
	}
	// Final fallback: if neither platform default is present, see if the
	// caller has exposed one via PATH under its canonical name.
	for _, name := range []string{"afplay", "aplay", "paplay", "ffplay"} {
		if _, err := exec.LookPath(name); err == nil {
			return name
		}
	}
	return ""
}
