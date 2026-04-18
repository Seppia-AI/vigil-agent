package observ

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// StatsProvider is the dependency the metrics handler needs from the
// scheduler. Keeping it as a tiny interface (rather than importing
// scheduler.Stats directly) breaks the import cycle that would
// otherwise force scheduler → observ → scheduler, and makes the
// handler trivially testable with a hand-rolled stub.
//
// All values are read once per scrape; no locking required because
// scheduler.Stats already returns an atomically-loaded snapshot.
type StatsProvider interface {
	StatsSnapshot() StatsSnapshot
}

// StatsSnapshot is the wire-format-friendly mirror of scheduler.Stats.
// Lives here (not in scheduler) so observ has zero internal imports
// and so we can evolve the Prom exposition independently of the
// scheduler's internal counters.
type StatsSnapshot struct {
	ScrapesOK           uint64
	ScrapesPartial      uint64
	ScrapesEmpty        uint64
	SamplesCollected    uint64
	BatchesSent         uint64
	BatchesFailed       uint64
	DroppedOverflow     uint64
	DroppedQuotaSamples uint64
	LastFlushUnix       int64
}

// metricSpec describes one Prometheus exposition row. The ordering
// matters: we sort by name in WriteTo so two consecutive scrapes
// produce byte-identical output (modulo the values), which makes
// diffing scrapes during debugging straightforward.
type metricSpec struct {
	name string
	typ  string // "counter" or "gauge"
	help string
	get  func(StatsSnapshot) float64
	// labels is empty today — every counter is global. The slice
	// exists so adding `samples_dropped_total{reason="…"}` later is
	// a one-line change rather than a refactor.
	labels []labelKV
}

type labelKV struct{ k, v string }

// builtin is the canonical list of metrics exposed at /metrics.
// `vigil_agent_` prefix follows the Prometheus naming convention; the
// `_total` suffix on counters and the `_unix` suffix on the wall-clock
// gauge are also conventional and will let prometheus's `rate()`
// function and `time() - … > N` style alerts work without surprise.
var builtin = []metricSpec{
	{"vigil_agent_scrapes_ok_total", "counter",
		"Scrapes that produced at least one sample with no collector errors.",
		func(s StatsSnapshot) float64 { return float64(s.ScrapesOK) }, nil},
	{"vigil_agent_scrapes_partial_total", "counter",
		"Scrapes where at least one collector errored but others succeeded.",
		func(s StatsSnapshot) float64 { return float64(s.ScrapesPartial) }, nil},
	{"vigil_agent_scrapes_empty_total", "counter",
		"Scrapes where every collector returned no samples.",
		func(s StatsSnapshot) float64 { return float64(s.ScrapesEmpty) }, nil},
	{"vigil_agent_samples_collected_total", "counter",
		"Total samples produced by collectors (across all scrapes).",
		func(s StatsSnapshot) float64 { return float64(s.SamplesCollected) }, nil},
	{"vigil_agent_batches_sent_total", "counter",
		"Batches successfully POSTed to the ingest endpoint.",
		func(s StatsSnapshot) float64 { return float64(s.BatchesSent) }, nil},
	{"vigil_agent_batches_failed_total", "counter",
		"Send attempts that returned a retriable sink error (5xx / network / 429).",
		func(s StatsSnapshot) float64 { return float64(s.BatchesFailed) }, nil},
	{"vigil_agent_dropped_overflow_total", "counter",
		"Batches evicted from the in-memory buffer because newer ones arrived during an outage.",
		func(s StatsSnapshot) float64 { return float64(s.DroppedOverflow) }, nil},
	{"vigil_agent_dropped_quota_samples_total", "counter",
		"Samples the server reported as dropped due to per-minute quota.",
		func(s StatsSnapshot) float64 { return float64(s.DroppedQuotaSamples) }, nil},
	{"vigil_agent_last_flush_unix", "gauge",
		"Wall-clock seconds of the last successful Send. 0 = never. Use `time() - vigil_agent_last_flush_unix > 300` to alert on a stuck agent.",
		func(s StatsSnapshot) float64 { return float64(s.LastFlushUnix) }, nil},
}

// MetricsHandler returns an http.Handler that renders the Prometheus
// text exposition format on every GET. It also exposes
// `vigil_agent_build_info{version="…"} 1` so dashboards can show
// which version is running without needing a separate label propagator.
//
// We refuse non-GET methods with 405 — `prometheus_target_interval`
// scrapers only ever GET, and accepting POSTs would risk somebody
// confusing /metrics with /vigil/vitals/<token>.
func MetricsHandler(p StatsProvider, agentVersion string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Standard exposition Content-Type. The version=0.0.4 tag
		// is the long-stable "text exposition format" version and
		// is what every Prometheus client accepts.
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		writeExposition(w, p.StatsSnapshot(), agentVersion)
	})
}

// writeExposition renders the metrics for a single snapshot. Pulled
// out of the handler so tests can call it directly with a synthetic
// snapshot rather than spinning up an httptest server for every assert.
func writeExposition(w io.Writer, snap StatsSnapshot, agentVersion string) {
	// Sort by name once so the output is deterministic regardless of
	// the order metrics were added to `builtin` above. Cheap (n=9).
	sorted := make([]metricSpec, len(builtin))
	copy(sorted, builtin)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].name < sorted[j].name })

	for _, m := range sorted {
		fmt.Fprintf(w, "# HELP %s %s\n", m.name, escapeHelp(m.help))
		fmt.Fprintf(w, "# TYPE %s %s\n", m.name, m.typ)
		writeSample(w, m.name, m.labels, m.get(snap))
	}

	// Build info goes last so it visually clusters at the bottom
	// of the response — convention from the official Go client.
	if agentVersion == "" {
		agentVersion = "dev"
	}
	fmt.Fprintln(w, "# HELP vigil_agent_build_info Agent build info; constant 1 with version label.")
	fmt.Fprintln(w, "# TYPE vigil_agent_build_info gauge")
	writeSample(w, "vigil_agent_build_info",
		[]labelKV{{"version", agentVersion}}, 1)
}

// writeSample renders one `name{labels} value` line per the Prometheus
// text format. We omit the optional timestamp suffix because Prometheus
// itself ignores it on scrapes (only useful for federated proxies,
// which the agent doesn't target).
//
// formatFloat is used (not %g) because Prometheus is strict about
// scientific-notation digits — `%g` would emit `1e+06` which the
// scraper accepts but is visually unfriendly in `curl /metrics`.
func writeSample(w io.Writer, name string, labels []labelKV, value float64) {
	if len(labels) == 0 {
		fmt.Fprintf(w, "%s %s\n", name, formatFloat(value))
		return
	}
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for i, kv := range labels {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(kv.k)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(kv.v))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	fmt.Fprintf(w, "%s %s\n", b.String(), formatFloat(value))
}

// formatFloat picks the most compact lossless rendering of v that the
// Prometheus exposition format accepts. Integer values (the common
// case for our counters) emit without a decimal point so a curl of
// /metrics looks like `vigil_agent_scrapes_ok_total 47` rather than
// `… 47.000000`.
func formatFloat(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
}

// escapeHelp escapes the only two characters the Prom text format
// requires escaping inside HELP text: backslash and newline. Quotes
// are NOT escaped here (they are inside label VALUES, but not HELP).
func escapeHelp(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	return r.Replace(s)
}

// escapeLabelValue escapes the three characters the format requires:
// backslash, double-quote, and newline. Used by label-bearing metrics
// (currently just build_info), kept ready for samples_dropped_total.
func escapeLabelValue(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}
