#!/bin/sh
# vigil-agent installer.
#
# Usage:
#   curl -fsSL https://get.seppia.ai/vigil | sh -s -- --token=<probe_token>
#   sh install.sh --uninstall
#
# Goals:
#   1. Detect OS+arch, fetch the matching release tarball from GitHub,
#      verify it against the published SHA256SUMS, install the binary
#      atomically at $PREFIX/bin/vigil-agent.
#   2. Drop a minimal /etc/seppia/vigil.yml with the operator's probe
#      token (only on first install — never overwrites an existing
#      config).
#   3. Wire up systemd (Linux only, when systemd is available and we're
#      root). Enable + start the service so the operator gets samples
#      flowing without a second command. macOS / non-systemd hosts get
#      the binary + config; the operator wires up their own service
#      manager (launchd, supervisord, …).
#   4. Idempotent. Re-running upgrades the binary in place; the config
#      and systemd state are preserved.
#   5. POSIX sh — runs on bash, dash, busybox ash, FreeBSD sh. No
#      bash-isms, no GNU-isms.
#
# This script lives in the open-source vigil-agent repo so operators
# can audit what runs as root on their hosts before piping curl into
# sh. The hosted copy at https://get.seppia.ai/vigil is a verbatim
# mirror of `scripts/install.sh` on the latest release tag.

set -eu

# ─── Constants ───────────────────────────────────────────────────────────────

GH_OWNER="Seppia-AI"
GH_REPO="vigil-agent"
DEFAULT_INGEST_URL="https://api.seppia.ai"
DEFAULT_PREFIX="/usr/local"
DEFAULT_CONFIG_PATH="/etc/seppia/vigil.yml"
SYSTEMD_UNIT_PATH="/etc/systemd/system/vigil-agent.service"
USER_AGENT="vigil-agent-installer/1.0"

# ─── CLI ─────────────────────────────────────────────────────────────────────

VERSION=""           # empty = resolve "latest" from GitHub releases
TOKEN=""
INGEST_URL=""        # empty = omit from config (agent uses its compiled-in default)
PREFIX="${DEFAULT_PREFIX}"
CONFIG_PATH="${DEFAULT_CONFIG_PATH}"
SCRAPE_INTERVAL=""   # empty = omit from config (agent uses its compiled-in default)
NO_SYSTEMD=0
FORCE_CONFIG=0
DRY_RUN=0
UNINSTALL=0

print_help() {
    cat <<'EOF'
vigil-agent installer

Usage:
  install.sh [--token=<tok>] [options]
  install.sh --uninstall

Options:
  --token=<tok>        Probe token from the Vigil dashboard (Vitals → probe →
                       Install snippet). Required for fresh installs; ignored
                       on upgrades when /etc/seppia/vigil.yml already exists.

  --version=<ver>      Pin a specific release (e.g. v0.1.3). Default: latest.
  --ingest-url=<url>   Vigil API base URL. Default: https://api.seppia.ai.
                       Override for self-hosted / staging environments.
  --scrape-interval=<s>  Seconds between scrapes. Default: 60. The server
                       may clamp this to a per-account minimum on first POST.
  --prefix=<dir>       Install prefix (binary goes in <prefix>/bin).
                       Default: /usr/local.
  --config=<path>      Path to write the config file at on first install.
                       Default: /etc/seppia/vigil.yml.
  --no-systemd         Skip systemd unit setup (binary + config only).
                       Auto-enabled on macOS and non-systemd hosts.
  --force-config       Overwrite an existing config file (DANGEROUS — wipes
                       any local edits to scrape_interval / labels / etc.).
  --dry-run            Print every step without executing. Safe to pipe.
  --uninstall          Stop + disable the systemd unit, remove the binary.
                       Leaves /etc/seppia/vigil.yml and the `vigil` user
                       intact so a re-install picks up the same probe.
  --help, -h           Show this help.

Environment overrides (lower precedence than flags):
  VIGIL_AGENT_VERSION   same as --version
  VIGIL_TOKEN           same as --token
  VIGIL_INGEST_URL      same as --ingest-url

Examples:
  # Fresh install, latest version, default ingest URL
  curl -fsSL https://get.seppia.ai/vigil | sh -s -- --token=abc123

  # Self-hosted Vigil server
  curl -fsSL https://get.seppia.ai/vigil | sh -s -- \
      --token=abc123 --ingest-url=https://vigil.internal.example

  # Upgrade in place (reuses existing config)
  curl -fsSL https://get.seppia.ai/vigil | sh

  # Pin to a specific release for fleet rollouts
  curl -fsSL https://get.seppia.ai/vigil | sh -s -- --version=v0.1.3

  # Remove
  curl -fsSL https://get.seppia.ai/vigil | sh -s -- --uninstall
EOF
}

