// mcp_modality_proxy_test.go — coverage for the mod3 MCP proxy tools.
//
// Strategy: stand up a fake mod3 via httptest.NewServer, point an MCPServer's
// proxy at it, exercise each tool handler directly (handler function +
// typed input, bypassing the MCP SDK's JSON marshal layer). Playback is
// stubbed via disablePlayback or the injectable player field so tests don't
// spawn afplay.
package engine

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── fake mod3 with synthesize + session routes ──────────────────────────────

type fakeMod3Proxy struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	captured []capturedProxyRequest

	// Overrides for per-endpoint behavior.
	synthesizeHandler  http.HandlerFunc
	stopHandler        http.HandlerFunc
	voicesHandler      http.HandlerFunc
	healthHandler      http.HandlerFunc
	regHandler         http.HandlerFunc
	deregHandler       http.HandlerFunc
	listSessionHandler http.HandlerFunc
}

type capturedProxyRequest struct {
	Method string
	Path   string
	Query  string
	Body   []byte
}

// synthWav is a tiny valid-ish WAV header + ~1KB of silence. Not a real
// audio file — the proxy doesn't parse it, it just forwards/plays bytes.
var synthWav = func() []byte {
	b := make([]byte, 1024)
	copy(b, []byte("RIFF\x00\x00\x00\x00WAVEfmt "))
	return b
}()

func newFakeMod3Proxy(t *testing.T) *fakeMod3Proxy {
	t.Helper()
	fm := &fakeMod3Proxy{t: t}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/synthesize", func(w http.ResponseWriter, r *http.Request) {
		fm.capture(r)
		if fm.synthesizeHandler != nil {
			fm.synthesizeHandler(w, r)
			return
		}
		// Emit the full mod3 header surface so the proxy can extract it.
		w.Header().Set("X-Mod3-Job-Id", "job-test-0001")
		w.Header().Set("X-Mod3-Voice", "bm_lewis")
		w.Header().Set("X-Mod3-Duration-Sec", "1.23")
		w.Header().Set("X-Mod3-Sample-Rate", "24000")
		w.Header().Set("X-Mod3-Rtf", "9.29")
		w.Header().Set("X-Mod3-Chunks", "1")
		w.Header().Set("Content-Type", "audio/wav")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(synthWav)
	})

	mux.HandleFunc("POST /v1/stop", func(w http.ResponseWriter, r *http.Request) {
		fm.capture(r)
		if fm.stopHandler != nil {
			fm.stopHandler(w, r)
			return
		}
		writeFakeJSON(w, http.StatusOK, map[string]any{
			"status":        "stopped",
			"dropped_jobs":  0,
			"interrupted":   true,
			"session_id":    r.URL.Query().Get("session_id"),
			"job_id_target": r.URL.Query().Get("job_id"),
		})
	})

	mux.HandleFunc("GET /v1/voices", func(w http.ResponseWriter, r *http.Request) {
		fm.capture(r)
		if fm.voicesHandler != nil {
			fm.voicesHandler(w, r)
			return
		}
		writeFakeJSON(w, http.StatusOK, map[string]any{
			"voices": []map[string]any{
				{"id": "bm_lewis", "language": "en-GB"},
				{"id": "af_bella", "language": "en-US"},
			},
		})
	})

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		fm.capture(r)
		if fm.healthHandler != nil {
			fm.healthHandler(w, r)
			return
		}
		writeFakeJSON(w, http.StatusOK, map[string]any{
			"status":       "ok",
			"model_loaded": true,
			"engine":       "kokoro",
		})
	})

	mux.HandleFunc("POST /v1/sessions/register", func(w http.ResponseWriter, r *http.Request) {
		fm.capture(r)
		if fm.regHandler != nil {
			fm.regHandler(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeFakeJSON(w, http.StatusOK, map[string]any{
			"session_id":     body["session_id"],
			"participant_id": body["participant_id"],
			"assigned_voice": "bm_lewis",
		})
	})

	mux.HandleFunc("POST /v1/sessions/{id}/deregister", func(w http.ResponseWriter, r *http.Request) {
		fm.capture(r)
		if fm.deregHandler != nil {
			fm.deregHandler(w, r)
			return
		}
		writeFakeJSON(w, http.StatusOK, map[string]any{
			"status":     "ok",
			"session_id": r.PathValue("id"),
		})
	})

	mux.HandleFunc("GET /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		fm.capture(r)
		if fm.listSessionHandler != nil {
			fm.listSessionHandler(w, r)
			return
		}
		writeFakeJSON(w, http.StatusOK, map[string]any{
			"sessions":   []any{},
			"voice_pool": []string{"bm_lewis", "af_bella"},
		})
	})

	fm.srv = httptest.NewServer(mux)
	t.Cleanup(func() { fm.srv.Close() })
	return fm
}

