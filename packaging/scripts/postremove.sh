#!/bin/sh
# Post-remove script for vigil-agent .deb and .rpm packages.
#
# Runs AFTER package files are deleted. Just reload systemd so the now-
# missing unit file disappears from `systemctl list-unit-files`.
#
# Notes:
#   - The `vigil` user/group and /etc/seppia are intentionally left in
#     place on a normal `remove` so a re-install doesn't lose the
#     operator's token. A future `--purge` hook can remove them; not
#     shipped today to keep the script surface minimal.
#   - We don't `daemon-reload` if systemctl isn't available (Alpine, etc.).

set -eu

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload >/dev/null 2>&1 || true
fi

exit 0
