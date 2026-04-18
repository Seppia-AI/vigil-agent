package collector

import (
	"context"
	"regexp"
	"strings"
	"testing"
)

// These tests run against the REAL host, not mocks. They're intentionally
// lenient — we only assert shape (names match the standard set, no
// negative bytes, etc.), not specific values which would be flaky.
//
// CI matrix: linux/amd64, linux/arm64, darwin/amd64, darwin/arm64. Every
// collector here must produce SOMETHING on every platform we support
// (with the documented exceptions captured below).

// stdMetricRe is the conservative pattern of metric names this agent
// ships. A sample whose name doesn't match would be rejected at ingest
// — bug we want to catch in CI before it ships.
var stdMetricRe = regexp.MustCompile(`^(cpu\.usage(\.\d+)?|cpu\.load_(1|5|15)m|mem\.(used|total|available|cached)|swap\.used|disk\.(used|total|read_bytes|write_bytes)|net\.(rx|tx)_(bytes|errors)|uptime\.seconds)$`)

func TestClampPct(t *testing.T) {
	cases := []struct {
		in, want float64
	}{
		{-1, 0},
		{0, 0},
		{50, 50},
		{100, 100},
		{103.2, 100},
	}
	for _, c := range cases {
		if got := clampPct(c.in); got != c.want {
			t.Errorf("clampPct(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestCPUCollector_NamesAndShape(t *testing.T) {
	c := NewCPU()
	if c.Name() != "cpu" {
		t.Fatalf("Name() = %q, want cpu", c.Name())
	}
	// First call primes gopsutil's per-call baseline; samples are
	// allowed to be 0.
	_, _ = c.Collect(context.Background())

	samples, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(samples) == 0 {
		t.Fatal("expected at least one sample")
	}
	sawAggregate := false
	for _, s := range samples {
		if !stdMetricRe.MatchString(s.Name) {
			t.Errorf("non-standard metric name: %q", s.Name)
		}
		if s.Name == "cpu.usage" {
			sawAggregate = true
		}
		if strings.HasPrefix(s.Name, "cpu.usage") {
			if s.Value < 0 || s.Value > 100 {
				t.Errorf("cpu.usage out of [0,100]: %s = %v", s.Name, s.Value)
			}
		}
		if strings.HasPrefix(s.Name, "cpu.load_") {
			if s.Value < 0 {
				t.Errorf("negative loadavg: %s = %v", s.Name, s.Value)
			}
		}
	}
	if !sawAggregate {
		t.Error("expected cpu.usage aggregate sample")
	}
}

func TestMemCollector_AllFourSamples(t *testing.T) {
	samples, err := NewMem().Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := map[string]bool{
		"mem.used": false, "mem.total": false,
		"mem.available": false, "mem.cached": false,
	}
	for _, s := range samples {
		if _, ok := want[s.Name]; !ok {
			t.Errorf("unexpected mem metric: %q", s.Name)
			continue
		}
		want[s.Name] = true
		if s.Value < 0 {
			t.Errorf("negative bytes for %s: %v", s.Name, s.Value)
		}
	}
	for n, seen := range want {
		if !seen {
			t.Errorf("missing mem metric: %s", n)
		}
	}
}

func TestSwapCollector_OneSample(t *testing.T) {
	samples, err := NewSwap().Collect(context.Background())
	if err != nil {
		// Sandboxed CI may deny swap stat; tolerate.
		t.Skipf("swap unavailable in this environment: %v", err)
	}
	if len(samples) != 1 || samples[0].Name != "swap.used" {
		t.Fatalf("expected single swap.used sample, got %+v", samples)
	}
	if samples[0].Value < 0 {
		t.Errorf("negative swap.used: %v", samples[0].Value)
	}
}

func TestDiskCollector_HasUsageOrIO(t *testing.T) {
	samples, err := NewDisk().Collect(context.Background())
	// Partial errors are allowed; we only fail if we got NOTHING.
	if err != nil && len(samples) == 0 {
		t.Fatalf("disk: total failure: %v", err)
	}
	for _, s := range samples {
		if !stdMetricRe.MatchString(s.Name) {
			t.Errorf("non-standard disk metric: %q", s.Name)
		}
		if s.Value < 0 {
			t.Errorf("negative %s: %v", s.Name, s.Value)
		}
		// Per-fs samples MUST carry mount + device labels; per-IO
		// samples MUST carry device. Catches accidental label drops.
		switch s.Name {
		case "disk.used", "disk.total":
			if s.Labels["mount"] == "" || s.Labels["device"] == "" {
				t.Errorf("missing labels on %s: %+v", s.Name, s.Labels)
			}
		case "disk.read_bytes", "disk.write_bytes":
			if s.Labels["device"] == "" {
				t.Errorf("missing device label on %s", s.Name)
			}
		}
	}
}

func TestNetCollector_SkipsLoopbackAndContainerBridges(t *testing.T) {
	samples, err := NewNet().Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, s := range samples {
		if !stdMetricRe.MatchString(s.Name) {
			t.Errorf("non-standard net metric: %q", s.Name)
		}
		iface := s.Labels["iface"]
		if iface == "" {
			t.Errorf("missing iface label on %s", s.Name)
		}
		if skipIface(iface) {
			t.Errorf("filtered iface %q leaked through", iface)
		}
		if s.Value < 0 {
			t.Errorf("negative %s on %s: %v", s.Name, iface, s.Value)
		}
	}
}

func TestSkipIface(t *testing.T) {
	cases := map[string]bool{
		"lo":         true,
		"lo0":        true,
		"docker0":    true,
		"br-1234":    true,
		"veth1a2b":   true,
		"cni0":       true,
		"flannel.1":  true,
		"cali12345":  true,
		"weave":      true,
		"kube-ipvs0": true,
		"eth0":       false,
		"en0":        false,
		"wlan0":      false,
		"":           false, // we don't drop the empty string here; collector should never produce it
	}
	for n, want := range cases {
		if got := skipIface(n); got != want {
			t.Errorf("skipIface(%q) = %v, want %v", n, got, want)
		}
	}
}

func TestUptimeCollector(t *testing.T) {
	samples, err := NewUptime().Collect(context.Background())
	if err != nil {
		t.Skipf("uptime unavailable in this environment: %v", err)
	}
	if len(samples) != 1 || samples[0].Name != "uptime.seconds" {
		t.Fatalf("expected single uptime.seconds, got %+v", samples)
	}
	if samples[0].Value <= 0 {
		t.Errorf("uptime should be > 0: %v", samples[0].Value)
	}
}

func TestCollectHostInfo_PopulatesOSAndArch(t *testing.T) {
	hi := CollectHostInfo()
	if hi.OS == "" || hi.Arch == "" {
		t.Errorf("OS/Arch should always be set: %+v", hi)
	}
	// Hostname can theoretically be empty in extremely stripped
	// containers, but our test environment always has one. Assert
	// loosely so a containerised CI doesn't break us.
	if hi.Hostname == "" {
		t.Logf("note: hostname empty (may be normal in stripped container)")
	}
}

func TestAll_ReturnsExpectedFamilies(t *testing.T) {
	got := map[string]bool{}
	for _, c := range All() {
		got[c.Name()] = true
	}
	for _, want := range []string{"cpu", "mem", "swap", "disk", "net", "uptime"} {
		if !got[want] {
			t.Errorf("All() missing %q collector", want)
		}
	}
}
