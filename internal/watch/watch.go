// Package watch detects file changes under the data root by periodic stat
// scans (no kernel notifications, no file contents read) and reports diffs to
// the API's sync endpoint. Each interval it walks the root, builds a snapshot
// from stat() + OS file identity, and diffs it against index.txt; unchanged
// files are recognised by (size, mtime) and never reopened.
//
// Rename/move keeps the same OS file-ID: a vanished path plus an identical
// file-ID at a new path is one update in files[] (new path), not delete+create.
// Creates/modifications go in files[]; removals go in deleted[] as the vanished
// file's OS file-ID, not its path.
package watch
