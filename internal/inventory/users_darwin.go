package inventory

import (
	"context"
	"encoding/binary"
	"os"
	"strings"
	"time"
)

// utmpxPath is where macOS keeps who is signed in.
var utmpxPath = "/var/run/utmpx"

// A record of /var/run/utmpx as macOS writes it: user (256), id (4), line (32),
// pid (4), type (2), 2 bytes of padding, the time as 32-bit seconds and
// microseconds, host (256), 64 reserved. gopsutil reads another layout, and
// finds nobody.
const (
	utmpxRecord      = 628
	utmpxUserProcess = 7
)

// signedIn is who is signed in to this Mac now, read from its login records.
func signedIn(context.Context) []User {
	data, err := os.ReadFile(utmpxPath)
	if err != nil {
		return nil
	}
	return parseUtmpx(data)
}

func parseUtmpx(data []byte) []User {
	var out []User
	for off := 0; off+utmpxRecord <= len(data); off += utmpxRecord {
		r := data[off : off+utmpxRecord]
		if int16(binary.LittleEndian.Uint16(r[296:298])) != utmpxUserProcess {
			continue
		}
		user := User{Name: cString(r[0:256]), Terminal: cString(r[260:292])}
		if sec := int32(binary.LittleEndian.Uint32(r[300:304])); sec > 0 {
			user.Since = time.Unix(int64(sec), 0).UTC()
		}
		out = append(out, user)
	}
	return out
}

func cString(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimSpace(string(b))
}
