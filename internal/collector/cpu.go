package collector

import (
	"context"
	"fmt"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/load"
)

// CPUCollector emits:
//
//	cpu.usage             aggregated busy% across all cores      gauge 0–100
//	cpu.usage.<N>         per-core busy% (label core="0"..."N-1") gauge 0–100
//	cpu.load_1m           5/15-minute load averages              gauge
//	cpu.load_5m
//	cpu.load_15m
//
// Implementation notes:
//
//   - cpu.Percent(0, *) is non-blocking and returns the percentage of CPU
//     time consumed *since the last call to cpu.Percent on the same
//     measurement (per-CPU vs aggregate)*. gopsutil keeps that state in
//     a package-level map keyed on the boolean. So the very first scrape
//     after agent start returns 0 (no baseline yet) — that's fine, we
//     emit it anyway for shape consistency, and the second sample
//     onwards is real.
//
//   - load averages come from /proc/loadavg on Linux and the equivalent
//     sysctl on macOS. They're cheap and stateless.
//
//   - We do NOT emit cpu.user/cpu.system/cpu.iowait splits even though
//     gopsutil exposes them. The metric surface is intentionally narrow;
//     finer breakdowns can be added later without breaking the wire shape.
type CPUCollector struct{}

// NewCPU constructs a CPU collector. There is no state on the struct
// because gopsutil already keeps the per-call baseline internally.
func NewCPU() *CPUCollector { return &CPUCollector{} }

// Name implements Collector.
func (*CPUCollector) Name() string { return "cpu" }

// Collect implements Collector.
func (*CPUCollector) Collect(_ context.Context) ([]Sample, error) {
	out := make([]Sample, 0, 8)

	// Aggregate usage. cpu.Percent(0, false) returns a 1-element slice
	// with the system-wide busy percentage since the previous call.
	if pct, err := cpu.Percent(0, false); err == nil && len(pct) == 1 {
		out = append(out, Sample{
			Name:  "cpu.usage",
			Value: clampPct(pct[0]),
		})
	} else if err != nil {
		// Don't bail — we still want to attempt per-core + load.
		// The Registry surfaces partial errors via its error slice.
		return out, fmt.Errorf("cpu.Percent(aggregate): %w", err)
	}

	// Per-core usage. Same caveat about the very-first call returning
	// zeros; subsequent ticks are accurate.
	if perCore, err := cpu.Percent(0, true); err == nil {
		for i, v := range perCore {
			out = append(out, Sample{
				Name:  fmt.Sprintf("cpu.usage.%d", i),
				Value: clampPct(v),
				// Also tag with `core=<N>` so the server can
				// aggregate by label without parsing the metric
				// name. Cheap insurance; the label is bounded
				// by core count which is itself small.
				Labels: map[string]string{"core": fmt.Sprintf("%d", i)},
			})
		}
	}

	// Load averages. Cheap; ignore the error if /proc/loadavg isn't
	// available (some containers strip it) — that's a degraded host,
	// not an agent bug.
	if avg, err := load.Avg(); err == nil && avg != nil {
		out = append(out,
			Sample{Name: "cpu.load_1m", Value: avg.Load1},
			Sample{Name: "cpu.load_5m", Value: avg.Load5},
			Sample{Name: "cpu.load_15m", Value: avg.Load15},
		)
	}

	return out, nil
}

// clampPct guards against gopsutil's occasional > 100 readings on busy
// systems where the sampling window catches multiple ticks. The server's
// chart expects 0–100 and a value of 103.2 looks like a bug to operators.
func clampPct(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	default:
		return v
	}
}
