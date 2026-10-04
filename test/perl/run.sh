#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Run the Perl test suite inside a container that has Proxmox VE's real
# storage library installed. The plugin is mounted where PVE's custom
# plugin loader looks for it.
set -eu

root=$(cd "$(dirname "$0")/../.." && pwd)
image=${PVE_PERLTEST_IMAGE:-pve-rclone-backup-perltest:trixie}

if ! docker image inspect "$image" >/dev/null 2>&1; then
    docker build -t "$image" "$root/test/perl"
fi

# Exercise the real daemon and CLI too when they have been built (make build).
daemon_args=""
if [ -x "$root/bin/pve-rclone-backupd" ]; then
    daemon_args="-v $root/bin/pve-rclone-backupd:/usr/sbin/pve-rclone-backupd:ro -e PVE_RCLONE_BACKUPD=/usr/sbin/pve-rclone-backupd"
fi
if [ -x "$root/bin/pve-rclone-backup" ]; then
    daemon_args="$daemon_args -v $root/bin/pve-rclone-backup:/usr/bin/pve-rclone-backup:ro -e PVE_RCLONE_BACKUP_CLI=/usr/bin/pve-rclone-backup"
fi

# shellcheck disable=SC2086 # daemon_args is a list of options
exec docker run --rm $daemon_args \
    -v "$root/perl/PVE/Storage/Custom:/usr/share/perl5/PVE/Storage/Custom:ro" \
    -v "$root/perl/PVE/BackupProvider/Plugin/Rclone.pm:/usr/share/perl5/PVE/BackupProvider/Plugin/Rclone.pm:ro" \
    -v "$root/perl/t:/src/t:ro" \
    -v "$root/schema/testdata:/src/schema/testdata:ro" \
    "$image" prove -T -r "$@" /src/t
