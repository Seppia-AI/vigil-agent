# vigil-agent

The host metrics agent for [Seppia Vigil Vitals](https://seppia.ai/vigil).

A small static Go binary that scrapes a fixed set of system metrics
(CPU, memory, swap, disk, network, uptime) and ships them to the Vigil
ingest endpoint as JSON. One per host. No dependencies, no daemon stack,
no agent-of-the-agent.

> Status: pre-1.0, in active development. Not yet production-ready.

---

## Why a custom agent?

Vigil Vitals could in principle accept Prometheus `node_exporter`
metrics or an OpenTelemetry Collector pipeline, and may in the future.
For now this agent is the supported path. It is:

- A **single binary** the operator drops on the host. No JVM, no Python,
  no Helm chart, no sidecar.
- A **fixed metric surface** that maps 1:1 to what the Vigil UI knows
  how to chart, so a fresh probe is useful in 30 seconds without
  configuring relabel rules.
- An **opinionated default** for scrape interval, batching, back-off
  and label cardinality — so a fresh deployment behaves well without
  any tuning.

---

## Install

The one-line installer is the supported path. It detects OS/arch,
downloads the matching release tarball, verifies its sha256 against the
published `SHA256SUMS`, drops the binary at `/usr/local/bin/vigil-agent`,
writes a minimal config at `/etc/seppia/vigil.yml`, and (on systemd
hosts) enables `vigil-agent.service`. macOS gets the binary + config;
launchd integration is not yet included.

```sh
curl -fsSL https://get.seppia.ai/vigil | sudo sh -s -- --token=<probe_token>
```

The `--token` value is shown in the **Install & verify** snippet on the
probe's detail page in the Vigil dashboard, pre-filled. Each probe has
its own token.

Re-run the same command any time to **upgrade in place** (existing config
preserved). To remove the agent:

```sh
curl -fsSL https://get.seppia.ai/vigil | sudo sh -s -- --uninstall
```

Uninstall stops + disables the systemd unit and removes the binary; it
deliberately leaves `/etc/seppia/vigil.yml` and the `vigil` system user
in place so a re-install picks up the same probe without re-tokenising.

### Installer flags

| Flag                  | Purpose                                                                                  |
|-----------------------|------------------------------------------------------------------------------------------|
| `--token=<tok>`       | Probe token. Required on first install; ignored on upgrade.                              |
| `--version=<ver>`     | Pin a specific release (e.g. `v0.1.3`). Default: GitHub's latest release.                |
| `--ingest-url=<url>`  | Vigil API base URL. Default: `https://api.seppia.ai`. Set for self-hosted environments.  |
| `--scrape-interval=<s>` | Seconds between scrapes. Default: agent's compiled-in default (60s).                   |
| `--prefix=<dir>`      | Install prefix (binary → `<prefix>/bin/vigil-agent`). Default: `/usr/local`.             |
| `--config=<path>`     | Config file path. Default: `/etc/seppia/vigil.yml`.                                      |
| `--no-systemd`        | Skip systemd setup. Auto-enabled on macOS and non-systemd Linux hosts.                   |
| `--force-config`      | Overwrite an existing config file (DANGEROUS — wipes any local edits).                   |
| `--dry-run`           | Print every step without executing. Safe to pipe.                                        |
| `--uninstall`         | Stop + remove the agent. Leaves config and user account intact.                          |
| `--help`              | Full usage.                                                                              |

The installer is **POSIX `sh`** — it runs unchanged on Alpine / BusyBox,
FreeBSD `sh`, and any GNU bash. The source lives at
[`scripts/install.sh`](./scripts/install.sh) in this repo and is what
`get.seppia.ai/vigil` serves; auditing it before piping curl into a
root shell is encouraged.

Manual download (e.g. for air-gapped installs) is available on the
[Releases](https://github.com/Seppia-AI/vigil-agent/releases) page.

---

## Configuration

Config lives at `/etc/seppia/vigil.yml`. Every field can be overridden
by an environment variable.

| YAML key             | Env var                   | Default              | Notes                                                                     |
|----------------------|---------------------------|----------------------|---------------------------------------------------------------------------|
| `ingest_url`         | `VIGIL_INGEST_URL`        | `https://api.seppia.ai` | Vigil API base URL. The agent appends `/vigil/vitals/<token>`.         |
| `token`              | `VIGIL_TOKEN`             | _(required)_         | Probe token. Created in the Vigil UI; revocable; rotatable.               |
| `scrape_interval_s`  | `VIGIL_SCRAPE_INTERVAL_S` | `60`                 | Seconds between scrapes. The server may clamp this to a per-account minimum on first POST. |
| `labels`             | `VIGIL_LABELS`            | _(empty)_            | Static `key=value` labels merged into every sample. Max 8 (server cap).   |
| `metrics_allowlist`  | `VIGIL_METRICS_ALLOWLIST` | _(empty = all standard)_ | Optional client-side filter. Server filters too — this is for noisy hosts. |
| `log_level`          | `VIGIL_LOG_LEVEL`         | `info`               | `debug`, `info`, `warn`, `error`.                                         |
| `disable_update_check` | `VIGIL_DISABLE_UPDATE_CHECK` | `false`           | When `true`, suppress the once-a-day check against `<ingest_url>/vigil/agent/latest_version`. CLI: `--no-update-check`. |
| `update_check_url`   | `VIGIL_UPDATE_CHECK_URL`  | _(derived)_          | Override the URL the updater polls. Empty = `<ingest_url>/vigil/agent/latest_version`. |

`vigil-agent --check-config` validates the merged config and exits
non-zero on the first problem (missing token, unreachable ingest URL,
oversized labels map, etc.).

---

## CLI

```
vigil-agent                       # run the daemon (default; uses /etc/seppia/vigil.yml)
vigil-agent run                   # explicit alias of the above (legibility for systemd ExecStart)
vigil-agent --version             # print build info
vigil-agent --check-config        # validate config and exit
vigil-agent --once                # scrape once, print JSON to stdout, exit (no network)
vigil-agent --dry-run             # daemon, but log batches to stderr instead of POSTing
vigil-agent --insecure            # DEV ONLY: skip TLS certificate verification
vigil-agent --log-format=json     # text (default) or json
vigil-agent --log-level=debug     # debug | info (default) | warn | error
vigil-agent --metrics-addr=127.0.0.1:9090   # opt-in /metrics + /healthz endpoint
vigil-agent --drain-timeout=5s    # max time to flush buffered batches after SIGTERM (0 = drop)
vigil-agent --no-update-check     # suppress the once-a-day "newer release?" check
```

### Update check

Once a day the agent GETs `<ingest_url>/vigil/agent/latest_version` and
parses a tiny JSON body:

```json
{
  "version":      "v0.1.3",
  "released_at":  "2026-04-15T08:00:00Z",
  "download_url": "https://github.com/Seppia-AI/vigil-agent/releases/tag/v0.1.3"
}
```

If the returned `version` is strictly newer than this binary's
`version.Version`, the agent logs **one** line:

```
event=update.available  running_version=v0.1.0  latest_version=v0.1.3  released_at=2026-04-15T08:00:00Z  download_url=...
```

That's the entire feature surface. The agent **never** auto-downloads or
restarts; the operator (or `apt-get upgrade`, or a re-run of the
installer) decides when to apply the upgrade. Subsequent checks against
the same `latest_version` stay silent (de-duped per process), and any
upstream error (network, 5xx, parse failure) is logged at `debug` and
otherwise swallowed.

Set `disable_update_check: true` in the YAML, `VIGIL_DISABLE_UPDATE_CHECK=1`
in the environment, or pass `--no-update-check` at startup to suppress
the check entirely. Builds whose `Version` doesn't parse as semver
(e.g. local `go run` from source, where it's `"dev"`) auto-disable the
check with a single debug log line.

### Process lifecycle

On `SIGTERM` / `SIGINT` the agent enters a bounded **drain phase**:

1. The scrape ticker stops — no new batches enter the buffer.
2. The send loop continues popping and POSTing whatever is still
   queued, with the same back-off rules as in steady state.
3. After `--drain-timeout` (default 5 s) the agent exits regardless;
   any batches still in the buffer are abandoned and a
   `event=drain.deadline_exceeded` line is logged.

A clean drain logs `event=drain.complete` and returns exit 0. Pair the
flag value with your systemd unit's `TimeoutStopSec` — drain should be
strictly less, otherwise systemd will SIGKILL mid-flush.

### Exit codes

Stable; the canonical source is [`internal/exitcode`](./internal/exitcode/exitcode.go).
The install script and the packaged systemd unit pattern-match on them:

| Code | Name      | Meaning                                                                              |
|-----:|-----------|--------------------------------------------------------------------------------------|
| `0`  | `OK`      | clean exit — graceful drain, `--check-config` / `--once` / `--version` success       |
| `1`  | `Config`  | config error or fatal sink response — missing token, bad URL, **token revoked (404)** |
| `2`  | `Runtime` | unexpected scheduler / runtime error (transient retries OK) |
| `3`  | `Usage`   | bad CLI flag or unknown positional                                                    |

Recommended systemd snippet:

```ini
Restart=on-failure
RestartSec=5s
RestartPreventExitStatus=1 3   # don't loop on Config/Usage errors
TimeoutStopSec=10s             # > vigil-agent --drain-timeout
```

---

## License

Apache-2.0. See [LICENSE](./LICENSE).

The agent is open source so operators can audit what runs as root on
their hosts.
