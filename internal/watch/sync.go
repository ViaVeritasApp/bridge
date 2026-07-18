package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"bridge/internal/config"
	"bridge/internal/httpx"
	"bridge/internal/logx"
	"bridge/internal/platform"
	"bridge/internal/utils"
)

// FileRelation mirrors the API's FileRelationDto: one (resource_type,
// resource_id) pair so the API can authorize READ access before accepting the sync.
type FileRelation struct {
	ResourceType string `json:"resource_type,omitempty"`
	ResourceID   string `json:"resource_id,omitempty"`
}

// Wire formats matching the API's SyncDto / SyncFileItemDto.
type fileItem struct {
	Path          string         `json:"path"`
	Name          string         `json:"name"`
	Size          int64          `json:"size"`
	Filetype      string         `json:"content_type,omitempty"`
	DeviceFileIno string         `json:"inode"` // OS file identity "<dev>:<ino>"
	ModifiedAt    string         `json:"modified_at,omitempty"`
	Resources     []FileRelation `json:"resources,omitempty"`
}

type syncBody struct {
	Mode  string     `json:"mode"`
	Files []fileItem `json:"files"`
	// Deleted holds each removed file's OS file-ID ("<dev>:<ino>"), not its
	// path, so the server matches deletions by identity.
	Deleted []string `json:"deleted,omitempty"`
}

// SyncAttribution is one entry in the API's sync response: the row id assigned
// for a file, keyed by path. The bridge folds these into index.txt so Get/
// Delete/Move can return the API file-id without re-querying.
type SyncAttribution struct {
	Path          string `json:"path"`
	DeviceFileIno string `json:"deviceFileIno,omitempty"`
	FileID        string `json:"fileId"`
}

type syncResp struct {
	OK    bool              `json:"ok"`
	Files []SyncAttribution `json:"files,omitempty"`
}

// ScanOnce walks the data root once and syncs any diff to the API. It returns
// the sync error (nil on success/"no changes"/skip such as a missing data
// root); the caller feeds that to the heartbeat. Config is reloaded each tick
// so `configure` edits apply without a restart.
func ScanOnce(ctx context.Context, cfg config.Config, client *http.Client, url string) error {
	r, err := scan(ctx, cfg, nil)
	if err != nil {
		logx.Warn("watch", "%v; not syncing this tick", err)
		return nil
	}
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
			all = append(all, itemFor(rel, e))
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
		files = append(files, itemFor(rel, r.snapshot[rel]))
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

// ConfirmedUpload describes a just-uploaded file after API acknowledgement.
// ConfirmUpload returns it so the CRM sees the API-assigned fileId in the same
// response.
type ConfirmedUpload struct {
	Path          string // root-relative, as it lives in the index
	Name          string
	Size          int64
	Filetype      string
	DeviceFileIno string
	ModifiedAt    string
	APIFileID     string // empty if the API ack didn't include attribution
}

// ConfirmUpload syncs a single just-uploaded file plus its CRM resource
// relations, then records it in index.txt so the next scan treats it as
// unchanged (never re-creating it without those relations). relPath is the
// root-relative path; size and inode come from stat-ing it. userAuth, when
// non-empty, is forwarded verbatim as the Authorization header; the bridge
// always identifies via X-Client-Id + X-Client-Secret and the API derives the
// org from that identity. The returned ConfirmedUpload carries the metadata
// echoed to the CRM, including the API-assigned APIFileID.
func ConfirmUpload(ctx context.Context, cfg config.Config, client *http.Client, relPath string, resources []FileRelation, userAuth string) (ConfirmedUpload, error) {
	root := strings.TrimRight(cfg.DataDir(), string(os.PathSeparator))
	if root == "" {
		return ConfirmedUpload{}, fmt.Errorf("no data root configured")
	}
	abs := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(relPath, "/")))
	info, err := os.Stat(abs)
	if err != nil {
		return ConfirmedUpload{}, fmt.Errorf("stat uploaded file %s: %w", relPath, err)
	}
	if info.IsDir() {
		return ConfirmedUpload{}, fmt.Errorf("%s is a directory, not a file", relPath)
	}

	rel := utils.ToRootRel(root, abs)
	fid, _ := platform.Current().ComputeFileID(abs, info)
	mime := utils.MIMEByExt(rel)
	modAt := info.ModTime().UTC().Format(time.RFC3339)
	item := fileItem{
		Path:          rel,
		Name:          filepath.Base(rel),
		Size:          info.Size(),
		Filetype:      mime,
		DeviceFileIno: fid,
		ModifiedAt:    modAt,
		Resources:     resources,
	}

	url := httpx.NormalizeURL(cfg.APIBaseURL, "/bridges/sync")
	attrs, err := postSync(ctx, client, url, httpx.Auth{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, UserAuth: userAuth}, syncBody{Mode: "incremental", Files: []fileItem{item}})
	if err != nil {
		return ConfirmedUpload{}, err
	}
	apiFileID := ""
	for _, a := range attrs {
		if a.Path == rel {
			apiFileID = a.FileID
			break
		}
	}

	// Fold the confirmed file (with apiFileId) into the index so the scan sees
	// it as unchanged (size+mtime) not a new create, and Get/Delete/Move can
	// read the apiFileId back without re-querying.
	idx, _ := loadIndex()
	if idx.Files == nil {
		idx.Files = map[string]entry{}
	}
	idx.Files[rel] = entry{Size: info.Size(), ModUnix: info.ModTime().Unix(), FileID: fid, MIME: mime, APIFileID: apiFileID}
	if serr := saveIndex(idx); serr != nil {
		logx.Warn("watch", "confirmed %s synced but index update failed: %v", rel, serr)
	}
	logx.Ok("watch", "confirmed upload synced: %s (size=%d apiFileId=%s)", rel, info.Size(), apiFileID)
	return ConfirmedUpload{
		Path:          rel,
		Name:          filepath.Base(rel),
		Size:          info.Size(),
		Filetype:      mime,
		DeviceFileIno: fid,
		ModifiedAt:    modAt,
		APIFileID:     apiFileID,
	}, nil
}