func (fm *fakeMod3Proxy) capture(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	fm.mu.Lock()
	defer fm.mu.Unlock()
	fm.captured = append(fm.captured, capturedProxyRequest{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: body,
	})
}

func (fm *fakeMod3Proxy) last() capturedProxyRequest {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	if len(fm.captured) == 0 {
		fm.t.Fatalf("no captured requests")
	}
	return fm.captured[len(fm.captured)-1]
}

// newProxyMCP builds a minimal MCPServer whose proxy points at fm, with
// playback fully disabled so tests don't touch the audio stack.
func newProxyMCP(t *testing.T, fm *fakeMod3Proxy) *MCPServer {
	t.Helper()
	m := &MCPServer{
		cfg:       &Config{Mod3URL: fm.srv.URL},
		mod3Proxy: &modalityProxy{disablePlayback: true},
	}
	return m
}

// decodeToolText parses the JSON text content of a CallToolResult.
func decodeToolText(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatalf("empty result")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected *mcp.TextContent, got %T", res.Content[0])
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(tc.Text), &out); err != nil {
		t.Fatalf("decode result text: %v (raw=%q)", err, tc.Text)
	}
	return out
}

// ─── mod3_speak ──────────────────────────────────────────────────────────────

func TestMod3Speak_SuccessPath(t *testing.T) {
	fm := newFakeMod3Proxy(t)
	m := newProxyMCP(t, fm)

	res, _, err := m.toolMod3Speak(context.Background(), nil, mod3SpeakInput{
		Text: "hello world",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got IsError=true: %v", res.Content)
	}

	out := decodeToolText(t, res)
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("expected ok=true, got %v", out["ok"])
	}
	if bytes, _ := out["bytes"].(float64); bytes < 100 {
		t.Fatalf("expected bytes > 100, got %v", out["bytes"])
	}

	metrics, _ := out["metrics"].(map[string]any)
	if metrics == nil {
		t.Fatal("expected metrics map")
	}
	if jobID, _ := metrics["job-id"].(string); jobID != "job-test-0001" {
		t.Fatalf("expected job-id=job-test-0001, got %v", metrics["job-id"])
	}
	if dur, _ := metrics["duration-sec"].(float64); dur != 1.23 {
		t.Fatalf("expected duration-sec=1.23, got %v", metrics["duration-sec"])
	}
	// Integer parse path — sample-rate comes as "24000".
	if sr, ok := metrics["sample-rate"].(float64); !ok || sr != 24000 {
		t.Fatalf("expected sample-rate=24000, got %v (%T)", metrics["sample-rate"], metrics["sample-rate"])
	}
	if got, _ := out["playback_status"].(string); got != "disabled" {
		t.Fatalf("expected playback_status=disabled, got %v", out["playback_status"])
	}
}

