package collector

import (
	"context"
	"fmt"
	"strings"

	"github.com/shirou/gopsutil/v4/disk"
)

// DiskCollector emits two families of samples.
//
// Per-filesystem usage gauges, labelled with mountpoint + device:
//
//	disk.used   {mount="/", device="/dev/sda1"}
//	disk.total  {mount="/", device="/dev/sda1"}
//
// Per-device cumulative IO counters (bytes since boot), labelled with
// the device name:
//
//	disk.read_bytes   {device="sda"}
//	disk.write_bytes  {device="sda"}
//
// The IO counters are sent RAW (monotonically increasing); rate is
// computed server-side at chart time. See package doc-comment in
// collector.go.
//
// Filesystems we deliberately skip:
//   - any whose fstype is in skipFstypes (tmpfs, devtmpfs, overlay, …) —
//     they're either zero-sized, ephemeral, or massively over-counted
//     when several containers share an overlay.
//   - bind mounts on Linux (gopsutil exposes them as separate entries
//     pointing at the same physical device, double-counting the same
//     bytes). We dedupe by device name.
//
// These filters are conservative on purpose: we'd rather miss a niche
// fs than ship a misleading "host is 200% full" chart.
type DiskCollector struct{}

// NewDisk returns a DiskCollector ready to scrape per-fs and per-device samples.
func NewDisk() *DiskCollector { return &DiskCollector{} }

// Name implements Collector.
func (*DiskCollector) Name() string { return "disk" }

// skipFstypes are filesystems that are ephemeral, virtual, or otherwise
// not interesting for capacity planning. lowercase-matched.
var skipFstypes = map[string]struct{}{
	"tmpfs":       {},
	"devtmpfs":    {},
	"devfs":       {},
	"proc":        {},
	"sysfs":       {},
	"cgroup":      {},
	"cgroup2":     {},
	"overlay":     {},
	"squashfs":    {},
	"autofs":      {},
	"binfmt_misc": {},
	"debugfs":     {},
	"mqueue":      {},
	"hugetlbfs":   {},
	"pstore":      {},
	"securityfs":  {},
	"tracefs":     {},
}

// Collect implements Collector.
func (*DiskCollector) Collect(_ context.Context) ([]Sample, error) {
	out := make([]Sample, 0, 16)

	// Per-fs usage. `false` = physical filesystems only (no /proc et al
	// in the first pass) but gopsutil's idea of "physical" is loose so
	// we filter again ourselves.
	parts, err := disk.Partitions(false)
	if err != nil {
		// Don't bail — IO counters might still work.
		// We surface the partial error to the caller.
		err = fmt.Errorf("disk.Partitions: %w", err)
	}

	seenDev := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		if _, skip := skipFstypes[strings.ToLower(p.Fstype)]; skip {
			continue
		}
		// Bind-mount dedupe: if we've already counted this device,
		// skip the duplicate. The first one wins (usually the
		// canonical mountpoint).
		if _, dup := seenDev[p.Device]; dup {
			continue
		}
		seenDev[p.Device] = struct{}{}

		usage, err := disk.Usage(p.Mountpoint)
		if err != nil {
			// One bad fs (offline NFS, sleeping disk, …) shouldn't
			// kill the whole collector. Skip it.
			continue
		}
		labels := map[string]string{
			"mount":  p.Mountpoint,
			"device": p.Device,
		}
		out = append(out,
			Sample{Name: "disk.used", Value: float64(usage.Used), Labels: labels},
			Sample{Name: "disk.total", Value: float64(usage.Total), Labels: labels},
		)
	}

	// Per-device IO. `nil` = all physical devices (loop and dm-* are
	// hidden by gopsutil already on Linux). We emit raw counters; rate
	// is the server's job.
	io, ioErr := disk.IOCounters()
	if ioErr != nil {
		if err != nil {
			return out, fmt.Errorf("%w; disk.IOCounters: %w", err, ioErr)
		}
		return out, fmt.Errorf("disk.IOCounters: %w", ioErr)
	}
	for dev, c := range io {
		labels := map[string]string{"device": dev}
		out = append(out,
			Sample{Name: "disk.read_bytes", Value: float64(c.ReadBytes), Labels: labels},
			Sample{Name: "disk.write_bytes", Value: float64(c.WriteBytes), Labels: labels},
		)
	}

	return out, err
}
