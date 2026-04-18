// Package exitcode centralises the agent's process exit codes.
//
// These values are PUBLIC contract — the install script and the systemd
// unit pattern-match on them ("Restart=on-failure" only kicks in for
// non-zero exits, and our installer treats Config vs Runtime errors
// differently). Don't change a value once shipped; add a new one instead.
//
// Keep this list in sync with the table in README.md.
package exitcode

const (
	// OK is a clean exit: graceful shutdown, --check-config success,
	// --version, --help, --once on a successful scrape, etc.
	OK = 0

	// Config signals a configuration problem: missing required field,
	// unparseable YAML, invalid value, file not found when an explicit
	// --config path was supplied. Operator action required; no amount
	// of restarting will fix this, so systemd units should NOT auto-
	// retry on this code (use `RestartPreventExitStatus=1`).
	Config = 1

	// Runtime is an unrecoverable runtime error: token revoked by the
	// server (404), repeated 4xx from ingest, panic recovered at the
	// top level, etc. systemd CAN retry these (transient network /
	// DNS issues live here too) but with backoff.
	Runtime = 2

	// Usage is a CLI misuse: unknown flag, conflicting flags. The Go
	// `flag` package exits with 2 by convention, but our `Runtime`
	// already owns 2 — main.go translates flag-parse failures to this
	// value before exiting.
	Usage = 3
)
