package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
)

// VerifySums checks a release's SHA256SUMS.txt against its signature file: one
// base64 ed25519 signature a line, any of them by any of keys.
func VerifySums(sums, signature []byte, keys []ed25519.PublicKey) error {
	if len(keys) == 0 {
		return errors.New("this build has no key to check a release with")
	}
	sc := bufio.NewScanner(bytes.NewReader(signature))
	for sc.Scan() {
		sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sc.Text()))
		if err != nil || len(sig) != ed25519.SignatureSize {
			continue
		}
		for _, key := range keys {
			if ed25519.Verify(key, sums, sig) {
				return nil
			}
		}
	}
	return errors.New("the release's signature doesn't match a key this build trusts")
}

// ParseSums reads `sha256sum` output: the hex digest of each file by its name.
func ParseSums(sums []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 || len(fields[0]) != 64 {
			continue
		}
		out[strings.TrimPrefix(fields[1], "*")] = strings.ToLower(fields[0])
	}
	return out
}

// What the release workflow names its archives.
var assetPattern = regexp.MustCompile(`^bridge_(.+)_(windows|darwin|linux)_(amd64|arm64|armv7)\.(tar\.gz|zip)$`)

// checkAsset makes sure the archive is the build of version for this system: a
// name is part of what the signature covers, so a signed archive of another
// version or system can't be passed off as this one.
func checkAsset(name, version, goos, arch string) error {
	m := assetPattern.FindStringSubmatch(name)
	if m == nil {
		return fmt.Errorf("%q isn't a bridge build", name)
	}
	if m[1] != version || m[2] != goos || m[3] != arch {
		return fmt.Errorf("%q isn't the %s build for %s/%s", name, version, goos, arch)
	}
	return nil
}

// extract takes the program out of an archive into dst: the file named bin at
// any depth, at most limit bytes.
func extract(archive, bin, dst string, limit int64) error {
	var (
		r     io.Reader
		close func()
	)
	switch {
	case strings.HasSuffix(archive, ".zip"):
		zr, err := zip.OpenReader(archive)
		if err != nil {
			return fmt.Errorf("open the archive: %w", err)
		}
		defer zr.Close()
		for _, f := range zr.File {
			if path.Base(f.Name) == bin && !f.FileInfo().IsDir() {
				rc, err := f.Open()
				if err != nil {
					return err
				}
				r, close = rc, func() { rc.Close() }
				break
			}
		}
	default:
		f, err := os.Open(archive)
		if err != nil {
			return err
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("open the archive: %w", err)
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if err != nil {
				break
			}
			if path.Base(h.Name) == bin && h.Typeflag == tar.TypeReg {
				r, close = tr, func() {}
				break
			}
		}
	}
	if r == nil {
		return fmt.Errorf("the archive holds no %s", bin)
	}
	defer close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(r, limit+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n > limit {
		return fmt.Errorf("%s is larger than a bridge can be", bin)
	}
	return os.Chmod(dst, 0o755)
}
