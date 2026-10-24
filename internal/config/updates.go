package config

import (
	"runtime"
	"runtime/debug"
	"strings"
)

// ReleaseArch names this build's architecture as the release archives do
// (amd64, arm64, armv7), so ViaVeritas can offer the build that replaces it.
func ReleaseArch() string {
	if runtime.GOARCH != "arm" {
		return runtime.GOARCH
	}
	goarm := "7"
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			// "7", or "7,softfloat" since Go 1.22.
			if s.Key == "GOARM" && s.Value != "" {
				goarm, _, _ = strings.Cut(s.Value, ",")
			}
		}
	}
	return "armv" + goarm
}
