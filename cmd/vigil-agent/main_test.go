package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Seppia-AI/vigil-agent/internal/config"
	"github.com/Seppia-AI/vigil-agent/internal/exitcode"
)

// withCleanEnv mirrors the helper in the config package — duplicated
// here so cmd tests don't have to import test code from another
// package. Keep the env list in sync with config.allEnvVars.
func withCleanEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		config.EnvConfig,
		config.EnvIngestURL,
		config.EnvToken,
		config.EnvScrapeIntervalS,
		config.EnvLabels,
		config.EnvMetricsAllow,
		config.EnvLogLevel,
		config.EnvLogFormat,
	}
	saved := map[string]string{}
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok {
			saved[k] = v
		}
		_ = os.Unsetenv(k)
	}
	t.Cleanup(func() {
		for _, k := range keys {
			if v, ok := saved[k]; ok {
				_ = os.Setenv(k, v)
			} else {
				_ = os.Unsetenv(k)
			}
		}
	})
}

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "vigil.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestRun_Version(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--version"}, &stdout, &stderr)

	if code != exitcode.OK {
		t.Errorf("exit = %d, want %d", code, exitcode.OK)
	}
	if !strings.Contains(stdout.String(), "vigil-agent") {
		t.Errorf("stdout does not look like a version line: %q", stdout.String())
	}
}

func TestRun_BadFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--no-such-flag"}, &stdout, &stderr)

	if code != exitcode.Usage {
		t.Errorf("exit = %d, want %d (Usage)", code, exitcode.Usage)
	}
}

func TestRun_CheckConfig_OK(t *testing.T) {
	withCleanEnv(t)
	path := writeYAML(t, `
token: test-token-1234
ingest_url: https://api.seppia.ai
scrape_interval_s: 60
log_level: info
log_format: text
`)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--check-config", "--config", path}, &stdout, &stderr)

	if code != exitcode.OK {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "config: OK") {
		t.Errorf("expected 'config: OK' in stdout, got %q", stdout.String())
	}
	// Token must NEVER appear unredacted in any output stream.
	if strings.Contains(stdout.String()+stderr.String(), "test-token-1234") {
		t.Errorf("raw token leaked to output:\nstdout=%q\nstderr=%q", stdout.String(), stderr.String())
	}
}

func TestRun_CheckConfig_MissingToken(t *testing.T) {
	withCleanEnv(t)
	path := writeYAML(t, `
ingest_url: https://api.seppia.ai
`)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--check-config", "--config", path}, &stdout, &stderr)

	if code != exitcode.Config {
		t.Errorf("exit = %d, want %d (Config)", code, exitcode.Config)
	}
	if !strings.Contains(stderr.String(), "INVALID") {
		t.Errorf("stderr should mention INVALID, got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "token") {
		t.Errorf("stderr should mention the failing field, got %q", stderr.String())
	}
}

func TestRun_RunSubcommandAcceptedAsAlias(t *testing.T) {
	// `vigil-agent run --version` must behave identically to
	// `vigil-agent --version` — `run` is a documented alias used in
	// systemd unit files for legibility, NOT a separate subcommand
	// with its own behaviour. (We can't drive the daemon path from
	// a unit test here, so we sniff the version flag instead;
	// version_only does not start the daemon.)
	//
	// Note: Go's flag package stops at the first non-flag, so flags
	// must come BEFORE `run` on the command line.
	var stdout, stderr bytes.Buffer
	code := run([]string{"--version", "run"}, &stdout, &stderr)
	if code != exitcode.OK {
		t.Errorf("exit = %d, want %d; stderr=%q", code, exitcode.OK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "vigil-agent") {
		t.Errorf("version line missing: %q", stdout.String())
	}
}

func TestRun_SystemdExecStartShape(t *testing.T) {
	// Regression test for the v0.2.0 production incident where the
	// systemd unit shipped with `ExecStart=… run --config /etc/seppia/vigil.yml`.
	// Go's flag package stops at the first non-flag arg, so `run`
	// caused `--config /etc/seppia/vigil.yml` to be parsed as
	// positional args and the daemon refused to start with
	// `unexpected argument(s): [run --config /etc/seppia/vigil.yml]`.
	//
	// Anything that ships in the systemd unit MUST exercise this
	// path. We use --check-config (no daemon, no network, but the
	// same flag-parsing front-door as the daemon) so the test is
	// hermetic and fast.
	withCleanEnv(t)
	path := writeYAML(t, `
token: test-token-1234
ingest_url: https://api.seppia.ai
scrape_interval_s: 60
`)

	t.Run("canonical_order_works", func(t *testing.T) {
		// This is the SHAPE the systemd unit and the install.sh
		// patched ExecStart use today: flags first, `run` last.
		// (--check-config short-circuits before the daemon spins
		// up, but it goes through the SAME argv parsing.)
		var stdout, stderr bytes.Buffer
		code := run([]string{"--check-config", "--config", path, "run"}, &stdout, &stderr)
		if code != exitcode.OK {
			t.Fatalf("canonical ExecStart shape failed: exit=%d stderr=%q", code, stderr.String())
		}
	})

	t.Run("legacy_broken_order_rejected", func(t *testing.T) {
		// And THIS is the shape that broke production. We assert
		// it still fails loudly (exit=Usage with a clear error)
		// so that if anyone ever "fixes" the parser to be
		// permissive, this test screams.
		var stdout, stderr bytes.Buffer
		code := run([]string{"run", "--check-config", "--config", path}, &stdout, &stderr)
		if code != exitcode.Usage {
			t.Errorf("legacy broken order should fail with Usage; got exit=%d stderr=%q",
				code, stderr.String())
		}
	})
}

func TestRun_RejectsUnknownPositional(t *testing.T) {
	// Anything other than `run` as the positional arg is a typo
	// (e.g. someone meant `--once` and wrote `once`). Refuse it
	// with a Usage exit so they see the help instead of silently
	// starting the daemon with nothing to do.
	var stdout, stderr bytes.Buffer
	code := run([]string{"once"}, &stdout, &stderr)
	if code != exitcode.Usage {
		t.Errorf("exit = %d, want %d (Usage)", code, exitcode.Usage)
	}
	if !strings.Contains(stderr.String(), "unexpected argument") {
		t.Errorf("stderr should explain the rejection, got: %q", stderr.String())
	}
}

func TestRun_CheckConfig_ExplicitMissingFile(t *testing.T) {
	withCleanEnv(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--check-config", "--config", "/nonexistent/vigil.yml"}, &stdout, &stderr)

	if code != exitcode.Config {
		t.Errorf("exit = %d, want %d (Config)", code, exitcode.Config)
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Errorf("stderr should mention 'not found', got %q", stderr.String())
	}
}
