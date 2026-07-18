// Package files implements the bridge's native file endpoints (Upload, Get,
// Delete, Move) that replace dufs. Each authorises against the upstream
// ViaVeritas API (the source of truth; the bridge is unauthenticated here) and
// returns a common JSON metadata block (path, name, size, device file-id, API
// row id).
//
//	POST   /files                                    multipart: metadata + file — Upload
//	GET    /files?path&expires&userId&sig            HMAC-signed URL from the API — Get
//	DELETE /files?path[&organizationId]              Authorization forwarded to API — Delete
//	PATCH  /files?path[&organizationId]              Authorization forwarded to API — Move
//
// Path safety: every input goes through resolveUnderRoot, which rejects any
// path that would escape the data root — stricter than legacy /confirm's
// filepath.Join.
package files

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"bridge/internal/config"
	"bridge/internal/httpx"
	"bridge/internal/logx"
	"bridge/internal/platform"
	"bridge/internal/uploadgate"
	"bridge/internal/utils"
	"bridge/internal/watch"
)

// fileResponse is the common JSON envelope non-Get endpoints return; Get
// echoes the same fields as X-Bridge-* headers.
type fileResponse struct {
	OK            bool   `json:"ok"`
	Path          string `json:"path"`
	Name          string `json:"name"`
	Size          int64  `json:"size"`
	Filetype      string `json:"filetype,omitempty"`
	DeviceFileIno string `json:"deviceFileIno,omitempty"`
	ModifiedAt    string `json:"modifiedAt,omitempty"`
	FileID        string `json:"fileId,omitempty"`
}

// uploadMetadata is the JSON the CRM places in the multipart `metadata` part.
type uploadMetadata struct {
	Size           int64                `json:"size"`
	Name           string               `json:"name"`
	Path           string               `json:"path"`
	Description    string               `json:"description"`
	OrganizationID string               `json:"organization_id"`
	Resources      []watch.FileRelation `json:"resources,omitempty"`
}

// Max buffered size of the JSON `metadata` part; the file itself streams via
// the second part.
const maxMetadataBytes = 64 * 1024

