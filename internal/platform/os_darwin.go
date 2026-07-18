//go:build darwin

package platform

// osImpl is the macOS OperatingSystem implementation; its methods live across
// the *_darwin.go and *_unix.go files.
type osImpl struct{}

// Compile-time check that osImpl satisfies OperatingSystem.
var _ OperatingSystem = osImpl{}

func init() { current = osImpl{} }
