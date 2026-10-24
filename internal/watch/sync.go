package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"bridge/internal/config"
	"bridge/internal/httpx"
	"bridge/internal/logx"
)

// Wire formats matching the API's SyncDto / SyncFileItemDto.
type fileItem struct {
	Path          string `json:"path"`
	Name          string `json:"name"`
	Size          int64  `json:"size"`
	Filetype      string `json:"content_type,omitempty"`
	DeviceFileIno string `json:"inode"` // OS file identity "<dev>:<ino>"
	ModifiedAt    string `json:"modified_at,omitempty"`
}

type syncBody struct {
	Mode  string     `json:"mode"`
	Files []fileItem `json:"files"`
	// Deleted holds each removed file's OS file-ID ("<dev>:<ino>"), not its
	// path, so the server matches deletions by identity.
	Deleted []string `json:"deleted,omitempty"`
}

// SyncAttribution is one entry in the API's sync response: the row id assigned
// for a file, keyed by path. The bridge folds these into index.txt so Get can
// return the API file-id without re-querying.
type SyncAttribution struct {
	Path          string `json:"path"`
	DeviceFileIno string `json:"deviceFileIno,omitempty"`
	FileID        string `json:"fileId"`
}

type syncResp struct {
	OK    bool              `json:"ok"`
	Files []SyncAttribution `json:"files,omitempty"`
}

// lock keeps a scan and a command from the API off the data root and index.txt
// at the same time.
var lock sync.Mutex

// Exclusive runs fn while no scan runs; the API's commands change the data root
// through it.
func Exclusive(fn func()) {
	lock.Lock()
	defer lock.Unlock()
	fn()
}

var nudges = make(chan struct{}, 1)

// Nudge asks for a scan now rather than at the next interval: after a command
// changed the data root, so the API hears of it at once even if it never got
// the command's answer.
func Nudge() {
	select {
	case nudges <- struct{}{}:
	default:
	}
}

// Nudged is where the service loop hears a Nudge.
func Nudged() <-chan struct{} { return nudges }

var lastScan struct {
	mu      sync.Mutex
	done    bool
	files   int
	skipped int
}

// LastScan reports what the latest scan found: the files indexed and the entries
// it couldn't read. ok is false before the first scan finishes.
func LastScan() (files, skipped int, ok bool) {
	lastScan.mu.Lock()
	defer lastScan.mu.Unlock()
	return lastScan.files, lastScan.skipped, lastScan.done
}

