package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode"
)

// Folder is one folder the bridge syncs. In Files it is the folder Name inside
// the bridge's folder, and every path the bridge sends starts with it
// (`/<Name>/Scans/a.pdf`). ID prefixes its files' identities, so they stay
// unique across disks; it comes from the path, so the same folder added again
// is the same folder.
type Folder struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

// MaxFolders is how many folders one bridge syncs at most.
const MaxFolders = 50

// caseless: the disks of macOS and Windows treat names that differ only in case
// as one, so folders are compared that way there.
var caseless = runtime.GOOS == "darwin" || runtime.GOOS == "windows"

func fold(s string) string {
	if caseless {
		return strings.ToLower(s)
	}
	return s
}

// folderID is a short id from the folder's cleaned path.
func folderID(path string) string {
	sum := sha256.Sum256([]byte(fold(filepath.Clean(path))))
	return hex.EncodeToString(sum[:4])
}

// within reports whether path is dir or inside it.
func within(path, dir string) bool {
	path, dir = fold(filepath.Clean(path)), fold(filepath.Clean(dir))
	if path == dir {
		return true
	}
	if !strings.HasSuffix(dir, string(os.PathSeparator)) {
		dir += string(os.PathSeparator)
	}
	return strings.HasPrefix(path, dir)
}

// real is path with its links resolved, so a folder reached two ways is seen as one.
func resolvedPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

