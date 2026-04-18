package collector

import (
	"context"
	"fmt"
	"strings"

	"github.com/shirou/gopsutil/v4/net"
)

// NetCollector emits per-interface cumulative counters (raw, since boot):
//
//	net.rx_bytes   {iface="eth0"}    bytes received
//	net.tx_bytes   {iface="eth0"}    bytes transmitted
//	net.rx_errors  {iface="eth0"}    receive errors (CRC, frame, drop)
//	net.tx_errors  {iface="eth0"}    transmit errors
//
// Like the disk IO counters, these are sent RAW. Rate is computed
// server-side at chart time. See package doc-comment in collector.go.
//
// We skip a small set of interfaces that are useless for monitoring:
//   - loopback (`lo`) — always shows traffic to localhost services
//     and dwarfs the real interfaces in absolute numbers.
//   - docker bridges, kubernetes veth pairs, virtual bridges (`docker0`,
//     `br-*`, `veth*`, `cni*`, `flannel*`, `cali*`) — high churn
//     and high cardinality on container-heavy hosts; their absolute
//     numbers also lie because they double-count traffic that exits
//     via the parent interface.
type NetCollector struct{}

// NewNet returns a NetCollector ready to scrape per-interface counters.
func NewNet() *NetCollector { return &NetCollector{} }

// Name implements Collector.
func (*NetCollector) Name() string { return "net" }

// skipIface returns true for interface names we don't want to ship
// samples for. Match is case-insensitive; we check both exact name and
// common prefixes.
func skipIface(name string) bool {
	n := strings.ToLower(name)
	if n == "lo" || n == "lo0" {
		return true
	}
	for _, p := range []string{
		"docker", "br-", "veth", "cni", "flannel", "cali", "weave", "kube-",
	} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// Collect implements Collector.
func (*NetCollector) Collect(_ context.Context) ([]Sample, error) {
	// `true` = per-interface counters. Without it we'd get a single
	// summed entry which would be useless for diagnosing "which NIC
	// is saturated".
	stats, err := net.IOCounters(true)
	if err != nil {
		return nil, fmt.Errorf("net.IOCounters: %w", err)
	}

	out := make([]Sample, 0, len(stats)*4)
	for _, s := range stats {
		if skipIface(s.Name) {
			continue
		}
		labels := map[string]string{"iface": s.Name}
		out = append(out,
			Sample{Name: "net.rx_bytes", Value: float64(s.BytesRecv), Labels: labels},
			Sample{Name: "net.tx_bytes", Value: float64(s.BytesSent), Labels: labels},
			Sample{Name: "net.rx_errors", Value: float64(s.Errin), Labels: labels},
			Sample{Name: "net.tx_errors", Value: float64(s.Errout), Labels: labels},
		)
	}

	return out, nil
}