// LookupIndex returns the index record for relPath for echoing in Get/Delete/
// Move without re-stat-ing or recomputing the file-id. ok=false (empty record)
// when relPath isn't indexed yet (e.g. an upload the scan hasn't observed).
func LookupIndex(relPath string) (deviceFileIno, mime, apiFileID string, size int64, modUnix int64, ok bool) {
	idx, _ := loadIndex()
	e, found := idx.Files[relPath]
	if !found {
		return "", "", "", 0, 0, false
	}
	return e.FileID, e.MIME, e.APIFileID, e.Size, e.ModUnix, true
}

// ConfirmDelete posts a single-file deletion (mode:"incremental" with the
// inode in deleted[]) and drops the row from index.txt. userAuth is forwarded
// like ConfirmUpload. deviceFileIno is the API's match key — pass the value
// already in the index for relPath.
func ConfirmDelete(ctx context.Context, cfg config.Config, client *http.Client, relPath, deviceFileIno, userAuth string) error {
	if deviceFileIno == "" {
		return fmt.Errorf("ConfirmDelete: deviceFileIno is required (API matches deletions by inode, not path)")
	}
	url := httpx.NormalizeURL(cfg.APIBaseURL, "/bridges/sync")
	if _, err := postSync(ctx, client, url, httpx.Auth{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, UserAuth: userAuth}, syncBody{Mode: "incremental", Deleted: []string{deviceFileIno}}); err != nil {
		return err
	}
	idx, _ := loadIndex()
	if idx.Files != nil {
		delete(idx.Files, relPath)
		if serr := saveIndex(idx); serr != nil {
			logx.Warn("watch", "deleted %s synced but index update failed: %v", relPath, serr)
		}
	}
	logx.Ok("watch", "confirmed delete synced: %s (deviceFileIno=%s)", relPath, deviceFileIno)
	return nil
}

