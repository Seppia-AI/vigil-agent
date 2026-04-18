// Package version exposes build-time metadata for the agent.
//
// All three values are intentionally var (not const) so the release
// pipeline can inject real values via -ldflags at link time, e.g.:
//
//	go build -ldflags "\
//	  -X github.com/Seppia-AI/vigil-agent/internal/version.Version=v0.1.0 \
//	  -X github.com/Seppia-AI/vigil-agent/internal/version.Commit=$(git rev-parse --short HEAD) \
//	  -X github.com/Seppia-AI/vigil-agent/internal/version.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
//
// Local `go build` (no ldflags) leaves the defaults in place, which is what
// developers see when they run from source.
package version

import "fmt"

var (
	// Version is the SemVer release tag (e.g. "v0.1.0"). "dev" for local builds.
	Version = "dev"

	// Commit is the short git SHA the binary was built from. "none" for local builds.
	Commit = "none"

	// Date is the RFC3339 UTC build timestamp. "unknown" for local builds.
	Date = "unknown"
)

// String returns a single-line human-readable build identifier suitable for
// `--version` output and the User-Agent header on outbound requests.
func String() string {
	return fmt.Sprintf("vigil-agent %s (commit %s, built %s)", Version, Commit, Date)
}
