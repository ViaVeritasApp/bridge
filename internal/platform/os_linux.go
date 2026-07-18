//go:build linux

package platform

// osImpl is the Linux OperatingSystem implementation; its methods live across
// the *_linux.go and *_unix.go files.
type osImpl struct{}

// Compile-time check that osImpl satisfies OperatingSystem.
var _ OperatingSystem = osImpl{}

func init() { current = osImpl{} }
