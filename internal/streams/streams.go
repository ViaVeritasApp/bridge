// Package streams carries what goes on for a while between ViaVeritas and this
// computer (a terminal, a command's output, a connection into its network) as
// streams on the bridge's socket. ViaVeritas opens one (stream_open, answered
// by its ack); both sides send data (stream_data: base64, numbered by seq, at
// most ChunkSize bytes each); each grants the other room to send more
// (stream_ack, Window bytes at most unanswered); either ends it
// (stream_close). A stream belongs to the socket it was opened on and ends with
// it. What a stream may do is its kind's feature: a kind whose feature isn't
// Effective is refused, and closed as soon as it stops being.
package streams

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"bridge/internal/config"
	"bridge/internal/logx"
)

const (
	// ChunkSize is the most one stream_data carries: well under the API's 1 MB message limit once in base64.
	ChunkSize = 32 << 10
	// Window is how much a side sends before the other acks it.
	Window = 256 << 10
	// MaxStreams is how many streams one bridge holds open at once.
	MaxStreams = 8

	ackEvery   = Window / 4
	maxPending = 64
)

// Codes a stream closes with.
const (
	CodeDone        = "done"
	CodeFeatureOff  = "feature_off"
	CodePaused      = "paused"
	CodeLimit       = "limit"
	CodeUnknownKind = "unknown_kind"
	CodeInvalid     = "invalid"
	CodeFlow        = "flow"
	// CodeEnded: ended on this computer (its dashboard, or the CLI).
	CodeEnded = "ended"
	CodeError = "error"
	// CodeUnavailable: what the stream needs isn't there (nobody is signed in to run a terminal as).
	CodeUnavailable = "unavailable"
	// CodeIdle, CodeTimeout and CodeTooLong: a terminal left idle, a command past its time, a session past its longest.
	CodeIdle    = "idle"
	CodeTimeout = "timeout"
	CodeTooLong = "max_time"
)

// Refusal is a kind's check refusing a stream with a code of its own (a
// proxy's target not on the list: "not_allowed"); any other error refuses it
// as CodeUnavailable.
type Refusal struct{ Code, Message string }

func (r *Refusal) Error() string { return r.Message }

// ErrClosed is what Read and Write answer once the stream has ended.
var ErrClosed = errors.New("the stream is closed")

// Sender emits events on the socket the streams were opened on. Data
// (control false) waits behind acks, closes and pings.
type Sender interface {
	Emit(ctx context.Context, event string, data any, control bool) error
}

// User is who opened a stream in ViaVeritas.
type User struct {
	ID    string `json:"id,omitempty"`
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

// Size is a terminal's, in characters.
type Size struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// Handler runs one stream until it's done. The stream closes when it returns:
// with CodeDone for nil, else CodeError and the error's message.
type Handler func(s *Stream) error

// Open is what stream_open carries. Window is how much the bridge may send
// before ViaVeritas acks it.
type Open struct {
	ID     string          `json:"id"`
	Kind   string          `json:"kind"`
	User   User            `json:"user"`
	Params json.RawMessage `json:"params,omitempty"`
	Window int             `json:"window,omitempty"`
}

// OpenAnswer is stream_open's ack. Window is how much ViaVeritas may send
// before the bridge acks it.
type OpenAnswer struct {
	OK      bool   `json:"ok"`
	Window  int    `json:"window,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type dataMsg struct {
	ID  string `json:"id"`
	Seq uint64 `json:"seq"`
	D   string `json:"d"`
}

type ackMsg struct {
	ID    string `json:"id"`
	Bytes int    `json:"bytes"`
	// Reset: whoever reads the stream lost what was in flight (it came back from
	// a dropped connection), so all of the window is free again.
	Reset bool `json:"reset,omitempty"`
}

type closeMsg struct {
	ID       string `json:"id"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	// What went each way, for ViaVeritas's record of the session.
	BytesIn  int64 `json:"bytes_in"`
	BytesOut int64 `json:"bytes_out"`
}

type resizeMsg struct {
	ID string `json:"id"`
	Size
}

// Info is an open stream, as the dashboard lists it.
type Info struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	User     User      `json:"user"`
	OpenedAt time.Time `json:"opened_at"`
	BytesIn  int64     `json:"bytes_in"`
	BytesOut int64     `json:"bytes_out"`
}