# Parse args. Accept `--key value` and `--key=value` so users don't have to
# remember which form is supported. Unknown flags abort early — silently
# ignoring them in a script that shells `mv` and `systemctl` as root would
# eat real bug reports.
while [ $# -gt 0 ]; do
    case "$1" in
        --token=*)            TOKEN="${1#*=}";              shift ;;
        --token)              TOKEN="${2:-}";               shift 2 ;;
        --version=*)          VERSION="${1#*=}";            shift ;;
        --version)            VERSION="${2:-}";             shift 2 ;;
        --ingest-url=*)       INGEST_URL="${1#*=}";         shift ;;
        --ingest-url)         INGEST_URL="${2:-}";          shift 2 ;;
        --scrape-interval=*)  SCRAPE_INTERVAL="${1#*=}";    shift ;;
        --scrape-interval)    SCRAPE_INTERVAL="${2:-}";     shift 2 ;;
        --prefix=*)           PREFIX="${1#*=}";             shift ;;
        --prefix)             PREFIX="${2:-}";              shift 2 ;;
        --config=*)           CONFIG_PATH="${1#*=}";        shift ;;
        --config)             CONFIG_PATH="${2:-}";         shift 2 ;;
        --no-systemd)         NO_SYSTEMD=1;                 shift ;;
        --force-config)       FORCE_CONFIG=1;               shift ;;
        --dry-run)            DRY_RUN=1;                    shift ;;
        --uninstall)          UNINSTALL=1;                  shift ;;
        --help|-h)            print_help; exit 0 ;;
        *)
            printf 'install.sh: unknown option: %s\n\n' "$1" >&2
            print_help >&2
            exit 64
            ;;
    esac
done

# Env-var fallbacks.
[ -z "${VERSION}"    ] && VERSION="${VIGIL_AGENT_VERSION:-}"
[ -z "${TOKEN}"      ] && TOKEN="${VIGIL_TOKEN:-}"
[ -z "${INGEST_URL}" ] && INGEST_URL="${VIGIL_INGEST_URL:-}"

# ─── Output helpers ──────────────────────────────────────────────────────────

# Colour only when stdout is a TTY. Piping to a file shouldn't get ANSI noise.
if [ -t 1 ]; then
    C_RESET=$(printf '\033[0m')
    C_DIM=$(printf '\033[2m')
    C_BOLD=$(printf '\033[1m')
    C_GREEN=$(printf '\033[32m')
    C_RED=$(printf '\033[31m')
    C_YELLOW=$(printf '\033[33m')
else
    C_RESET=''; C_DIM=''; C_BOLD=''; C_GREEN=''; C_RED=''; C_YELLOW=''
fi

info()  { printf '%s==>%s %s\n' "${C_BOLD}" "${C_RESET}" "$*"; }
ok()    { printf '%s ✓%s %s\n' "${C_GREEN}" "${C_RESET}" "$*"; }
warn()  { printf '%s ⚠%s %s\n' "${C_YELLOW}" "${C_RESET}" "$*" >&2; }
die()   { printf '%s ✗%s %s\n' "${C_RED}" "${C_RESET}" "$*" >&2; exit 1; }
dim()   { printf '%s%s%s\n' "${C_DIM}" "$*" "${C_RESET}"; }

