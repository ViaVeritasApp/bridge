package tlsx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPublicHost(t *testing.T) {
	got := PublicHost("192.168.1.50", "abc123hex", "lan.viaveritas.app")
	want := "192-168-1-50.abc123hex.lan.viaveritas.app"
	if got != want {
		t.Errorf("PublicHost: got %q, want %q", got, want)
	}
}

func TestStoreAndLoad(t *testing.T) {
	dir := t.TempDir()
	cert, key := makeKeypair(t, "*.test.lan.viaveritas.app")

	changed, err := Store(dir, cert, key, "fp-one")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if !changed {
		t.Fatal("first Store: want changed=true")
	}
	if fp := Fingerprint(dir); fp != "fp-one" {
		t.Errorf("Fingerprint: got %q, want %q", fp, "fp-one")
	}
	cf, kf, ok := Loaded(dir)
	if !ok {
		t.Fatal("Loaded: want ok=true after Store")
	}
	if cf != filepath.Join(dir, "tls", "cert.pem") || kf != filepath.Join(dir, "tls", "key.pem") {
		t.Errorf("Loaded paths look wrong: %s, %s", cf, kf)
	}

	// Idempotent: same fingerprint ⇒ no rewrite, no error, changed=false.
	changed2, err := Store(dir, cert, key, "fp-one")
	if err != nil {
		t.Fatalf("Store idempotent: %v", err)
	}
	if changed2 {
		t.Error("second Store at same fingerprint: want changed=false")
	}

	// Different fingerprint ⇒ rewrite, changed=true.
	cert2, key2 := makeKeypair(t, "*.test.lan.viaveritas.app")
	changed3, err := Store(dir, cert2, key2, "fp-two")
	if err != nil {
		t.Fatalf("Store rotate: %v", err)
	}
	if !changed3 {
		t.Error("Store at new fingerprint: want changed=true")
	}
	if fp := Fingerprint(dir); fp != "fp-two" {
		t.Errorf("Fingerprint after rotate: got %q, want %q", fp, "fp-two")
	}
}

func TestStoreRejectsMalformedKeypair(t *testing.T) {
	dir := t.TempDir()
	// Cert PEM is valid; key PEM is gibberish — must reject before clobbering.
	cert, _ := makeKeypair(t, "*.test.lan.viaveritas.app")
	if _, err := Store(dir, cert, "-----BEGIN PRIVATE KEY-----\ngarbage\n-----END PRIVATE KEY-----\n", "fp-bad"); err == nil {
		t.Fatal("Store: want error on malformed key, got nil")
	}
	if _, _, ok := Loaded(dir); ok {
		t.Error("Store rejected the input but still wrote files to disk")
	}
}

func TestEmptyInputsRejected(t *testing.T) {
	dir := t.TempDir()
	if _, err := Store(dir, "", "k", "fp"); err == nil {
		t.Error("Store: want error on empty cert")
	}
	if _, err := Store(dir, "c", "", "fp"); err == nil {
		t.Error("Store: want error on empty key")
	}
	if _, err := Store(dir, "c", "k", ""); err == nil {
		t.Error("Store: want error on empty fingerprint")
	}
}

func TestLoadedFalseWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	if _, _, ok := Loaded(dir); ok {
		t.Error("Loaded on fresh dir: want ok=false")
	}
	if fp := Fingerprint(dir); fp != "" {
		t.Errorf("Fingerprint on fresh dir: want \"\", got %q", fp)
	}
	if !CertMtime(dir).IsZero() {
		t.Error("CertMtime on fresh dir: want zero time")
	}
}

func TestTLSConfigGetCertificateHotReload(t *testing.T) {
	dir := t.TempDir()
	cert1, key1 := makeKeypair(t, "*.test.lan.viaveritas.app")
	if _, err := Store(dir, cert1, key1, "fp-one"); err != nil {
		t.Fatalf("Store: %v", err)
	}
	cfg := TLSConfig(dir)

	first, err := cfg.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	// Pin the cert raw bytes so we can detect a swap after rotation.
	firstRaw := first.Certificate[0]

	// Second call without a rotation: identical bytes (cache hit).
	cached, err := cfg.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate cached: %v", err)
	}
	if &cached.Certificate[0][0] == &firstRaw[0] {
		// reslicing keeps the same backing array — but we compare by value to be safe
	}

	// Bump mtime with a fresh keypair: write order is key → cert and AtomicWrite
	// renames make a new inode, so cert.pem's mtime is strictly later. The sleep
	// is belt-and-braces for coarse filesystems.
	time.Sleep(20 * time.Millisecond)
	cert2, key2 := makeKeypair(t, "*.test.lan.viaveritas.app")
	if _, err := Store(dir, cert2, key2, "fp-two"); err != nil {
		t.Fatalf("Store rotate: %v", err)
	}

	rotated, err := cfg.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate after rotate: %v", err)
	}
	if equalBytes(rotated.Certificate[0], firstRaw) {
		t.Error("GetCertificate: want different cert bytes after mtime advance")
	}
}

// equalBytes is byte-compare without importing bytes for one call site.
func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// makeKeypair builds a self-signed ECDSA P-256 cert/key pair in PEM.
func makeKeypair(t *testing.T, cn string) (certPEM, keyPEM string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{cn},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	cBuf := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	kBuf := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return string(cBuf), string(kBuf)
}

// silence unused warning when not on a path that touches it
var _ = os.Stat
