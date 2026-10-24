package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	pty "github.com/aymanbagabas/go-pty"

	"bridge/internal/config"
	"bridge/internal/streams"
)

const (
	// Idle: a terminal nothing is typed in or written to for this long ends.
	Idle = 15 * time.Minute
	// MaxTime is the longest a terminal stays open.
	MaxTime = 4 * time.Hour
	// MaxTerminals and MaxCommands open at once on this bridge.
	MaxTerminals = 3
	MaxCommands  = 4
	// DefaultTimeout and MaxTimeout of a command.
	DefaultTimeout = 5 * time.Minute
	MaxTimeout     = time.Hour
	// MaxCommand is the longest command line taken.
	MaxCommand = 4096
)

var configOf = config.LoadOrDefaults

// Register offers terminals ("terminal") and commands ("exec") on reg; cfg is
// where it reads who they run as and whether the screen says so.
func Register(reg *streams.Registry, cfg func() config.Config) {
	configOf = cfg
	reg.HandleChecked("terminal", config.FeatureTerminal, MaxTerminals, checkAccount, Terminal)
	reg.HandleChecked("exec", config.FeatureTerminal, MaxCommands, checkCommand, Exec)
}

type terminalParams struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

type commandParams struct {
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// checkAccount refuses a session before it opens when there is nobody to run it as.
func checkAccount(streams.Open) error {
	a, err := Resolve(configOf().SessionsRunAs())
	if err != nil {
		return err
	}
	a.Release()
	return nil
}

func checkCommand(o streams.Open) error {
	var p commandParams
	if err := json.Unmarshal(o.Params, &p); err != nil || strings.TrimSpace(p.Command) == "" {
		return errors.New("no command to run")
	}
	if len(p.Command) > MaxCommand {
		return fmt.Errorf("a command is %d characters at most", MaxCommand)
	}
	return checkAccount(o)
}

// Terminal runs a login shell in a pseudo-terminal: what ViaVeritas sends is
// typed into it, what it writes goes back, and a resize resizes it. It ends
// when the shell exits (with its exit code), when ViaVeritas closes it, after
// Idle without input or output, or after MaxTime.
func Terminal(s *streams.Stream) error {
	var p terminalParams
	_ = json.Unmarshal(s.Params, &p)
	cfg := configOf()
	acct, err := Resolve(cfg.SessionsRunAs())
	if err != nil {
		return err
	}
	defer acct.Release()
	s.Info(map[string]any{"run_as": acct.Label(), "system": acct.System})

	pt, err := pty.New()
	if err != nil {
		return fmt.Errorf("a terminal couldn't be made on this computer: %w", err)
	}
	defer pt.Close()
	_ = pt.Resize(clamp(p.Cols, 20, 500, 80), clamp(p.Rows, 5, 200, 24))
	prepareTTY(pt, acct)
	cmd := pt.Command(acct.Shell, acct.shellArgs()...)
	cmd.Env, cmd.Dir, cmd.SysProcAttr = acct.env(), acct.Home, acct.sysProcAttr(false)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("the shell didn't start: %w", err)
	}
	tell(cfg, fmt.Sprintf("%s opened a terminal on this computer, as %s.", who(s.User), acct.Label()))

	var last atomic.Int64
	touch := func() { last.Store(time.Now().UnixNano()) }
	touch()
	output := make(chan struct{})
	go func() {
		defer close(output)
		buf := make([]byte, streams.ChunkSize)
		for {
			n, err := pt.Read(buf)
			if n > 0 {
				touch()
				if _, werr := s.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		buf := make([]byte, streams.ChunkSize)
		for {
			n, err := s.Read(buf)
			if n > 0 {
				touch()
				if _, werr := pt.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		for {
			select {
			case size := <-s.Resizes():
				_ = pt.Resize(clamp(size.Cols, 20, 500, 80), clamp(size.Rows, 5, 200, 24))
			case <-s.Context().Done():
				return
			}
		}
	}()
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	longest := time.NewTimer(MaxTime)
	defer longest.Stop()
	for {
		select {
		case <-exited:
			// What the shell wrote last still goes out before the stream closes.
			select {
			case <-output:
			case <-time.After(2 * time.Second):
			}
			code := 0
			if cmd.ProcessState != nil {
				code = cmd.ProcessState.ExitCode()
			}
			s.Exit(code)
			return nil
		case <-s.Context().Done():
			stopProcess(cmd.Process)
			return nil
		case <-longest.C:
			stopProcess(cmd.Process)
			s.Close(streams.CodeTooLong, "it was open four hours, the longest a terminal stays")
			return nil
		case <-tick.C:
			if time.Since(time.Unix(0, last.Load())) >= Idle {
				stopProcess(cmd.Process)
				s.Close(streams.CodeIdle, "nothing happened in it for fifteen minutes")
				return nil
			}
		}
	}
}

// Exec runs one command line in the account's shell, its output (and errors)
// streamed back as it comes, then its exit code. It ends early when ViaVeritas
// closes it, or past its timeout (5 minutes unless asked, an hour at most).
func Exec(s *streams.Stream) error {
	var p commandParams
	_ = json.Unmarshal(s.Params, &p)
	timeout := DefaultTimeout
	if p.TimeoutSeconds > 0 {
		timeout = min(time.Duration(p.TimeoutSeconds)*time.Second, MaxTimeout)
	}
	cfg := configOf()
	acct, err := Resolve(cfg.SessionsRunAs())
	if err != nil {
		return err
	}
	defer acct.Release()
	s.Info(map[string]any{"run_as": acct.Label(), "system": acct.System})

	ctx, cancel := context.WithTimeout(s.Context(), timeout)
	defer cancel()
	name, args := acct.commandArgs(p.Command)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env, cmd.Dir, cmd.SysProcAttr = acct.env(), acct.Home, acct.sysProcAttr(true)
	cmd.Stdout, cmd.Stderr = s, s
	cmd.Cancel = func() error {
		stopProcess(cmd.Process)
		return nil
	}
	cmd.WaitDelay = 5 * time.Second
	tell(cfg, fmt.Sprintf("%s ran a command on this computer, as %s: %s", who(s.User), acct.Label(), firstLine(p.Command)))

	err = cmd.Run()
	switch {
	case s.Context().Err() != nil:
		return nil
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		s.Close(streams.CodeTimeout, fmt.Sprintf("it ran longer than %s, so it was stopped", timeout))
		return nil
	case cmd.ProcessState == nil:
		return fmt.Errorf("the command didn't start: %w", err)
	}
	s.Exit(cmd.ProcessState.ExitCode())
	return nil
}

// tell says on the screen what was opened, when the computer wants it said; the log says it anyway.
func tell(cfg config.Config, text string) {
	if cfg.SessionNotices() {
		announce(text)
		return
	}
	logInfo(text)
}

func clamp(v, lo, hi, fallback int) int {
	if v <= 0 {
		return fallback
	}
	return max(lo, min(hi, v))
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if len(line) > 120 {
		line = line[:120] + "…"
	}
	return line
}

func who(u streams.User) string {
	switch {
	case u.Name != "":
		return u.Name
	case u.Email != "":
		return u.Email
	}
	return "Someone in ViaVeritas"
}