// ConfirmMove syncs a single rename and updates index.txt: the new path
// appears in files[] with the same inode, which is how the API detects a move
// (the row keeps its id and apiFileId). The caller already did the on-disk
// os.Rename; this only stats the destination and posts the diff.
func ConfirmMove(ctx context.Context, cfg config.Config, client *http.Client, fromRel, toRel, userAuth string) (ConfirmedUpload, error) {
	root := strings.TrimRight(cfg.DataDir(), string(os.PathSeparator))
	if root == "" {
		return ConfirmedUpload{}, fmt.Errorf("no data root configured")
	}
	toAbs := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(toRel, "/")))
	info, err := os.Stat(toAbs)
	if err != nil {
		return ConfirmedUpload{}, fmt.Errorf("stat moved file %s: %w", toRel, err)
	}
	if info.IsDir() {
		return ConfirmedUpload{}, fmt.Errorf("%s is a directory, not a file", toRel)
	}
	to := utils.ToRootRel(root, toAbs)
	fid, _ := platform.Current().ComputeFileID(toAbs, info)
	mime := utils.MIMEByExt(to)
	modAt := info.ModTime().UTC().Format(time.RFC3339)

	// Preserve the old path's apiFileId so the local index can echo it without
	// waiting for the next sync. The API matches by inode alone now — the wire
	// no longer carries apiFileId.
	prevAPIFileID := ""
	if idx, _ := loadIndex(); idx.Files != nil {
		if e, ok := idx.Files[fromRel]; ok {
			prevAPIFileID = e.APIFileID
		}
	}
	item := fileItem{
		Path:          to,
		Name:          filepath.Base(to),
		Size:          info.Size(),
		Filetype:      mime,
		DeviceFileIno: fid,
		ModifiedAt:    modAt,
	}

	url := httpx.NormalizeURL(cfg.APIBaseURL, "/bridges/sync")
	attrs, err := postSync(ctx, client, url, httpx.Auth{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, UserAuth: userAuth}, syncBody{Mode: "incremental", Files: []fileItem{item}})
	if err != nil {
		return ConfirmedUpload{}, err
	}
	apiFileID := prevAPIFileID
	for _, a := range attrs {
		if a.Path == to && a.FileID != "" {
			apiFileID = a.FileID
			break
		}
	}

	idx, _ := loadIndex()
	if idx.Files == nil {
		idx.Files = map[string]entry{}
	}
	delete(idx.Files, fromRel)
	idx.Files[to] = entry{Size: info.Size(), ModUnix: info.ModTime().Unix(), FileID: fid, MIME: mime, APIFileID: apiFileID}
	if serr := saveIndex(idx); serr != nil {
		logx.Warn("watch", "moved %s -> %s synced but index update failed: %v", fromRel, to, serr)
	}
	logx.Ok("watch", "confirmed move synced: %s -> %s (apiFileId=%s)", fromRel, to, apiFileID)
	return ConfirmedUpload{
		Path:          to,
		Name:          filepath.Base(to),
		Size:          info.Size(),
		Filetype:      mime,
		DeviceFileIno: fid,
		ModifiedAt:    modAt,
		APIFileID:     apiFileID,
	}, nil
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

// ValidateItem is the exported input to Validate: one file to authorize,
// mirroring fileItem so other packages can build it without the internal wire
// format. Inode is required by the API; for pre-upload checks with no real
// inode, pass a synthetic non-empty value (e.g. "pending:<hex>") — validate
// mode returns before any DB write, so the placeholder is harmless.
type ValidateItem struct {
	Path        string
	Name        string
	Size        int64
	ContentType string
	Inode       string
	Resources   []FileRelation
}

// Validate posts mode:"validate" to /bridges/sync so the API runs resource-
// access + quota checks without persisting. It returns the proxy triple
// (status, body, ok) so the caller can surface the API's verbatim response on
// rejection, matching the old /authorize semantics.
func Validate(ctx context.Context, cfg config.Config, client *http.Client, userAuth string, items []ValidateItem) (int, []byte, bool) {
	files := make([]fileItem, 0, len(items))
	for _, it := range items {
		files = append(files, fileItem{
			Path:          it.Path,
			Name:          it.Name,
			Size:          it.Size,
			Filetype:      it.ContentType,
			DeviceFileIno: it.Inode,
			Resources:     it.Resources,
		})
	}
	buf, err := json.Marshal(syncBody{Mode: "validate", Files: files})
	if err != nil {
		return http.StatusInternalServerError, []byte(fmt.Sprintf(`{"error":"encode validate body: %s"}`, err.Error())), false
	}
	url := httpx.NormalizeURL(cfg.APIBaseURL, "/bridges/sync")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return http.StatusInternalServerError, []byte(fmt.Sprintf(`{"error":"build validate request: %s"}`, err.Error())), false
	}
	req.Header.Set("Content-Type", "application/json")
	httpx.SetAuth(req, httpx.Auth{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, UserAuth: userAuth})

	start := time.Now()
	res, err := client.Do(req)
	took := time.Since(start).Round(time.Millisecond)
	if err != nil {
		return http.StatusBadGateway, []byte(fmt.Sprintf(`{"error":"call /bridges/sync (validate): %s"}`, err.Error())), false
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 16*1024))
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		logx.Ok("watch", "validate ok (status=%d, took %s, files=%d)", res.StatusCode, took, len(files))
		return res.StatusCode, body, true
	}
	logx.Warn("watch", "validate rejected (status=%d, took %s): %s", res.StatusCode, took, strings.TrimSpace(string(body)))
	return res.StatusCode, body, false
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
