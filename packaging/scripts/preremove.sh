#!/bin/sh
# Pre-remove script for vigil-agent .deb and .rpm packages.
#
# Runs BEFORE the package's files are deleted. We stop + disable the
# unit here (not in postremove) so systemd has the unit file on disk to
# load when we issue the stop. Failures are tolerated — the package
# manager should never refuse to uninstall because the daemon was
# already gone.
#
# We deliberately do NOT delete /etc/seppia/vigil.yml, the `vigil` user,
# or the `vigil` group. Operators who want a clean wipe can `--purge`
# (handled in postremove.sh) or remove them by hand. This matches what
# `postgresql`, `nginx`, etc. do.

set -eu

if command -v systemctl >/dev/null 2>&1; then
    systemctl --no-reload disable --now vigil-agent.service >/dev/null 2>&1 || true
fi

exit 0
