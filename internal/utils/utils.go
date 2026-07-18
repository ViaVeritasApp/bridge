// Package utils holds small, dependency-free helpers shared across the bridge:
// line/text, path, and host:port parsing.
package utils

import (
	"mime"
	"net"
	"path/filepath"
	"strings"
)

// TailLines returns the last n lines of b (with trailing newlines). It scans
// backwards to avoid per-line allocation, and returns all of b when it has
// fewer than n lines.
func TailLines(b []byte, n int) []byte {
	if n <= 0 || len(b) == 0 {
		return b
	}
	i := len(b)
	if b[i-1] == '\n' {
		i-- // don't count the file's final newline as a line boundary
	}
	count := 0
	for ; i > 0; i-- {
		if b[i-1] == '\n' {
			count++
			if count == n {
				return b[i:]
			}
		}
	}
	return b
}

// ToRootRel turns an absolute path into a root-relative, forward-slash path
// with a leading slash (e.g. "/patients/scan.pdf").
func ToRootRel(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		rel = strings.TrimPrefix(p, root)
	}
	return "/" + strings.TrimPrefix(filepath.ToSlash(rel), "/")
}

// MIMEByExt derives a MIME type from the file extension only. Charset params are
// stripped; unknown extensions fall back to application/octet-stream.
func MIMEByExt(path string) string {
	if ext := filepath.Ext(path); ext != "" {
		if m := mime.TypeByExtension(ext); m != "" {
			if i := strings.IndexByte(m, ';'); i >= 0 {
				return strings.TrimSpace(m[:i])
			}
			return m
		}
	}
	return "application/octet-stream"
}

// HostOf returns the host part of "host:port", or def when addr has no port.
func HostOf(addr, def string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil && h != "" {
		return h
	}
	return def
}

// PortOf returns the port part of "host:port", or def when addr has no port.
func PortOf(addr, def string) string {
	if _, p, err := net.SplitHostPort(addr); err == nil && p != "" {
		return p
	}
	return def
}
