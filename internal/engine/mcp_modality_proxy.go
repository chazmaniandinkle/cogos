// mcp_modality_proxy.go — kernel-side MCP proxy for mod3 voice tools.
//
// Wave 3 of the mod3-kernel integration (ADR-082 + channel-provider RFC),
// consolidated in Wave 3.5 with Wave 2's session-ID authority.
// The kernel becomes the MCP front door for mod3; the previous OpenClaw
// gateway pattern in the installed binary read metrics but discarded audio
// bytes. This proxy fixes that: it forwards HTTP calls to mod3, captures the
// audio/wav payload, plays it locally via afplay/aplay (fire-and-forget by
// default), and returns mod3's metric headers (X-Mod3-*) to the MCP caller.
//
// Design locks:
//
//  1. MCP transport = HTTP proxy. Synthesis/control tool handlers here
//     POST/GET against cfg.Mod3URL + "/v1/*". Mod3 is NOT an MCP server to
//     the kernel. The installed binary's OpenClaw gateway is a separate
//     concern — we are the next kernel build and will supersede it when
//     deployed.
//  2. Session authority = kernel-owned (Wave 3.5). The session-family tools
//     (register/deregister/list) do NOT call mod3 directly — they call the
//     kernel's RegisterChannelSession / DeregisterChannelSession /
//     ListChannelSessions methods on the Server, which mint the session_id
//     and forward to mod3. Session ID minting happens in exactly one place.
//  3. Playback strategy = Option (A), server-side. Kernel receives audio/wav,
//     writes to a tempfile, execs `afplay` (macOS) or `aplay` (Linux),
//     fire-and-forget. Callers can opt in to blocking with blocking=true.
//     Forward-compatible with Option (B) session-routed playback once the
//     Wave 4 dashboard WebSocket lands — a future session-router check can
//     gate this path when a browser subscriber exists.
//
// Tools registered (prefix `mod3_` to namespace against cog_* kernel tools):
//
//   - mod3_speak                — synthesize + (optionally) play      (direct to mod3)
//   - mod3_stop                 — cancel current/queued speech        (direct to mod3)
//   - mod3_voices               — list available voices               (direct to mod3)
//   - mod3_status               — mod3 /health probe + build info     (direct to mod3)
//   - mod3_register_session     — kernel-minted session registration  (via kernel)
//   - mod3_deregister_session   — session deregister                  (via kernel)
//   - mod3_list_sessions        — merged kernel+mod3 session roster   (via kernel)
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

	// subscriberCheck, when non-nil, is consulted before spawning the local
	// player in mod3_speak. If it returns (true, nil) the kernel skips
	// afplay — mod3's /ws/audio/{session_id} WebSocket is already pushing
	// the WAV to a dashboard subscriber (Wave 4.3). Errors and false return
	// values fall through to the normal playback path. Nil means "use the
	// default HTTP implementation" (GET {Mod3URL}/v1/sessions/{id}/subscribers).
	subscriberCheck func(ctx context.Context, sessionID string) (bool, error)
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
	mcp.AddTool(m.server, m.trackTool(&mcp.Tool{
		Name: "mod3_speak",
		Description: "Synthesize text to speech via mod3 and play the audio " +
			"locally. Required: text. Optional: session_id, voice, speed, " +
			"emotion, blocking (wait for playback to finish). Returns mod3 " +
			"metrics (job_id, duration_sec, rtf, voice) and a playback_status " +
			"flag. Fallback: curl -X POST http://localhost:7860/v1/synthesize " +
			"-d '{\"text\":\"...\"}' -o out.wav && afplay out.wav",
	}), withToolObserver(m, "mod3_speak", m.toolMod3Speak))

	mcp.AddTool(m.server, m.trackTool(&mcp.Tool{
		Name: "mod3_stop",
		Description: "Stop current mod3 speech and/or cancel queued jobs. " +
			"Optional: session_id, job_id (cancel one specific job). Empty " +
			"cancels current playback and clears the queue. Returns mod3's " +
			"barge-in interruption context. Fallback: curl -X POST " +
			"http://localhost:7860/v1/stop",
	}), withToolObserver(m, "mod3_stop", m.toolMod3Stop))

	mcp.AddTool(m.server, m.trackTool(&mcp.Tool{
		Name: "mod3_voices",
		Description: "List available mod3 voices, optionally scoped to a " +
			"session. Optional: session_id. Returns the voice catalogue mod3 " +
			"exposes (id, name, language, gender metadata per voice). " +
			"Fallback: curl http://localhost:7860/v1/voices",
	}), withToolObserver(m, "mod3_voices", m.toolMod3Voices))

	mcp.AddTool(m.server, m.trackTool(&mcp.Tool{
		Name: "mod3_status",
		Description: "Probe mod3's /health endpoint. Returns the raw health " +
			"payload (model_loaded, engine info, queue_depth, etc). 502 if " +
			"mod3 is unreachable. Fallback: curl http://localhost:7860/health",
	}), withToolObserver(m, "mod3_status", m.toolMod3Status))

	mcp.AddTool(m.server, m.trackTool(&mcp.Tool{
		Name: "mod3_register_session",
		Description: "Register a channel-participant session. Routes through " +
			"the kernel's /v1/channel-sessions/register endpoint so " +
			"session_id minting stays centralized (ADR-082 Wave 3.5). " +
			"Required: participant_id. Optional: session_id (kernel mints " +
			"a cs-* short UUID when absent), participant_type " +
			"(agent|user|provider), preferred_voice, preferred_output_device, " +
			"priority, kinds (e.g. [\"audio\"] per channel-provider RFC), " +
			"metadata (opaque pass-through). Returns the merged {kernel, " +
			"mod3} block: kernel identity record + mod3's full " +
			"SessionRegisterResponse (assigned_voice, voice_conflict, " +
			"output_device, queue_depth).",
	}), withToolObserver(m, "mod3_register_session", m.toolMod3RegisterSession))

	mcp.AddTool(m.server, m.trackTool(&mcp.Tool{
		Name: "mod3_deregister_session",
		Description: "Deregister a channel-participant session. Routes " +
			"through the kernel's /v1/channel-sessions/{id}/deregister " +
			"endpoint so the kernel drops its identity record in sync with " +
			"mod3. Required: session_id. Returns mod3's deregister " +
			"acknowledgment (released_voice, dropped_jobs).",
	}), withToolObserver(m, "mod3_deregister_session", m.toolMod3DeregisterSession))

	mcp.AddTool(m.server, m.trackTool(&mcp.Tool{
		Name: "mod3_list_sessions",
		Description: "List channel-participant sessions via the kernel's " +
			"/v1/channel-sessions endpoint. Returns a merged {kernel, mod3} " +
			"block: kernel identity records + mod3's live per-channel state " +
			"(voice_pool, voice_holders, serializer policy).",
	}), withToolObserver(m, "mod3_list_sessions", m.toolMod3ListSessions))
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
	// Kinds / Metadata are the channel-provider RFC fields that flow
	// through to mod3 unchanged. See cogos_session_register primitive.
	Kinds    []string       `json:"kinds,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
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

	// Wave 4.3 — if the session has a live dashboard WebSocket subscriber,
	// mod3 is already routing the WAV there. Skip the kernel's local player
	// so we don't double-play. The check is scoped to sessions that were
	// actually named on the speak call; session_id="" always falls through
	// to the normal afplay path so CLI invocations keep working.
	if in.SessionID != "" {
		subscribed, checkErr := m.checkSessionSubscriber(ctx, in.SessionID)
		if checkErr != nil {
			// Log-worthy but not fatal — fall back to local playback.
			slog.Debug("mod3 proxy: subscriber check failed",
				"session_id", in.SessionID, "err", checkErr)
			result["subscriber_check_error"] = checkErr.Error()
		}
		if subscribed {
			result["playback_status"] = "routed_ws"
			return marshalResult(result)
		}
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

// checkSessionSubscriber asks mod3 whether ``sessionID`` has at least one
// active dashboard WebSocket subscriber for audio playback. Returns
// ``(subscribed, nil)`` on success, ``(false, err)`` on transport failure.
// ``(false, nil)`` — the default when the proxy has no check configured —
// also suppresses the routing path, so legacy callers see the exact same
// afplay behavior as before.
//
// Injectable via modalityProxy.subscriberCheck for tests. The default is a
// GET against mod3's /v1/sessions/{id}/subscribers endpoint with a 1.5s
// timeout inherited from defaultMod3ProxyTimeout.
func (m *MCPServer) checkSessionSubscriber(ctx context.Context, sessionID string) (bool, error) {
	p := m.getModalityProxy()
	if p.subscriberCheck != nil {
		return p.subscriberCheck(ctx, sessionID)
	}
	// Default implementation — HTTP GET. Scoped to 1.5s so a wedged mod3
	// can't block a speak for more than that; falls back to afplay on timeout.
	checkCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	raw, _, status, err := m.proxyMod3Bytes(checkCtx, http.MethodGet,
		"/v1/sessions/"+url.PathEscape(sessionID)+"/subscribers", nil, "")
	if err != nil {
		return false, err
	}
	if status < 200 || status >= 300 {
		return false, fmt.Errorf("mod3 returned %d: %s", status, truncate(string(raw), 200))
	}
	var body struct {
		Subscribed bool `json:"subscribed"`
		Count      int  `json:"count"`
	}
	if unmarshalErr := json.Unmarshal(raw, &body); unmarshalErr != nil {
		return false, fmt.Errorf("decode subscribers response: %w", unmarshalErr)
	}
	return body.Subscribed, nil
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

// toolMod3RegisterSession routes through the kernel's shared
// RegisterChannelSession so session_id minting happens in exactly one place
// (ADR-082 Wave 3.5). The previous Wave 3 implementation called mod3's
// /v1/sessions/register directly, which bypassed Wave 2's kernel-owned
// minting authority; that path is now gone.
func (m *MCPServer) toolMod3RegisterSession(ctx context.Context, req *mcp.CallToolRequest, in mod3RegisterSessionInput) (*mcp.CallToolResult, any, error) {
	if in.ParticipantID == "" {
		return textResult("participant_id is required")
	}
	if m.channelSessionBackend == nil {
		return mod3ErrorResult("channel-session backend not configured")
	}
	resp, ferr := m.channelSessionBackend.RegisterChannelSession(ctx, channelSessionRegisterRequest{
		SessionID:             in.SessionID,
		ParticipantID:         in.ParticipantID,
		ParticipantType:       in.ParticipantType,
		PreferredVoice:        in.PreferredVoice,
		PreferredOutputDevice: in.PreferredOutputDevice,
		Priority:              in.Priority,
		Kinds:                 in.Kinds,
		Metadata:              in.Metadata,
	})
	if ferr != nil {
		return mod3ErrorResult(channelSessionForwardErrorText(ferr))
	}
	return marshalResult(resp)
}

func (m *MCPServer) toolMod3DeregisterSession(ctx context.Context, req *mcp.CallToolRequest, in mod3DeregisterSessionInput) (*mcp.CallToolResult, any, error) {
	if in.SessionID == "" {
		return textResult("session_id is required")
	}
	if m.channelSessionBackend == nil {
		return mod3ErrorResult("channel-session backend not configured")
	}
	mod3Resp, status, ferr := m.channelSessionBackend.DeregisterChannelSession(ctx, in.SessionID)
	if ferr != nil {
		return mod3ErrorResult(channelSessionForwardErrorText(ferr))
	}
	// Parse mod3's JSON body; surface mod3's non-2xx bodies intact as
	// tool errors. The HTTP handler passes these through verbatim; the
	// MCP tool wraps them so the caller sees the mod3 body text.
	var parsed any
	if len(mod3Resp) > 0 {
		if jsonErr := json.Unmarshal(mod3Resp, &parsed); jsonErr != nil {
			parsed = map[string]any{"raw": string(mod3Resp)}
		}
	}
	if status < 200 || status >= 300 {
		return mod3ErrorResult(fmt.Sprintf("mod3 returned %d: %v", status, parsed))
	}
	return marshalResult(parsed)
}

func (m *MCPServer) toolMod3ListSessions(ctx context.Context, req *mcp.CallToolRequest, in mod3ListSessionsInput) (*mcp.CallToolResult, any, error) {
	if m.channelSessionBackend == nil {
		return mod3ErrorResult("channel-session backend not configured")
	}
	resp, _, ferr := m.channelSessionBackend.ListChannelSessions(ctx)
	if ferr != nil {
		return mod3ErrorResult(channelSessionForwardErrorText(ferr))
	}
	return marshalResult(resp)
}

// channelSessionForwardErrorText renders a *channelSessionForwardError into
// the "mod3 unreachable" / "mod3 returned N: body" shape the legacy MCP
// tool paths used, keeping error surfaces stable for callers that previously
// matched on those strings.
func channelSessionForwardErrorText(ferr *channelSessionForwardError) string {
	switch ferr.Kind {
	case "mod3_unreachable":
		return ferr.Message
	case "mod3_rejected":
		var parsed any
		if len(ferr.Mod3Body) > 0 {
			if jsonErr := json.Unmarshal(ferr.Mod3Body, &parsed); jsonErr != nil {
				parsed = map[string]any{"raw": string(ferr.Mod3Body)}
			}
		}
		return fmt.Sprintf("mod3 returned %d: %v", ferr.HTTPStatus, parsed)
	default:
		return ferr.Message
	}
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
