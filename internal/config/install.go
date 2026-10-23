package config

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// InstalledVersion asks the installed program for its version: "" when there
// is none, or it can't say (one from before `version` existed).
func InstalledVersion() string {
	path := InstalledBinary()
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

var releaseVersion = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)$`)

// CompareVersions is -1, 0 or 1 for plain releases (v1.2.3), and 0 when either
// isn't one (a developer's build): nothing is refused for it.
func CompareVersions(a, b string) int {
	ma, mb := releaseVersion.FindStringSubmatch(a), releaseVersion.FindStringSubmatch(b)
	if ma == nil || mb == nil {
		return 0
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(ma[i])
		y, _ := strconv.Atoi(mb[i])
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}
