package setupui

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"strings"
)

func newToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// isLoopback reports whether the request originates from a loopback address.
func isLoopback(r *http.Request) bool {
	ip := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		ip = h
	}
	p := net.ParseIP(ip)
	return p != nil && p.IsLoopback()
}

// hostAllowed rejects requests whose Host header isn't loopback. With the
// loopback peer check this blocks DNS rebinding: a page rebinding its name to
// 127.0.0.1 still sends its own name in Host, so it can't read the wizard page
// (and CSRF token) or drive the root-privileged API.
func hostAllowed(r *http.Request) bool {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]") // strip IPv6 brackets
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// sameSiteOK blocks cross-site requests via the Fetch Metadata header. An absent
// header (older/non-browser clients) is allowed — loopback + Host remain the
// primary gate there.
func sameSiteOK(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return true
	}
	return false
}

// loopback enforces loopback origin only (no CSRF). Used for /api/ping and
// /api/bye, which just stop this local server and must work via
// navigator.sendBeacon (which can't set headers).
func (s *srv) loopback(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLoopback(r) || !hostAllowed(r) || !sameSiteOK(r) {
			http.Error(w, "local only", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

// guard enforces loopback origin + CSRF token on every API call. These run as
// root, so the gate matters: only the wizard page we served (holding the token)
// may trigger them.
func (s *srv) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLoopback(r) || !hostAllowed(r) || !sameSiteOK(r) {
			http.Error(w, `{"error":"local access only"}`, http.StatusForbidden)
			return
		}
		if r.Header.Get("X-CSRF") != s.token {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "invalid session token — reopen the setup page"})
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
