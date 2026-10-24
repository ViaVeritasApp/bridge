package inventory

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func record(user, line string, kind int16, sec int32) []byte {
	r := make([]byte, utmpxRecord)
	copy(r[0:256], user)
	copy(r[260:292], line)
	binary.LittleEndian.PutUint16(r[296:298], uint16(kind))
	binary.LittleEndian.PutUint32(r[300:304], uint32(sec))
	return r
}

func TestSignedInOnAMac(t *testing.T) {
	var data []byte
	data = append(data, record("utmpx-1.00", "", 10, 0)...)
	data = append(data, record("", "", 2, 1791553396)...)
	data = append(data, record("anna", "console", 7, 1791553427)...)
	data = append(data, record("", "ttys002", 8, 1792224408)...)
	data = append(data, record("anna", "ttys009", 7, 1792861971)...)
	path := filepath.Join(t.TempDir(), "utmpx")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	utmpxPath = path
	users := signedIn(nil)
	if len(users) != 2 || users[0].Name != "anna" || users[0].Terminal != "console" || users[1].Terminal != "ttys009" || users[0].Since.Unix() != 1791553427 {
		t.Fatalf("users %+v", users)
	}
}

// This Mac's own records, when there are any: someone running the tests is signed in.
func TestSignedInHere(t *testing.T) {
	if _, err := os.Stat("/var/run/utmpx"); err != nil {
		t.Skip("no login records")
	}
	utmpxPath = "/var/run/utmpx"
	t.Logf("signed in: %+v", signedIn(nil))
}
