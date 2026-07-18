// Package socketlog streams the bridge's service log to ViaVeritas: a logx sink
// feeds each line into a Socket.IO `logs` event on the `/bridges` namespace,
// with exponential-backoff reconnects. The Socket.IO/Engine.IO v4 client is
// hand-rolled on a plain WebSocket (only CONNECT, ping/pong, and EVENT are
// needed) to avoid a full client library.
package socketlog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"bridge/internal/config"
	"bridge/internal/logx"
)

// namespace is the Socket.IO namespace the API's bridge gateway listens on.
const namespace = "/bridges"

const (
	minBackoff    = 5 * time.Second
	maxBackoff    = 30 * time.Second
	healthyWindow = 60 * time.Second
)

// Run streams the service log to ViaVeritas until ctx is cancelled: it installs
// a logx sink and keeps one Socket.IO connection alive, reconnecting with 5s→30s
// backoff (reset after a healthy minute).
func Run(ctx context.Context, cfg config.Config) {
	// Buffered so a slow/down socket never stalls logging — a full buffer drops
	// lines rather than blocking logx.emit.
	logLines := make(chan string, 256)
	logx.SetSink(func(line string) {
		select {
		case logLines <- line:
		default:
		}
	})
	defer logx.SetSink(nil)

	backoff := minBackoff
	for ctx.Err() == nil {
		start := time.Now()
		err := serveConn(ctx, cfg, logLines)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > healthyWindow {
			backoff = minBackoff // the connection was healthy — reset
		}
		logx.Warn("socketlog", "log stream disconnected (%v); reconnecting in %s", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// soConn serializes writes — coder/websocket forbids concurrent Write, and both
// the reader (pongs) and the emitter write.
type soConn struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (c *soConn) send(ctx context.Context, msg string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.conn.Write(wctx, websocket.MessageText, []byte(msg))
}

// serveConn opens one Socket.IO connection, performs the handshake, then
// forwards log lines until the connection drops or ctx ends.
func serveConn(ctx context.Context, cfg config.Config, logLines <-chan string) error {
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// DeviceGuard (API) requires x-device-type, x-organization-id, and an
	// "Authorization: <clientId>:<clientSecret>" header (device.guard.ts
	// getAuth()). The bridge always identifies as device type "bridge".
	hdr := http.Header{}
	hdr.Set("x-device-type", "bridge")
	hdr.Set("x-organization-id", cfg.OrganizationID)
	hdr.Set("Authorization", cfg.ClientID+":"+cfg.ClientSecret)

	dialCtx, dialCancel := context.WithTimeout(connCtx, 20*time.Second)
	conn, _, err := websocket.Dial(dialCtx, socketURL(cfg.APIBaseURL), &websocket.DialOptions{HTTPHeader: hdr})
	dialCancel()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	conn.SetReadLimit(1 << 20)
	defer conn.CloseNow()
	c := &soConn{conn: conn}

	// Engine.IO OPEN: "0{"sid":...,"pingInterval":...,"pingTimeout":...}".
	pingInterval, pingTimeout := 25*time.Second, 20*time.Second
	{
		rctx, rc := context.WithTimeout(connCtx, 20*time.Second)
		_, data, rerr := conn.Read(rctx)
		rc()
		if rerr != nil {
			return fmt.Errorf("read open: %w", rerr)
		}
		if len(data) == 0 || data[0] != '0' {
			return fmt.Errorf("unexpected first packet: %q", trunc(data))
		}
		var open struct {
			PingInterval int `json:"pingInterval"`
			PingTimeout  int `json:"pingTimeout"`
		}
		if json.Unmarshal(data[1:], &open) == nil {
			if open.PingInterval > 0 {
				pingInterval = time.Duration(open.PingInterval) * time.Millisecond
			}
			if open.PingTimeout > 0 {
				pingTimeout = time.Duration(open.PingTimeout) * time.Millisecond
			}
		}
	}

	// A healthy server pings every pingInterval, so a read exceeding
	// pingInterval+pingTimeout means the connection is dead.
	readDeadline := pingInterval + pingTimeout
	if readDeadline < 30*time.Second {
		readDeadline = 30 * time.Second
	}

	// Socket.IO CONNECT to the namespace, then wait for its CONNECT ack.
	if err := c.send(connCtx, "40"+namespace+","); err != nil {
		return fmt.Errorf("send connect: %w", err)
	}
	for {
		rctx, rc := context.WithTimeout(connCtx, readDeadline)
		_, data, rerr := conn.Read(rctx)
		rc()
		if rerr != nil {
			return fmt.Errorf("read connect ack: %w", rerr)
		}
		s := string(data)
		switch {
		case s == "2": // Engine.IO ping during the handshake
			if err := c.send(connCtx, "3"); err != nil {
				return err
			}
		case strings.HasPrefix(s, "40"+namespace): // Socket.IO CONNECT ack
			goto connected
		case strings.HasPrefix(s, "44"+namespace): // Socket.IO CONNECT_ERROR
			return fmt.Errorf("namespace connect rejected: %s", trunc(data))
		case s == "1": // Engine.IO close
			return fmt.Errorf("server closed the connection during handshake")
		}
	}
connected:
	logx.Info("socketlog", "log stream connected (%s)", namespace)

	// Reader goroutine: answer Engine.IO pings and detect disconnects — the only
	// caller of conn.Read once the handshake is done.
	readErr := make(chan error, 1)
	go func() {
		for {
			rctx, rc := context.WithTimeout(connCtx, readDeadline)
			_, data, rerr := conn.Read(rctx)
			rc()
			if rerr != nil {
				readErr <- rerr
				return
			}
			s := string(data)
			switch {
			case s == "2": // ping → pong
				if err := c.send(connCtx, "3"); err != nil {
					readErr <- err
					return
				}
			case s == "1" || strings.HasPrefix(s, "41"): // Engine close / SIO disconnect
				readErr <- fmt.Errorf("server closed the connection")
				return
			}
		}
	}()

	// Emitter: forward each log line as a `logs` event; json.Marshal escapes the
	// line, so any content is safe inside the packet.
	for {
		select {
		case <-connCtx.Done():
			return connCtx.Err()
		case rerr := <-readErr:
			return rerr
		case line := <-logLines:
			payload, err := json.Marshal([]any{"logs", map[string]string{
				"logMessage": strings.TrimRight(line, "\r\n"),
			}})
			if err != nil {
				continue
			}
			if err := c.send(connCtx, "42"+namespace+","+string(payload)); err != nil {
				return fmt.Errorf("emit: %w", err)
			}
		}
	}
}

// socketURL turns the API base URL into the Socket.IO WebSocket endpoint
// (straight over the websocket transport, no HTTP-polling phase).
func socketURL(apiBaseURL string) string {
	u := strings.TrimRight(apiBaseURL, "/")
	switch {
	case strings.HasPrefix(u, "https://"):
		u = "wss://" + strings.TrimPrefix(u, "https://")
	case strings.HasPrefix(u, "http://"):
		u = "ws://" + strings.TrimPrefix(u, "http://")
	}
	return u + "/socket.io/?EIO=4&transport=websocket"
}

func trunc(b []byte) string {
	const max = 200
	if len(b) > max {
		return string(b[:max]) + "…"
	}
	return string(b)
}
