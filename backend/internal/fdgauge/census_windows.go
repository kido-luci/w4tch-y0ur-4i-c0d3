//go:build windows

package fdgauge

const supported = false

// census is never called with supported false; the stub exists so the
// package compiles for the windows cross-build.
func census() (string, error) { return "", nil }
