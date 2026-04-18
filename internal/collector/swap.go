package collector

import (
	"context"
	"fmt"

	"github.com/shirou/gopsutil/v4/mem"
)

// SwapCollector emits one gauge:
//
//	swap.used    bytes of swap currently in use
//
// We emit this even on hosts with no swap configured (value = 0); the
// server's chart shows a flat line at zero which is a useful "no swap
// in play" signal vs. nothing on the chart at all.
//
// swap.total / swap.free are not currently emitted; if added later, this
// collector grows two more samples — no API changes needed.
type SwapCollector struct{}

// NewSwap returns a SwapCollector ready to scrape swap.used.
func NewSwap() *SwapCollector { return &SwapCollector{} }

// Name implements Collector.
func (*SwapCollector) Name() string { return "swap" }

// Collect implements Collector.
func (*SwapCollector) Collect(_ context.Context) ([]Sample, error) {
	s, err := mem.SwapMemory()
	if err != nil {
		return nil, fmt.Errorf("mem.SwapMemory: %w", err)
	}
	return []Sample{
		{Name: "swap.used", Value: float64(s.Used)},
	}, nil
}
