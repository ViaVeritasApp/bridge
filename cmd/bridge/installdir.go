package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/kardianos/service"

	"bridge/internal/config"
	"bridge/internal/logx"
	"bridge/internal/platform"
)

// The state a bridge set up beside its program carries into the installation.
var stateFiles = []string{"config.json", "index.txt", filepath.Join("tls", "cert.pem"), filepath.Join("tls", "key.pem"), filepath.Join("tls", "meta.json")}

// installProgram puts this program where the service runs it
// (config.InstalledBinary), so wherever it was unpacked doesn't matter, and
// brings along the state of a bridge set up beside it. A newer installed
// version is kept unless force.
func installProgram(force bool) error {
	exe, err := runningBinary()
	if err != nil {
		return fmt.Errorf("locate this program: %w", err)
	}
	target := config.InstalledBinary()
	if same, _ := sameFile(exe, target); same {
		return nil
	}

	installed := config.InstalledVersion()
	if installed != "" && !force && config.CompareVersions(installed, config.DefaultVersion) > 0 {
		return fmt.Errorf("a newer bridge (%s) is installed in %s: run that one, or pass --force to replace it with %s",
			installed, config.InstallDir(), config.DefaultVersion)
	}

	if err := os.MkdirAll(config.InstallDir(), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", config.InstallDir(), err)
	}
	// Windows can't replace a program that runs; elsewhere it's tidier stopped too.
	if s, serr := service.New(&program{}, svcConfig); serr == nil {
		_ = platform.Current().StopService(s, svcConfig.Name)
	}
	if err := replaceFile(exe, target, 0o755); err != nil {
		return fmt.Errorf("copy the program to %s: %w", target, err)
	}
	if err := migrateState(filepath.Dir(exe)); err != nil {
		return err
	}
	if installed != "" && installed != config.DefaultVersion {
		logx.Ok("bridge", "updated %s from %s to %s", config.InstallDir(), installed, config.DefaultVersion)
	} else {
		logx.Ok("bridge", "installed %s in %s", config.DefaultVersion, config.InstallDir())
	}
	return nil
}

// migrateState copies a bridge's config, index and certificate from beside the
// program into the installation, unless the installation has its own.
func migrateState(from string) error {
	to := config.InstallDir()
	if from == to {
		return nil
	}
	if _, err := os.Stat(filepath.Join(to, "config.json")); err == nil {
		return nil
	}
	if _, err := os.Stat(filepath.Join(from, "config.json")); err != nil {
		return nil
	}
	for _, name := range stateFiles {
		src := filepath.Join(from, name)
		if _, err := os.Stat(src); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		dst := filepath.Join(to, name)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		if err := replaceFile(src, dst, 0o600); err != nil {
			return fmt.Errorf("move %s into %s: %w", name, to, err)
		}
	}
	logx.Ok("bridge", "moved its settings from %s to %s", from, to)
	return nil
}

func runningBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	return exe, nil
}

func sameFile(a, b string) (bool, error) {
	sa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(sa, sb), nil
}

// replaceFile writes src's bytes beside dst and renames them over it, so dst
// is never half written.
func replaceFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