// ScanOnce walks the data root once and syncs any diff to the API. It returns
// the sync error (nil on success/"no changes"/skip such as a missing data
// root); the caller feeds that to the heartbeat. Config is reloaded each tick
// so `configure` edits apply without a restart.
func ScanOnce(ctx context.Context, cfg config.Config, client *http.Client, url string) error {
	lock.Lock()
	defer lock.Unlock()

	r, err := scan(ctx, cfg, nil)
	if err != nil {
		logx.Warn("watch", "%v; not syncing this tick", err)
		return nil
	}
	lastScan.mu.Lock()
	lastScan.done, lastScan.files, lastScan.skipped = true, len(r.snapshot), r.skipped
	lastScan.mu.Unlock()
	if r.skip != "" {
		// Surface a skip on first occurrence and whenever the reason changes
		// (usually a misconfiguration to fix); stay quiet on identical repeats.
		syncLog.mu.Lock()
		changed := r.skip != syncLog.lastSkip
		syncLog.lastSkip = r.skip
		syncLog.caughtAny = false
		syncLog.mu.Unlock()
		if changed {
			logx.Warn("watch", "not syncing: %s", r.skip)
		} else {
			logx.Debug("watch", "still not syncing: %s", r.skip)
		}
		return nil
	}
	// Reached a non-skip path: forget the last skip so a future one is loud.
	syncLog.mu.Lock()
	syncLog.lastSkip = ""
	syncLog.mu.Unlock()

	if !r.hadIndex {
		// First run: send the full inventory once for a server baseline; later
		// ticks are incremental diffs.
		all := make([]fileItem, 0, len(r.snapshot))
		for rel, e := range r.snapshot {
			if e.FileID != "" {
				all = append(all, itemFor(rel, e))
			}
		}
		sort.Slice(all, func(i, j int) bool { return all[i].Path < all[j].Path })
		if r.skipped > 0 {
			logx.Warn("watch", "first run: full sync of %d file(s); %d unreadable & skipped (scan %s)", len(all), r.skipped, r.scanTook)
		} else {
			logx.Info("watch", "first run: full sync of %d file(s) (scan took %s)", len(all), r.scanTook)
		}
		attrs, err := postSync(ctx, client, url, httpx.Auth{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret}, syncBody{Mode: "full", Files: all})
		if err != nil {
			return err
		}
		applyAttributions(r.snapshot, attrs)
		persist(r.snapshot)
		resetCaughtUp()
		return nil
	}

	changed := append(append([]string{}, r.added...), r.modified...)
	if len(changed) == 0 && len(r.deleted) == 0 {
		logCaughtUp(len(r.snapshot), r.skipped, r.scanTook)
		// Caught up. Persist anyway so hidden-file config changes or
		// refreshed mtimes land on disk.
		persist(r.snapshot)
		return nil
	}

	files := make([]fileItem, 0, len(changed))
	for _, rel := range changed {
		// The API keys files by their OS identity; one without it waits for a
		// scan that can read it.
		if e := r.snapshot[rel]; e.FileID != "" {
			files = append(files, itemFor(rel, e))
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	logx.Info("watch", "incremental sync: %d changed, %d deleted (scan took %s)", len(files), len(r.deleted), r.scanTook)
	attrs, err := postSync(ctx, client, url, httpx.Auth{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret}, syncBody{Mode: "incremental", Files: files, Deleted: r.deleted})
	if err != nil {
		return err
	}
	applyAttributions(r.snapshot, attrs)
	persist(r.snapshot)
	resetCaughtUp()
	return nil
}

func itemFor(rel string, e entry) fileItem {
	return fileItem{
		Path:          rel,
		Name:          filepath.Base(rel),
		Size:          e.Size,
		Filetype:      e.MIME,
		DeviceFileIno: e.FileID,
		ModifiedAt:    time.Unix(e.ModUnix, 0).UTC().Format(time.RFC3339),
	}
}

// postSync sends one sync request and returns the server's per-file
// attributions. It errors only on request failure or a non-2xx rejection; the
// caller persists the index (folding in attributions) only on success. A 2xx
// with no/undecodable body yields "no attributions" — non-fatal, since the API
// may return any 2xx shape.
func postSync(ctx context.Context, client *http.Client, url string, auth httpx.Auth, body syncBody) ([]SyncAttribution, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		err = fmt.Errorf("marshal sync body failed: %v", err)
		logx.Warn("watch", "%s", err)
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(buf))
	if err != nil {
		err = fmt.Errorf("request build failed: %v", err)
		logx.Warn("watch", "%s", err)
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	httpx.SetAuth(req, auth)

	start := time.Now()
	res, err := client.Do(req)
	took := time.Since(start).Round(time.Millisecond)
	if err != nil {
		err = fmt.Errorf("sync send failed: %v", err)
		logx.Warn("watch", "%s (took %s)", err, took)
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		var r syncResp
		_ = json.NewDecoder(res.Body).Decode(&r) // tolerant — missing/garbage body just yields zero attributions
		logx.Ok("watch", "sync ok (mode=%s, status=%d, took %s, attributions=%d)", body.Mode, res.StatusCode, took, len(r.Files))
		return r.Files, nil
	}
	detail := strings.TrimSpace(httpx.ErrorSnippet(res, 512))
	logx.Warn("watch", "sync rejected (status=%d, took %s): %s", res.StatusCode, took, detail)
	return nil, fmt.Errorf("sync rejected: HTTP %d %s", res.StatusCode, detail)
}

// applyAttributions folds the API's per-path file-ids into a snapshot (or any
// path→entry map) so the next index.txt write carries the apiFileId column.
func applyAttributions(snapshot map[string]entry, attrs []SyncAttribution) {
	if len(attrs) == 0 || len(snapshot) == 0 {
		return
	}
	for _, a := range attrs {
		if a.FileID == "" {
			continue
		}
		if e, ok := snapshot[a.Path]; ok && e.APIFileID != a.FileID {
			e.APIFileID = a.FileID
			snapshot[a.Path] = e
		}
	}
}
