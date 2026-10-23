// Package commands carries out the changes the API sends on the bridge's
// socket: moving, renaming and deleting what it synced. The API has checked the
// member's access to every file first; here each path is held to the data
// root and nothing is overwritten. index.txt is left as it was: the scan that
// follows at once finds the change and syncs it, so Files catches up even when
// the API never got the answer (it gave up waiting, or the socket dropped).
// When it did get it and changed Files already, that sync changes nothing.
package commands

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"bridge/internal/config"
	"bridge/internal/files"
	"bridge/internal/logx"
	"bridge/internal/watch"
)

// Move renames a file, or with Dir a folder and everything in it. Paths are
// root-relative with forward slashes ("/Scans/a.pdf", "/Scans").
type Move struct {
	From string `json:"from"`
	To   string `json:"to"`
	Dir  bool   `json:"dir,omitempty"`
}

// Command is what the API sends: a batch of moves or of file deletions, then,
// with Prune, the folders left empty under that one are removed.
type Command struct {
	ID    string   `json:"id"`
	Type  string   `json:"type"`
	Moves []Move   `json:"moves,omitempty"`
	Paths []string `json:"paths,omitempty"`
	Prune string   `json:"prune,omitempty"`
}

// Item is the outcome for one move (keyed by its From) or deletion.
type Item struct {
	Path    string `json:"path"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"` // not_found, exists, invalid, failed
	Message string `json:"message,omitempty"`
}

type Result struct {
	Items []Item `json:"items"`
}

const (
	errNotFound = "not_found"
	errExists   = "exists"
	errInvalid  = "invalid"
	errFailed   = "failed"
)

// A command the API resends (its socket dropped before the answer arrived) is
// answered as before rather than run twice.
var done = struct {
	mu      sync.Mutex
	order   []string
	results map[string]Result
}{results: map[string]Result{}}

const remember = 64

// A message goes back with each item that failed; long ones are cut, so a
// batch's answer stays well under the socket's message limit.
const maxMessage = 200

// Run carries out cmd and answers what happened to each item.
func Run(cfg config.Config, cmd Command) Result {
	if cmd.ID != "" {
		done.mu.Lock()
		r, seen := done.results[cmd.ID]
		done.mu.Unlock()
		if seen {
			return r
		}
	}

	var r Result
	watch.Exclusive(func() { r = run(config.LoadOr(cfg), cmd) })

	if cmd.ID != "" {
		done.mu.Lock()
		done.results[cmd.ID] = r
		done.order = append(done.order, cmd.ID)
		if len(done.order) > remember {
			delete(done.results, done.order[0])
			done.order = done.order[1:]
		}
		done.mu.Unlock()
	}
	return r
}

func run(cfg config.Config, cmd Command) Result {
	items := []Item{}
	if len(cfg.Folders) == 0 {
		for _, p := range targets(cmd) {
			items = append(items, Item{Path: p, Error: errFailed, Message: "no folder is synced"})
		}
		return Result{Items: items}
	}

	switch cmd.Type {
	case "move":
		done := 0
		for _, m := range cmd.Moves {
			item := move(cfg, m)
			items = append(items, item)
			if item.OK {
				done++
			}
		}
		logx.Ok("commands", "move: %d of %d done", done, len(cmd.Moves))
	case "delete":
		done := 0
		for _, p := range cmd.Paths {
			item := remove(cfg, p)
			items = append(items, item)
			if item.OK {
				done++
			}
		}
		logx.Ok("commands", "delete: %d of %d gone", done, len(cmd.Paths))
	default:
		for _, p := range targets(cmd) {
			items = append(items, Item{Path: p, Error: errInvalid, Message: "unknown command " + cmd.Type})
		}
		return Result{Items: items}
	}

	if cmd.Prune != "" {
		prune(cfg, cmd.Prune)
	}
	watch.Nudge()
	return Result{Items: items}
}

