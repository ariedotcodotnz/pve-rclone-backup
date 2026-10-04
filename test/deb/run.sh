#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Install, use and purge a built package in the container with Proxmox
# VE's storage library (test/perl). pve-manager is not installable there,
# so dependencies are checked separately from installation.
set -eu

deb=$(cd "$(dirname "$1")" && pwd)/$(basename "$1")
root=$(cd "$(dirname "$0")/../.." && pwd)
image=${PVE_PERLTEST_IMAGE:-pve-rclone-backup-perltest:trixie}
if ! docker image inspect "$image" >/dev/null 2>&1; then
    docker build -t "$image" "$root/test/perl"
fi
exec docker run --rm -v "$deb:/tmp/pkg.deb:ro" -v "$root/test/deb/check.sh:/tmp/check.sh:ro" "$image" sh /tmp/check.sh /tmp/pkg.deb
