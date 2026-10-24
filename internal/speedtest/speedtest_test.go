package speedtest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
)

func TestRunAgainstAnAPI(t *testing.T) {
	var uploaded atomic.Int64
	mux := http.NewServeMux()
	ok := func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer tok" }
	mux.HandleFunc("/bridges/speedtest/ping", func(w http.ResponseWriter, r *http.Request) {
		if !ok(r) {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/bridges/speedtest/download", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.ParseInt(r.URL.Query().Get("bytes"), 10, 64)
		_, _ = io.Copy(w, io.LimitReader(&pattern{x: 1}, n))
	})
	mux.HandleFunc("/bridges/speedtest/upload", func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		uploaded.Add(n)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res := Run(context.Background(), srv.URL, Request{Token: "tok", Seconds: 2, MaxBytes: 4 << 20, Streams: 4})
	if !res.OK || res.BytesDown != 4<<20 || res.BytesUp != 4<<20 || res.DownloadBPS <= 0 || res.UploadBPS <= 0 {
		t.Fatalf("result %+v", res)
	}
	if got := uploaded.Load(); got != 4<<20 {
		t.Fatalf("the API got %d bytes, want %d", got, 4<<20)
	}
	if res.LatencyMS.Min <= 0 || res.LatencyMS.Avg < res.LatencyMS.Min || res.LatencyMS.Max < res.LatencyMS.Avg {
		t.Fatalf("latency %+v", res.LatencyMS)
	}
	if bad := Run(context.Background(), srv.URL, Request{Token: "wrong", Seconds: 1}); bad.OK || bad.Code != "refused" {
		t.Fatalf("a refused token: %+v", bad)
	}
}

func TestPatternDoesntRepeat(t *testing.T) {
	g := &pattern{x: 7}
	a, b := make([]byte, 64), make([]byte, 64)
	_, _ = g.Read(a)
	_, _ = g.Read(b)
	if string(a) == string(b) {
		t.Fatal("two reads gave the same bytes")
	}
}