func move(cfg config.Config, m Move) Item {
	item := Item{Path: m.From}
	fromFolder, fromRoot, from, err := files.Locate(cfg, m.From)
	if errors.Is(err, config.ErrUnknownFolder) {
		return fail(item, errNotFound, err)
	}
	if err != nil {
		return fail(item, errInvalid, err)
	}
	toFolder, toRoot, to, err := files.Locate(cfg, m.To)
	if err != nil {
		return fail(item, errInvalid, fmt.Errorf("outside the synced folders: %w", err))
	}
	// Each synced folder may be on a disk of its own.
	if fromFolder.ID != toFolder.ID {
		return fail(item, errInvalid, fmt.Errorf("it stays in %q: a file doesn't move to another synced folder", fromFolder.Name))
	}
	if from == fromRoot || to == toRoot {
		return fail(item, errInvalid, errors.New("a synced folder is renamed or removed in the bridge itself"))
	}
	if m.Dir && strings.HasPrefix(to+string(os.PathSeparator), from+string(os.PathSeparator)) {
		return fail(item, errInvalid, errors.New("a folder cannot move into itself"))
	}
	// The scan leaves out names that start with a dot: what moved under one would
	// vanish from Files while it stays on disk.
	if !cfg.IncludeHidden && hidden(m.To) {
		return fail(item, errInvalid, errors.New("names starting with a dot aren't synced"))
	}

	info, err := os.Lstat(from)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fail(item, errNotFound, err)
		}
		return fail(item, errFailed, err)
	}
	if m.Dir != info.IsDir() || (!m.Dir && !info.Mode().IsRegular()) {
		return fail(item, errInvalid, errors.New("not the kind of item expected"))
	}

	// A name that differs only in case is the same entry on a case-insensitive
	// disk (macOS, Windows): renaming it is allowed.
	if existing, err := os.Lstat(to); err == nil {
		if !os.SameFile(info, existing) {
			return fail(item, errExists, errors.New("something by that name is there already"))
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fail(item, errFailed, err)
	}

	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return fail(item, errFailed, err)
	}
	if err := os.Rename(from, to); err != nil {
		return fail(item, errFailed, err)
	}
	item.OK = true
	return item
}

func remove(cfg config.Config, rel string) Item {
	item := Item{Path: rel}
	_, root, abs, err := files.Locate(cfg, rel)
	if errors.Is(err, config.ErrUnknownFolder) {
		return fail(item, errNotFound, err)
	}
	if err != nil {
		return fail(item, errInvalid, err)
	}
	if abs == root {
		return fail(item, errInvalid, errors.New("a synced folder is removed in the bridge itself"))
	}
	info, err := os.Lstat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fail(item, errNotFound, err)
		}
		return fail(item, errFailed, err)
	}
	if !info.Mode().IsRegular() {
		return fail(item, errInvalid, errors.New("only files are deleted"))
	}
	if err := os.Remove(abs); err != nil {
		return fail(item, errFailed, err)
	}
	item.OK = true
	return item
}

// prune removes the folders left empty in rel and under it, deepest first,
// rel too, never a synced folder itself. A folder still holding anything (a
// hidden or unsynced file) stays.
func prune(cfg config.Config, rel string) {
	_, root, abs, err := files.Locate(cfg, rel)
	if err != nil || abs == root {
		return
	}
	var dirs []string
	_ = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		_ = os.Remove(d) // fails on a folder that isn't empty, which stays
	}
}

func targets(cmd Command) []string {
	if cmd.Type == "move" {
		out := make([]string, len(cmd.Moves))
		for i, m := range cmd.Moves {
			out[i] = m.From
		}
		return out
	}
	return cmd.Paths
}

// hidden reports whether any part of a root-relative path starts with a dot.
func hidden(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

func fail(item Item, code string, err error) Item {
	item.Error = code
	item.Message = err.Error()
	if len(item.Message) > maxMessage {
		item.Message = item.Message[:maxMessage] + "…"
	}
	return item
}
