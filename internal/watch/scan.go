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
	added    []string // paths new since the last index
	modified []string // paths whose size/mtime changed
	deleted  []string // OS file-IDs to send in the sync's deleted[]
	hadIndex bool     // false => the next sync is a full sync
	scanTook time.Duration
	skip     string // non-empty => nothing to sync (human-readable reason)
	skipped  int    // entries the walk could not read (permissions/races)
	// unavailable names the folders that couldn't be read, whose files were kept.
	unavailable []string
}

// scan walks every synced folder once and diffs them against index.txt, with
// no network or disk writes. Paths are as Files has them, each under its
// folder's name (`/Scans/2026/a.pdf`), and a file's identity is its folder's id
// with its inode, so files on different disks never collide. A folder that
// can't be read (an unplugged drive, a folder macOS keeps the service out of)
// keeps what the index had for it: its files must not leave Files for that.
func scan(ctx context.Context, cfg config.Config, onScan func(scanned int)) (scanResult, error) {
	if len(cfg.Folders) == 0 {
		return scanResult{skip: "no folder to sync yet"}, nil
	}

	old, hadIndex := loadIndex()
	start := time.Now()

	snapshot := make(map[string]entry, len(old.Files))
	var added, modified, unavailable []string
	var skipped, scanned int

	for _, f := range cfg.Folders {
		got, err := scanFolder(ctx, cfg, f, old, &scanned, onScan)
		if err != nil {
			return scanResult{}, err
		}
		skipped += got.skipped
		if got.unavailable != "" {
			logx.Warn("watch", "%s: %s; its files stay as they were until it can be read", f.Name, got.unavailable)
			unavailable = append(unavailable, f.Name)
			for rel, e := range old.Files {
				if strings.HasPrefix(e.FileID, f.ID+":") || strings.HasPrefix(rel, "/"+f.Name+"/") {
					snapshot[rel] = e
				}
			}
			continue
		}
		for rel, e := range got.snapshot {
			snapshot[rel] = e
		}
		added = append(added, got.added...)
		modified = append(modified, got.modified...)
	}

	if len(unavailable) == len(cfg.Folders) {
		return scanResult{
			skip:        "none of its folders can be read: " + strings.Join(unavailable, ", "),
			skipped:     skipped,
			hadIndex:    hadIndex,
			scanTook:    time.Since(start).Round(time.Millisecond),
			unavailable: unavailable,
		}, nil
	}

	// Paths present last scan but gone now are deletion candidates; a move may
	// reclaim one below. A folder no longer synced is gone whole.
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

	// Report deletions by file-ID (from the old index entry), not path. An
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

	return scanResult{
		snapshot:    snapshot,
		added:       added,
		modified:    modified,
		deleted:     deleted,
		hadIndex:    hadIndex,
		scanTook:    time.Since(start).Round(time.Millisecond),
		skipped:     skipped,
		unavailable: unavailable,
	}, nil
}

type folderScan struct {
	snapshot        map[string]entry
	added, modified []string
	skipped         int
	unavailable     string // why it can't be read now; "" when it was
}

// scanFolder walks one synced folder, staying on its own filesystem.
func scanFolder(ctx context.Context, cfg config.Config, f config.Folder, old index, scanned *int, onScan func(int)) (folderScan, error) {
	out := folderScan{snapshot: map[string]entry{}}
	root := strings.TrimRight(filepath.Clean(f.Path), string(os.PathSeparator))
	if root == "" {
		root = string(os.PathSeparator)
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		out.unavailable = fmt.Sprintf("%s isn't there", root)
		return out, nil
	}
	if !rootInfo.IsDir() {
		out.unavailable = fmt.Sprintf("%s isn't a folder", root)
		return out, nil
	}
	rootDev, devKnown := platform.DeviceOf(rootInfo)
	prefix := "/" + f.Name

	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			// Unreadable subtree (permissions, races): skip but keep scanning.
			// Failure at the root or a top-level child usually means real
			// misconfiguration (wrong owner / 0700 / protected location) —
			// surface loudly; deeper failures are likely races, stay at Debug.
			out.skipped++
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
			// Another disk mounted inside the folder isn't part of it.
			if devKnown {
				if info, ierr := d.Info(); ierr == nil {
					if dev, ok := platform.DeviceOf(info); ok && dev != rootDev {
						logx.Debug("watch", "not crossing into %s: another filesystem", p)
						return fs.SkipDir
					}
				}
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
		// the setup's live "files scanned" counter.
		*scanned++
		if onScan != nil {
			onScan(*scanned)
		}
		rel := prefix + utils.ToRootRel(root, p)
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
		// recompute (keeps Windows steady-state stat-only). One without this
		// folder's identity is evaluated again.
		if prev, ok := old.Files[rel]; ok && prev.Size == size && prev.ModUnix == modUnix && strings.HasPrefix(prev.FileID, f.ID+":") {
			out.snapshot[rel] = prev
			return nil
		}

		key, ok := platform.Current().ComputeFileID(p, info)
		if !ok {
			logx.Debug("watch", "file-ID unavailable for %s; reporting without move detection", p)
		}
		e := entry{Size: size, ModUnix: modUnix, FileID: identity(f, key), MIME: utils.MIMEByExt(p)}
		out.snapshot[rel] = e
		if _, existed := old.Files[rel]; existed {
			out.modified = append(out.modified, rel)
		} else {
			out.added = append(out.added, rel)
		}
		return nil
	})
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	if walkErr != nil {
		return out, fmt.Errorf("scan of %s failed: %w", f.Name, walkErr)
	}
	// A folder that is there with nothing readable in it but entries it
	// couldn't read is a permissions problem (or macOS's protection), not an
	// empty folder: treating it as empty would take its files out of Files.
	if len(out.snapshot) == 0 && out.skipped > 0 {
		out.unavailable = fmt.Sprintf("%s has %d unreadable entr%s and no readable files — check its permissions", root, out.skipped, plural(out.skipped, "y", "ies"))
	}
	return out, nil
}

// identity is a file's id as ViaVeritas knows it: its folder's id and its
// inode (file index on Windows), the device left out, as the folder fixes it
// and a remounted drive may change it.
func identity(f config.Folder, key string) string {
	if key == "" {
		return ""
	}
	_, ino, found := strings.Cut(key, ":")
	if !found {
		ino = key
	}
	return f.ID + ":" + ino
}
