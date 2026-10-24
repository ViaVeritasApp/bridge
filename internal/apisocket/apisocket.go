// Package apisocket keeps the bridge's socket to ViaVeritas: the `/bridges`
// Socket.IO namespace. On it the API sends the changes members make to the
// bridge's files in the portal (internal/commands), opens streams
// (internal/streams) and says when its switches for the bridge's features
// change. The bridge announces itself with a `health` event on connecting and
// every 30 seconds. The Socket.IO/Engine.IO v4 client is hand-rolled on a plain
// WebSocket (only CONNECT, ping/pong, EVENT and ACK are needed) to avoid a full
// client library.
//
// One goroutine reads, one writes. The writer sends control messages (pongs,
// answers, acks, closes) ahead of stream data, so a busy stream never delays a
// pong. The reader hands stream events to the registry in the order they came.
package apisocket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"bridge/internal/commands"
	"bridge/internal/config"
	"bridge/internal/logx"
	"bridge/internal/streams"
)

// namespace is the Socket.IO namespace the API's bridge gateway listens on.
const namespace = "/bridges"

const (
	minBackoff     = 5 * time.Second
	maxBackoff     = 30 * time.Second
	refusedBackoff = 5 * time.Minute
	healthyWindow  = 60 * time.Second
	healthInterval = 30 * time.Second
	writeTimeout   = 10 * time.Second
)

var started = time.Now()

// errRefused: ViaVeritas turned the bridge's credentials down. Trying again
// soon won't change that (the bridge was deleted, or paired elsewhere).
var errRefused = errors.New("ViaVeritas refused this bridge's credentials")

// Status is the socket as it stands, for the dashboard.
type Status struct {
	Connected bool   `json:"connected"`
	Since     string `json:"since,omitempty"`
	// Error is why the last connection ended, or why it can't connect.
	Error string `json:"error,omitempty"`
	// Refused: ViaVeritas refused its credentials; pairing again fixes it.
	Refused bool `json:"refused,omitempty"`
}

var (
	statusMu sync.Mutex
	status   Status
	current  *conn
	currCtx  context.Context
)

// ErrNotConnected: the socket isn't up now.
var ErrNotConnected = errors.New("the socket to ViaVeritas isn't connected")

// Emit sends an event on the socket while it is connected; data waits behind
// control messages. ErrNotConnected otherwise.
func Emit(event string, data any) error {
	statusMu.Lock()
	c, ctx := current, currCtx
	statusMu.Unlock()
	if c == nil {
		return ErrNotConnected
	}
	return c.Emit(ctx, event, data, false)
}

// EmitAck sends an event and waits up to timeout for the API to acknowledge
// it, answering what it acked with. ErrNotConnected while the socket is down.
func EmitAck(ctx context.Context, event string, data any, timeout time.Duration) (json.RawMessage, error) {
	statusMu.Lock()
	c := current
	statusMu.Unlock()
	if c == nil {
		return nil, ErrNotConnected
	}
	return c.EmitAck(ctx, event, data, timeout)
}

// A Handler answers an event from the API; what it returns goes back as the
// event's ack when the API asked for one. It runs in a goroutine of its own.
type Handler func(ctx context.Context, data json.RawMessage) any

var (
	hooksMu  sync.Mutex
	handlers = map[string]Handler{}
	onConn   []func(ctx context.Context)
)

// Handle answers an event the API sends.
func Handle(event string, h Handler) {
	hooksMu.Lock()
	defer hooksMu.Unlock()
	handlers[event] = h
}

// OnConnect runs fn each time the socket connects, in a goroutine of its own,
// with a context that ends with the connection.
func OnConnect(fn func(ctx context.Context)) {
	hooksMu.Lock()
	defer hooksMu.Unlock()
	onConn = append(onConn, fn)
}

// CurrentStatus is the socket as it stands now.
func CurrentStatus() Status {
	statusMu.Lock()
	defer statusMu.Unlock()
	return status
}

func setStatus(s Status) {
	statusMu.Lock()
	status = s
	statusMu.Unlock()
}

