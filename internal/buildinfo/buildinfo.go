// Package buildinfo carries version metadata injected at link time.
//
// The values are overridden by -ldflags at build time:
//
//	-X github.com/relaypool/relaypool/internal/buildinfo.Version=...
//	-X github.com/relaypool/relaypool/internal/buildinfo.Commit=...
//
// Keeping them as package variables (not consts) is what makes -X work.
package buildinfo

import "runtime"

var (
	// Version is the release version, e.g. "0.1.0" or "dev".
	Version = "dev"
	// Commit is the short git SHA the binary was built from.
	Commit = "unknown"
	// BuiltAt is the RFC3339 build timestamp.
	BuiltAt = "unknown"
)

// GoVersion reports the toolchain that compiled this binary. It is read at
// runtime rather than injected so it is always truthful.
func GoVersion() string { return runtime.Version() }