type kind struct {
	feature config.Feature
	max     int
	check   func(Open) error
	handler Handler
}

// Registry holds the kinds of stream this bridge offers and those open now.
type Registry struct {
	mu     sync.Mutex
	kinds  map[string]kind
	open   map[string]*Stream
	sender Sender
	ctx    context.Context
	config func() config.Config
}

// New is an empty registry.
func New() *Registry {
	return &Registry{kinds: map[string]kind{}, open: map[string]*Stream{}, config: config.LoadOrDefaults}
}

// Default is the service's registry: the socket attaches to it, the dashboard lists it.
var Default = New()

// Handle offers a kind of stream: feature decides whether it may run ("" for
// always), max how many of it may be open at once (0: MaxStreams).
func (r *Registry) Handle(name string, feature config.Feature, max int, h Handler) {
	r.HandleChecked(name, feature, max, nil, h)
}

// HandleChecked offers a kind of stream whose check says, before the stream is
// answered, whether what it needs is there: its error refuses the stream as
// CodeUnavailable, with its text as the reason.
func (r *Registry) HandleChecked(name string, feature config.Feature, max int, check func(Open) error, h Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kinds[name] = kind{feature: feature, max: max, check: check, handler: h}
}

// SetConfig is where the registry reads the switches from.
func (r *Registry) SetConfig(fn func() config.Config) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.config = fn
}

// Attach takes a new socket: streams open on it from now on.
func (r *Registry) Attach(ctx context.Context, s Sender) {
	r.Detach()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ctx, r.sender = ctx, s
}

// Detach ends every stream with the socket they were on: nothing is sent, the
// socket is gone.
func (r *Registry) Detach() {
	r.mu.Lock()
	open := r.open
	r.open = map[string]*Stream{}
	r.sender, r.ctx = nil, nil
	r.mu.Unlock()
	for _, s := range open {
		s.stop()
	}
}