// Run keeps one Socket.IO connection to ViaVeritas alive until ctx is
// cancelled, reconnecting with 5s→30s backoff (reset after a healthy minute),
// or after 5 minutes when its credentials were refused.
func Run(ctx context.Context, live *config.Live) {
	streams.Default.SetConfig(live.Get)
	live.OnChange(func(old, next config.Config) {
		if config.SwitchesChanged(old, next) {
			streams.Default.Enforce(next)
		}
	})

	backoff := minBackoff
	for ctx.Err() == nil {
		start := time.Now()
		err := serveConn(ctx, live.Get())
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > healthyWindow {
			backoff = minBackoff // the connection was healthy — reset
		}
		wait := backoff
		refused := errors.Is(err, errRefused)
		if refused {
			wait = refusedBackoff
		}
		setStatus(Status{Error: errText(err), Refused: refused})
		logx.Warn("apisocket", "socket disconnected (%v); reconnecting in %s", err, wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if !refused {
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// conn is one connection: what the reader and the handlers send goes through
// its two queues to the writer.
type conn struct {
	ws   *websocket.Conn
	ctrl chan []byte
	data chan []byte
	dead chan struct{}
	once sync.Once
	err  error

	// What the API answers to events the bridge sent with an ack id.
	acksMu  sync.Mutex
	acks    map[string]chan json.RawMessage
	nextAck uint64
}

var errGone = errors.New("the socket closed")

func newConn(ws *websocket.Conn) *conn {
	return &conn{ws: ws, ctrl: make(chan []byte, 64), data: make(chan []byte, 16), dead: make(chan struct{}), acks: map[string]chan json.RawMessage{}}
}

// fail closes the connection once, remembering why.
func (c *conn) fail(err error) {
	c.once.Do(func() {
		c.err = err
		close(c.dead)
		c.ws.CloseNow()
	})
}

func (c *conn) enqueue(ctx context.Context, msg string, control bool) error {
	q := c.data
	if control {
		q = c.ctrl
	}
	select {
	case q <- []byte(msg):
		return nil
	case <-c.dead:
		return errGone
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Emit sends an event; control ones go ahead of stream data.
func (c *conn) Emit(ctx context.Context, event string, data any, control bool) error {
	payload, err := json.Marshal([]any{event, data})
	if err != nil {
		return err
	}
	return c.enqueue(ctx, "42"+namespace+","+string(payload), control)
}

// EmitAck sends an event with an ack id and waits for the API's answer, the
// first value it acks with.
func (c *conn) EmitAck(ctx context.Context, event string, data any, timeout time.Duration) (json.RawMessage, error) {
	payload, err := json.Marshal([]any{event, data})
	if err != nil {
		return nil, err
	}
	answer := make(chan json.RawMessage, 1)
	c.acksMu.Lock()
	c.nextAck++
	id := strconv.FormatUint(c.nextAck, 10)
	c.acks[id] = answer
	c.acksMu.Unlock()
	defer func() {
		c.acksMu.Lock()
		delete(c.acks, id)
		c.acksMu.Unlock()
	}()

	if err := c.enqueue(ctx, "42"+namespace+","+id+string(payload), false); err != nil {
		return nil, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case got := <-answer:
		return got, nil
	case <-timer.C:
		return nil, fmt.Errorf("%s: no answer in %s", event, timeout)
	case <-c.dead:
		return nil, errGone
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// acked hands the API's answer `43/bridges,<id>[...]` to the event waiting on it.
func (c *conn) acked(s string) {
	rest := strings.TrimPrefix(s, "43")
	if !strings.HasPrefix(rest, namespace+",") {
		return
	}
	rest = rest[len(namespace)+1:]
	i := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
	}
	var parts []json.RawMessage
	if i == 0 || json.Unmarshal([]byte(rest[i:]), &parts) != nil {
		return
	}
	var first json.RawMessage
	if len(parts) > 0 {
		first = parts[0]
	}
	c.acksMu.Lock()
	answer := c.acks[rest[:i]]
	c.acksMu.Unlock()
	if answer != nil {
		select {
		case answer <- first:
		default:
		}
	}
}

// answer acks an event the API sent with an ack id.
func (c *conn) answer(ctx context.Context, ackID string, data any) error {
	payload, err := json.Marshal([]any{data})
	if err != nil {
		return err
	}
	return c.enqueue(ctx, "43"+namespace+","+ackID+string(payload), true)
}

// write runs until the connection fails or ctx ends, control messages first.
func (c *conn) write(ctx context.Context) {
	for {
		var msg []byte
		select {
		case msg = <-c.ctrl:
		default:
			select {
			case msg = <-c.ctrl:
			case msg = <-c.data:
			case <-ctx.Done():
				return
			case <-c.dead:
				return
			}
		}
		wctx, cancel := context.WithTimeout(ctx, writeTimeout)
		err := c.ws.Write(wctx, websocket.MessageText, msg)
		cancel()
		if err != nil {
			c.fail(fmt.Errorf("write: %w", err))
			return
		}
	}
}

// serveConn opens one Socket.IO connection, performs the handshake, then serves
// it until the connection drops or ctx ends.
func serveConn(ctx context.Context, cfg config.Config) error {
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The API checks x-device-type, x-organization-id, and an
	// "Authorization: <clientId>:<clientSecret>" header once, as the namespace
	// connects (device-auth.ts). The bridge always identifies as device type "bridge".
	hdr := http.Header{}
	hdr.Set("x-device-type", "bridge")
	hdr.Set("x-organization-id", cfg.OrganizationID)
	hdr.Set("Authorization", cfg.ClientID+":"+cfg.ClientSecret)

	dialCtx, dialCancel := context.WithTimeout(connCtx, 20*time.Second)
	ws, _, err := websocket.Dial(dialCtx, socketURL(cfg.APIBaseURL), &websocket.DialOptions{HTTPHeader: hdr})
	dialCancel()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	ws.SetReadLimit(4 << 20) // a batch of commands names up to 500 paths
	defer ws.CloseNow()

	// Engine.IO OPEN: "0{"sid":...,"pingInterval":...,"pingTimeout":...}".
	pingInterval, pingTimeout := 25*time.Second, 20*time.Second
	{
		rctx, rc := context.WithTimeout(connCtx, 20*time.Second)
		_, data, rerr := ws.Read(rctx)
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

	// Socket.IO CONNECT to the namespace, then wait for its CONNECT ack. The
	// writer isn't running yet, so the handshake writes directly.
	hctx, hc := context.WithTimeout(connCtx, writeTimeout)
	err = ws.Write(hctx, websocket.MessageText, []byte("40"+namespace+","))
	hc()
	if err != nil {
		return fmt.Errorf("send connect: %w", err)
	}
	for {
		rctx, rc := context.WithTimeout(connCtx, readDeadline)
		_, data, rerr := ws.Read(rctx)
		rc()
		if rerr != nil {
			return fmt.Errorf("read connect ack: %w", rerr)
		}
		s := string(data)
		switch {
		case s == "2": // Engine.IO ping during the handshake
			pctx, pc := context.WithTimeout(connCtx, writeTimeout)
			err := ws.Write(pctx, websocket.MessageText, []byte("3"))
			pc()
			if err != nil {
				return err
			}
		case strings.HasPrefix(s, "40"+namespace): // Socket.IO CONNECT ack
			goto connected
		case strings.HasPrefix(s, "44"+namespace): // Socket.IO CONNECT_ERROR
			// "unauthorized": the credentials were refused. Anything else ("unavailable") is
			// the API's own trouble, and trying again soon may work.
			if strings.Contains(s, `"unauthorized"`) {
				return fmt.Errorf("%w: %s", errRefused, trunc(data))
			}
			return fmt.Errorf("ViaVeritas couldn't take the connection: %s", trunc(data))
		case s == "1": // Engine.IO close
			return fmt.Errorf("server closed the connection during handshake")
		}
	}
connected:
	logx.Ok("apisocket", "socket connected (%s)", namespace)
	setStatus(Status{Connected: true, Since: time.Now().UTC().Format(time.RFC3339)})

	c := newConn(ws)
	go c.write(connCtx)
	streams.Default.Attach(connCtx, c)
	defer streams.Default.Detach()
	statusMu.Lock()
	current, currCtx = c, connCtx
	statusMu.Unlock()
	defer func() {
		statusMu.Lock()
		if current == c {
			current, currCtx = nil, nil
		}
		statusMu.Unlock()
	}()

	if err := c.Emit(connCtx, "health", health(), true); err != nil {
		return fmt.Errorf("emit health: %w", err)
	}
	hooksMu.Lock()
	hooks := append([]func(context.Context){}, onConn...)
	hooksMu.Unlock()
	for _, fn := range hooks {
		go fn(connCtx)
	}

	// Reader: answer Engine.IO pings, hand events on and detect disconnects —
	// the only caller of ws.Read once the handshake is done.
	go func() {
		for {
			rctx, rc := context.WithTimeout(connCtx, readDeadline)
			_, data, rerr := ws.Read(rctx)
			rc()
			if rerr != nil {
				c.fail(fmt.Errorf("read: %w", rerr))
				return
			}
			s := string(data)
			switch {
			case s == "2": // ping → pong
				if err := c.enqueue(connCtx, "3", true); err != nil {
					return
				}
			case s == "1" || strings.HasPrefix(s, "41"): // Engine close / SIO disconnect
				c.fail(errors.New("server closed the connection"))
				return
			case strings.HasPrefix(s, "42"):
				if e, ok := parseEvent(s); ok {
					dispatch(connCtx, c, e)
				}
			case strings.HasPrefix(s, "43"):
				c.acked(s)
			}
		}
	}()

	tick := time.NewTicker(healthInterval)
	defer tick.Stop()
	for {
		select {
		case <-connCtx.Done():
			return connCtx.Err()
		case <-c.dead:
			return c.err
		case <-tick.C:
			if err := c.Emit(connCtx, "health", health(), true); err != nil {
				return fmt.Errorf("emit health: %w", err)
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

// dispatch runs in the reader: stream events go to the registry at once and in
// order (it never blocks); everything else gets a goroutine of its own.
func dispatch(ctx context.Context, c *conn, e event) {
	switch e.name {
	case "stream_open":
		var o streams.Open
		answer := streams.OpenAnswer{Code: streams.CodeInvalid, Message: "unreadable stream_open"}
		if json.Unmarshal(e.data, &o) == nil {
			answer = streams.Default.Open(o)
		}
		if e.ackID != "" {
			_ = c.answer(ctx, e.ackID, answer)
		}
	case "stream_data":
		streams.Default.Data(e.data)
	case "stream_ack":
		streams.Default.Ack(e.data)
	case "stream_close":
		streams.Default.Close(e.data)
	case "stream_resize":
		streams.Default.Resize(e.data)
	default:
		go handle(ctx, c, e)
	}
}

func handle(ctx context.Context, c *conn, e event) {
	switch e.name {
	case "command":
		var cmd commands.Command
		if err := json.Unmarshal(e.data, &cmd); err != nil {
			logx.Warn("apisocket", "unreadable command: %v", err)
			return
		}
		// The folders as they are now: one added since the socket opened counts.
		result := commands.Run(config.LoadOrDefaults(), cmd)
		if e.ackID == "" {
			return
		}
		if err := c.answer(ctx, e.ackID, result); err != nil {
			logx.Warn("apisocket", "could not answer command %s: %v", cmd.ID, err)
		}
	case "features":
		var m struct {
			Portal map[config.Feature]bool `json:"portal"`
		}
		if err := json.Unmarshal(e.data, &m); err != nil || m.Portal == nil {
			return
		}
		ApplyPortalFeatures(m.Portal)
	case "exception":
		logx.Warn("apisocket", "the API refused a message: %s", trunc(e.data))
	default:
		hooksMu.Lock()
		h := handlers[e.name]
		hooksMu.Unlock()
		if h == nil {
			return
		}
		answer := h(ctx, e.data)
		if e.ackID != "" {
			if err := c.answer(ctx, e.ackID, answer); err != nil {
				logx.Warn("apisocket", "could not answer %s: %v", e.name, err)
			}
		}
	}
}

// ApplyPortalFeatures keeps ViaVeritas's switches as it said them: a
// reload-modify-save, so nothing else on disk is undone. The running service
// sees the change within config.LiveInterval and closes what may no longer run.
func ApplyPortalFeatures(portal map[config.Feature]bool) {
	cfg, err := config.Load()
	if err != nil {
		return
	}
	next, changed := cfg.WithPortalFeatures(portal)
	if !changed {
		return
	}
	if err := config.Save(next); err != nil {
		logx.Warn("apisocket", "could not keep ViaVeritas's switches: %v", err)
		return
	}
	var off []string
	for _, f := range config.Features {
		if !next.PortalAllows(f) {
			off = append(off, string(f))
		}
	}
	if len(off) == 0 {
		logx.Info("apisocket", "ViaVeritas allows every feature")
	} else {
		logx.Info("apisocket", "ViaVeritas switched off: %s", strings.Join(off, ", "))
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
