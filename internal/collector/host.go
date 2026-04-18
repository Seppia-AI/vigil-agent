package collector

import (
	"os"

	"github.com/shirou/gopsutil/v4/host"
)

// HostInfo is the lightweight metadata block the agent stamps onto every
// outbound batch (Batch.Hostname / OS / Arch / Kernel). It's collected
// ONCE at startup, not per-scrape, because none of these values change at
// the cadence we scrape at and re-fetching kernel info every minute is
// needless syscall pressure.
//
// AgentVersion is filled in by the caller (cmd/vigil-agent) so the
// collector package doesn't have to import internal/version, which would
// make this package useful for tests in isolation.
type HostInfo struct {
	Hostname     string
	OS           string
	Arch         string
	Kernel       string
	AgentVersion string
}

// CollectHostInfo gathers the static-ish host metadata. Order of
// preference for hostname is:
//
//  1. gopsutil host.Info().Hostname (uses the kernel's gethostname() on
//     Linux, sysctl on macOS — same answer in 99% of cases).
//  2. os.Hostname() — pure Go fallback for the (rare) case gopsutil
//     can't read /proc or sysctl, e.g. in a stripped-down container.
//
// Anything we can't determine is left blank; the ingest endpoint
// tolerates missing fields.
//
// AgentVersion is set by the caller after this returns.
func CollectHostInfo() HostInfo {
	hi := HostInfo{}
	hi.OS, hi.Arch = runtimeOSArch()

	if info, err := host.Info(); err == nil && info != nil {
		hi.Hostname = info.Hostname
		hi.Kernel = info.KernelVersion
	}
	if hi.Hostname == "" {
		// Fallback for containers that strip /etc/hostname plumbing.
		// gopsutil returns "" not an error in that case, so we only
		// hit this branch for the truly degraded host.
		if h, err := os.Hostname(); err == nil {
			hi.Hostname = h
		}
	}

	return hi
}
