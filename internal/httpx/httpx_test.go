package httpx

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"testing"
	"time"
)

const (
	testKey    = "bridge-token-secret"
	testUserID = "user_123"
)

// signFor produces the HMAC the API would issue, so happy-path tests don't
// hand-roll the expected hex.
func signFor(path, expires, userID, key string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(path + "|" + expires + "|" + userID))
	return hex.EncodeToString(m.Sum(nil))
}

func TestVerifyHMAC_HappyPath(t *testing.T) {
	expires := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	sig := signFor("/patients/scan.pdf", expires, testUserID, testKey)
	if err := VerifyHMAC("/patients/scan.pdf", expires, sig, testUserID, testKey); err != nil {
		t.Fatalf("VerifyHMAC: %v", err)
	}
}

func TestVerifyHMAC_ExpiredReturnsErrSigExpired(t *testing.T) {
	expires := strconv.FormatInt(time.Now().Add(-time.Second).Unix(), 10)
	sig := signFor("/p.pdf", expires, testUserID, testKey)
	err := VerifyHMAC("/p.pdf", expires, sig, testUserID, testKey)
	if !errors.Is(err, ErrSigExpired) {
		t.Errorf("expired: got %v, want ErrSigExpired", err)
	}
}

func TestVerifyHMAC_ForgedReturnsErrSigInvalid(t *testing.T) {
	expires := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	// Sign with a different key, then try to validate with the real one.
	bad := signFor("/p.pdf", expires, testUserID, "other-key")
	err := VerifyHMAC("/p.pdf", expires, bad, testUserID, testKey)
	if !errors.Is(err, ErrSigInvalid) {
		t.Errorf("forged: got %v, want ErrSigInvalid", err)
	}
}

func TestVerifyHMAC_AnyInputTamperRejected(t *testing.T) {
	expires := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	sig := signFor("/orig.pdf", expires, testUserID, testKey)

	// Each row mutates one signed input; the verifier must reject all of them.
	cases := []struct {
		name, path, expires, userID string
	}{
		{"path swap", "/other.pdf", expires, testUserID},
		{"expires swap", "/orig.pdf", strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10), testUserID},
		{"user swap", "/orig.pdf", expires, "user_999"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := VerifyHMAC(c.path, c.expires, sig, c.userID, testKey); !errors.Is(err, ErrSigInvalid) {
				t.Errorf("got %v, want ErrSigInvalid", err)
			}
		})
	}
}

func TestVerifyHMAC_MalformedInputs(t *testing.T) {
	cases := []struct{ path, expires, sig, userID, key, name string }{
		{"", "1", "ab", testUserID, testKey, "empty path"},
		{"/p", "", "ab", testUserID, testKey, "empty expires"},
		{"/p", "1", "", testUserID, testKey, "empty sig"},
		{"/p", "1", "ab", "", testKey, "empty userID"},
		{"/p", "1", "ab", testUserID, "", "empty key"},
		{"/p", "not-a-number", "ab", testUserID, testKey, "non-numeric expires"},
		{"/p", "1", "not-hex-zz", testUserID, testKey, "non-hex sig"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := VerifyHMAC(c.path, c.expires, c.sig, c.userID, c.key); !errors.Is(err, ErrSigInvalid) {
				t.Errorf("got %v, want ErrSigInvalid", err)
			}
		})
	}
}
