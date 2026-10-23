package bridgesrv

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bridge/internal/config"
)

func TestOriginAllowed(t *testing.T) {
	allow := []string{"https://app.viaveritas.app", " https://crm.viaveritas.app "}
	cases := []struct {
		origin string
		want   bool
	}{
		{"https://app.viaveritas.app", true},
		{"https://crm.viaveritas.app", true}, // allowlist entry is trimmed
		{"https://evil.example", false},
		{"https://sub.app.viaveritas.app", false}, // no subdomain widening
		{"http://app.viaveritas.app", false},      // scheme must match exactly
		{"", false},
	}
	for _, c := range cases {
		if got := originAllowed(c.origin, allow); got != c.want {
			t.Errorf("originAllowed(%q) = %v; want %v", c.origin, got, c.want)
		}
	}
	if originAllowed("https://app.viaveritas.app", nil) {
		t.Error("empty allowlist must disable reflection")
	}
}

// withCORS relies on config.LoadOr(cfg) falling back to the passed cfg when no
// config.json sits next to the test binary — true in `go test` and CI.
func newCORS(t *testing.T, allow ...string) http.Handler {
	t.Helper()
	cfg := config.Config{AllowedOrigin: allow}
	return withCORS(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

func TestWithCORSReflectsAllowedOrigin(t *testing.T) {
	h := newCORS(t, "https://app.viaveritas.app")
	req := httptest.NewRequest(http.MethodGet, "/files", nil)
	req.Header.Set("Origin", "https://app.viaveritas.app")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.viaveritas.app" {
		t.Errorf("Allow-Origin = %q; want the reflected origin", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Allow-Credentials = %q; want true", got)
	}
	if got := rec.Header().Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q; want Origin", got)
	}
}

// The core BR-5 guarantee: a disallowed origin must never receive CORS headers,
// and there must be no wildcard fallback.
func TestWithCORSDeniesUnknownOrigin(t *testing.T) {
	h := newCORS(t, "https://app.viaveritas.app")
	req := httptest.NewRequest(http.MethodGet, "/files", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q; want empty for a disallowed origin", got)
	}
}

func TestWithCORSNeverWildcard(t *testing.T) {
	// Even with an empty allowlist and a present Origin, we must not emit `*`.
	h := newCORS(t)
	req := httptest.NewRequest(http.MethodGet, "/files", nil)
	req.Header.Set("Origin", "https://anything.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q; want no header (never wildcard)", got)
	}
}

func TestWithCORSPreflight(t *testing.T) {
	h := newCORS(t, "https://app.viaveritas.app")
	req := httptest.NewRequest(http.MethodOptions, "/files", nil)
	req.Header.Set("Origin", "https://app.viaveritas.app")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d; want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.viaveritas.app" {
		t.Errorf("preflight Allow-Origin = %q; want the reflected origin", got)
	}
}

// Chrome's Private Network Access preflight: an allowed origin is let in, any
// other gets nothing.
func TestWithCORSPrivateNetworkPreflight(t *testing.T) {
	h := newCORS(t, "https://app.viaveritas.app")
	for origin, want := range map[string]string{"https://app.viaveritas.app": "true", "https://evil.example": ""} {
		req := httptest.NewRequest(http.MethodOptions, "/files", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Private-Network", "true")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Private-Network"); got != want {
			t.Errorf("origin %s: Allow-Private-Network = %q; want %q", origin, got, want)
		}
	}
}

// The network learns that the bridge is up, nothing about the computer or its folder.
func TestHealthzSaysOnlyThatItIsUp(t *testing.T) {
	rec := httptest.NewRecorder()
	handleHealthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"ok":true}` {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}
