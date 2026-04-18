package collector

import (
	"context"
	"strings"
	"time"
)

// Registry runs a fixed list of collectors on each scrape, merges their
// samples into a single Batch, and returns any per-collector errors as
// a slice (NOT an aggregated error string — callers want to log them
// individually with the collector name attached).
//
// The Registry is also responsible for:
//   - stamping the Batch with a single timestamp + the static HostInfo
//   - applying static labels from config (merged into every sample)
//   - applying the metrics_allowlist filter (if non-empty)
//
// It deliberately does NOT do retries, batching across scrapes, or HTTP
// — those live one layer up (in internal/scheduler and internal/ingest).
// Keeping the Registry pure makes `--once` trivial: run it, JSON-encode
// the result, print, exit.
type Registry struct {
	collectors []Collector
	host       HostInfo
	labels     map[string]string
	allowlist  map[string]struct{} // empty = allow all
}

// NewRegistry builds a Registry. `collectors` is typically All() but
// tests inject fakes. `host` is captured once at startup and never
// mutated (see CollectHostInfo). `labels` is the static labels map from
// config; it may be nil. `allowlist` likewise — nil/empty means "ship
// every metric the collectors produce".
//
// Both maps are SHALLOW-COPIED to insulate the Registry from later
// mutations to the originals (the scheduler may reload config in a
// future version; we don't want a half-applied config to leak through).
func NewRegistry(collectors []Collector, hi HostInfo, labels map[string]string, allowlist []string) *Registry {
	r := &Registry{
		collectors: collectors,
		host:       hi,
		labels:     copyLabels(labels),
		allowlist:  toSet(allowlist),
	}
	return r
}

// Error pairs an error with the collector that produced it, so the
// caller can log "[cpu] failed: %v" without losing context. We could
// just use `fmt.Errorf("%s: %w", name, err)` but having the name in a
// struct field makes structured logging easier downstream.
type Error struct {
	Collector string
	Err       error
}

// Error implements the standard error interface.
func (e Error) Error() string {
	return e.Collector + ": " + e.Err.Error()
}

// Unwrap exposes the underlying collector error for errors.Is/As.
func (e Error) Unwrap() error { return e.Err }

// Scrape runs every registered collector once, merges their samples into
// a Batch stamped with `now`, and returns the batch alongside any
// per-collector errors that happened on the way.
//
// Important: a non-nil error slice does NOT mean the batch is empty or
// unusable. Partial scrapes are normal and shippable — e.g. a missing
// /proc/loadavg in a stripped container errors the cpu collector but
// the mem/disk/net samples are still good and the agent should still
// POST them.
func (r *Registry) Scrape(ctx context.Context, now time.Time) (Batch, []Error) {
	all := make([]Sample, 0, 64)
	var errs []Error

	for _, c := range r.collectors {
		samples, err := c.Collect(ctx)
		if err != nil {
			errs = append(errs, Error{Collector: c.Name(), Err: err})
			// Even on error a collector may have returned partial
			// samples (the disk collector does this when one fs
			// errors but the rest succeed). Append whatever we got.
		}
		all = append(all, samples...)
	}

	all = r.applyAllowlist(all)
	all = r.applyStaticLabels(all)

	return Batch{
		Ts:           now.UTC(),
		Hostname:     r.host.Hostname,
		OS:           r.host.OS,
		Arch:         r.host.Arch,
		Kernel:       r.host.Kernel,
		AgentVersion: r.host.AgentVersion,
		Metrics:      all,
	}, errs
}

// applyAllowlist filters out samples whose Name isn't in the configured
// allowlist. An empty allowlist (the default) is a no-op — we ship
// everything the collectors produce.
//
// Match is on the FULL metric name including any per-sample suffix the
// collector added — i.e. "cpu.usage.0" must be listed explicitly if you
// want per-core data through. Operators who want all per-core samples
// can use a wildcard at config time… except wildcards aren't supported
// today. If that becomes a real ask, this is the place to add a glob
// matcher.
func (r *Registry) applyAllowlist(samples []Sample) []Sample {
	if len(r.allowlist) == 0 {
		return samples
	}
	out := samples[:0] // reuse the backing array
	for _, s := range samples {
		if _, ok := r.allowlist[s.Name]; ok {
			out = append(out, s)
		}
	}
	return out
}

// applyStaticLabels merges the configured static labels (env, region,
// etc.) into every sample's Labels map. Conflict resolution: existing
// per-sample labels WIN over static ones. That way a collector that
// emitted `iface=eth0` doesn't get clobbered by a config that mistakenly
// also set `iface=overridden`.
func (r *Registry) applyStaticLabels(samples []Sample) []Sample {
	if len(r.labels) == 0 {
		return samples
	}
	for i := range samples {
		if samples[i].Labels == nil {
			samples[i].Labels = make(map[string]string, len(r.labels))
		}
		for k, v := range r.labels {
			if _, present := samples[i].Labels[k]; !present {
				samples[i].Labels[k] = v
			}
		}
	}
	return samples
}

// FormatErrors returns a human-friendly multiline string summarising the
// errs slice, or "" when the slice is empty. Used by `--once` and the
// daemon's startup logger.
func FormatErrors(errs []Error) string {
	if len(errs) == 0 {
		return ""
	}
	var b strings.Builder
	for i, e := range errs {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("[")
		b.WriteString(e.Collector)
		b.WriteString("] ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

func copyLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func toSet(in []string) map[string]struct{} {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(in))
	for _, s := range in {
		out[s] = struct{}{}
	}
	return out
}
