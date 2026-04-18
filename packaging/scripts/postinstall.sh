#!/bin/sh
# Post-install script for vigil-agent .deb and .rpm packages.
#
# Goals (MUST be idempotent — runs on every install AND every upgrade):
#   1. Ensure the `vigil` system user/group exists.
#   2. Ensure /etc/seppia exists with sane perms.
#   3. Drop a default config at /etc/seppia/vigil.yml IF the operator
#      hasn't already created one. Never overwrite.
#   4. Reload systemd so the just-shipped unit is visible.
#
# Deliberately NOT done here:
#   - `systemctl enable` / `start`. Operator must put their probe token in
#     /etc/seppia/vigil.yml first; auto-starting would just produce
#     "missing token" loops in the journal. Install docs tell them:
#         sudo systemctl enable --now vigil-agent
#   - Package upgrade restart. We `try-restart` only — if the unit isn't
#     active, leave it that way.

set -eu

USER_NAME=vigil
GROUP_NAME=vigil
CONFIG_DIR=/etc/seppia
CONFIG_FILE="${CONFIG_DIR}/vigil.yml"
EXAMPLE_FILE="${CONFIG_DIR}/vigil.yml.example"

# 1. system group + user (no home, no shell). Use the most-portable form
#    available: prefer `groupadd`/`useradd`, fall back to `addgroup`/`adduser`
#    on Alpine-style hosts. Both branches are no-ops if the entity exists.
if ! getent group "${GROUP_NAME}" >/dev/null 2>&1; then
    if command -v groupadd >/dev/null 2>&1; then
        groupadd --system "${GROUP_NAME}"
    elif command -v addgroup >/dev/null 2>&1; then
        addgroup -S "${GROUP_NAME}"
    fi
fi

if ! id -u "${USER_NAME}" >/dev/null 2>&1; then
    if command -v useradd >/dev/null 2>&1; then
        useradd --system \
                --gid "${GROUP_NAME}" \
                --no-create-home \
                --home-dir /nonexistent \
                --shell /usr/sbin/nologin \
                --comment "Seppia Vigil agent" \
                "${USER_NAME}"
    elif command -v adduser >/dev/null 2>&1; then
        adduser -S -G "${GROUP_NAME}" -H -h /nonexistent -s /sbin/nologin \
                -g "Seppia Vigil agent" "${USER_NAME}"
    fi
fi

# 2. config directory. root:vigil 0750 — readable by the daemon, writable
#    only by the operator (root).
mkdir -p "${CONFIG_DIR}"
chown root:"${GROUP_NAME}" "${CONFIG_DIR}" 2>/dev/null || true
chmod 0750 "${CONFIG_DIR}"

# 3. seed default config from the shipped example. Mode 0640 root:vigil so
#    the token is readable by the daemon but not world-readable.
if [ ! -e "${CONFIG_FILE}" ] && [ -f "${EXAMPLE_FILE}" ]; then
    cp "${EXAMPLE_FILE}" "${CONFIG_FILE}"
    chown root:"${GROUP_NAME}" "${CONFIG_FILE}" 2>/dev/null || true
    chmod 0640 "${CONFIG_FILE}"
fi

# 4. systemd. `daemon-reload` on every run; `try-restart` only if the unit
#    is already enabled+active (i.e. this is a package upgrade, not a
#    fresh install).
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload >/dev/null 2>&1 || true
    systemctl try-restart vigil-agent.service >/dev/null 2>&1 || true
fi

exit 0
