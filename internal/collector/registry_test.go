package collector

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeCollector is a controllable Collector used to drive Registry tests
// without depending on the host's real CPU/disk/net counters.
type fakeCollector struct {
	name    string
	samples []Sample
	err     error
	calls   int
}

func (f *fakeCollector) Name() string { return f.name }

func (f *fakeCollector) Collect(_ context.Context) ([]Sample, error) {
	f.calls++
	return f.samples, f.err
}

func TestRegistry_Scrape_MergesAllCollectors(t *testing.T) {
	a := &fakeCollector{name: "a", samples: []Sample{
		{Name: "a.one", Value: 1},
		{Name: "a.two", Value: 2},
	}}
	b := &fakeCollector{name: "b", samples: []Sample{
		{Name: "b.one", Value: 10},
	}}

	hi := HostInfo{Hostname: "h", OS: "linux", Arch: "amd64", Kernel: "k", AgentVersion: "v"}
	r := NewRegistry([]Collector{a, b}, hi, nil, nil)

	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	batch, errs := r.Scrape(context.Background(), now)

	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if got, want := len(batch.Metrics), 3; got != want {
		t.Fatalf("metric count: got %d want %d", got, want)
	}
	if !batch.Ts.Equal(now) {
		t.Errorf("Ts: got %v want %v", batch.Ts, now)
	}
	// Host metadata should ride along on the batch verbatim.
	if batch.Hostname != "h" || batch.OS != "linux" || batch.AgentVersion != "v" {
		t.Errorf("host metadata not stamped: %+v", batch)
	}
}

func TestRegistry_Scrape_PartialFailureStillReturnsGoodSamples(t *testing.T) {
	// Mimic a real-world case: one collector errors but still returns
	// some samples (e.g. disk collector where one fs is offline but
	// the rest worked). The Registry must surface BOTH the partial
	// samples AND the error.
	bad := &fakeCollector{
		name:    "bad",
		samples: []Sample{{Name: "bad.partial", Value: 1}},
		err:     errors.New("one fs unreadable"),
	}
	good := &fakeCollector{name: "good", samples: []Sample{{Name: "good.one", Value: 2}}}

	r := NewRegistry([]Collector{bad, good}, HostInfo{}, nil, nil)
	batch, errs := r.Scrape(context.Background(), time.Now())

	if len(batch.Metrics) != 2 {
		t.Errorf("partial samples lost; got %d want 2", len(batch.Metrics))
	}
	if len(errs) != 1 || errs[0].Collector != "bad" {
		t.Errorf("expected single error from 'bad', got %+v", errs)
	}
	// errors.Unwrap should round-trip so callers can use errors.Is/As.
	if !errors.Is(errs[0], bad.err) {
		t.Errorf("Error must wrap underlying error")
	}
}

func TestRegistry_Allowlist_FiltersToListedNames(t *testing.T) {
	c := &fakeCollector{name: "c", samples: []Sample{
		{Name: "keep.me", Value: 1},
		{Name: "drop.me", Value: 2},
		{Name: "also.keep", Value: 3},
	}}
	r := NewRegistry([]Collector{c}, HostInfo{}, nil, []string{"keep.me", "also.keep"})

	batch, _ := r.Scrape(context.Background(), time.Now())
	if len(batch.Metrics) != 2 {
		t.Fatalf("allowlist failed; got %d samples want 2: %+v", len(batch.Metrics), batch.Metrics)
	}
	for _, s := range batch.Metrics {
		if s.Name == "drop.me" {
			t.Errorf("non-allowlisted sample leaked through")
		}
	}
}

func TestRegistry_StaticLabels_MergedWithoutOverridingCollectorLabels(t *testing.T) {
	// Collector emits an `iface=eth0` label. Static labels include
	// `iface=overridden` and a fresh `env=prod`. The collector's
	// per-sample label MUST win for `iface`, and `env=prod` should be
	// added.
	c := &fakeCollector{name: "c", samples: []Sample{
		{Name: "n", Value: 1, Labels: map[string]string{"iface": "eth0"}},
		{Name: "m", Value: 2}, // no labels at all → should get env=prod
	}}
	static := map[string]string{"iface": "overridden", "env": "prod"}
	r := NewRegistry([]Collector{c}, HostInfo{}, static, nil)

	batch, _ := r.Scrape(context.Background(), time.Now())
	if got := batch.Metrics[0].Labels["iface"]; got != "eth0" {
		t.Errorf("static label clobbered collector label: iface=%q", got)
	}
	if got := batch.Metrics[0].Labels["env"]; got != "prod" {
		t.Errorf("static label not applied to sample with existing labels: env=%q", got)
	}
	if got := batch.Metrics[1].Labels["env"]; got != "prod" {
		t.Errorf("static label not applied to sample with nil labels: env=%q", got)
	}
}

func TestRegistry_StaticLabels_CopiedDefensively(t *testing.T) {
	// The Registry must shallow-copy the labels map at construction
	// time so a later mutation of the caller's map can't leak into
	// future scrapes (a config-reload scenario).
	c := &fakeCollector{name: "c", samples: []Sample{{Name: "n", Value: 1}}}
	static := map[string]string{"env": "prod"}
	r := NewRegistry([]Collector{c}, HostInfo{}, static, nil)

	static["env"] = "TAMPERED"
	delete(static, "env")

	batch, _ := r.Scrape(context.Background(), time.Now())
	if got := batch.Metrics[0].Labels["env"]; got != "prod" {
		t.Errorf("registry leaked external map mutation: env=%q", got)
	}
}

func TestFormatErrors_Empty(t *testing.T) {
	if got := FormatErrors(nil); got != "" {
		t.Errorf("empty input must return \"\", got %q", got)
	}
}

func TestFormatErrors_PrefixesCollectorName(t *testing.T) {
	errs := []Error{
		{Collector: "cpu", Err: errors.New("loadavg missing")},
		{Collector: "disk", Err: errors.New("/data unreadable")},
	}
	got := FormatErrors(errs)
	if !strings.Contains(got, "[cpu] loadavg missing") {
		t.Errorf("missing cpu prefix: %q", got)
	}
	if !strings.Contains(got, "[disk] /data unreadable") {
		t.Errorf("missing disk prefix: %q", got)
	}
}
