package setupui

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"bridge/internal/config"
	"bridge/internal/tlsx"
	"bridge/internal/utils"
	"bridge/internal/watch"
)

// Hooks are what the setup asks of the program that serves it.
type Hooks struct {
	// ServiceState is "running", "stopped", "not_installed" or "unknown".
	ServiceState func() string
}

// The most of the log one read sends, so a long gap doesn't stall the page.
const logChunk = 512 * 1024

// handleLogs tails the service log: the last `lines` lines, or what was
// written after byte `after` (an earlier answer's offset). `reset` says the
// file started over (rotated), so the page drops what it shows.
func (s *srv) handleLogs(w http.ResponseWriter, r *http.Request) {
	cfg, _ := config.Load()
	path := cfg.LogFile()
	lines, _ := strconv.Atoi(r.URL.Query().Get("lines"))
	if lines <= 0 || lines > 2000 {
		lines = 300
	}
	after, aerr := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)

	f, err := os.Open(path)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"offset": 0, "text": "", "missing": true, "path": path})
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	size := info.Size()

	if aerr == nil && after >= 0 && after <= size {
		from := after
		if size-from > logChunk {
			from = size - logChunk
		}
		buf := make([]byte, size-from)
		if _, err := f.ReadAt(buf, from); err != nil && err != io.EOF {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"offset": size, "text": string(buf), "path": path})
		return
	}

	// First read, or the file is shorter than what was read before.
	from := int64(0)
	if size > logChunk {
		from = size - logChunk
	}
	buf := make([]byte, size-from)
	if _, err := f.ReadAt(buf, from); err != nil && err != io.EOF {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"offset": size,
		"text":   string(utils.TailLines(buf, lines)),
		"reset":  aerr == nil,
		"path":   path,
	})
}

func (s *srv) handleRestart(w http.ResponseWriter, r *http.Request) {
	s.runSelf(w, r, "restart")
}

// indexCount is how many files index.txt lists, all and per folder, read
// again only when it changed.
type indexCount struct {
	mu        sync.Mutex
	mod       time.Time
	size      int64
	count     int
	perFolder map[string]int
}

func (c *indexCount) read() (count int, at time.Time, perFolder map[string]int, ok bool) {
	info, err := os.Stat(watch.IndexPath())
	if err != nil {
		return 0, time.Time{}, nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !info.ModTime().Equal(c.mod) || info.Size() != c.size {
		b, err := os.ReadFile(watch.IndexPath())
		if err != nil {
			return 0, time.Time{}, nil, false
		}
		per := map[string]int{}
		n := 0
		for i, line := range bytes.Split(b, []byte("\n")) {
			if i == 0 || len(line) == 0 {
				continue // the header
			}
			n++
			// size,mod,fileID,mime,apiFileId,path: the path is last, and its first segment the folder.
			if parts := bytes.SplitN(line, []byte(","), 6); len(parts) == 6 {
				path := strings.TrimPrefix(string(parts[5]), "/")
				name, _, _ := strings.Cut(path, "/")
				per[name]++
			}
		}
		c.mod, c.size, c.count, c.perFolder = info.ModTime(), info.Size(), n, per
	}
	return c.count, c.mod, c.perFolder, true
}

// certUntil is when the bridge's certificate for its secure address runs out.
func certUntil(dir string) (time.Time, bool) {
	certFile, _, ok := tlsx.Loaded(dir)
	if !ok {
		return time.Time{}, false
	}
	b, err := os.ReadFile(certFile)
	if err != nil {
		return time.Time{}, false
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return time.Time{}, false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, false
	}
	return cert.NotAfter, true
}
