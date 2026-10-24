// Package speedtest measures the computer's connection to ViaVeritas when
// ViaVeritas asks (a `speedtest` event with a one-time token): the round trip
// of small requests, then data down and up over a few connections at once,
// each direction for a few seconds or up to a limit, whichever comes first.
// It talks to the API's own speedtest routes, so it measures the way to
// ViaVeritas, not to the internet at large.
package speedtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Request is what ViaVeritas asks for.
type Request struct {
	Token string `json:"token"`
	// Seconds each direction runs at most.
	Seconds int `json:"seconds"`
	// MaxBytes each direction moves at most, over all its connections.
	MaxBytes int64 `json:"max_bytes"`
	// Streams is how many connections at once.
	Streams int `json:"streams"`
}

// Latency is the round trip of a small request, in milliseconds.
type Latency struct {
	Min float64 `json:"min"`
	Avg float64 `json:"avg"`
	Max float64 `json:"max"`
}

// Result is what the test found; OK false with a Code when it didn't run.
type Result struct {
	OK          bool    `json:"ok"`
	Code        string  `json:"code,omitempty"`
	Message     string  `json:"message,omitempty"`
	LatencyMS   Latency `json:"latency_ms"`
	JitterMS    float64 `json:"jitter_ms"`
	DownloadBPS float64 `json:"download_bps"`
	UploadBPS   float64 `json:"upload_bps"`
	BytesDown   int64   `json:"bytes_down"`
	BytesUp     int64   `json:"bytes_up"`
	DurationMS  int64   `json:"duration_ms"`
}

const pings = 10

// Run measures the way to the API at base.
func Run(ctx context.Context, base string, req Request) Result {
	started := time.Now()
	base = strings.TrimRight(base, "/") + "/bridges/speedtest"
	req.Seconds = max(1, min(req.Seconds, 15))
	req.Streams = max(1, min(req.Streams, 8))
	if req.MaxBytes <= 0 {
		req.MaxBytes = 25 << 20
	}
	client := &http.Client{Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConnsPerHost: req.Streams,
		DisableCompression:  true,
		ForceAttemptHTTP2:   false,
	}}
	defer client.CloseIdleConnections()
	auth := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+req.Token) }

	// Round trips: the first sets up the connection, so it isn't counted.
	var rtts []float64
	for i := 0; i <= pings; i++ {
		r, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/ping", nil)
		auth(r)
		t := time.Now()
		res, err := client.Do(r)
		if err != nil {
			return failed("unreachable", err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return failed("refused", fmt.Errorf("the API answered %s", res.Status))
		}
		if i > 0 {
			rtts = append(rtts, float64(time.Since(t).Microseconds())/1000)
		}
	}

	out := Result{OK: true}
	out.LatencyMS, out.JitterMS = latency(rtts)

	per := req.MaxBytes / int64(req.Streams)
	window := time.Duration(req.Seconds) * time.Second

	down, downTook := parallel(ctx, req.Streams, window, func(ctx context.Context, moved *atomic.Int64) error {
		r, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/download?bytes="+strconv.FormatInt(per, 10), nil)
		auth(r)
		res, err := client.Do(r)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("the API answered %s", res.Status)
		}
		_, err = io.Copy(io.Discard, &counter{r: res.Body, n: moved})
		return err
	})
	up, upTook := parallel(ctx, req.Streams, window, func(ctx context.Context, moved *atomic.Int64) error {
		r, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/upload", &counter{r: io.LimitReader(&pattern{x: 2463534242}, per), n: moved})
		r.ContentLength = per
		r.Header.Set("Content-Type", "application/octet-stream")
		auth(r)
		res, err := client.Do(r)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("the API answered %s", res.Status)
		}
		return nil
	})

	out.BytesDown, out.BytesUp = down, up
	if downTook > 0 {
		out.DownloadBPS = math.Round(float64(down) * 8 / downTook.Seconds())
	}
	if upTook > 0 {
		out.UploadBPS = math.Round(float64(up) * 8 / upTook.Seconds())
	}
	out.DurationMS = time.Since(started).Milliseconds()
	if down == 0 && up == 0 {
		return failed("failed", errors.New("no data went through"))
	}
	return out
}

func failed(code string, err error) Result {
	return Result{Code: code, Message: err.Error()}
}

// parallel runs n transfers until they finish or the window ends, and says
// how much they moved and how long that took. A transfer cut short by the
// window counts what it moved.
func parallel(ctx context.Context, n int, window time.Duration, transfer func(context.Context, *atomic.Int64) error) (int64, time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	var moved atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = transfer(ctx, &moved)
		}()
	}
	wg.Wait()
	return moved.Load(), time.Since(start)
}

// latency is the round trips' least, mean and most, and how much they vary
// from one to the next (the mean of the differences, as speedtests count jitter).
func latency(rtts []float64) (Latency, float64) {
	if len(rtts) == 0 {
		return Latency{}, 0
	}
	sorted := append([]float64(nil), rtts...)
	sort.Float64s(sorted)
	sum, jitter := 0.0, 0.0
	for i, v := range rtts {
		sum += v
		if i > 0 {
			jitter += math.Abs(v - rtts[i-1])
		}
	}
	if len(rtts) > 1 {
		jitter /= float64(len(rtts) - 1)
	}
	r := func(v float64) float64 { return math.Round(v*10) / 10 }
	return Latency{Min: r(sorted[0]), Avg: r(sum / float64(len(rtts))), Max: r(sorted[len(sorted)-1])}, r(jitter)
}

// counter counts what is read through it.
type counter struct {
	r io.Reader
	n *atomic.Int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// pattern is bytes that don't compress (xorshift), made without a random source.
type pattern struct{ x uint32 }

func (g *pattern) Read(p []byte) (int, error) {
	for i := range p {
		g.x ^= g.x << 13
		g.x ^= g.x >> 17
		g.x ^= g.x << 5
		p[i] = byte(g.x)
	}
	return len(p), nil
}