// Open answers a stream_open, starting its handler when it may run.
func (r *Registry) Open(o Open) OpenAnswer {
	// The kind's own check runs outside the lock: it may ask the system (who is signed in).
	r.mu.Lock()
	k, known := r.kinds[o.Kind]
	r.mu.Unlock()
	if known && k.check != nil {
		if err := k.check(o); err != nil {
			var refusal *Refusal
			if errors.As(err, &refusal) {
				return OpenAnswer{Code: refusal.Code, Message: refusal.Message}
			}
			return OpenAnswer{Code: CodeUnavailable, Message: err.Error()}
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sender == nil {
		return OpenAnswer{Code: CodeInvalid, Message: "the bridge isn't connected"}
	}
	if o.ID == "" {
		return OpenAnswer{Code: CodeInvalid, Message: "the stream has no id"}
	}
	if _, taken := r.open[o.ID]; taken {
		return OpenAnswer{Code: CodeInvalid, Message: "a stream with that id is open"}
	}
	k, ok := r.kinds[o.Kind]
	if !ok {
		return OpenAnswer{Code: CodeUnknownKind, Message: "this bridge has no " + o.Kind + " streams"}
	}
	if k.feature != "" {
		if code, msg := refusal(r.config(), k.feature); code != "" {
			return OpenAnswer{Code: code, Message: msg}
		}
	}
	if len(r.open) >= MaxStreams {
		return OpenAnswer{Code: CodeLimit, Message: "too many streams are open on this bridge"}
	}
	if k.max > 0 {
		n := 0
		for _, s := range r.open {
			if s.Kind == o.Kind {
				n++
			}
		}
		if n >= k.max {
			return OpenAnswer{Code: CodeLimit, Message: "too many " + o.Kind + " streams are open on this bridge"}
		}
	}

	window := o.Window
	if window <= 0 || window > 4*Window {
		window = Window
	}
	ctx, cancel := context.WithCancel(r.ctx)
	s := &Stream{
		ID: o.ID, Kind: o.Kind, User: o.User, Params: o.Params, OpenedAt: time.Now(),
		r: r, sender: r.sender, sockCtx: r.ctx, ctx: ctx, cancel: cancel,
		credit: window, window: window,
		notify: make(chan struct{}, 1), creditNotify: make(chan struct{}, 1),
		resizes: make(chan Size, 4), pending: map[uint64][]byte{},
	}
	r.open[o.ID] = s
	logx.Info("streams", "opened %s stream %s for %s", o.Kind, short(o.ID), who(o.User))
	go func() {
		err := k.handler(s)
		if err != nil {
			s.end(CodeError, err.Error(), nil)
			return
		}
		s.end(CodeDone, "", nil)
	}()
	return OpenAnswer{OK: true, Window: Window}
}

// refusal is why a stream of a kind under feature may not run now, or "".
func refusal(cfg config.Config, f config.Feature) (code, message string) {
	switch {
	case !cfg.Allows(f):
		return CodeFeatureOff, "it is switched off on this computer"
	case cfg.Paused(f):
		return CodePaused, "remote access is paused on this computer"
	case !cfg.PortalAllows(f):
		return CodeFeatureOff, "it is switched off in ViaVeritas"
	}
	return "", ""
}

func (r *Registry) get(id string) *Stream {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.open[id]
}

// Data takes a stream_data: called by the socket's reader, it never blocks.
func (r *Registry) Data(raw json.RawMessage) {
	var m dataMsg
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	s := r.get(m.ID)
	if s == nil {
		return
	}
	b, err := base64.StdEncoding.DecodeString(m.D)
	if err != nil {
		s.end(CodeInvalid, "unreadable data", nil)
		return
	}
	s.receive(m.Seq, b)
}

// Ack takes a stream_ack: room to send more.
func (r *Registry) Ack(raw json.RawMessage) {
	var m ackMsg
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	if s := r.get(m.ID); s != nil {
		s.grant(m.Bytes, m.Reset)
	}
}

// Close takes a stream_close: ViaVeritas ended it.
func (r *Registry) Close(raw json.RawMessage) {
	var m closeMsg
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	if s := r.get(m.ID); s != nil {
		s.closeRemote()
	}
}

// Resize takes a stream_resize: a terminal's new size.
func (r *Registry) Resize(raw json.RawMessage) {
	var m resizeMsg
	if json.Unmarshal(raw, &m) != nil || m.Cols <= 0 || m.Rows <= 0 {
		return
	}
	if s := r.get(m.ID); s != nil {
		select {
		case s.resizes <- m.Size:
		default: // the handler hasn't taken the last ones: the newest is what counts
			select {
			case <-s.resizes:
			default:
			}
			select {
			case s.resizes <- m.Size:
			default:
			}
		}
	}
}

// End closes a stream from this computer, saying why; false when it isn't open.
func (r *Registry) End(id, code, message string) bool {
	s := r.get(id)
	if s == nil {
		return false
	}
	s.end(code, message, nil)
	return true
}

// Enforce closes the streams whose feature cfg no longer lets run.
func (r *Registry) Enforce(cfg config.Config) {
	r.mu.Lock()
	var stop []*Stream
	for _, s := range r.open {
		if f := r.kinds[s.Kind].feature; f != "" {
			if code, _ := refusal(cfg, f); code != "" {
				stop = append(stop, s)
			}
		}
	}
	r.mu.Unlock()
	for _, s := range stop {
		code, msg := refusal(cfg, r.kinds[s.Kind].feature)
		s.end(code, msg, nil)
	}
}

// List is every open stream, oldest first.
func (r *Registry) List() []Info {
	r.mu.Lock()
	out := make([]Info, 0, len(r.open))
	for _, s := range r.open {
		out = append(out, s.info())
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].OpenedAt.Before(out[j].OpenedAt) })
	return out
}

func (r *Registry) remove(s *Stream) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.open[s.ID] == s {
		delete(r.open, s.ID)
	}
}

