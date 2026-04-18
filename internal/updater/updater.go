// Package updater implements the agent's "is there a newer release?" check.
//
// The contract is intentionally tiny: every ~24 hours, GET the configured
// version-check URL, parse the three-key payload, compare its `version`
// against this binary's own version.Version, and if newer, log ONE
// `event=update.available` line. That's it. We never auto-download, never
// auto-replace the binary, never restart the daemon. The operator (or apt-
// unattended-upgrades, or whatever they run) decides when to actually
// apply the upgrade.
//
// Why no auto-update:
//   - A buggy auto-update is the single fastest way to take down a fleet
//     of monitoring agents at 3am — exactly the hosts the operator needs
//     to be reachable when something else breaks.
//   - The package channels (apt/yum) and the install.sh re-run path
//     already give operators a working upgrade story.
//   - "There's a newer version" log lines are easy to alert on later
//     (Loki/grep/etc.) without us shipping a self-replacement codepath.
package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultInterval is the period between checks once the agent is up. One
// day is the right cadence: shorter would burn the server's GitHub-API
// budget for no operator-visible benefit (humans don't react to release
// notes faster than that), longer would mean a critical fix sits unseen
// for a week.
const DefaultInterval = 24 * time.Hour

// DefaultInitialDelayMin / Max bound the random startup jitter. Without
// jitter, a rolling restart of an entire fleet would have every agent
// hit the version-check endpoint within the same second of boot — a
// pointless thundering herd on the cache.
//
// 30s lower bound is enough that the agent has finished its first scrape
// (so update-check work doesn't compete with the first batch); 5min
// upper bound spreads even a 1k-agent fleet thinly enough to never spike.
const (
	DefaultInitialDelayMin = 30 * time.Second
	DefaultInitialDelayMax = 5 * time.Minute
)

// httpTimeout caps the entire round trip. The endpoint is heavily cached
// on the server side; a request that takes more than 10s is almost
// certainly a network problem, not a slow upstream. Failing fast keeps
// us from holding a goroutine + socket open across a real outage.
const httpTimeout = 10 * time.Second

// LatestRelease mirrors the JSON payload the server returns. Field tags
// are explicit so a server-side rename would break this at compile-time
// of the tests rather than silently leaving Version empty in production.
type LatestRelease struct {
	Version     string `json:"version"`
	ReleasedAt  string `json:"released_at"`
	DownloadURL string `json:"download_url"`
}

// Options is the union of everything the updater needs to do its job.
// All fields except CurrentVersion + URL have sensible defaults filled
// in by New.
type Options struct {
	// CurrentVersion is the running binary's version (typically
	// version.Version). The literal "dev" — or any other string that
	// fails semver parsing — disables the check entirely with a single
	// debug log line. We never want to nag a developer running from
	// `go run` that v0.1.0 is "newer" than their working copy.
	CurrentVersion string

	// URL is the absolute URL of the server-side version-check endpoint,
	// e.g. https://api.seppia.ai/vigil/agent/latest_version. Required.
	URL string

	// HTTPClient lets tests inject a stub. Defaults to a dedicated
	// client with a 10s timeout — we deliberately do NOT reuse
	// http.DefaultClient because we don't want our tiny one-call-a-day
	// to share connection pool quirks with anyone else's code.
	HTTPClient *http.Client

	// Interval is the delay between successful or unsuccessful checks.
	// Defaults to DefaultInterval (24h).
	Interval time.Duration

	// InitialDelay is the deterministic warm-up wait before the FIRST
	// check fires. If zero, a random value in
	// [DefaultInitialDelayMin, DefaultInitialDelayMax] is used.
	// Tests pin this to a small constant.
	InitialDelay time.Duration

	// Logger is the slog handle the updater writes to. It SHOULD be
	// the same one the rest of the daemon uses so the agent_version
	// binding (set by observ.NewLogger) shows up on every event.
	Logger *slog.Logger

	// UserAgent is the User-Agent header value sent on each check.
	// Defaults to "vigil-agent-updater/<currentVersion>". Stays
	// distinguishable from the ingest sink's UA in server logs.
	UserAgent string
}

// Updater is the long-lived background poller. Construct via New and
// drive via Run; the zero value is not useful.
type Updater struct {
	current      semver
	currentRaw   string
	canCheck     bool // false = parse of CurrentVersion failed → skip silently
	url          string
	client       *http.Client
	interval     time.Duration
	initialDelay time.Duration
	logger       *slog.Logger
	userAgent    string

	mu             sync.Mutex
	lastSeenLatest string // de-dupe: only log update.available when latest changes
}

