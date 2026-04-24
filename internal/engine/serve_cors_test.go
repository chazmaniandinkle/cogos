package engine

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCORSPreflight_OKStatus verifies that an OPTIONS preflight returns
// 204 No Content with the expected Access-Control-* headers. The handler
// under test is the wrapped server handler (mux + CORS middleware).
func TestCORSPreflight_OKStatus(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodOptions, "/v1/channel-sessions/register", nil)
	req.Header.Set("Origin", "http://localhost:7860")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "Content-Type")

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d; want 204", w.Code)
	}
	// All allow-* headers should be present on the preflight response.
	wantHeaders := map[string]string{
		"Access-Control-Allow-Methods": "GET, POST, PATCH, PUT, DELETE, OPTIONS",
		"Access-Control-Allow-Headers": "Content-Type, Mcp-Session-Id, X-Workspace-Root, Authorization",
		"Access-Control-Max-Age":       "86400",
	}
	for h, want := range wantHeaders {
		if got := w.Header().Get(h); got != want {
			t.Errorf("%s = %q; want %q", h, got, want)
		}
	}
}

// TestCORSPreflight_AllowsBrowserOrigin verifies that a preflight from a
// loopback dashboard origin echoes the Origin header (not "*") so
// credentialed requests can round-trip, and sets Allow-Credentials.
func TestCORSPreflight_AllowsBrowserOrigin(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	for _, origin := range []string{
		"http://localhost:7860",
		"http://127.0.0.1:6931",
		"http://localhost",
	} {
		origin := origin
		t.Run(origin, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodOptions, "/v1/manifest", nil)
			req.Header.Set("Origin", origin)
			req.Header.Set("Access-Control-Request-Method", "GET")

			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			if got := w.Header().Get("Access-Control-Allow-Origin"); got != origin {
				t.Errorf("Allow-Origin = %q; want %q (echo for loopback)", got, origin)
			}
			if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
				t.Errorf("Allow-Credentials = %q; want %q (echo origin enables credentials)", got, "true")
			}
			if got := w.Header().Get("Vary"); got != "Origin" {
				t.Errorf("Vary = %q; want %q", got, "Origin")
			}
		})
	}
}

// TestCORSPreflight_NonLoopbackOriginGetsStar verifies the widening
// fallback for non-loopback origins (future pod/Tailnet deployments).
// Note: the kernel binds loopback by default, so this path is mostly
// about not breaking future non-dev deployments.
func TestCORSPreflight_NonLoopbackOriginGetsStar(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodOptions, "/v1/manifest", nil)
	req.Header.Set("Origin", "http://evil.example.com")

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Allow-Origin = %q; want %q (widen for non-loopback)", got, "*")
	}
	// Allow-Credentials must not be set for star origin.
	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Allow-Credentials = %q; want empty (star + credentials is illegal)", got)
	}
}

// TestCORSHeaders_OnRealGET verifies that a real (non-preflight) GET
// response still carries Access-Control-Allow-Origin when an Origin
// header is present. Uses /v1/manifest which is known to return 200.
func TestCORSHeaders_OnRealGET(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/manifest", nil)
	req.Header.Set("Origin", "http://localhost:7860")

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:7860" {
		t.Errorf("Allow-Origin = %q; want echo of loopback origin", got)
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Errorf("Content-Type = %q; want application/json (handler must still run)",
			w.Header().Get("Content-Type"))
	}
}

// TestCORSHeaders_NoOriginNoChange verifies that requests without an
// Origin header (curl, MCP CLI, same-origin fetches) are untouched —
// no CORS headers are added, and the underlying handler still runs.
func TestCORSHeaders_NoOriginNoChange(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/manifest", nil)
	// Deliberately no Origin header.

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q; want empty (no Origin header should skip CORS)", got)
	}
}

// TestIsLoopbackOrigin_Table unit-tests the pure helper so any future
// change to the allow-policy is easy to catch.
func TestIsLoopbackOrigin_Table(t *testing.T) {
	t.Parallel()
	cases := []struct {
		origin string
		want   bool
	}{
		{"http://localhost", true},
		{"http://localhost:7860", true},
		{"http://127.0.0.1", true},
		{"http://127.0.0.1:6931", true},
		{"https://localhost:8443", true},
		{"https://127.0.0.1:8443", true},
		{"http://evil.example.com", false},
		{"http://192.168.1.2:7860", false},
		{"", false},
		{"null", false},
		{"file://", false},
	}
	for _, tc := range cases {
		if got := isLoopbackOrigin(tc.origin); got != tc.want {
			t.Errorf("isLoopbackOrigin(%q) = %v; want %v", tc.origin, got, tc.want)
		}
	}
}