# Run a command unless --dry-run; in dry-run mode just print it. Always
# preserves quoting via "$@".
run() {
    if [ "${DRY_RUN}" -eq 1 ]; then
        printf '   %s$ %s%s\n' "${C_DIM}" "$*" "${C_RESET}"
        return 0
    fi
    "$@"
}

# ─── Pre-flight ──────────────────────────────────────────────────────────────

require_cmd() {
    command -v "$1" >/dev/null 2>&1 \
        || die "required command not found: $1 (install it and re-run)"
}

# tar is non-negotiable; mkdir/mv/chmod are POSIX-mandated and present
# everywhere we care about.
require_cmd tar
require_cmd uname

# Need at least one downloader.
DOWNLOADER=""
if   command -v curl >/dev/null 2>&1; then DOWNLOADER=curl
elif command -v wget >/dev/null 2>&1; then DOWNLOADER=wget
else die "neither curl nor wget is installed; install one and re-run"
fi

# Need at least one sha256 implementation.
SHA256_CMD=""
if   command -v sha256sum  >/dev/null 2>&1; then SHA256_CMD=sha256sum
elif command -v shasum     >/dev/null 2>&1; then SHA256_CMD="shasum -a 256"
elif command -v openssl    >/dev/null 2>&1; then SHA256_CMD="openssl dgst -sha256 -r"
else die "no sha256 tool found (need sha256sum, shasum, or openssl)"
fi

# Most steps need root — we write to /usr/local/bin, /etc/seppia, and
# /etc/systemd/system. Allow non-root in --dry-run so users can preview
# what would happen without sudo.
EUID_VAL=$(id -u)
if [ "${EUID_VAL}" -ne 0 ] && [ "${DRY_RUN}" -eq 0 ]; then
    die "must run as root (try: sudo sh install.sh ...)"
fi

# ─── OS / arch detection ─────────────────────────────────────────────────────

detect_os() {
    uname_s=$(uname -s)
    case "${uname_s}" in
        Linux)   echo linux ;;
        Darwin)  echo darwin ;;
        *) die "unsupported OS: ${uname_s} (supported: Linux, Darwin)" ;;
    esac
}

detect_arch() {
    uname_m=$(uname -m)
    case "${uname_m}" in
        x86_64|amd64)        echo amd64 ;;
        aarch64|arm64)       echo arm64 ;;
        *) die "unsupported arch: ${uname_m} (supported: x86_64, arm64)" ;;
    esac
}

OS=$(detect_os)
ARCH=$(detect_arch)

# Force --no-systemd on Darwin (no systemd) and on Linux hosts that don't
# have it. macOS users get the binary + config; supervisor wiring is on
# them.
HAS_SYSTEMD=0
if [ "${OS}" = "linux" ] \
   && command -v systemctl >/dev/null 2>&1 \
   && [ -d /run/systemd/system ]; then
    HAS_SYSTEMD=1
fi
if [ "${HAS_SYSTEMD}" -eq 0 ] && [ "${NO_SYSTEMD}" -eq 0 ]; then
    NO_SYSTEMD=1
    [ "${OS}" = "linux" ] && warn "systemd not detected; skipping service setup"
fi

# ─── Network helpers ─────────────────────────────────────────────────────────

# Print URL contents to stdout. Args: url
http_get() {
    case "${DOWNLOADER}" in
        curl) curl -fsSL --user-agent "${USER_AGENT}" "$1" ;;
        wget) wget -qO- --user-agent="${USER_AGENT}" "$1" ;;
    esac
}

# Save URL contents to a file. Args: url, dest
http_get_to() {
    case "${DOWNLOADER}" in
        curl) curl -fsSL --user-agent "${USER_AGENT}" -o "$2" "$1" ;;
        wget) wget -q   --user-agent="${USER_AGENT}" -O "$2" "$1" ;;
    esac
}