// New validates Options and fills in defaults. Returns an error only if a
// truly required field is missing (URL); a malformed CurrentVersion is
// tolerated by setting canCheck=false so the daemon can still start —
// disabling the update check is preferable to crashing on every boot just
// because someone built the binary with `-ldflags '-X …Version=mybranch'`.
func New(opts Options) (*Updater, error) {
	if strings.TrimSpace(opts.URL) == "" {
		return nil, errors.New("updater: URL is required")
	}
	if _, err := url.Parse(opts.URL); err != nil {
		return nil, fmt.Errorf("updater: bad URL %q: %w", opts.URL, err)
	}

	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: httpTimeout}
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.UserAgent == "" {
		opts.UserAgent = "vigil-agent-updater/" + nonEmpty(opts.CurrentVersion, "dev")
	}

	u := &Updater{
		currentRaw:   opts.CurrentVersion,
		url:          opts.URL,
		client:       opts.HTTPClient,
		interval:     opts.Interval,
		initialDelay: opts.InitialDelay,
		logger:       opts.Logger,
		userAgent:    opts.UserAgent,
	}

	if cur, err := parseSemver(opts.CurrentVersion); err == nil {
		u.current = cur
		u.canCheck = true
	} else {
		u.logger.Debug("update check disabled: cannot parse running version",
			slog.String("event", "update.disabled"),
			slog.String("running_version", opts.CurrentVersion),
			slog.String("reason", err.Error()),
		)
	}

	return u, nil
}

// Run blocks until ctx is cancelled, performing an update check after
// the initial delay and every Interval thereafter. Errors are NEVER
// returned — the update check is best-effort, and a network blip is not
// something we want to trip the daemon on. Returns nil when ctx is done.
//
// Safe to call exactly once per Updater.
func (u *Updater) Run(ctx context.Context) error {
	if !u.canCheck {
		// canCheck=false means parseSemver failed in New(); the
		// `update.disabled` event was already logged there. Block
		// here so the caller's "go updater.Run(ctx)" pattern still
		// works without surprising early returns.
		<-ctx.Done()
		return nil
	}

	delay := u.initialDelay
	if delay <= 0 {
		delay = randomInitialDelay()
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			u.checkOnce(ctx)
			timer.Reset(u.interval)
		}
	}
}

// checkOnce performs a single fetch+compare. Errors are logged at debug
// (network blips happen, alerting on them would be noise) and swallowed.
// Public only via Run; exposed in this package for the unit test that
// asserts on log output without waiting for a 24h cycle.
func (u *Updater) checkOnce(ctx context.Context) {
	rel, err := u.fetch(ctx)
	if err != nil {
		u.logger.Debug("update check failed",
			slog.String("event", "update.check_failed"),
			slog.String("err", err.Error()),
		)
		return
	}

	latest, err := parseSemver(rel.Version)
	if err != nil {
		u.logger.Debug("update check: server returned unparseable version",
			slog.String("event", "update.check_failed"),
			slog.String("server_version", rel.Version),
			slog.String("err", err.Error()),
		)
		return
	}

	if !isNewer(u.current, latest) {
		u.logger.Debug("update check: already up to date",
			slog.String("event", "update.up_to_date"),
			slog.String("running_version", u.currentRaw),
			slog.String("latest_version", rel.Version),
		)
		return
	}

	// De-dupe: only log update.available the first time we see a
	// given new tag. The flag stays per-process, so a daemon restart
	// resets it — that's the right behaviour, not a bug: an operator
	// who restarts the agent SHOULD see the "you're behind" line in
	// the journal again.
	u.mu.Lock()
	first := u.lastSeenLatest != rel.Version
	u.lastSeenLatest = rel.Version
	u.mu.Unlock()
	if !first {
		return
	}

	u.logger.Info("a newer vigil-agent release is available",
		slog.String("event", "update.available"),
		slog.String("running_version", u.currentRaw),
		slog.String("latest_version", rel.Version),
		slog.String("released_at", rel.ReleasedAt),
		slog.String("download_url", rel.DownloadURL),
	)
}

// fetch performs the HTTP GET. Returns ErrUpstreamUnavailable for any
// non-200; the caller doesn't actually distinguish (everything is "skip
// and try tomorrow") but the wrapped error string ends up in debug logs
// for ops post-mortems.
func (u *Updater) fetch(ctx context.Context) (LatestRelease, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.url, nil)
	if err != nil {
		return LatestRelease{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", u.userAgent)

	resp, err := u.client.Do(req)
	if err != nil {
		return LatestRelease{}, fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Read+drop a small portion of the body so connection reuse
		// works; ignore the read error — we're already on the failure
		// path. 1 KiB is plenty for the server's `{ "error": "..." }`.
		_, _ = io.CopyN(io.Discard, resp.Body, 1024)
		return LatestRelease{}, fmt.Errorf("status %d", resp.StatusCode)
	}

	var rel LatestRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16*1024)).Decode(&rel); err != nil {
		return LatestRelease{}, fmt.Errorf("decode: %w", err)
	}
	if strings.TrimSpace(rel.Version) == "" {
		return LatestRelease{}, errors.New("server returned empty version")
	}
	return rel, nil
}

// randomInitialDelay returns a duration uniformly drawn from
// [DefaultInitialDelayMin, DefaultInitialDelayMax]. Uses math/rand/v2 —
// no need for crypto-grade randomness here, just thundering-herd
// protection.
func randomInitialDelay() time.Duration {
	span := DefaultInitialDelayMax - DefaultInitialDelayMin
	// gosec G404: jitter for thundering-herd avoidance, not a security primitive.
	return DefaultInitialDelayMin + time.Duration(rand.Int64N(int64(span))) //nolint:gosec
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