// Stream is one open stream, as its handler sees it: an io.Reader of what
// ViaVeritas sends and an io.Writer back. Write from one goroutine at a time.
type Stream struct {
	ID       string
	Kind     string
	User     User
	Params   json.RawMessage
	OpenedAt time.Time

	r       *Registry
	sender  Sender
	sockCtx context.Context
	ctx     context.Context
	cancel  context.CancelFunc

	mu           sync.Mutex
	inbound      [][]byte
	queued       int
	consumed     int
	nextIn       uint64
	pending      map[uint64][]byte
	remoteClosed bool
	closed       bool
	credit       int
	window       int
	nextOut      uint64
	notify       chan struct{}
	creditNotify chan struct{}
	resizes      chan Size

	wmu      sync.Mutex
	finished sync.Once
	bytesIn  atomic.Int64
	bytesOut atomic.Int64
}

// Context ends when the stream does.
func (s *Stream) Context() context.Context { return s.ctx }

// Resizes are a terminal's new sizes, newest last.
func (s *Stream) Resizes() <-chan Size { return s.resizes }

func (s *Stream) info() Info {
	return Info{ID: s.ID, Kind: s.Kind, User: s.User, OpenedAt: s.OpenedAt, BytesIn: s.bytesIn.Load(), BytesOut: s.bytesOut.Load()}
}

func wake(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// receive queues data in seq order, holding what comes early.
func (s *Stream) receive(seq uint64, b []byte) {
	s.mu.Lock()
	if s.closed || s.remoteClosed {
		s.mu.Unlock()
		return
	}
	switch {
	case seq < s.nextIn:
		s.mu.Unlock()
		return // seen already
	case seq > s.nextIn:
		s.pending[seq] = b
		if len(s.pending) > maxPending {
			s.mu.Unlock()
			s.end(CodeFlow, "data arrived out of order", nil)
			return
		}
		s.mu.Unlock()
		return
	}
	s.push(b)
	for {
		next, ok := s.pending[s.nextIn]
		if !ok {
			break
		}
		delete(s.pending, s.nextIn)
		s.push(next)
	}
	over := s.queued > Window
	s.mu.Unlock()
	if over {
		s.end(CodeFlow, "more data arrived than the bridge made room for", nil)
		return
	}
	wake(s.notify)
}

// push appends in-order data; s.mu is held.
func (s *Stream) push(b []byte) {
	s.nextIn++
	if len(b) == 0 {
		return
	}
	s.inbound = append(s.inbound, b)
	s.queued += len(b)
	s.bytesIn.Add(int64(len(b)))
}

// Read is what ViaVeritas sent, in order. io.EOF once it ended the stream and
// everything it sent was read.
func (s *Stream) Read(p []byte) (int, error) {
	for {
		s.mu.Lock()
		if len(s.inbound) > 0 {
			head := s.inbound[0]
			n := copy(p, head)
			if n < len(head) {
				s.inbound[0] = head[n:]
			} else {
				s.inbound[0] = nil
				s.inbound = s.inbound[1:]
			}
			s.queued -= n
			s.consumed += n
			grant := 0
			if s.consumed >= ackEvery && !s.closed {
				grant, s.consumed = s.consumed, 0
			}
			s.mu.Unlock()
			if grant > 0 {
				_ = s.sender.Emit(s.sockCtx, "stream_ack", ackMsg{ID: s.ID, Bytes: grant}, true)
			}
			return n, nil
		}
		if s.remoteClosed || s.closed {
			s.mu.Unlock()
			return 0, io.EOF
		}
		s.mu.Unlock()
		select {
		case <-s.notify:
		case <-s.ctx.Done():
		}
	}
}

// Write sends p to ViaVeritas in chunks, waiting for room when it has none.
func (s *Stream) Write(p []byte) (int, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	total := 0
	for len(p) > 0 {
		n := min(len(p), ChunkSize)
		var seq uint64
		for {
			s.mu.Lock()
			if s.closed || s.remoteClosed {
				s.mu.Unlock()
				return total, ErrClosed
			}
			if s.credit > 0 {
				n = min(n, s.credit)
				s.credit -= n
				seq = s.nextOut
				s.nextOut++
				s.mu.Unlock()
				break
			}
			s.mu.Unlock()
			select {
			case <-s.creditNotify:
			case <-s.ctx.Done():
				return total, ErrClosed
			}
		}
		if err := s.sender.Emit(s.ctx, "stream_data", dataMsg{ID: s.ID, Seq: seq, D: base64.StdEncoding.EncodeToString(p[:n])}, false); err != nil {
			return total, ErrClosed
		}
		s.bytesOut.Add(int64(n))
		p = p[n:]
		total += n
	}
	return total, nil
}

func (s *Stream) grant(bytes int, reset bool) {
	s.mu.Lock()
	if reset {
		s.credit = s.window
	} else if bytes > 0 {
		s.credit = min(s.credit+bytes, s.window)
	}
	s.mu.Unlock()
	wake(s.creditNotify)
}

// Exit closes the stream with what a command ended with.
func (s *Stream) Exit(code int) {
	s.end(CodeDone, "", &code)
}

// Close ends the stream from this side, saying why (CodeIdle, CodeTimeout…).
func (s *Stream) Close(code, message string) {
	s.end(code, message, nil)
}

// Info tells ViaVeritas something about the stream while it is open (what a
// terminal runs as): a stream_info event carrying fields and the stream's id.
func (s *Stream) Info(fields map[string]any) {
	msg := map[string]any{}
	for k, v := range fields {
		msg[k] = v
	}
	msg["id"] = s.ID
	_ = s.sender.Emit(s.sockCtx, "stream_info", msg, true)
}

// end closes the stream from this side, telling ViaVeritas why, once.
func (s *Stream) end(code, message string, exitCode *int) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	tell := !s.remoteClosed
	s.mu.Unlock()
	if tell && s.sockCtx.Err() == nil {
		// Queued with the data, behind what the stream sent before it: as control it could overtake its last chunks.
		_ = s.sender.Emit(s.sockCtx, "stream_close", closeMsg{ID: s.ID, Code: code, Message: message, ExitCode: exitCode, BytesIn: s.bytesIn.Load(), BytesOut: s.bytesOut.Load()}, false)
	}
	s.finish(code)
}

