package watch

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"bridge/internal/config"
	"bridge/internal/logx"
	"bridge/internal/utils"
)

// entry is the per-file on-disk index record. Size+ModUnix are the change key;
// FileID the move key (compact OS identity); MIME is cached to avoid re-derive;
// APIFileID is the API-assigned row id (set on the first sync ack).
type entry struct {
	Size      int64
	ModUnix   int64
	FileID    string // compact OS file identity, e.g. "1a2b:ff03"
	MIME      string
	APIFileID string // assigned by the API on sync; "" until the first sync ack
}

type index struct {
	Files map[string]entry // key = root-relative path, e.g. "/patients/scan.pdf"
}

// indexHeader is the file's first line. Writers emit the current version;
// loadIndex also accepts the previous version for in-place upgrade.
//   v2 line: size,mod,fileID,mime,path            (5 columns)
//   v3 line: size,mod,fileID,mime,apiFileId,path  (6 columns)
// Path stays last in both because it may contain commas.
const (
	indexHeader   = "bridge index v3"
	indexHeaderV2 = "bridge index v2"
)

func indexPath() string { return filepath.Join(config.Dir(), "index.txt") }

// IndexPath is the on-disk location of index.txt (exported for `preview`/
// `status` output and diagnostics).
func IndexPath() string { return indexPath() }

// loadIndex parses index.txt. A missing file, bad header, or malformed line
// yields an empty index and false (a first run). Both v3 (6 cols) and v2 (5
// cols, apiFileId → "") headers are accepted for a silent in-place upgrade;
// the first saveIndex after upgrade rewrites as v3.
func loadIndex() (index, bool) {
	idx := index{Files: map[string]entry{}}
	f, err := os.Open(indexPath())
	if err != nil {
		return idx, false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // tolerate long path lines
	if !sc.Scan() {
		logx.Info("watch", "index.txt missing/old header; treating as first run")
		return index{Files: map[string]entry{}}, false
	}
	header := sc.Text()
	var wantCols int
	switch header {
	case indexHeader:
		wantCols = 6
	case indexHeaderV2:
		wantCols = 5
		logx.Info("watch", "index.txt is %s; migrating to %s on next save", indexHeaderV2, indexHeader)
	default:
		logx.Info("watch", "index.txt has unknown header %q; treating as first run", header)
		return index{Files: map[string]entry{}}, false
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		// Path stays last (SplitN by wantCols) so commas inside it are safe.
		parts := strings.SplitN(line, ",", wantCols)
		if len(parts) != wantCols {
			logx.Warn("watch", "index.txt malformed line; treating as first run")
			return index{Files: map[string]entry{}}, false
		}
		size, e1 := strconv.ParseInt(parts[0], 10, 64)
		mod, e2 := strconv.ParseInt(parts[1], 10, 64)
		if e1 != nil || e2 != nil {
			logx.Warn("watch", "index.txt unparseable numbers; treating as first run")
			return index{Files: map[string]entry{}}, false
		}
		e := entry{Size: size, ModUnix: mod, FileID: parts[2], MIME: parts[3]}
		if wantCols == 6 {
			e.APIFileID = parts[4]
			idx.Files[parts[5]] = e
		} else {
			idx.Files[parts[4]] = e
		}
	}
	if sc.Err() != nil {
		logx.Warn("watch", "index.txt read error (%v); treating as first run", sc.Err())
		return index{Files: map[string]entry{}}, false
	}
	return idx, true
}

// saveIndex writes index.txt atomically (temp + rename) so a crash mid-write
// never leaves a half-written index. The header is always written, even empty.
func saveIndex(idx index) error {
	if err := os.MkdirAll(config.Dir(), 0o700); err != nil {
		return err
	}
	paths := make([]string, 0, len(idx.Files))
	for p := range idx.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var b bytes.Buffer
	b.WriteString(indexHeader)
	b.WriteByte('\n')
	for _, p := range paths {
		e := idx.Files[p]
		// size,mod,fileID,mime,apiFileId,path — see indexHeader doc above.
		fmt.Fprintf(&b, "%d,%d,%s,%s,%s,%s\n", e.Size, e.ModUnix, e.FileID, e.MIME, e.APIFileID, p)
	}

	return utils.AtomicWrite(indexPath(), b.Bytes(), 0o600)
}

func persist(snapshot map[string]entry) {
	if err := saveIndex(index{Files: snapshot}); err != nil {
		logx.Warn("watch", "could not persist index.txt: %v", err)
	}
}

// ResetIndex deletes index.txt so the next scan is a first run and re-sends the
// full inventory. Used by `resync` / setup UI when server records and the local
// index diverge.
func ResetIndex() error {
	err := os.Remove(indexPath())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	resetCaughtUp()
	return nil
}

// LookupIndex returns the index record for relPath for echoing in Get without
// re-stat-ing or recomputing the file-id. ok=false (empty record) when relPath
// isn't indexed yet (e.g. a file the scan hasn't observed).
func LookupIndex(relPath string) (deviceFileIno, mime, apiFileID string, size int64, modUnix int64, ok bool) {
	idx, _ := loadIndex()
	e, found := idx.Files[relPath]
	if !found {
		return "", "", "", 0, 0, false
	}
	return e.FileID, e.MIME, e.APIFileID, e.Size, e.ModUnix, true
}