func TestMod3Speak_ForwardsSessionID(t *testing.T) {
	fm := newFakeMod3Proxy(t)
	m := newProxyMCP(t, fm)

	_, _, err := m.toolMod3Speak(context.Background(), nil, mod3SpeakInput{
		Text:      "hello",
		SessionID: "cs-abc123",
		Voice:     "af_bella",
		Speed:     1.1,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cap := fm.last()
	var forwarded map[string]any
	if err := json.Unmarshal(cap.Body, &forwarded); err != nil {
		t.Fatalf("decode forwarded body: %v", err)
	}
	if forwarded["session_id"] != "cs-abc123" {
		t.Fatalf("expected session_id=cs-abc123, got %v", forwarded["session_id"])
	}
	if forwarded["voice"] != "af_bella" {
		t.Fatalf("expected voice=af_bella, got %v", forwarded["voice"])
	}
	if speed, _ := forwarded["speed"].(float64); speed != 1.1 {
		t.Fatalf("expected speed=1.1, got %v", forwarded["speed"])
	}
}

func TestMod3Speak_OmitsSessionIDWhenAbsent(t *testing.T) {
	fm := newFakeMod3Proxy(t)
	m := newProxyMCP(t, fm)

	_, _, err := m.toolMod3Speak(context.Background(), nil, mod3SpeakInput{Text: "plain"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cap := fm.last()
	var forwarded map[string]any
	if err := json.Unmarshal(cap.Body, &forwarded); err != nil {
		t.Fatalf("decode forwarded body: %v", err)
	}
	if _, present := forwarded["session_id"]; present {
		t.Fatalf("expected no session_id key, got %v", forwarded["session_id"])
	}
}

func TestMod3Speak_EmptyTextRejects(t *testing.T) {
	m := &MCPServer{cfg: &Config{Mod3URL: "http://unused"}}
	res, _, err := m.toolMod3Speak(context.Background(), nil, mod3SpeakInput{Text: "   "})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// textResult — IsError is false because it's a validation message, but
	// the content must mention the required field.
	if len(res.Content) == 0 {
		t.Fatal("expected content")
	}
	tc := res.Content[0].(*mcp.TextContent)
	if !strings.Contains(tc.Text, "required") {
		t.Fatalf("expected 'required' in message, got %q", tc.Text)
	}
}

func TestMod3Speak_Mod3DownReturnsCleanError(t *testing.T) {
	// Port that refuses connections.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close() // release the port so dials get ECONNREFUSED

	m := &MCPServer{
		cfg:       &Config{Mod3URL: "http://" + addr},
		mod3Proxy: &modalityProxy{disablePlayback: true},
	}
	res, _, err := m.toolMod3Speak(context.Background(), nil, mod3SpeakInput{Text: "hi"})
	if err != nil {
		t.Fatalf("handler should not return Go error, got %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError=true when mod3 is unreachable")
	}
	tc := res.Content[0].(*mcp.TextContent)
	if !strings.Contains(strings.ToLower(tc.Text), "mod3 unreachable") {
		t.Fatalf("expected 'mod3 unreachable' message, got %q", tc.Text)
	}
}

func TestMod3Speak_PreservesMod3ErrorBody(t *testing.T) {
	fm := newFakeMod3Proxy(t)
	fm.synthesizeHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"detail":"text must not be empty"}`))
	}
	m := newProxyMCP(t, fm)

	res, _, err := m.toolMod3Speak(context.Background(), nil, mod3SpeakInput{Text: "bad"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError=true on mod3 422")
	}
	tc := res.Content[0].(*mcp.TextContent)
	if !strings.Contains(tc.Text, "422") {
		t.Fatalf("expected '422' in error text, got %q", tc.Text)
	}
	if !strings.Contains(tc.Text, "text must not be empty") {
		t.Fatalf("expected mod3 body preserved, got %q", tc.Text)
	}
}

func TestMod3Speak_SkipPlaybackReturnsBase64(t *testing.T) {
	fm := newFakeMod3Proxy(t)
	m := newProxyMCP(t, fm)

	res, _, err := m.toolMod3Speak(context.Background(), nil, mod3SpeakInput{
		Text:         "skip",
		SkipPlayback: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := decodeToolText(t, res)
	if got, _ := out["playback_status"].(string); got != "skipped" {
		t.Fatalf("expected playback_status=skipped, got %v", out["playback_status"])
	}
	if b64, _ := out["audio_base64"].(string); b64 == "" {
		t.Fatal("expected audio_base64 populated when skip_playback=true")
	}
}

// ─── mod3_stop / voices / status / sessions ──────────────────────────────────

func TestMod3Stop_ForwardsSessionAndJob(t *testing.T) {
	fm := newFakeMod3Proxy(t)
	m := newProxyMCP(t, fm)

	res, _, err := m.toolMod3Stop(context.Background(), nil, mod3StopInput{
		SessionID: "cs-abc",
		JobID:     "job-xyz",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IsError {
		t.Fatal("expected success")
	}

	cap := fm.last()
	if !strings.Contains(cap.Query, "session_id=cs-abc") {
		t.Fatalf("expected session_id in query, got %q", cap.Query)
	}
	if !strings.Contains(cap.Query, "job_id=job-xyz") {
		t.Fatalf("expected job_id in query, got %q", cap.Query)
	}

	out := decodeToolText(t, res)
	if out["status"] != "stopped" {
		t.Fatalf("expected status=stopped, got %v", out["status"])
	}
}

func TestMod3Voices_ReturnsRawList(t *testing.T) {
	fm := newFakeMod3Proxy(t)
	m := newProxyMCP(t, fm)

	res, _, err := m.toolMod3Voices(context.Background(), nil, mod3VoicesInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IsError {
		t.Fatal("expected success")
	}
	out := decodeToolText(t, res)
	voices, _ := out["voices"].([]any)
	if len(voices) != 2 {
		t.Fatalf("expected 2 voices, got %d", len(voices))
	}
}

func TestMod3Voices_ThreadsSessionID(t *testing.T) {
	fm := newFakeMod3Proxy(t)
	m := newProxyMCP(t, fm)

	_, _, _ = m.toolMod3Voices(context.Background(), nil, mod3VoicesInput{SessionID: "cs-qq"})
	cap := fm.last()
	if !strings.Contains(cap.Query, "session_id=cs-qq") {
		t.Fatalf("expected session_id in query, got %q", cap.Query)
	}
}

func TestMod3Status_HitsHealth(t *testing.T) {
	fm := newFakeMod3Proxy(t)
	m := newProxyMCP(t, fm)

	res, _, err := m.toolMod3Status(context.Background(), nil, mod3StatusInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IsError {
		t.Fatal("expected success")
	}
	out := decodeToolText(t, res)
	if out["status"] != "ok" {
		t.Fatalf("expected status=ok, got %v", out["status"])
	}

	cap := fm.last()
	if cap.Path != "/health" {
		t.Fatalf("expected /health, got %q", cap.Path)
	}
}

func TestMod3Status_Mod3DownClean(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	_ = l.Close()

	m := &MCPServer{cfg: &Config{Mod3URL: "http://" + addr}}
	res, _, err := m.toolMod3Status(context.Background(), nil, mod3StatusInput{})
	if err != nil {
		t.Fatalf("handler should not Go-error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError=true")
	}
}

func TestMod3RegisterSession_ForwardsBody(t *testing.T) {
	fm := newFakeMod3Proxy(t)
	m := newProxyMCP(t, fm)

	res, _, err := m.toolMod3RegisterSession(context.Background(), nil, mod3RegisterSessionInput{
		SessionID:             "cs-regtest",
		ParticipantID:         "cog",
		ParticipantType:       "agent",
		PreferredVoice:        "bm_lewis",
		PreferredOutputDevice: "system-default",
		Priority:              2,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IsError {
		t.Fatal("expected success")
	}

	cap := fm.last()
	var body map[string]any
	if err := json.Unmarshal(cap.Body, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["session_id"] != "cs-regtest" {
		t.Fatalf("bad session_id: %v", body["session_id"])
	}
	if body["participant_id"] != "cog" {
		t.Fatalf("bad participant_id: %v", body["participant_id"])
	}
	if body["preferred_voice"] != "bm_lewis" {
		t.Fatalf("bad preferred_voice: %v", body["preferred_voice"])
	}
}

func TestMod3RegisterSession_RejectsWithoutParticipant(t *testing.T) {
	fm := newFakeMod3Proxy(t)
	m := newProxyMCP(t, fm)

	res, _, err := m.toolMod3RegisterSession(context.Background(), nil, mod3RegisterSessionInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tc := res.Content[0].(*mcp.TextContent)
	if !strings.Contains(tc.Text, "participant_id") {
		t.Fatalf("expected validation message, got %q", tc.Text)
	}
}

func TestMod3DeregisterSession_PathEscape(t *testing.T) {
	fm := newFakeMod3Proxy(t)
	m := newProxyMCP(t, fm)

	res, _, err := m.toolMod3DeregisterSession(context.Background(), nil, mod3DeregisterSessionInput{
		SessionID: "cs-drop",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IsError {
		t.Fatal("expected success")
	}
	cap := fm.last()
	if cap.Path != "/v1/sessions/cs-drop/deregister" {
		t.Fatalf("expected /v1/sessions/cs-drop/deregister, got %q", cap.Path)
	}
}

func TestMod3DeregisterSession_RequiresID(t *testing.T) {
	m := &MCPServer{cfg: &Config{Mod3URL: "http://unused"}}
	res, _, err := m.toolMod3DeregisterSession(context.Background(), nil, mod3DeregisterSessionInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tc := res.Content[0].(*mcp.TextContent)
	if !strings.Contains(tc.Text, "session_id") {
		t.Fatalf("expected session_id message, got %q", tc.Text)
	}
}

func TestMod3ListSessions_ReturnsRaw(t *testing.T) {
	fm := newFakeMod3Proxy(t)
	m := newProxyMCP(t, fm)

	res, _, err := m.toolMod3ListSessions(context.Background(), nil, mod3ListSessionsInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IsError {
		t.Fatal("expected success")
	}
	out := decodeToolText(t, res)
	vp, _ := out["voice_pool"].([]any)
	if len(vp) != 2 {
		t.Fatalf("expected 2 voices in pool, got %d", len(vp))
	}
}

// ─── metric extraction unit test ─────────────────────────────────────────────

func TestExtractMod3Metrics_TypesByValue(t *testing.T) {
	h := http.Header{}
	h.Set("X-Mod3-Job-Id", "abc123")
	h.Set("X-Mod3-Duration-Sec", "2.50")
	h.Set("X-Mod3-Sample-Rate", "24000")
	h.Set("X-Mod3-Rtf", "8.1")
	h.Set("Content-Type", "audio/wav") // should be skipped

	out := extractMod3Metrics(h)
	if len(out) != 4 {
		t.Fatalf("expected 4 metrics, got %d: %v", len(out), out)
	}
	if got, _ := out["job-id"].(string); got != "abc123" {
		t.Fatalf("job-id: got %v", out["job-id"])
	}
	if got, ok := out["duration-sec"].(float64); !ok || got != 2.5 {
		t.Fatalf("duration-sec: got %v (%T)", out["duration-sec"], out["duration-sec"])
	}
	if got, ok := out["sample-rate"].(int64); !ok || got != 24000 {
		t.Fatalf("sample-rate should parse to int64, got %v (%T)",
			out["sample-rate"], out["sample-rate"])
	}
	if _, present := out["content-type"]; present {
		t.Fatal("content-type should be skipped")
	}
}

// ─── playback injection test ─────────────────────────────────────────────────

// TestPlayAudio_StubPlayer — validate the playback plumbing by injecting a
// small shell-script player that records the file it received. Proves the
// audio bytes reach the player (the bug the installed binary has today is
// that they don't). Only runs on OSes where a shell is available.
func TestPlayAudio_StubPlayer(t *testing.T) {
	dir := t.TempDir()
	recPath := filepath.Join(dir, "received.log")
	stubPath := filepath.Join(dir, "stub-player.sh")

	// Player writes its last-arg path and the first 4 bytes of the wav to
	// received.log; proves (a) the player got a path, (b) the file exists
	// at that path with our bytes.
	stubBody := `#!/bin/sh
path="$1"
hdr=$(dd if="$path" bs=4 count=1 2>/dev/null)
printf 'path=%s hdr=%s\n' "$path" "$hdr" > "` + recPath + `"
`
	if err := os.WriteFile(stubPath, []byte(stubBody), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	p := &modalityProxy{player: stubPath}
	if err := p.playAudio(synthWav, true); err != nil {
		t.Fatalf("playAudio: %v", err)
	}

	got, err := os.ReadFile(recPath)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	line := string(got)
	if !strings.Contains(line, "hdr=RIFF") {
		t.Fatalf("player did not see RIFF header; got %q", line)
	}
	if !strings.Contains(line, "path=") {
		t.Fatalf("player did not get a path arg; got %q", line)
	}
}

// TestPlayAudio_NonBlockingSpawn — fire-and-forget. Use a sleep-style stub
// and assert playAudio returns before the stub would finish. Prevents the
// regression where speech synthesis blocks the MCP response on playback.
func TestPlayAudio_NonBlockingSpawn(t *testing.T) {
	dir := t.TempDir()
	stubPath := filepath.Join(dir, "sleeper.sh")
	stubBody := `#!/bin/sh
sleep 5
`
	if err := os.WriteFile(stubPath, []byte(stubBody), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	p := &modalityProxy{player: stubPath}

	done := make(chan struct{})
	go func() {
		if err := p.playAudio(synthWav, false); err != nil {
			t.Errorf("playAudio: %v", err)
		}
		close(done)
	}()

	select {
	case <-done:
		// Expected: playAudio returns immediately in non-blocking mode.
	case <-time.After(2 * time.Second):
		t.Fatal("playAudio(blocking=false) did not return within 2s")
	}
}
