// Package files serves the bridge's files to the CRM over the local network and
// holds the path checks every change to the data root goes through.
//
//	GET /files?path&expires&user_id&sig[&download=1]   HMAC-signed URL from the API
//
// Changes (moves, renames, deletes) never come over HTTP: the API sends them on
// the bridge's socket, after checking the member's access (internal/commands).
//
// Path safety: every input goes through ResolveUnderRoot, which rejects any
// path that would escape the data root.
package files

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"bridge/internal/config"
	"bridge/internal/httpx"
	"bridge/internal/utils"
	"bridge/internal/watch"
)

// HandleGet streams ?path=… after validating the API-signed HMAC
// (path|expires|user_id, keyed by the bridge's signing key), exposing index
// metadata as X-Bridge-* headers. With download=1 the browser saves it under
// its name rather than showing it.
func HandleGet(w http.ResponseWriter, r *http.Request, cfg config.Config) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	rel := q.Get("path")
	expires := q.Get("expires")
	sig := q.Get("sig")
	userID := q.Get("user_id")
	if rel == "" || expires == "" || sig == "" || userID == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing signed-URL parameters (path, expires, user_id, sig)")
		return
	}
	c := config.LoadOr(cfg)
	if !c.Effective(config.FeatureFiles) {
		writeJSONError(w, http.StatusForbidden, "files are switched off for this bridge")
		return
	}
	if c.SigningKey == "" {
		writeJSONError(w, http.StatusServiceUnavailable, "no signing key yet; it arrives with the next heartbeat")
		return
	}
	if err := httpx.VerifyHMAC(rel, expires, sig, userID, c.SigningKey); err != nil {
		if errors.Is(err, httpx.ErrSigExpired) {
			writeJSONError(w, http.StatusGone, "signed URL expired")
			return
		}
		writeJSONError(w, http.StatusUnauthorized, "signed URL invalid")
		return
	}

	folder, root, abs, perr := Locate(c, rel)
	if errors.Is(perr, config.ErrUnknownFolder) {
		writeJSONError(w, http.StatusNotFound, "no such file")
		return
	}
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
	relCanon := "/" + folder.Name + utils.ToRootRel(root, abs)
	mimeType := utils.MIMEByExt(relCanon)
	deviceFileIno, _, apiFileID, _, _, _ := watch.LookupIndex(relCanon)

	h := w.Header()
	h.Set("Content-Type", mimeType)
	h.Set("X-Bridge-Path", relCanon)
	h.Set("X-Bridge-Name", filepath.Base(relCanon))
	h.Set("X-Bridge-Filetype", mimeType)
	if deviceFileIno != "" {
		h.Set("X-Bridge-Device-File-Ino", deviceFileIno)
	}
	if apiFileID != "" {
		h.Set("X-Bridge-File-Id", apiFileID)
	}
	if q.Get("download") == "1" {
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(relCanon)}))
	}
	// http.ServeContent handles Range/If-Modified-Since/Content-Length for us.
	http.ServeContent(w, r, filepath.Base(relCanon), info.ModTime(), f)
}

// ResolveUnderRoot joins root + rel and verifies the result stays under root
// via filepath.Rel. Rejects "..", drive-letter prefixes like `C:\…` and
// backslashes (rejected on any host — wire format is forward-slash only), and
// anything that would Rel-walk outside the root.
// Locate is the file on disk for a path as Files has it (`/Scans/2026/a.pdf`):
// its synced folder, that folder's root and the file's absolute path, which
// stays inside the root.
func Locate(cfg config.Config, p string) (config.Folder, string, string, error) {
	folder, inner, err := cfg.SplitPath(p)
	if err != nil {
		return folder, "", "", err
	}
	root := strings.TrimRight(filepath.Clean(folder.Path), string(os.PathSeparator))
	if root == "" {
		root = string(os.PathSeparator)
	}
	abs, err := ResolveUnderRoot(root, inner)
	return folder, root, abs, err
}

func ResolveUnderRoot(root, rel string) (string, error) {
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
