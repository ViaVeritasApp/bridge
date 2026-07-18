package setupui

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"bridge/internal/platform"
)

func (s *srv) handleBrowse(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if strings.TrimSpace(p) == "" {
		p = suggestedRoot()
	}
	// No path: present drive letters as the "root". Windows-only in practice —
	// on macOS/Linux suggestedRoot falls back to "/", so p is never "".
	if p == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"path": "", "writable": false,
			"crumbs": []platform.Crumb{{Label: "This PC", Path: ""}},
			"dirs":   platform.Current().DriveLetters(),
		})
		return
	}

	abs, err := filepath.Abs(p)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad path"})
		return
	}
	abs = filepath.Clean(abs)
	entries, err := os.ReadDir(abs)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "can't open this folder (" + err.Error() + ")"})
		return
	}

	dirs := make([]platform.DirEntry, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") { // hide dotfolders from non-technical users
			continue
		}
		dirs = append(dirs, platform.DirEntry{Name: name, Path: filepath.Join(abs, name)})
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name)
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"path":     abs,
		"writable": dirWritable(abs),
		"crumbs":   platform.Current().Breadcrumbs(abs),
		"dirs":     dirs,
	})
}

// dirWritable probes by creating and removing a temp entry. We run elevated so
// this is usually true; a false means a genuinely read-only mount.
func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".bridge-wtest-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	_ = os.Remove(name)
	return true
}