// CheckFolderName is nil for a name a folder in Files can take: it is one
// segment of its path, and a name starting with a dot isn't synced.
func CheckFolderName(name string) error {
	switch {
	case name == "":
		return errors.New("the name is empty")
	case len(name) > 255:
		return errors.New("the name is longer than 255 characters")
	case name == "." || name == "..", strings.HasPrefix(name, "."):
		return errors.New("a name can't start with a dot")
	case strings.ContainsAny(name, `/\`):
		return errors.New("a name can't hold / or \\")
	case strings.TrimSpace(name) != name:
		return errors.New("a name can't start or end with a space")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return errors.New("a name can't hold control characters")
		}
	}
	return nil
}

// defaultFolderName is the folder's own name, with " 2", " 3"… when another
// folder has it.
func defaultFolderName(path string, taken []Folder) string {
	base := filepath.Base(filepath.Clean(path))
	if vol := filepath.VolumeName(path); base == "" || base == "." || base == string(os.PathSeparator) || base == vol || base == vol+string(os.PathSeparator) {
		base = strings.TrimSuffix(strings.TrimSuffix(vol, ":"), string(os.PathSeparator))
	}
	base = strings.TrimLeft(strings.TrimSpace(base), ".")
	if CheckFolderName(base) != nil {
		base = "Folder"
	}
	name := base
	for n := 2; nameTaken(name, taken, ""); n++ {
		name = base + " " + strconv.Itoa(n)
	}
	return name
}

func nameTaken(name string, folders []Folder, except string) bool {
	for _, f := range folders {
		if f.ID != except && strings.EqualFold(f.Name, name) {
			return true
		}
	}
	return false
}

// CheckFolder answers the folder path would sync as, or why it can't: it must
// be a folder that is there, apart from every folder synced already (neither
// one of them, inside one, nor holding one), and apart from the bridge's own
// files (its settings would sync, secrets included).
func (c Config) CheckFolder(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("choose a folder")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("%s isn't there", abs)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s isn't a folder", abs)
	}
	resolved := resolvedPath(abs)
	if len(c.Folders) >= MaxFolders {
		return "", fmt.Errorf("a bridge syncs %d folders at most", MaxFolders)
	}
	for _, f := range c.Folders {
		other := resolvedPath(f.Path)
		switch {
		case within(resolved, other) && within(other, resolved):
			return "", fmt.Errorf("%s is synced already, as %q", abs, f.Name)
		case within(resolved, other):
			return "", fmt.Errorf("%s is inside %q, which is synced already", abs, f.Name)
		case within(other, resolved):
			return "", fmt.Errorf("%s holds %q, which is synced already", abs, f.Name)
		}
	}
	for _, own := range []string{InstallDir(), Dir(), c.LogDir()} {
		own = resolvedPath(own)
		if within(resolved, own) || within(own, resolved) {
			return "", fmt.Errorf("%s holds the bridge's own files (%s): choose a folder apart from them", abs, own)
		}
	}
	return abs, nil
}

// AddFolder syncs one more folder, named name or after itself.
func (c Config) AddFolder(path, name string) (Config, Folder, error) {
	abs, err := c.CheckFolder(path)
	if err != nil {
		return c, Folder{}, err
	}
	f := Folder{ID: folderID(abs), Path: abs}
	if name = strings.TrimSpace(name); name == "" {
		f.Name = defaultFolderName(abs, c.Folders)
	} else {
		if err := CheckFolderName(name); err != nil {
			return c, Folder{}, err
		}
		if nameTaken(name, c.Folders, "") {
			return c, Folder{}, fmt.Errorf("another folder is called %q", name)
		}
		f.Name = name
	}
	c.Folders = append(append([]Folder{}, c.Folders...), f)
	return c, f, nil
}

// FindFolder is the folder by its name (any case) or its path.
func (c Config) FindFolder(nameOrPath string) (Folder, bool) {
	for _, f := range c.Folders {
		if strings.EqualFold(f.Name, nameOrPath) || within(f.Path, nameOrPath) && within(nameOrPath, f.Path) {
			return f, true
		}
	}
	return Folder{}, false
}

// RemoveFolder stops syncing a folder: its files leave Files, and nothing on
// the computer changes.
func (c Config) RemoveFolder(nameOrPath string) (Config, Folder, error) {
	f, ok := c.FindFolder(nameOrPath)
	if !ok {
		return c, Folder{}, fmt.Errorf("no synced folder is %q", nameOrPath)
	}
	kept := make([]Folder, 0, len(c.Folders)-1)
	for _, it := range c.Folders {
		if it.ID != f.ID {
			kept = append(kept, it)
		}
	}
	c.Folders = kept
	return c, f, nil
}

// RenameFolder changes a folder's name in Files: its files move there, the same
// files, and nothing on the computer changes.
func (c Config) RenameFolder(nameOrPath, name string) (Config, Folder, error) {
	f, ok := c.FindFolder(nameOrPath)
	if !ok {
		return c, Folder{}, fmt.Errorf("no synced folder is %q", nameOrPath)
	}
	name = strings.TrimSpace(name)
	if err := CheckFolderName(name); err != nil {
		return c, Folder{}, err
	}
	if nameTaken(name, c.Folders, f.ID) {
		return c, Folder{}, fmt.Errorf("another folder is called %q", name)
	}
	folders := append([]Folder{}, c.Folders...)
	for i := range folders {
		if folders[i].ID == f.ID {
			folders[i].Name = name
			f = folders[i]
		}
	}
	c.Folders = folders
	return c, f, nil
}

// ErrUnknownFolder: a path whose first segment names no synced folder.
var ErrUnknownFolder = errors.New("no synced folder by that name")

// SplitPath splits a path as Files has it (`/Scans/2026/a.pdf`) into its
// synced folder and the path inside it (`/2026/a.pdf`; `/` for the folder itself).
func (c Config) SplitPath(p string) (Folder, string, error) {
	rest := strings.TrimPrefix(p, "/")
	name, inner, _ := strings.Cut(rest, "/")
	if name == "" {
		return Folder{}, "", errors.New("the path names no synced folder")
	}
	for _, f := range c.Folders {
		if f.Name == name {
			return f, "/" + inner, nil
		}
	}
	return Folder{}, "", fmt.Errorf("%w: %q", ErrUnknownFolder, name)
}

// FolderHolding is the synced folder path is in, or holds, if any.
func (c Config) FolderHolding(path string) (Folder, bool) {
	resolved := resolvedPath(path)
	for _, f := range c.Folders {
		other := resolvedPath(f.Path)
		if within(resolved, other) || within(other, resolved) {
			return f, true
		}
	}
	return Folder{}, false
}
