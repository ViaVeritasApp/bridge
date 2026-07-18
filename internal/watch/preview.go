package watch

import (
	"context"
	"os"
	"strings"
	"time"

	"bridge/internal/config"
)

// Plan summarises what the next sync would upload, as computed by Preview.
type Plan struct {
	Mode       string // "full" | "incremental" | "none" | "skip"
	Reason     string // set when Mode == "skip"
	DataRoot   string // resolved data root ("" if unset)
	IndexFound bool   // false => the next sync is a full sync
	Created    int    // full: every file; incremental: new files
	Modified   int    // incremental only
	Deleted    int    // file-IDs that would be sent in deleted[]
	Total      int    // files[] count that would be uploaded
	Indexed    int    // files already recorded in index.txt (snapshot size)
	Skipped    int    // entries the scan could not read (permissions/races)
	ScanTook   time.Duration
}

// Preview runs the same scan + diff as the live sync but uploads and writes
// nothing (index.txt untouched); safe to run before pairing or install.
func Preview(ctx context.Context, cfg config.Config) (Plan, error) {
	return PreviewProgress(ctx, cfg, nil)
}

// PreviewProgress is Preview with an optional onScan callback reporting the
// running file count as the walk proceeds. onScan may be nil and runs on the
// scanning goroutine — keep it cheap.
func PreviewProgress(ctx context.Context, cfg config.Config, onScan func(scanned int)) (Plan, error) {
	r, err := scan(ctx, cfg, onScan)
	if err != nil {
		return Plan{}, err
	}
	p := Plan{
		DataRoot:   strings.TrimRight(cfg.DataDir(), string(os.PathSeparator)),
		IndexFound: r.hadIndex,
		Indexed:    len(r.snapshot),
		Skipped:    r.skipped,
		ScanTook:   r.scanTook,
	}
	switch {
	case r.skip != "":
		p.Mode = "skip"
		p.Reason = r.skip
	case !r.hadIndex:
		p.Mode = "full"
		p.Created = len(r.snapshot)
		p.Total = len(r.snapshot)
	case len(r.added)+len(r.modified)+len(r.deleted) == 0:
		p.Mode = "none"
	default:
		p.Mode = "incremental"
		p.Created = len(r.added)
		p.Modified = len(r.modified)
		p.Deleted = len(r.deleted)
		p.Total = len(r.added) + len(r.modified)
	}
	return p, nil
}
