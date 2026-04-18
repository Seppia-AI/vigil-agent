package collector

import (
	"context"
	"fmt"

	"github.com/shirou/gopsutil/v4/host"
)

// UptimeCollector emits one gauge:
//
//	uptime.seconds   seconds since the kernel booted
//
// Exposing this as a separate metric (rather than computing
// `time.Since(BootTime)` client-side) is useful for two reasons:
//
//   1. The chart can clearly show reboots as drops to ~0 — we get
//      "host restarted" detection without any extra signalling.
//   2. The number is monotonic and bounded — handy as a "is the agent
//      reaching the host at all?" canary that's independent of any
//      counter that might legitimately stall (idle network, no IO).
type UptimeCollector struct{}

func NewUptime() *UptimeCollector { return &UptimeCollector{} }

func (*UptimeCollector) Name() string { return "uptime" }

func (*UptimeCollector) Collect(_ context.Context) ([]Sample, error) {
	u, err := host.Uptime()
	if err != nil {
		return nil, fmt.Errorf("host.Uptime: %w", err)
	}
	return []Sample{
		{Name: "uptime.seconds", Value: float64(u)},
	}, nil
}
