// Package apisocket keeps the bridge's socket to ViaVeritas: the `/bridges`
// Socket.IO namespace, over which the API sends the changes members make to the
// bridge's files in the portal (internal/commands) and the bridge streams its
// service log when socket_logs is on. It announces itself with a `health` event
// on connecting and every 30 seconds, which is how the API knows it can take
// commands. The Socket.IO/Engine.IO v4 client is hand-rolled on a plain
// WebSocket (only CONNECT, ping/pong, EVENT and ACK are needed) to avoid a full
// client library.
package apisocket

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"bridge/internal/commands"
	"bridge/internal/config"
	"bridge/internal/logx"
)

// namespace is the Socket.IO namespace the API's bridge gateway listens on.
const namespace = "/bridges"

const (
	minBackoff     = 5 * time.Second
	maxBackoff     = 30 * time.Second
	healthyWindow  = 60 * time.Second
	healthInterval = 30 * time.Second
)

var started = time.Now()

// Run keeps one Socket.IO connection to ViaVeritas alive until ctx is
// cancelled, reconnecting with 5s→30s backoff (reset after a healthy minute).
func Run(ctx context.Context, cfg config.Config) {
	// Buffered so a slow/down socket never stalls logging — a full buffer drops
	// lines rather than blocking logx.emit.
	logLines := make(chan string, 256)
	if cfg.SocketLogs {
		logx.SetSink(func(line string) {
			select {
			case logLines <- line:
			default:
			}
		})
		defer logx.SetSink(nil)
	}

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
		logx.Warn("apisocket", "socket disconnected (%v); reconnecting in %s", err, backoff)
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

// soConn serializes writes — coder/websocket forbids concurrent Write, and the
// reader (pongs), the command answers, health and the log emitter all write.
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

func (c *soConn) emit(ctx context.Context, event string, data any) error {
	payload, err := json.Marshal([]any{event, data})
	if err != nil {
		return err
	}
	return c.send(ctx, "42"+namespace+","+string(payload))
}

// serveConn opens one Socket.IO connection, performs the handshake, then serves
// it until the connection drops or ctx ends.
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
	conn.SetReadLimit(4 << 20) // a batch of commands names up to 500 paths
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
	logx.Info("apisocket", "socket connected (%s)", namespace)

	if err := c.emit(connCtx, "health", health()); err != nil {
		return fmt.Errorf("emit health: %w", err)
	}

	// Reader goroutine: answer Engine.IO pings, run commands and detect
	// disconnects — the only caller of conn.Read once the handshake is done.
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
			case strings.HasPrefix(s, "42"):
				if event, ok := parseEvent(s); ok {
					go handle(connCtx, c, cfg, event)
				}
			}
		}
	}()

	tick := time.NewTicker(healthInterval)
	defer tick.Stop()
	for {
		select {
		case <-connCtx.Done():
			return connCtx.Err()
		case rerr := <-readErr:
			return rerr
		case <-tick.C:
			if err := c.emit(connCtx, "health", health()); err != nil {
				return fmt.Errorf("emit health: %w", err)
			}
		case line := <-logLines:
			// json.Marshal escapes the line, so any content is safe inside the packet.
			if err := c.emit(connCtx, "logs", map[string]string{"logMessage": strings.TrimRight(line, "\r\n")}); err != nil {
				return fmt.Errorf("emit: %w", err)
			}
		}
	}
}

// event is one Socket.IO EVENT packet: `42/bridges,<ack id>["name",data]`.
type event struct {
	name  string
	ackID string
	data  json.RawMessage
}

func parseEvent(s string) (event, bool) {
	rest := strings.TrimPrefix(s, "42")
	if !strings.HasPrefix(rest, namespace+",") {
		return event{}, false
	}
	rest = rest[len(namespace)+1:]
	i := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
	}
	var parts []json.RawMessage
	if err := json.Unmarshal([]byte(rest[i:]), &parts); err != nil || len(parts) == 0 {
		return event{}, false
	}
	var name string
	if err := json.Unmarshal(parts[0], &name); err != nil {
		return event{}, false
	}
	e := event{name: name, ackID: rest[:i]}
	if len(parts) > 1 {
		e.data = parts[1]
	}
	return e, true
}

func handle(ctx context.Context, c *soConn, cfg config.Config, e event) {
	switch e.name {
	case "command":
		var cmd commands.Command
		if err := json.Unmarshal(e.data, &cmd); err != nil {
			logx.Warn("apisocket", "unreadable command: %v", err)
			return
		}
		// The folders as they are now: one added since the socket opened counts.
		result := commands.Run(config.LoadOr(cfg), cmd)
		if e.ackID == "" {
			return
		}
		payload, err := json.Marshal([]any{result})
		if err != nil {
			return
		}
		if err := c.send(ctx, "43"+namespace+","+e.ackID+string(payload)); err != nil {
			logx.Warn("apisocket", "could not answer command %s: %v", cmd.ID, err)
		}
	case "exception":
		logx.Warn("apisocket", "the API refused a message: %s", trunc(e.data))
	}
}

// health is the API's DeviceHealthDto.
func health() map[string]any {
	return map[string]any{"uptime": int(time.Since(started).Seconds())}
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
