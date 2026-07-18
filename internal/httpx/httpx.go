// Package httpx centralizes the small HTTP plumbing shared by packages talking
// to the ViaVeritas API: client construction, auth headers, URL normalization,
// error-body extraction, and HMAC verification. It does no request orchestration
// so each caller keeps its own logging and body handling.
package httpx

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client returns an *http.Client with the given timeout.
func Client(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

// Auth carries the bridge's self-identifying credentials and an optional
// end-user Authorization header to forward.
type Auth struct {
	ClientID     string // -> X-Client-Id, identifies the bridge
	ClientSecret string // -> X-Client-Secret, authenticates the bridge
	UserAuth     string // -> Authorization, the caller's forwarded credentials
}

// SetAuth sets X-Client-Id, X-Client-Secret and (when present) Authorization,
// skipping empty values. Content-Type is left to the caller.
func SetAuth(req *http.Request, a Auth) {
	if a.ClientID != "" {
		req.Header.Set("X-Client-Id", a.ClientID)
	}
	if a.ClientSecret != "" {
		req.Header.Set("X-Client-Secret", a.ClientSecret)
	}
	if a.UserAuth != "" {
		req.Header.Set("Authorization", a.UserAuth)
	}
}

// NormalizeURL trims trailing slashes from base and appends path (which should
// start with "/").
func NormalizeURL(base, path string) string {
	for len(base) > 0 && base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}
	return base + path
}

// WithQuery appends one query parameter to rawURL, choosing "?" or "&" as
// needed and escaping key and value. Returns rawURL unchanged when value is
// empty, so an absent organization id never adds a stray parameter.
func WithQuery(rawURL, key, value string) string {
	if value == "" {
		return rawURL
	}
	sep := "?"
	if strings.Contains(rawURL, "?") {
		sep = "&"
	}
	return rawURL + sep + url.QueryEscape(key) + "=" + url.QueryEscape(value)
}

// ErrorSnippet reads up to max bytes of r.Body for embedding in an error
// message. It does not trim — callers apply strings.TrimSpace themselves.
func ErrorSnippet(r *http.Response, max int) string {
	buf := make([]byte, max)
	n, _ := r.Body.Read(buf)
	return string(buf[:n])
}

// Errors returned by VerifyHMAC so handlers can map to the right status code
// (401 vs 410) without parsing strings.
var (
	ErrSigInvalid = errors.New("signature invalid")
	ErrSigExpired = errors.New("signature expired")
)

// VerifyHMAC validates a `path|expires|userID` HMAC-SHA256 signature the API
// issued for a bridge Get URL. key is the per-bridge shared secret, expiresUnix
// the embedded expiry, userID the API-side user id, sigHex the hex HMAC. All
// comparisons are constant-time.
//
// The bridge never authenticates users; userID is in the signature only to bind
// the URL to a user in the API's audit trail (a leaked URL can't be replayed
// under another identity) and to keep the verifier in lockstep with signBridgePath.
func VerifyHMAC(path, expiresUnix, sigHex, userID, key string) error {
	if path == "" || expiresUnix == "" || sigHex == "" || userID == "" || key == "" {
		return ErrSigInvalid
	}
	exp, err := strconv.ParseInt(expiresUnix, 10, 64)
	if err != nil {
		return ErrSigInvalid
	}
	want, err := hex.DecodeString(sigHex)
	if err != nil {
		return ErrSigInvalid
	}
	// Only decide invalid vs expired after the format checks pass, so a
	// malformed signature is never reported as merely expired (an attacker
	// could otherwise probe the difference).
	if time.Now().Unix() > exp {
		return ErrSigExpired
	}
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(path + "|" + expiresUnix + "|" + userID))
	if !hmac.Equal(m.Sum(nil), want) {
		return ErrSigInvalid
	}
	return nil
}
