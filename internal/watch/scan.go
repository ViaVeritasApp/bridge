package watch

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"bridge/internal/config"
	"bridge/internal/logx"
	"bridge/internal/platform"
	"bridge/internal/utils"
)

// scanResult is the outcome of one data-root scan diffed against the index,
// with no side effects. scan computes it; ScanOnce and Preview consume it.
type scanResult struct {
	snapshot map[string]entry
	added    []string // root-relative paths new since the last index
	modified []string // root-relative paths whose size/mtime changed
	deleted  []string // OS file-IDs to send in the sync's deleted[]
	hadIndex bool     // false => the next sync is a full sync
	scanTook time.Duration
	skip     string // non-empty => nothing to sync (human-readable reason)
	skipped  int    // entries the walk could not read (permissions/races)
}

// scan walks the data root once and diffs it against index.txt, with no
// network or disk writes. A missing/non-directory root yields skip (an expected
// pre-configure state, not an error); a cancelled context or failed walk errors.
func scan(ctx context.Context, cfg config.Config, onScan func(scanned int)) (scanResult, error) {
	root := strings.TrimRight(cfg.DataDir(), string(os.PathSeparator))
	if root == "" {
		return scanResult{skip: "no data root configured"}, nil
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return scanResult{skip: fmt.Sprintf("data root %s is not a directory yet", root)}, nil
	}

	old, hadIndex := loadIndex()
	start := time.Now()

	snapshot := make(map[string]entry, len(old.Files))
	var added, modified []string
	var skipped int
	var scanned int

	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			// Unreadable subtree (permissions, races): skip but keep scanning.
			// Failure at the root or a top-level child usually means real
			// misconfiguration (wrong owner / 0700 / protected location) —
			// surface loudly; deeper failures are likely races, stay at Debug.
			skipped++
			if p == root || filepath.Dir(p) == root {
				logx.Warn("watch", "cannot read %s: %v (files under it will NOT sync)", p, err)
			} else {
				logx.Debug("watch", "skipping %s: %v", p, err)
			}
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p == root {
			return nil
		}
		name := d.Name()
		hidden := strings.HasPrefix(name, ".")
		if d.IsDir() {
			if hidden && !cfg.IncludeHidden {
				return fs.SkipDir
			}
			return nil
		}
		if hidden && !cfg.IncludeHidden {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // skip symlinks, devices, sockets, etc.
		}
		// Count every regular file and report the running total — drives
		// the setup wizard's live "files scanned" counter.
		scanned++
		if onScan != nil {
			onScan(scanned)
		}
		rel := utils.ToRootRel(root, p)
		// A newline in a filename would break the line-per-file index format.
		if strings.ContainsAny(rel, "\n\r") {
			logx.Debug("watch", "skipping %s: path contains a newline", p)
			return nil
		}

		info, ierr := d.Info()
		if ierr != nil {
			logx.Debug("watch", "stat failed for %s: %v", p, ierr)
			return nil
		}
		size := info.Size()
		modUnix := info.ModTime().Unix()

		// Unchanged since last scan: reuse the cached record, no file-ID
		// recompute (keeps Windows steady-state stat-only). An empty FileID
		// (older build that failed to resolve it) is re-evaluated so the
		// move key / device_file_ino get populated.
		if prev, ok := old.Files[rel]; ok && prev.Size == size && prev.ModUnix == modUnix && prev.FileID != "" {
			snapshot[rel] = prev
			return nil
		}

		key, ok := platform.Current().ComputeFileID(p, info)
		if !ok {
			logx.Debug("watch", "file-ID unavailable for %s; reporting without move detection", p)
		}
		e := entry{Size: size, ModUnix: modUnix, FileID: key, MIME: utils.MIMEByExt(p)}
		snapshot[rel] = e
		if _, existed := old.Files[rel]; existed {
			modified = append(modified, rel)
		} else {
			added = append(added, rel)
		}
		return nil
	})
	if ctx.Err() != nil {
		return scanResult{}, ctx.Err()
	}
	if walkErr != nil {
		return scanResult{}, fmt.Errorf("scan walk failed: %w", walkErr)
	}

	// Paths present last scan but gone now are deletion candidates; a move may
	// reclaim one below.
	deletedCandidates := make(map[string]bool)
	for rel := range old.Files {
		if _, ok := snapshot[rel]; !ok {
			deletedCandidates[rel] = true
		}
	}

	// Move detection: an added path whose file-ID + size uniquely matches
	// exactly one vanished path is a move — keep it in files[] (new path) and
	// drop the old path from deleted[]. Ambiguity falls back to create + delete.
	addedByFID := map[string]int{}
	for _, rel := range added {
		if fid := snapshot[rel].FileID; fid != "" {
			addedByFID[fid]++
		}
	}
	for _, rel := range added {
		cur := snapshot[rel]
		if cur.FileID == "" || addedByFID[cur.FileID] != 1 {
			continue
		}
		var match string
		matches := 0
		for oldRel := range deletedCandidates {
			oe := old.Files[oldRel]
			if oe.FileID == cur.FileID && oe.Size == cur.Size {
				match = oldRel
				matches++
			}
		}
		if matches == 1 {
			delete(deletedCandidates, match)
			logx.Debug("watch", "move detected: %s -> %s (file-ID %s)", match, rel, cur.FileID)
		}
	}

	// Report deletions by OS file-ID (from the old index entry), not path. An
	// entry with no recorded file-ID can't be matched by identity, so it's
	// dropped with a debug note.
	deleted := make([]string, 0, len(deletedCandidates))
	for rel := range deletedCandidates {
		if fid := old.Files[rel].FileID; fid != "" {
			deleted = append(deleted, fid)
		} else {
			logx.Debug("watch", "deleted %s has no recorded file-ID; omitting from deleted[]", rel)
		}
	}
	sort.Strings(deleted)

	took := time.Since(start).Round(time.Millisecond)

	// Root is a directory but nothing readable was found while entries were
	// skipped: almost certainly a permissions/ownership problem. Report as skip
	// so neither preview nor live sync treats it as "0 files" — which on a first
	// run would persist an empty index and permanently mask the real files.
	if len(snapshot) == 0 && skipped > 0 {
		return scanResult{
			skip:     fmt.Sprintf("data root %s has %d unreadable entr%s and no readable files — check folder permissions/ownership", root, skipped, plural(skipped, "y", "ies")),
			skipped:  skipped,
			hadIndex: hadIndex,
			scanTook: took,
		}, nil
	}

	return scanResult{
		snapshot: snapshot,
		added:    added,
		modified: modified,
		deleted:  deleted,
		hadIndex: hadIndex,
		scanTook: took,
		skipped:  skipped,
	}, nil
}
