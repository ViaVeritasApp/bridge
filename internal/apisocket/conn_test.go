package apisocket

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"bridge/internal/config"
)

// stub is a Socket.IO server as far as the bridge needs one: it opens the
// Engine.IO session, takes the namespace CONNECT, then hands the connection to
// the test.
func stub(t *testing.T, refuse bool, then func(ctx context.Context, ws *websocket.Conn)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-device-type") != "bridge" || r.Header.Get("Authorization") != "id:secret" {
			http.Error(w, "missing headers", http.StatusBadRequest)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ctx := r.Context()
		_ = ws.Write(ctx, websocket.MessageText, []byte(`0{"sid":"e","pingInterval":25000,"pingTimeout":20000}`))
		if _, msg, err := ws.Read(ctx); err != nil || string(msg) != "40/bridges," {
			t.Errorf("connect = %q, %v", msg, err)
			return
		}
		if refuse {
			_ = ws.Write(ctx, websocket.MessageText, []byte(`44/bridges,{"message":"unauthorized"}`))
			return
		}
		_ = ws.Write(ctx, websocket.MessageText, []byte(`40/bridges,{"sid":"n"}`))
		then(ctx, ws)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func paired(url string) config.Config {
	cfg := config.Defaults()
	cfg.APIBaseURL, cfg.BridgeID, cfg.OrganizationID, cfg.ClientID, cfg.ClientSecret = url, "b", "o", "id", "secret"
	return cfg
}

// await reads until a message starts with prefix.
func await(t *testing.T, ctx context.Context, ws *websocket.Conn, prefix string) string {
	t.Helper()
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for {
		_, msg, err := ws.Read(rctx)
		if err != nil {
			t.Errorf("waiting for %q: %v", prefix, err)
			return ""
		}
		if strings.HasPrefix(string(msg), prefix) {
			return string(msg)
		}
	}
}

func TestRefusedCredentials(t *testing.T) {
	url := stub(t, true, nil)
	err := serveConn(context.Background(), paired(url))
	if !errors.Is(err, errRefused) {
		t.Fatalf("serveConn = %v, want errRefused", err)
	}
}

func TestStreamThroughTheSocket(t *testing.T) {
	done := make(chan struct{})
	url := stub(t, false, func(ctx context.Context, ws *websocket.Conn) {
		defer close(done)
		await(t, ctx, ws, `42/bridges,["health"`)

		_ = ws.Write(ctx, websocket.MessageText, []byte(`42/bridges,7["stream_open",{"id":"s1","kind":"echo","window":262144,"user":{"email":"anna@example.com"}}]`))
		if got := await(t, ctx, ws, "43/bridges,7"); got != `43/bridges,7[{"ok":true,"window":262144}]` {
			t.Errorf("open answer = %s", got)
		}

		_ = ws.Write(ctx, websocket.MessageText, []byte(`42/bridges,["stream_data",{"id":"s1","seq":0,"d":"aGk="}]`))
		_ = ws.Write(ctx, websocket.MessageText, []byte("2"))
		// The pong and the echo, in whichever order they come.
		pong, echo := false, ""
		rctx, rcancel := context.WithTimeout(ctx, 2*time.Second)
		for !pong || echo == "" {
			_, msg, err := ws.Read(rctx)
			if err != nil {
				t.Errorf("pong=%v echo=%q: %v", pong, echo, err)
				break
			}
			switch s := string(msg); {
			case s == "3":
				pong = true
			case strings.HasPrefix(s, `42/bridges,["stream_data"`):
				echo = s
			}
		}
		rcancel()
		if echo != `42/bridges,["stream_data",{"id":"s1","seq":0,"d":"aGk="}]` {
			t.Errorf("echo = %s", echo)
		}

		_ = ws.Write(ctx, websocket.MessageText, []byte(`42/bridges,8["stream_open",{"id":"s2","kind":"shell"}]`))
		if got := await(t, ctx, ws, "43/bridges,8"); !strings.Contains(got, `"code":"unknown_kind"`) {
			t.Errorf("unknown kind answer = %s", got)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- serveConn(ctx, paired(url)) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the exchange didn't finish")
	}
	if !CurrentStatus().Connected {
		t.Error("status doesn't say connected")
	}
	cancel()
	<-errc
}

func TestTheAPIAcknowledges(t *testing.T) {
	var got string
	url := stub(t, false, func(ctx context.Context, ws *websocket.Conn) {
		await(t, ctx, ws, `42/bridges,["health"`)
		msg := await(t, ctx, ws, `42/bridges,`)
		// 42/bridges,<id>["events",…]: answer it with the same id.
		rest := strings.TrimPrefix(msg, "42/bridges,")
		id := rest[:strings.Index(rest, "[")]
		got = rest[len(id):]
		_ = ws.Write(ctx, websocket.MessageText, []byte(`43/bridges,`+id+`[{"stored":2}]`))
		<-ctx.Done()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = serveConn(ctx, paired(url)) }()
	var answer []byte
	var err error
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if answer, err = EmitAck(ctx, "events", map[string]int{"n": 2}, 2*time.Second); !errors.Is(err, ErrNotConnected) {
			break
		}
	}
	if err != nil || string(answer) != `{"stored":2}` {
		t.Fatalf("EmitAck = %s, %v", answer, err)
	}
	if got != `["events",{"n":2}]` {
		t.Errorf("sent %s", got)
	}
}
