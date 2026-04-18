// Package collector defines the agent's metric-collection contract and
// hosts the per-family implementations (cpu, mem, swap, disk, net,
// uptime). The set is fixed and matches the server's accepted
// standard-metric names; arbitrary additions would be silently dropped at
// ingest.
//
// Design intent:
//
//   - One small Collector interface, one collector struct per metric family.
//     The Registry runs them in a fixed order on each scrape and merges
//     the results into a single Batch ready to JSON-encode and POST.
//
//   - Collectors are stateful (the cpu collector tracks the previous tick's
//     totals so it can report usage-since-last-call). State lives on the
//     collector struct, not in package globals — easier to test and means
//     two agents in the same process (unusual but legal) wouldn't trample
//     each other.
//
//   - Counters (net.*_bytes, disk.*_bytes, etc.) are sent RAW. We do NOT
//     compute rate agent-side because:
//       * Restarting the agent would lose the previous reading and the
//         first sample after restart would be bogus.
//       * The server can compute derivative() at query time across any
//         window the user picks, which the agent can't predict.
//       * It matches what Prometheus and OTLP do — minimum surprise.
//
//   - Collector errors are NEVER fatal to a scrape. A noisy filesystem or
//     a missing /proc entry should not stop us shipping the metrics that
//     DID succeed. The Registry collects partial results + a slice of
//     per-collector errors and lets the caller decide how loudly to
//     complain.
package collector

import (
	"context"
	"runtime"
	"time"
)

// Sample is one metric data point. The on-wire JSON shape is:
//   { "name": "...", "value": ..., "labels": { ... } }.
//
// `omitempty` on Labels keeps the wire size down for the (common) case
// of single-value metrics like uptime.seconds.
type Sample struct {
	Name   string            `json:"name"`
	Value  float64           `json:"value"`
	Labels map[string]string `json:"labels,omitempty"`
}

// Batch is the full payload the agent sends in one POST. The top-level
// fields (Hostname, OS, Arch, Kernel, AgentVersion) are optional metadata
// the server stores on the probe row.
//
// `Ts` is a single batch-level timestamp so the server doesn't need to
// trust per-sample times (which would also bloat the wire format).
// Samples within a batch are all considered to have happened at `Ts`.
type Batch struct {
	Ts           time.Time `json:"ts"`
	Hostname     string    `json:"hostname,omitempty"`
	OS           string    `json:"os,omitempty"`
	Arch         string    `json:"arch,omitempty"`
	Kernel       string    `json:"kernel,omitempty"`
	AgentVersion string    `json:"agent_version,omitempty"`
	Metrics      []Sample  `json:"metrics"`
}

// Collector produces samples for one metric family on demand. Implementations:
//
//   - MUST be safe to call repeatedly from a single goroutine. They are
//     not required to be safe under concurrent Collect() calls — the
//     Registry serialises them.
//   - SHOULD return as fast as possible. The CPU collector is the only
//     one that does any non-trivial blocking and even that is bounded
//     by the previous tick's data being already in memory.
//   - MUST NOT panic on a missing/erroring data source. Return (nil, err)
//     instead and let the Registry record it as a partial failure.
type Collector interface {
	// Name is a short identifier used in logs and registry output
	// (e.g. "cpu", "mem"). Stable across releases — operators may
	// have monitoring on `vigil_agent_collector_errors{collector="cpu"}`.
	Name() string

	// Collect returns the samples for this family at "now". The
	// caller stamps the batch with a single timestamp; collectors
	// don't pick their own.
	//
	// ctx is provided so future collectors that read from /proc on
	// extremely slow disks can respect the parent's deadline; today's
	// collectors all return in microseconds and ignore it.
	Collect(ctx context.Context) ([]Sample, error)
}

// All returns the standard set of collectors in the canonical emit
// order. Order matters only for human readability of `--once` output
// (cpu first reads more naturally than disk first); the server doesn't
// care.
//
// Adding a new collector here is the only place you should need to
// touch the registry — Run() iterates this slice.
func All() []Collector {
	return []Collector{
		NewCPU(),
		NewMem(),
		NewSwap(),
		NewDisk(),
		NewNet(),
		NewUptime(),
	}
}

// runtimeOSArch returns the GOOS / GOARCH the binary was built for.
// Pulled out into a helper so tests can swap it. Not exported because
// nothing outside this package needs to know.
func runtimeOSArch() (os, arch string) {
	return runtime.GOOS, runtime.GOARCH
}
