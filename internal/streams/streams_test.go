package streams

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"bridge/internal/config"
)

type emitted struct {
	event   string
	data    json.RawMessage
	control bool
}

type fakeSender struct{ ch chan emitted }

func newFake() *fakeSender { return &fakeSender{ch: make(chan emitted, 1024)} }

func (f *fakeSender) Emit(_ context.Context, event string, data any, control bool) error {
	b, _ := json.Marshal(data)
	f.ch <- emitted{event, b, control}
	return nil
}

// next is the next event of that name, failing the test after a second.
func (f *fakeSender) next(t *testing.T, event string) emitted {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case e := <-f.ch:
			if e.event == event {
				return e
			}
		case <-deadline:
			t.Fatalf("no %s within a second", event)
		}
	}
}

// none checks no event of that name comes for a little while.
func (f *fakeSender) none(t *testing.T, event string) {
	t.Helper()
	deadline := time.After(150 * time.Millisecond)
	for {
		select {
		case e := <-f.ch:
			if e.event == event {
				t.Fatalf("unexpected %s: %s", event, e.data)
			}
		case <-deadline:
			return
		}
	}
}

func setup(t *testing.T, cfg config.Config) (*Registry, *fakeSender) {
	t.Helper()
	r := New()
	r.SetConfig(func() config.Config { return cfg })
	r.Handle("echo", "", 0, Echo)
	f := newFake()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r.Attach(ctx, f)
	return r, f
}

func data(id string, seq uint64, s string) json.RawMessage {
	b, _ := json.Marshal(dataMsg{ID: id, Seq: seq, D: base64.StdEncoding.EncodeToString([]byte(s))})
	return b
}

func decode(t *testing.T, e emitted) (dataMsg, string) {
	t.Helper()
	var m dataMsg
	if err := json.Unmarshal(e.data, &m); err != nil {
		t.Fatal(err)
	}
	b, err := base64.StdEncoding.DecodeString(m.D)
	if err != nil {
		t.Fatal(err)
	}
	return m, string(b)
}

func TestEchoKeepsOrder(t *testing.T) {
	r, f := setup(t, config.Defaults())
	if a := r.Open(Open{ID: "s1", Kind: "echo"}); !a.OK || a.Window != Window {
		t.Fatalf("open = %+v", a)
	}
	// Out of order on the way in: the stream reads them as they were sent.
	r.Data(data("s1", 2, "c"))
	r.Data(data("s1", 0, "a"))
	r.Data(data("s1", 1, "b"))
	r.Data(data("s1", 1, "b")) // a repeat is dropped
	var got strings.Builder
	for want := uint64(0); got.Len() < 3; want++ {
		m, s := decode(t, f.next(t, "stream_data"))
		if m.Seq != want {
			t.Fatalf("seq = %d, want %d", m.Seq, want)
		}
		got.WriteString(s)
	}
	if got.String() != "abc" {
		t.Fatalf("echoed %q, want abc", got.String())
	}
	f.none(t, "stream_data") // the repeat isn't echoed
}

func TestDataIsControlLast(t *testing.T) {
	r, f := setup(t, config.Defaults())
	r.Open(Open{ID: "s1", Kind: "echo"})
	r.Data(data("s1", 0, "x"))
	e := f.next(t, "stream_data")
	if e.control {
		t.Fatal("stream data must queue behind control messages")
	}
	r.Close(json.RawMessage(`{"id":"s1"}`))
	f.none(t, "stream_close") // ViaVeritas closed it: nothing to tell back
	if len(r.List()) != 0 {
		t.Fatal("a closed stream is still listed")
	}
}

// A stream's close never overtakes what it sent: it queues with the data, which control messages go ahead of.
func TestCloseQueuesBehindData(t *testing.T) {
	r, f := setup(t, config.Defaults())
	r.Handle("once", "", 0, func(s *Stream) error {
		_, err := s.Write([]byte("last words"))
		return err
	})
	r.Open(Open{ID: "s1", Kind: "once"})
	if e := f.next(t, "stream_data"); e.control {
		t.Fatal("stream data must queue behind control messages")
	}
	if e := f.next(t, "stream_close"); e.control {
		t.Fatal("a stream's close must queue behind its data")
	}
}

func TestWriterWaitsForCredit(t *testing.T) {
	r, f := setup(t, config.Defaults())
	total := Window + 3*ChunkSize
	r.Handle("blast", "", 0, func(s *Stream) error {
		_, err := s.Write(make([]byte, total))
		return err
	})
	if a := r.Open(Open{ID: "b", Kind: "blast", Window: Window}); !a.OK {
		t.Fatalf("open = %+v", a)
	}
	sent := 0
	for sent < Window {
		_, s := decode(t, f.next(t, "stream_data"))
		if len(s) > ChunkSize {
			t.Fatalf("a chunk of %d bytes, more than %d", len(s), ChunkSize)
		}
		sent += len(s)
	}
	if sent != Window {
		t.Fatalf("sent %d before any ack, want the window (%d)", sent, Window)
	}
	f.none(t, "stream_data") // out of room until acked
	r.Ack(json.RawMessage(`{"id":"b","bytes":` + fmt.Sprint(2*ChunkSize) + `}`))
	for sent < Window+2*ChunkSize {
		_, s := decode(t, f.next(t, "stream_data"))
		sent += len(s)
	}
	f.none(t, "stream_data")
	// The reader came back from a dropped connection: all the room is free again.
	r.Ack(json.RawMessage(`{"id":"b","bytes":0,"reset":true}`))
	for sent < total {
		_, s := decode(t, f.next(t, "stream_data"))
		sent += len(s)
	}
	var c closeMsg
	_ = json.Unmarshal(f.next(t, "stream_close").data, &c)
	if c.Code != CodeDone {
		t.Fatalf("closed with %q, want done", c.Code)
	}
}