// closeRemote: ViaVeritas ended the stream; what it sent before still reads.
func (s *Stream) closeRemote() {
	s.mu.Lock()
	if s.closed || s.remoteClosed {
		s.mu.Unlock()
		return
	}
	s.remoteClosed = true
	s.mu.Unlock()
	wake(s.notify)
	s.finish("remote")
}

// stop ends the stream with its socket: there is nothing to tell.
func (s *Stream) stop() {
	s.mu.Lock()
	already := s.closed
	s.closed = true
	s.mu.Unlock()
	if !already {
		s.finish("gone")
	}
}

func (s *Stream) finish(why string) {
	s.finished.Do(func() {
		s.cancel()
		wake(s.notify)
		wake(s.creditNotify)
		s.r.remove(s)
		logx.Info("streams", "closed %s stream %s (%s; %d bytes in, %d out)", s.Kind, short(s.ID), why, s.bytesIn.Load(), s.bytesOut.Load())
	})
}

// Echo sends back what it reads: how the API checks a bridge's streams end to end.
func Echo(s *Stream) error {
	buf := make([]byte, ChunkSize)
	for {
		n, err := s.Read(buf)
		if n > 0 {
			if _, werr := s.Write(buf[:n]); werr != nil {
				return nil
			}
		}
		if err != nil {
			return nil
		}
	}
}

func init() {
	Default.Handle("echo", "", 2, Echo)
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func who(u User) string {
	switch {
	case u.Name != "" && u.Email != "":
		return u.Name + " <" + u.Email + ">"
	case u.Email != "":
		return u.Email
	case u.Name != "":
		return u.Name
	case u.ID != "":
		return u.ID
	}
	return "ViaVeritas"
}
