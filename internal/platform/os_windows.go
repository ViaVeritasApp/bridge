//go:build windows

package platform

// osImpl is the Windows OperatingSystem implementation; its methods live across
// the *_windows.go files.
type osImpl struct{}

// Compile-time check that osImpl satisfies OperatingSystem.
var _ OperatingSystem = osImpl{}

func init() { current = osImpl{} }
