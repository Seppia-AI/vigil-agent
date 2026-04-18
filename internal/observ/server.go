package observ

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// MetricsServer is the small HTTP server that exposes /metrics on a
// loopback address (configurable via --metrics-addr). It is INTENDED
// to be reachable only from localhost; we don't ship TLS or auth
// because the threat model is "another process on the same box can
// see operational counters", which is fine — those aren't secrets.
//
// Lifecycle: Start binds the listener immediately so a port conflict
// shows up at startup, then serves in a goroutine. Stop calls Shutdown
// with a 5s deadline; longer than the default ReadTimeout below so
// any in-flight scrape can finish first.
type MetricsServer struct {
	srv *http.Server
	ln  net.Listener
	log *slog.Logger
}

// StartMetricsServer binds `addr` and serves `/metrics` (built from
// `provider`) plus a `/healthz` probe. Returns an error if the bind
// fails — we want startup to fail loud rather than silently lose
// observability. addr="" is a programmer error; main.go gates on
// the empty case.
//
// We deliberately listen on TCP (not Unix socket): every Prometheus
// client on the planet talks TCP, and the loopback restriction is
// enforced via the address ("127.0.0.1:9xxx") rather than the
// transport. Operators who want broader exposure are welcome to
// pass "0.0.0.0:9xxx" and put their own auth in front.
func StartMetricsServer(addr string, provider StatsProvider, agentVersion string, log *slog.Logger) (*MetricsServer, error) {
	if addr == "" {
		return nil, errors.New("observ: metrics addr required")
	}
	if log == nil {
		log = Discard()
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("observ: bind %s: %w", addr, err)
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", MetricsHandler(provider, agentVersion))
	// Liveness probe for container orchestrators / launchd. Returns
	// 200 with body "ok" — no scheduler/sink check, intentionally,
	// because /healthz being 200 should mean "the process is alive
	// and binding the port", not "the agent is successfully shipping".
	// Prom counters cover the latter.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	srv := &http.Server{
		Handler: mux,
		// Conservative timeouts: a Prom scrape is sub-50ms, anything
		// taking >5s is a stuck client we should drop. Prevents a
		// misbehaving scraper from holding a goroutine forever.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	ms := &MetricsServer{srv: srv, ln: ln, log: log}

	go func() {
		ms.log.Info("metrics server listening",
			slog.String("addr", ln.Addr().String()),
			slog.String("path", "/metrics"),
		)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			ms.log.Error("metrics server crashed",
				slog.String("error", err.Error()),
			)
		}
	}()

	return ms, nil
}

// Addr returns the bound listener address. Useful for tests that pass
// addr=":0" to grab an ephemeral port and then need to know which
// port the kernel handed out.
func (m *MetricsServer) Addr() string {
	if m == nil || m.ln == nil {
		return ""
	}
	return m.ln.Addr().String()
}

// Stop shuts the server down with a 5s deadline. Idempotent: calling
// Stop on an already-stopped server is a no-op (Shutdown returns
// http.ErrServerClosed, which we swallow). Safe to defer in main.
func (m *MetricsServer) Stop() {
	if m == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = m.srv.Shutdown(ctx)
}
