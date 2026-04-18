package collector

import (
	"context"
	"fmt"

	"github.com/shirou/gopsutil/v4/mem"
)

// MemCollector emits virtual-memory gauges in BYTES:
//
//	mem.used        bytes currently in use (Total - Available)
//	mem.total       physical RAM size
//	mem.available   bytes the kernel says can be allocated without swap
//	mem.cached      page-cache + buffers (Linux); 0 on platforms that
//	                don't track it separately, which is fine — the
//	                server treats absence the same as zero.
//
// We deliberately use the kernel's `Available` figure (MemAvailable on
// Linux) rather than `Total - (Used + Cached + Buffers)` math because
// MemAvailable already accounts for reclaimable slab + page cache and
// matches what `free -m` shows users. The classic "Linux ate my RAM"
// confusion is much rarer with this number.
type MemCollector struct{}

// NewMem returns a MemCollector ready to scrape virtual-memory gauges.
func NewMem() *MemCollector { return &MemCollector{} }

// Name implements Collector.
func (*MemCollector) Name() string { return "mem" }

// Collect implements Collector.
func (*MemCollector) Collect(_ context.Context) ([]Sample, error) {
	v, err := mem.VirtualMemory()
	if err != nil {
		return nil, fmt.Errorf("mem.VirtualMemory: %w", err)
	}

	// Used is computed by gopsutil to match what `free` reports — i.e.
	// it deliberately excludes buffers/cache. On Linux this is
	// Total - Free - Buffers - Cached - Slab(Reclaimable). Keep it as
	// reported rather than recomputing here.
	return []Sample{
		{Name: "mem.used", Value: float64(v.Used)},
		{Name: "mem.total", Value: float64(v.Total)},
		{Name: "mem.available", Value: float64(v.Available)},
		{Name: "mem.cached", Value: float64(v.Cached)},
	}, nil
}