// freeBytesHeadroom is the min free disk required after an upload; reject
// (507) if metadata.size would leave less.
const freeBytesHeadroom = 64 * 1024 * 1024

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// HandleUpload handles POST /files (multipart `metadata` + `file`). It
// subsumes the old /prepare → write → /confirm dance: parse metadata, validate
// with the API (forwarded Authorization), stream the file to a temp file,
// atomically rename into place, then watch.ConfirmUpload syncs it and returns
// the API-assigned fileId.
func HandleUpload(w http.ResponseWriter, r *http.Request, cfg config.Config, client *http.Client) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !requireAuthHeader(w, r) {
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "expected multipart/form-data: "+err.Error())
		return
	}

	// Streaming multipart: read named parts in any order; require exactly one
	// metadata and one file.
	var meta uploadMetadata
	gotMeta := false
	var filePart io.ReadCloser
	defer func() {
		if filePart != nil {
			_ = filePart.Close()
		}
	}()
	for {
		p, perr := mr.NextPart()
		if perr == io.EOF {
			break
		}
		if perr != nil {
			writeJSONError(w, http.StatusBadRequest, "read multipart: "+perr.Error())
			return
		}
		switch p.FormName() {
		case "metadata":
			buf := make([]byte, maxMetadataBytes+1)
			n, _ := io.ReadFull(io.LimitReader(p, int64(len(buf))), buf)
			p.Close()
			if n > maxMetadataBytes {
				writeJSONError(w, http.StatusRequestEntityTooLarge, "metadata part exceeds 64 KiB")
				return
			}
			if jerr := json.Unmarshal(buf[:n], &meta); jerr != nil {
				writeJSONError(w, http.StatusBadRequest, "metadata is not valid JSON: "+jerr.Error())
				return
			}
			gotMeta = true
		case "file":
			// Keep open to stream below. NextPart on an unconsumed part
			// errors, so `file` must be the last part; we break out here.
			filePart = p
		default:
			p.Close()
		}
		if filePart != nil {
			break // any further parts ignored — file must be last
		}
	}
	if !gotMeta {
		writeJSONError(w, http.StatusBadRequest, "missing `metadata` part")
		return
	}
	if filePart == nil {
		writeJSONError(w, http.StatusBadRequest, "missing `file` part")
		return
	}
	if err := validateUploadMetadata(meta); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	c := liveConfig(cfg)
	root := strings.TrimRight(c.DataDir(), string(os.PathSeparator))
	if root == "" {
		writeJSONError(w, http.StatusServiceUnavailable, "no data root configured")
		return
	}

	// Reject early if we know we won't have room.
	if free := platform.Current().DiskFreeBytes(root); free > 0 && meta.Size > 0 && free < meta.Size+freeBytesHeadroom {
		writeJSONError(w, http.StatusInsufficientStorage, fmt.Sprintf("not enough free disk (%d bytes free, need %d + %d headroom)", free, meta.Size, freeBytesHeadroom))
		return
	}

	destAbs, perr := resolveUnderRoot(root, meta.Path)
	if perr != nil {
		writeJSONError(w, http.StatusBadRequest, perr.Error())
		return
	}

	// Validate with the API (mode:"validate" on /bridges/sync). The file isn't
	// on disk yet, so a synthetic "pending:<hex>" inode is used — validate mode
	// returns before any DB write, so it's harmless and still satisfies the DTO.
	if status, body, ok := watch.Validate(r.Context(), c, client, r.Header.Get("Authorization"), []watch.ValidateItem{{
		Path:        meta.Path,
		Name:        meta.Name,
		Size:        meta.Size,
		ContentType: utils.MIMEByExt(meta.Path),
		Inode:       "pending:" + randomHex(8),
		Resources:   meta.Resources,
	}}); !ok {
		proxyAPIError(w, status, body)
		return
	}

	// Stream file part → <root>/.tmp/<random>.part.
	tmpDir := filepath.Join(root, ".tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "prepare temp dir: "+err.Error())
		return
	}
	tmpName := filepath.Join(tmpDir, randomHex(16)+".part")
	tmpFile, err := os.OpenFile(tmpName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "open temp file: "+err.Error())
		return
	}
	cleanup := func() {
		tmpFile.Close()
		_ = os.Remove(tmpName)
	}

	// Take a sync lock so the periodic scan doesn't race the in-flight upload.
	d := uploadgate.LockDuration(meta.Size)
	uploadgate.Add(&uploadgate.Prepared{
		Name:           meta.Name,
		Path:           meta.Path,
		Description:    meta.Description,
		OrganizationID: meta.OrganizationID,
		Resources:      meta.Resources,
		Size:           meta.Size,
		Expiry:         time.Now().Add(d),
	})
	releaseGate := func() { _, _ = uploadgate.Take(meta.Path) }

	// LimitReader so the client can't push more bytes than it declared.
	var lr io.Reader = filePart
	if meta.Size > 0 {
		lr = io.LimitReader(filePart, meta.Size)
	}
	n, copyErr := io.Copy(tmpFile, lr)
	syncErr := tmpFile.Sync()
	tmpFile.Close()
	if copyErr != nil || syncErr != nil {
		cleanup()
		releaseGate()
		writeJSONError(w, http.StatusBadGateway, fmt.Sprintf("stream upload: copy=%v sync=%v wrote=%d", copyErr, syncErr, n))
		return
	}
	if meta.Size > 0 && n != meta.Size {
		cleanup()
		releaseGate()
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("upload truncated: declared size=%d, received=%d", meta.Size, n))
		return
	}

	// mkdir -p destination, atomic rename.
	if err := os.MkdirAll(filepath.Dir(destAbs), 0o755); err != nil {
		_ = os.Remove(tmpName)
		releaseGate()
		writeJSONError(w, http.StatusInternalServerError, "prepare destination dir: "+err.Error())
		return
	}
	if err := os.Rename(tmpName, destAbs); err != nil {
		_ = os.Remove(tmpName)
		releaseGate()
		writeJSONError(w, http.StatusInternalServerError, "finalise upload: "+err.Error())
		return
	}

	// Sync to API; release the gate regardless of ConfirmUpload's outcome — the
	// bytes are on disk (matches /confirm, which releases even on sync failure).
	releaseGate()
	conf, err := watch.ConfirmUpload(r.Context(), c, client, meta.Path, meta.Resources, r.Header.Get("Authorization"))
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "sync after upload failed: "+err.Error())
		return
	}
	logx.Ok("files", "uploaded path=%s size=%d apiFileId=%s", conf.Path, conf.Size, conf.APIFileID)
	writeJSON(w, http.StatusOK, fileResponse{
		OK:            true,
		Path:          conf.Path,
		Name:          conf.Name,
		Size:          conf.Size,
		Filetype:      conf.Filetype,
		DeviceFileIno: conf.DeviceFileIno,
		ModifiedAt:    conf.ModifiedAt,
		FileID:        conf.APIFileID,
	})
}

