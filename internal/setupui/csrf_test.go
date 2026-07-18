package setupui

import (
	"net/http"
	"testing"
)

func TestIsLoopback(t *testing.T) {
	cases := []struct {
		remoteAddr string
		want       bool
	}{
		{"127.0.0.1:54321", true},
		{"[::1]:54321", true},
		{"127.0.0.1", true}, // no port — falls back to the whole string
		{"10.0.0.5:54321", false},
		{"192.168.1.10:80", false},
		{"8.8.8.8:53", false},
		{"", false},
	}
	for _, c := range cases {
		r := &http.Request{RemoteAddr: c.remoteAddr}
		if got := isLoopback(r); got != c.want {
			t.Errorf("isLoopback(RemoteAddr=%q) = %v; want %v", c.remoteAddr, got, c.want)
		}
	}
}

// hostAllowed is the DNS-rebinding gate: a page that rebinds its name to
// 127.0.0.1 still sends its own name in the Host header, so anything other than
// a loopback name must be rejected.
func TestHostAllowed(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true},
		{"127.0.0.1:8765", true},
		{"localhost", true},
		{"localhost:8765", true},
		{"::1", true},
		{"[::1]:8765", true},
		{"attacker.example", false},
		{"attacker.example:8765", false},
		{"0.0.0.0", false},
		{"192.168.1.10:8765", false},
	}
	for _, c := range cases {
		r := &http.Request{Host: c.host}
		if got := hostAllowed(r); got != c.want {
			t.Errorf("hostAllowed(Host=%q) = %v; want %v", c.host, got, c.want)
		}
	}
}

func TestSameSiteOK(t *testing.T) {
	cases := []struct {
		fetchSite string
		want      bool
	}{
		{"", true}, // absent header (non-browser/older clients) is allowed
		{"same-origin", true},
		{"none", true},
		{"same-site", false},
		{"cross-site", false},
	}
	for _, c := range cases {
		r := &http.Request{Header: http.Header{}}
		if c.fetchSite != "" {
			r.Header.Set("Sec-Fetch-Site", c.fetchSite)
		}
		if got := sameSiteOK(r); got != c.want {
			t.Errorf("sameSiteOK(Sec-Fetch-Site=%q) = %v; want %v", c.fetchSite, got, c.want)
		}
	}
}