func TestReaderAcksWhatItConsumed(t *testing.T) {
	r, f := setup(t, config.Defaults())
	r.Open(Open{ID: "s1", Kind: "echo"})
	chunk := strings.Repeat("y", ChunkSize)
	for i := 0; i < 3; i++ {
		r.Data(data("s1", uint64(i), chunk))
	}
	var a ackMsg
	_ = json.Unmarshal(f.next(t, "stream_ack").data, &a)
	if a.ID != "s1" || a.Bytes < ackEvery {
		t.Fatalf("ack = %+v, want at least %d bytes", a, ackEvery)
	}
}

func TestMoreThanTheWindowCloses(t *testing.T) {
	r, f := setup(t, config.Defaults())
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	r.Handle("sink", "", 0, func(s *Stream) error {
		select {
		case <-block:
		case <-s.Context().Done():
		}
		return nil
	})
	r.Open(Open{ID: "k", Kind: "sink"})
	chunk := strings.Repeat("z", ChunkSize)
	for i := 0; i <= Window/ChunkSize; i++ {
		r.Data(data("k", uint64(i), chunk))
	}
	var c closeMsg
	_ = json.Unmarshal(f.next(t, "stream_close").data, &c)
	if c.Code != CodeFlow {
		t.Fatalf("closed with %q, want flow", c.Code)
	}
}

func TestSwitchesDecideWhatOpens(t *testing.T) {
	cfg := config.Defaults()
	r, f := setup(t, cfg)
	cur := cfg
	r.SetConfig(func() config.Config { return cur })
	r.Handle("shell", config.FeatureTerminal, 0, func(s *Stream) error {
		<-s.Context().Done()
		return nil
	})

	if a := r.Open(Open{ID: "t1", Kind: "shell"}); a.OK || a.Code != CodeFeatureOff {
		t.Fatalf("terminal off by default: open = %+v", a)
	}
	cur = cur.WithFeature(config.FeatureTerminal, true)
	cur, _ = cur.WithPortalFeatures(map[config.Feature]bool{config.FeatureTerminal: false})
	if a := r.Open(Open{ID: "t1", Kind: "shell"}); a.OK || a.Code != CodeFeatureOff {
		t.Fatalf("off in ViaVeritas: open = %+v", a)
	}
	cur, _ = cur.WithPortalFeatures(map[config.Feature]bool{})
	if a := r.Open(Open{ID: "t1", Kind: "shell"}); !a.OK {
		t.Fatalf("on both sides: open = %+v", a)
	}

	paused := cur
	paused.RemotePaused = true
	r.Enforce(paused)
	var c closeMsg
	_ = json.Unmarshal(f.next(t, "stream_close").data, &c)
	if c.ID != "t1" || c.Code != CodePaused {
		t.Fatalf("pausing closed %+v, want t1 paused", c)
	}
	cur = paused
	if a := r.Open(Open{ID: "t2", Kind: "shell"}); a.Code != CodePaused {
		t.Fatalf("while paused: open = %+v", a)
	}
}

func TestLimits(t *testing.T) {
	r, _ := setup(t, config.Defaults())
	r.Handle("wait", "", 2, func(s *Stream) error {
		<-s.Context().Done()
		return nil
	})
	for i := 0; i < 2; i++ {
		if a := r.Open(Open{ID: fmt.Sprint("w", i), Kind: "wait"}); !a.OK {
			t.Fatalf("open %d = %+v", i, a)
		}
	}
	if a := r.Open(Open{ID: "w2", Kind: "wait"}); a.Code != CodeLimit {
		t.Fatalf("a third of a kind capped at 2: %+v", a)
	}
	if a := r.Open(Open{ID: "w0", Kind: "echo"}); a.Code != CodeInvalid {
		t.Fatalf("a taken id: %+v", a)
	}
	if a := r.Open(Open{ID: "x", Kind: "nope"}); a.Code != CodeUnknownKind {
		t.Fatalf("an unknown kind: %+v", a)
	}
	for i := 0; len(r.List()) < MaxStreams; i++ {
		if a := r.Open(Open{ID: fmt.Sprint("e", i), Kind: "echo"}); !a.OK {
			t.Fatalf("echo %d = %+v", i, a)
		}
	}
	if a := r.Open(Open{ID: "over", Kind: "echo"}); a.Code != CodeLimit {
		t.Fatalf("past MaxStreams: %+v", a)
	}
}

func TestDetachEndsEverything(t *testing.T) {
	r, _ := setup(t, config.Defaults())
	ended := make(chan struct{})
	r.Handle("wait", "", 0, func(s *Stream) error {
		<-s.Context().Done()
		close(ended)
		return nil
	})
	r.Open(Open{ID: "w", Kind: "wait"})
	r.Detach()
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("the handler kept running after its socket went")
	}
	if a := r.Open(Open{ID: "w2", Kind: "wait"}); a.OK {
		t.Fatal("opened a stream with no socket")
	}
}

func TestEndFromThisComputer(t *testing.T) {
	r, f := setup(t, config.Defaults())
	r.Open(Open{ID: "s1", Kind: "echo", User: User{Email: "anna@example.com"}})
	if l := r.List(); len(l) != 1 || l[0].User.Email != "anna@example.com" {
		t.Fatalf("list = %+v", l)
	}
	if !r.End("s1", CodeEnded, "ended on the computer") {
		t.Fatal("End found nothing")
	}
	var c closeMsg
	_ = json.Unmarshal(f.next(t, "stream_close").data, &c)
	if c.Code != CodeEnded {
		t.Fatalf("closed with %q", c.Code)
	}
	if r.End("s1", CodeEnded, "") {
		t.Fatal("ended twice")
	}
}