# Resolve the latest release tag from the GitHub Releases API. Falls back
# to grep parsing so we don't add a `jq` dependency.
resolve_latest_version() {
    api_url="https://api.github.com/repos/${GH_OWNER}/${GH_REPO}/releases/latest"
    body=$(http_get "${api_url}") \
        || die "failed to query ${api_url} (network down? rate-limited?)"
    tag=$(printf '%s\n' "${body}" \
        | grep -m1 '"tag_name"' \
        | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')
    if [ -z "${tag}" ]; then
        die "could not parse tag_name from GitHub Releases response"
    fi
    echo "${tag}"
}

# ─── Install ─────────────────────────────────────────────────────────────────

do_install() {
    info "vigil-agent installer"
    dim  "  os=${OS}  arch=${ARCH}  prefix=${PREFIX}  systemd=$([ ${HAS_SYSTEMD} -eq 1 ] && echo yes || echo no)"

    # 1. Resolve version.
    if [ -z "${VERSION}" ]; then
        info "resolving latest release"
        VERSION=$(resolve_latest_version)
        ok "latest release: ${VERSION}"
    fi
    # GH tags are vX.Y.Z; goreleaser archive names use X.Y.Z. Strip the leading v.
    version_no_v="${VERSION#v}"

    # 2. Decide on a config decision EARLY so we can fail fast on a missing
    #    token before any network I/O for the binary.
    if [ -e "${CONFIG_PATH}" ] && [ "${FORCE_CONFIG}" -eq 0 ]; then
        write_config=0
        ok "existing config found at ${CONFIG_PATH} (will not overwrite)"
    else
        write_config=1
        if [ -z "${TOKEN}" ]; then
            die "no probe token provided. Pass --token=<tok> (from the Vigil dashboard) or set VIGIL_TOKEN."
        fi
    fi

    archive_name="vigil-agent_${version_no_v}_${OS}_${ARCH}.tar.gz"
    sha_name="SHA256SUMS"
    base_url="https://github.com/${GH_OWNER}/${GH_REPO}/releases/download/${VERSION}"

    # 3. Stage everything in a tempdir so a failed download / verify never
    #    touches /usr/local. POSIX-only mktemp invocation.
    tmpdir=$(mktemp -d 2>/dev/null || mktemp -d -t vigil-agent)
    trap 'rm -rf "${tmpdir}"' EXIT INT HUP TERM

    info "downloading ${archive_name}"
    run http_get_to "${base_url}/${archive_name}" "${tmpdir}/${archive_name}" \
        || die "download failed: ${base_url}/${archive_name}"
    run http_get_to "${base_url}/${sha_name}"     "${tmpdir}/${sha_name}" \
        || die "download failed: ${base_url}/${sha_name}"

    # 4. Verify checksum. Match the line for our archive only — the
    #    checksum file lists every artefact in the release.
    info "verifying sha256"
    if [ "${DRY_RUN}" -eq 0 ]; then
        expected=$(grep "[[:space:]]${archive_name}\$" "${tmpdir}/${sha_name}" \
            | awk '{print $1}')
        [ -n "${expected}" ] || die "no checksum entry for ${archive_name} in ${sha_name}"
        actual=$(${SHA256_CMD} "${tmpdir}/${archive_name}" | awk '{print $1}')
        if [ "${expected}" != "${actual}" ]; then
            die "sha256 mismatch! expected=${expected} actual=${actual}"
        fi
        ok "sha256 verified"
    else
        dim "   (dry-run: skipped sha256 verify)"
    fi

    # 5. Extract.
    info "extracting"
    run tar -xzf "${tmpdir}/${archive_name}" -C "${tmpdir}"

    # The tarball lays out as: vigil-agent (binary), README.md, LICENSE,
    # vigil.example.yml, systemd/vigil-agent.service.
    src_bin="${tmpdir}/vigil-agent"
    src_unit="${tmpdir}/systemd/vigil-agent.service"
    src_example="${tmpdir}/vigil.example.yml"

    if [ "${DRY_RUN}" -eq 0 ]; then
        [ -f "${src_bin}"     ] || die "tarball missing vigil-agent binary"
        [ -f "${src_unit}"    ] || die "tarball missing systemd/vigil-agent.service"
        [ -f "${src_example}" ] || die "tarball missing vigil.example.yml"
    fi

    # 6. Install binary atomically.
    bin_dst="${PREFIX}/bin/vigil-agent"
    info "installing binary → ${bin_dst}"
    run mkdir -p "${PREFIX}/bin"
    # Write to a sibling temp path then mv so an in-flight `vigil-agent run`
    # process keeps executing the old inode until systemd restarts it.
    if [ "${DRY_RUN}" -eq 0 ]; then
        cp "${src_bin}" "${bin_dst}.new"
        chmod 0755 "${bin_dst}.new"
        mv "${bin_dst}.new" "${bin_dst}"
    else
        dim "   (dry-run: would cp + chmod + mv)"
    fi
    ok "installed $(${bin_dst} --version 2>/dev/null || echo "${VERSION}")"

    # 7. Config — only on first install (or --force-config).
    if [ "${write_config}" -eq 1 ]; then
        config_dir=$(dirname "${CONFIG_PATH}")
        info "writing config → ${CONFIG_PATH}"
        run mkdir -p "${config_dir}"
        run chmod 0755 "${config_dir}"
        if [ "${DRY_RUN}" -eq 0 ]; then
            tmp_cfg="${tmpdir}/vigil.yml"
            {
                echo "# Generated by vigil-agent installer on $(date -u +%Y-%m-%dT%H:%M:%SZ)"
                echo "# Edit freely; the installer will not overwrite this file on upgrade."
                echo
                echo "token: \"${TOKEN}\""
                if [ -n "${INGEST_URL}" ] && [ "${INGEST_URL}" != "${DEFAULT_INGEST_URL}" ]; then
                    echo "ingest_url: \"${INGEST_URL}\""
                fi
                if [ -n "${SCRAPE_INTERVAL}" ]; then
                    echo "scrape_interval_s: ${SCRAPE_INTERVAL}"
                fi
            } > "${tmp_cfg}"
            mv "${tmp_cfg}" "${CONFIG_PATH}"
            chmod 0640 "${CONFIG_PATH}"
        else
            dim "   (dry-run: would write yaml with token=${TOKEN:+REDACTED})"
        fi
        ok "config written (mode 0640; token redacted from logs)"
    fi

    # 8. Validate config before wiring up the service so a typo in
    #    --ingest-url / a malformed token doesn't end up as a crash-loop
    #    in journalctl.
    if [ "${DRY_RUN}" -eq 0 ]; then
        info "validating config"
        if "${bin_dst}" --check-config --config "${CONFIG_PATH}" >/dev/null 2>&1; then
            ok "config valid"
        else
            warn "config validation failed — service NOT enabled. Run:"
            warn "  ${bin_dst} --check-config --config ${CONFIG_PATH}"
            warn "to see the error, then re-run this installer."
            return 0
        fi
    fi

    # 9. systemd.
    if [ "${NO_SYSTEMD}" -eq 1 ]; then
        info "skipping systemd setup (--no-systemd or unavailable)"
        if [ "${OS}" = "darwin" ]; then
            dim "   start manually with: ${bin_dst} run --config ${CONFIG_PATH}"
            dim "   (launchd plist integration not yet included)"
        fi
    else
        info "installing systemd unit → ${SYSTEMD_UNIT_PATH}"
        # Patch the unit's ExecStart to point at our chosen prefix instead
        # of /usr/bin (which is the .deb/.rpm convention). One sed call;
        # if the file ever drifts and the regex doesn't match, the unit
        # still installs in its packaged form which keys off /usr/bin —
        # so we error loudly instead of silently shipping a broken unit.
        if [ "${DRY_RUN}" -eq 0 ]; then
            sed "s|^ExecStart=.*|ExecStart=${bin_dst} run --config ${CONFIG_PATH}|" \
                "${src_unit}" > "${tmpdir}/vigil-agent.service"
            grep -q "^ExecStart=${bin_dst} run --config ${CONFIG_PATH}\$" \
                "${tmpdir}/vigil-agent.service" \
                || die "failed to patch ExecStart in unit file"
            mv "${tmpdir}/vigil-agent.service" "${SYSTEMD_UNIT_PATH}"
            chmod 0644 "${SYSTEMD_UNIT_PATH}"
        else
            dim "   (dry-run: would patch ExecStart and install unit)"
        fi

        info "ensuring vigil system user"
        ensure_vigil_user

        info "reloading systemd + enabling vigil-agent"
        run systemctl daemon-reload
        run systemctl enable vigil-agent.service
        # `restart` (not `start`) so an upgrade picks up the new binary
        # immediately. A no-op on first install since the unit isn't
        # running yet.
        run systemctl restart vigil-agent.service

        if [ "${DRY_RUN}" -eq 0 ]; then
            sleep 1
            if systemctl is-active --quiet vigil-agent.service; then
                ok "vigil-agent is running"
            else
                warn "vigil-agent failed to start. Check the logs:"
                warn "  journalctl -u vigil-agent.service --no-pager -n 50"
                exit 1
            fi
        fi
    fi

    info "done"
    dim "  config:  ${CONFIG_PATH}"
    dim "  binary:  ${bin_dst}"
    if [ "${NO_SYSTEMD}" -eq 0 ]; then
        dim "  service: systemctl status vigil-agent.service"
    fi
}

# ensure_vigil_user — idempotent system user/group creation. Mirrors the
# logic in packaging/scripts/postinstall.sh so a tarball install lands in
# the same end state as a .deb / .rpm install.
ensure_vigil_user() {
    if [ "${DRY_RUN}" -eq 1 ]; then
        dim "   (dry-run: would create user/group 'vigil')"
        return 0
    fi
    if ! getent group vigil >/dev/null 2>&1; then
        if   command -v groupadd >/dev/null 2>&1; then
            groupadd --system vigil
        elif command -v addgroup >/dev/null 2>&1; then
            addgroup -S vigil
        fi
    fi
    if ! id -u vigil >/dev/null 2>&1; then
        if   command -v useradd >/dev/null 2>&1; then
            useradd --system --gid vigil --no-create-home \
                    --home-dir /nonexistent --shell /usr/sbin/nologin \
                    --comment "Seppia Vigil agent" vigil
        elif command -v adduser >/dev/null 2>&1; then
            adduser -S -G vigil -H -h /nonexistent -s /sbin/nologin \
                    -g "Seppia Vigil agent" vigil
        fi
    fi
    # Hand the config file to vigil:vigil so the daemon can read it.
    if [ -e "${CONFIG_PATH}" ]; then
        chown root:vigil "${CONFIG_PATH}" 2>/dev/null || true
        chmod 0640       "${CONFIG_PATH}" 2>/dev/null || true
    fi
}

# ─── Uninstall ───────────────────────────────────────────────────────────────

do_uninstall() {
    info "vigil-agent uninstaller"

    if [ "${HAS_SYSTEMD}" -eq 1 ]; then
        if systemctl list-unit-files vigil-agent.service >/dev/null 2>&1; then
            info "stopping + disabling systemd unit"
            run systemctl disable --now vigil-agent.service || true
        fi
        if [ -f "${SYSTEMD_UNIT_PATH}" ]; then
            run rm -f "${SYSTEMD_UNIT_PATH}"
            run systemctl daemon-reload
        fi
    fi

    bin_dst="${PREFIX}/bin/vigil-agent"
    if [ -e "${bin_dst}" ]; then
        info "removing binary ${bin_dst}"
        run rm -f "${bin_dst}"
    fi

    ok "uninstalled"
    dim "  Kept: ${CONFIG_PATH} and the 'vigil' user/group."
    dim "  Re-run install.sh to put the agent back; the same probe token"
    dim "  will be reused. To wipe everything:"
    dim "    rm -rf ${CONFIG_PATH%/*}"
    dim "    userdel vigil 2>/dev/null; groupdel vigil 2>/dev/null"
}

# ─── Main ────────────────────────────────────────────────────────────────────

if [ "${UNINSTALL}" -eq 1 ]; then
    do_uninstall
else
    do_install
fi
