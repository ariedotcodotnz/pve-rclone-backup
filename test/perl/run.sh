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

exec docker run --rm \
    -v "$root/perl/PVE/Storage/Custom:/usr/share/perl5/PVE/Storage/Custom:ro" \
    -v "$root/perl/PVE/BackupProvider/Plugin/Rclone.pm:/usr/share/perl5/PVE/BackupProvider/Plugin/Rclone.pm:ro" \
    -v "$root/perl/t:/src/t:ro" \
    "$image" prove -T -r "$@" /src/t