// HandleGet streams ?path=… after validating the API-signed HMAC (path|expires,
// keyed by the bridge's auth token), exposing index metadata as X-Bridge-*
// headers so the CRM gets the file-id without a second call.
func HandleGet(w http.ResponseWriter, r *http.Request, cfg config.Config) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	rel := q.Get("path")
	expires := q.Get("expires")
	sig := q.Get("sig")
	userID := q.Get("userId")
	if rel == "" || expires == "" || sig == "" || userID == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing signed-URL parameters (path, expires, userId, sig)")
		return
	}
	c := liveConfig(cfg)
	if c.ClientSecret == "" {
		writeJSONError(w, http.StatusServiceUnavailable, "bridge unpaired; no signing key available")
		return
	}
	if err := httpx.VerifyHMAC(rel, expires, sig, userID, c.ClientSecret); err != nil {
		if errors.Is(err, httpx.ErrSigExpired) {
			writeJSONError(w, http.StatusGone, "signed URL expired")
			return
		}
		writeJSONError(w, http.StatusUnauthorized, "signed URL invalid")
		return
	}

	root := strings.TrimRight(c.DataDir(), string(os.PathSeparator))
	if root == "" {
		writeJSONError(w, http.StatusServiceUnavailable, "no data root configured")
		return
	}
	abs, perr := resolveUnderRoot(root, rel)
	if perr != nil {
		writeJSONError(w, http.StatusBadRequest, perr.Error())
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSONError(w, http.StatusNotFound, "no such file")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "open: "+err.Error())
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "stat: "+err.Error())
		return
	}
	if info.IsDir() {
		writeJSONError(w, http.StatusBadRequest, "path is a directory")
		return
	}
	relCanon := utils.ToRootRel(root, abs)
	mime := utils.MIMEByExt(relCanon)
	deviceFileIno, _, apiFileID, _, _, _ := watch.LookupIndex(relCanon)

	h := w.Header()
	h.Set("Content-Type", mime)
	h.Set("X-Bridge-Path", relCanon)
	h.Set("X-Bridge-Name", filepath.Base(relCanon))
	h.Set("X-Bridge-Filetype", mime)
	if deviceFileIno != "" {
		h.Set("X-Bridge-Device-File-Ino", deviceFileIno)
	}
	if apiFileID != "" {
		h.Set("X-Bridge-File-Id", apiFileID)
	}
	// http.ServeContent handles Range/If-Modified-Since/Content-Length for us.
	http.ServeContent(w, r, filepath.Base(relCanon), info.ModTime(), f)
}

// HandleDelete handles DELETE /files?path=…
func HandleDelete(w http.ResponseWriter, r *http.Request, cfg config.Config, client *http.Client) {
	if r.Method != http.MethodDelete {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !requireAuthHeader(w, r) {
		return
	}
	q := r.URL.Query()
	rel := q.Get("path")
	if rel == "" {
		writeJSONError(w, http.StatusBadRequest, "path is required")
		return
	}
	c := liveConfig(cfg)
	root := strings.TrimRight(c.DataDir(), string(os.PathSeparator))
	if root == "" {
		writeJSONError(w, http.StatusServiceUnavailable, "no data root configured")
		return
	}
	abs, perr := resolveUnderRoot(root, rel)
	if perr != nil {
		writeJSONError(w, http.StatusBadRequest, perr.Error())
		return
	}
	relCanon := utils.ToRootRel(root, abs)

	// Use what we know: index for fileId/deviceFileIno/mime, on-disk stat for
	// size/mtime. Looked up before validate so we pass the real inode.
	deviceFileIno, mime, apiFileID, idxSize, idxMod, hadIdx := watch.LookupIndex(relCanon)
	var size int64
	var modAt string
	if info, ierr := os.Stat(abs); ierr == nil && !info.IsDir() {
		size = info.Size()
		modAt = info.ModTime().UTC().Format(time.RFC3339)
		if mime == "" {
			mime = utils.MIMEByExt(relCanon)
		}
	} else if hadIdx {
		size = idxSize
		modAt = time.Unix(idxMod, 0).UTC().Format(time.RFC3339)
	}
	if mime == "" {
		mime = utils.MIMEByExt(relCanon)
	}

	validateInode := deviceFileIno
	if validateInode == "" {
		// Never made it into the index (upload + immediate delete); a placeholder
		// satisfies the API's required-inode validator (validate doesn't persist).
		validateInode = "pending:" + randomHex(8)
	}
	if status, body, ok := watch.Validate(r.Context(), c, client, r.Header.Get("Authorization"), []watch.ValidateItem{{
		Path:        relCanon,
		Name:        filepath.Base(relCanon),
		Size:        size,
		ContentType: mime,
		Inode:       validateInode,
	}}); !ok {
		proxyAPIError(w, status, body)
		return
	}

	if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
		writeJSONError(w, http.StatusInternalServerError, "remove: "+err.Error())
		return
	}

	if deviceFileIno != "" {
		if err := watch.ConfirmDelete(r.Context(), c, client, relCanon, deviceFileIno, r.Header.Get("Authorization")); err != nil {
			// Bytes already gone; log and return success — next scan reconciles.
			logx.Warn("files", "removed %s but API sync failed: %v", relCanon, err)
		}
	} else {
		logx.Warn("files", "removed %s with no recorded device_file_ino; API will reconcile on next scan", relCanon)
	}

	writeJSON(w, http.StatusOK, fileResponse{
		OK:            true,
		Path:          relCanon,
		Name:          filepath.Base(relCanon),
		Size:          size,
		Filetype:      mime,
		DeviceFileIno: deviceFileIno,
		ModifiedAt:    modAt,
		FileID:        apiFileID,
	})
}

