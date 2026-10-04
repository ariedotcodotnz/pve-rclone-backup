#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Runs inside the PVE storage library container.
set -eu
deb=$1
fail() { echo "FAIL: $*" >&2; exit 1; }

dpkg-deb --info "$deb" | grep -q 'Package: pve-rclone-backup' || fail "control"
# Slim images exclude /usr/share/doc from installation: check the archive.
dpkg-deb --contents "$deb" | grep './usr/share/doc/pve-rclone-backup/dr-runbook.md' >/dev/null || fail "runbook not packaged"
# Dependencies available in this container must be satisfied.
dpkg -i "$deb" >/tmp/install.log 2>&1 || {
    cat /tmp/install.log; fail "install"
}
for f in /usr/sbin/pve-rclone-backupd /usr/bin/pve-rclone-backup \
    /usr/share/perl5/PVE/Storage/Custom/RcloneBackupPlugin.pm \
    /usr/share/perl5/PVE/Storage/Custom/RcloneBackup/Client.pm \
    /usr/share/perl5/PVE/Storage/Custom/RcloneBackup/Schema.pm \
    /usr/share/perl5/PVE/BackupProvider/Plugin/Rclone.pm \
    /usr/lib/systemd/system/pve-rclone-backupd.service \
    /usr/share/bash-completion/completions/pve-rclone-backup; do
    [ -e "$f" ] || fail "missing $f"
done
[ "$(stat -c %a /usr/sbin/pve-rclone-backupd)" = 755 ] || fail "daemon mode"
[ "$(stat -c %U /usr/sbin/pve-rclone-backupd)" = root ] || fail "daemon owner"
/usr/sbin/pve-rclone-backupd --version | grep -q 'rclone engine v1\.' || fail "daemon version"
pve-rclone-backup version | grep -q '^pve-rclone-backup' || fail "cli version"
# PVE loads the plugin from the installed location.
perl -MPVE::Storage -e 'exit(PVE::Storage::Plugin->lookup("rclone-backup") ? 0 : 1)' 2>/dev/null || fail "plugin not registered"
# The daemon starts with the packaged layout (no /etc/pve here).
mkdir -p /run/pve-rclone-backup
/usr/sbin/pve-rclone-backupd --pve-dir /tmp/no-pve >/tmp/daemon.log 2>&1 &
pid=$!
for _ in $(seq 50); do [ -S /run/pve-rclone-backup/api.sock ] && break; sleep 0.1; done
pve-rclone-backup status >/dev/null || { cat /tmp/daemon.log; fail "daemon not serving"; }
# The vzdump hook reports archives and keeps a chained hook's arguments
# and exit status.
printf '#!/bin/sh\necho "$@" > /tmp/chained.args\nexit 3\n' > /usr/local/bin/my-hook && chmod +x /usr/local/bin/my-hook
printf 'script: /usr/local/bin/my-hook\n' > /etc/vzdump.conf
pve-rclone-backup hook install --chain --yes >/dev/null || fail "hook install"
grep -q '^script: /usr/share/pve-rclone-backup/vzdump-hook$' /etc/vzdump.conf || fail "hook not set"
status=0
TARGET=/var/lib/vz/dump/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst /usr/share/pve-rclone-backup/vzdump-hook backup-end snapshot 100 || status=$?
[ "$status" = 3 ] || fail "chained exit status $status"
[ "$(cat /tmp/chained.args)" = "backup-end snapshot 100" ] || fail "chained arguments"
kill $pid; wait $pid || fail "daemon exit status $?"
# Without the daemon the hook still never delays the job for long.
start=$(date +%s)
TARGET=/x.vma.zst /usr/share/pve-rclone-backup/vzdump-hook backup-end snapshot 100 || true
[ $(( $(date +%s) - start )) -le 6 ] || fail "hook blocked without the daemon"
[ -d /var/lib/pve-rclone-backup ] || fail "no state directory"

dpkg -r pve-rclone-backup >/dev/null
[ ! -e /usr/sbin/pve-rclone-backupd ] || fail "not removed"
grep -q '^script: /usr/local/bin/my-hook$' /etc/vzdump.conf || fail "hook left in vzdump.conf after removal"
[ -d /var/lib/pve-rclone-backup ] || fail "state removed before purge"
dpkg -P pve-rclone-backup >/dev/null
[ ! -e /var/lib/pve-rclone-backup ] || fail "state not purged"
echo "package OK"
