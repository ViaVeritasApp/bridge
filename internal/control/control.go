// Package control lets the bridge's own tools reach the running service: the
// dashboard and the CLI ask it how its socket, streams and updates stand, end
// a stream, and ask it to update now. The service listens on a random loopback port and writes the port
// and a token to control.json in its state folder, which only administrators
// can read (0600 owned by root; on Windows the folder is SYSTEM and
// Administrators only), so another user on the computer can't drive it.
package control

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"bridge/internal/apisocket"
	"bridge/internal/config"
	"bridge/internal/logx"
	"bridge/internal/selfupdate"
	"bridge/internal/streams"
	"bridge/internal/utils"
)

// State is the running service, as it says of itself.
type State struct {
	PID       int                `json:"pid"`
	Version   string             `json:"version"`
	StartedAt string             `json:"started_at"`
	Socket    apisocket.Status   `json:"socket"`
	Streams   []streams.Info     `json:"streams"`
	Update    *selfupdate.Status `json:"update,omitempty"`
}

var (
	updateNow    func() error
	updateStatus func() selfupdate.Status
)

// OnUpdate says how the service updates now when asked here, and how its
// updating stands. Set before Serve.
func OnUpdate(now func() error, status func() selfupdate.Status) {
	updateNow, updateStatus = now, status
}

type file struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
}

// ErrNotRunning: no service answers (it isn't installed, or isn't running).
var ErrNotRunning = errors.New("the bridge service isn't running")

func path() string { return filepath.Join(config.Dir(), "control.json") }

// listening is the port Serve answers on, 0 while it doesn't.
var listening atomic.Int64

// Port is the loopback port the service answers its tools on, 0 while it doesn't.
func Port() int { return int(listening.Load()) }

// Serve answers the bridge's tools until ctx ends.
func Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	listening.Store(int64(ln.Addr().(*net.TCPAddr).Port))
	defer listening.Store(0)
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		ln.Close()
		return err
	}
	token := hex.EncodeToString(raw)
	started := time.Now().UTC().Format(time.RFC3339)

	f, _ := json.Marshal(file{Port: ln.Addr().(*net.TCPAddr).Port, Token: token, PID: os.Getpid()})
	if err := utils.AtomicWrite(path(), f, 0o600); err != nil {
		ln.Close()
		return fmt.Errorf("write %s: %w", path(), err)
	}
	defer os.Remove(path())

	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/state", authed(func(w http.ResponseWriter, r *http.Request) {
		state := State{
			PID:       os.Getpid(),
			Version:   config.DefaultVersion,
			StartedAt: started,
			Socket:    apisocket.CurrentStatus(),
			Streams:   streams.Default.List(),
		}
		if updateStatus != nil {
			st := updateStatus()
			state.Update = &st
		}
		writeJSON(w, http.StatusOK, state)
	}))
	mux.HandleFunc("/update", authed(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || updateNow == nil {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := updateNow(); err != nil {
			code := "error"
			var refusal *selfupdate.Refusal
			if errors.As(err, &refusal) {
				code = refusal.Code
			}
			writeJSON(w, http.StatusConflict, map[string]string{"code": code, "message": err.Error()})
			return
		}
		logx.Info("control", "an update was asked for on this computer")
		w.WriteHeader(http.StatusAccepted)
	}))
	mux.HandleFunc("/end", authed(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct{ ID string }
		if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req) != nil || req.ID == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if !streams.Default.End(req.ID, streams.CodeEnded, "ended on the computer") {
			http.Error(w, "no such stream", http.StatusNotFound)
			return
		}
		logx.Info("control", "ended stream %s on this computer", req.ID)
		w.WriteHeader(http.StatusNoContent)
	}))

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func dial() (file, error) {
	b, err := os.ReadFile(path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return file{}, ErrNotRunning
		}
		return file{}, err
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil || f.Port == 0 {
		return file{}, ErrNotRunning
	}
	return f, nil
}

var client = &http.Client{Timeout: 3 * time.Second}

func call(method, route string, body any) (*http.Response, error) {
	f, err := dial()
	if err != nil {
		return nil, err
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, "http://127.0.0.1:"+strconv.Itoa(f.Port)+route, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+f.Token)
	res, err := client.Do(req)
	if err != nil {
		// control.json outlived its service (it was killed).
		return nil, ErrNotRunning
	}
	return res, nil
}

// Ask is how the running service stands.
func Ask() (State, error) {
	res, err := call(http.MethodGet, "/state", nil)
	if err != nil {
		return State{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return State{}, fmt.Errorf("the service answered %s", res.Status)
	}
	var s State
	if err := json.NewDecoder(res.Body).Decode(&s); err != nil {
		return State{}, err
	}
	return s, nil
}

// UpdateNow asks the service to install the newer version ViaVeritas offers:
// it answers once the update starts, which then goes on by itself.
func UpdateNow() error {
	res, err := call(http.MethodPost, "/update", nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusAccepted {
		return nil
	}
	var refusal struct{ Code, Message string }
	if json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&refusal) == nil && refusal.Message != "" {
		return &selfupdate.Refusal{Code: refusal.Code, Message: refusal.Message}
	}
	return fmt.Errorf("the service answered %s", res.Status)
}

// End ends a stream on this computer.
func End(id string) error {
	res, err := call(http.MethodPost, "/end", map[string]string{"id": id})
	if err != nil {
		return err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("no stream %s is open", id)
	}
	return fmt.Errorf("the service answered %s", res.Status)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