// HandleMove handles PATCH /files?path=… body {"toPath":"/new/path.pdf"}
func HandleMove(w http.ResponseWriter, r *http.Request, cfg config.Config, client *http.Client) {
	if r.Method != http.MethodPatch {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !requireAuthHeader(w, r) {
		return
	}
	q := r.URL.Query()
	fromRel := q.Get("path")
	if fromRel == "" {
		writeJSONError(w, http.StatusBadRequest, "path is required")
		return
	}
	var body struct {
		ToPath string `json:"toPath"`
	}
	defer r.Body.Close()
	if err := json.NewDecoder(io.LimitReader(r.Body, 8*1024)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if body.ToPath == "" {
		writeJSONError(w, http.StatusBadRequest, "toPath is required")
		return
	}

	c := liveConfig(cfg)
	root := strings.TrimRight(c.DataDir(), string(os.PathSeparator))
	if root == "" {
		writeJSONError(w, http.StatusServiceUnavailable, "no data root configured")
		return
	}
	fromAbs, perr := resolveUnderRoot(root, fromRel)
	if perr != nil {
		writeJSONError(w, http.StatusBadRequest, "path: "+perr.Error())
		return
	}
	toAbs, perr := resolveUnderRoot(root, body.ToPath)
	if perr != nil {
		writeJSONError(w, http.StatusBadRequest, "toPath: "+perr.Error())
		return
	}
	fromCanon := utils.ToRootRel(root, fromAbs)
	toCanon := utils.ToRootRel(root, toAbs)
	if fromCanon == toCanon {
		writeJSONError(w, http.StatusBadRequest, "path and toPath are identical after canonicalisation")
		return
	}

	// Source inode/mime/size from the index for the validate call; placeholder
	// inode if it never made it in (validate mode doesn't persist).
	fromInode, fromMime, _, fromSize, _, _ := watch.LookupIndex(fromCanon)
	if fromMime == "" {
		fromMime = utils.MIMEByExt(fromCanon)
	}
	validateInode := fromInode
	if validateInode == "" {
		validateInode = "pending:" + randomHex(8)
	}
	if status, bb, ok := watch.Validate(r.Context(), c, client, r.Header.Get("Authorization"), []watch.ValidateItem{{
		Path:        fromCanon,
		Name:        filepath.Base(fromCanon),
		Size:        fromSize,
		ContentType: fromMime,
		Inode:       validateInode,
	}}); !ok {
		proxyAPIError(w, status, bb)
		return
	}

	if err := os.MkdirAll(filepath.Dir(toAbs), 0o755); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "prepare destination dir: "+err.Error())
		return
	}
	if err := os.Rename(fromAbs, toAbs); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "rename: "+err.Error())
		return
	}
	conf, err := watch.ConfirmMove(r.Context(), c, client, fromCanon, toCanon, r.Header.Get("Authorization"))
	if err != nil {
		// File is renamed but the API doesn't know yet — next scan reconciles.
		logx.Warn("files", "moved %s -> %s but API sync failed: %v", fromCanon, toCanon, err)
		writeJSONError(w, http.StatusBadGateway, "rename succeeded but API sync failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, fileResponse{
		OK:            true,
		Path:          conf.Path,
		Name:          conf.Name,
		Size:          conf.Size,
		Filetype:      conf.Filetype,
		DeviceFileIno: conf.DeviceFileIno,
		ModifiedAt:    conf.ModifiedAt,
		FileID:        conf.APIFileID,
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// resolveUnderRoot joins root + rel and verifies the result stays under root
// via filepath.Rel. Rejects "..", drive-letter prefixes like `C:\…` and
// backslashes (rejected on any host — wire format is forward-slash only), and
// anything that would Rel-walk outside the root. Stricter than legacy
// /confirm's filepath.Join.
func resolveUnderRoot(root, rel string) (string, error) {
	if rel == "" {
		return "", errors.New("path is required")
	}
	// Backslashes are never legitimate; reject before FromSlash so it's
	// OS-independent (filepath.IsAbs on Unix returns false for "C:\\…", which
	// would otherwise sneak through).
	if strings.ContainsAny(rel, `\`) {
		return "", fmt.Errorf("path %q contains backslash; use forward slashes", rel)
	}
	if looksLikeWindowsAbsolute(rel) {
		return "", fmt.Errorf("path %q must be relative to the data root", rel)
	}
	root = strings.TrimRight(root, string(os.PathSeparator))
	clean := filepath.FromSlash(strings.TrimPrefix(rel, "/"))
	if filepath.IsAbs(clean) {
		return "", fmt.Errorf("path %q must be relative to the data root", rel)
	}
	abs := filepath.Join(root, clean)
	rl, err := filepath.Rel(root, abs)
	if err != nil || rl == ".." || strings.HasPrefix(rl, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes data root", rel)
	}
	if err := symlinkContained(root, abs); err != nil {
		return "", fmt.Errorf("path %q escapes data root", rel)
	}
	return abs, nil
}

// symlinkContained re-checks abs stays inside root after resolving symlinks —
// the lexical check only stops textual "..", not a symlink pointing outside.
// Both root and target are resolved (so a symlinked data root still works);
// for a not-yet-existing target the parent is resolved instead, still catching
// an escaping intermediate symlink.
func symlinkContained(root, abs string) error {
	rootEval, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil // root unresolved; the operation will fail on access anyway
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		parent, perr := filepath.EvalSymlinks(filepath.Dir(abs))
		if perr != nil {
			return nil // parent missing too; access fails downstream
		}
		resolved = filepath.Join(parent, filepath.Base(abs))
	}
	rl, err := filepath.Rel(rootEval, resolved)
	if err != nil || rl == ".." || strings.HasPrefix(rl, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("symlink escapes data root")
	}
	return nil
}

// looksLikeWindowsAbsolute reports whether s starts with a Windows drive
// designator (`C:`). Checked independently of host OS so a Windows CRM bug
// can't smuggle an outside-of-root path through a Linux bridge.
func looksLikeWindowsAbsolute(s string) bool {
	if len(s) >= 2 && s[1] == ':' {
		c := s[0]
		return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
	}
	return false
}

// validateUploadMetadata enforces the bridge's pre-API field rules: size and
// path mandatory. Resources are optional; the API authorizes them.
func validateUploadMetadata(m uploadMetadata) error {
	if m.Size <= 0 {
		return errors.New("metadata.size must be a positive integer (bytes)")
	}
	if m.Path == "" {
		return errors.New("metadata.path is required")
	}
	return nil
}

func proxyAPIError(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	if !json.Valid(body) {
		body, _ = json.Marshal(map[string]string{"error": strings.TrimSpace(string(body))})
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// requireAuthHeader fails closed on a mutation with no Authorization. The
// bridge forwards this header to the API (the access authority); without it,
// the API would never authorize the mutation on the bridge's identity alone.
// GET is exempt — it authenticates via an HMAC-signed URL.
func requireAuthHeader(w http.ResponseWriter, r *http.Request) bool {
	if strings.TrimSpace(r.Header.Get("Authorization")) == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing Authorization")
		return false
	}
	return true
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand shouldn't fail; if it does, fall back to a timestamp for
		// a unique-enough name.
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// liveConfig reloads config so a `configure`/`pair` edit applies without a
// restart, falling back to the caller's snapshot on error.
func liveConfig(fallback config.Config) config.Config {
	return config.LoadOr(fallback)
}
