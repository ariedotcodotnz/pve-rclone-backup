#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Runs inside the freshly installed test node (vm.sh base): switch to the
# no-subscription repository, update, and create the test guests. It can
# be run again after a failure.
set -eux
export DEBIAN_FRONTEND=noninteractive

# Repositories: the enterprise ones need a subscription.
cd /etc/apt/sources.list.d
if [ -f pve-enterprise.sources ]; then
    sed -e 's|https://enterprise.proxmox.com/debian/pve|http://download.proxmox.com/debian/pve|' \
        -e 's/pve-enterprise/pve-no-subscription/' pve-enterprise.sources >pve-no-subscription.sources
    rm -f pve-enterprise.sources ceph.sources
fi
cd /
apt-get update
apt-get -y full-upgrade
apt-get -y autoremove --purge

# VM 100: no operating system (it idles in the firmware when started), one
# disk with incompressible data so its archives span several segments.
if ! qm status 100 >/dev/null 2>&1; then
    qm create 100 --name e2e-vm --memory 256 --ostype l26 --scsihw virtio-scsi-single --tags offsite
    truncate -s 512M /root/vm100.raw
    dd if=/dev/urandom of=/root/vm100.raw bs=1M count=160 conv=notrunc status=none
    qm disk import 100 /root/vm100.raw local-lvm --format raw
    rm /root/vm100.raw
    qm set 100 --scsi0 local-lvm:vm-100-disk-0
    [ -c /dev/kvm ] || qm set 100 --kvm 0
fi
sha256sum <"$(pvesm path local-lvm:vm-100-disk-0)" | cut -d' ' -f1 >/root/vm100.sha256

# Containers 200 (unprivileged) and 201 (privileged) from the smallest
# template, each with a payload whose checksum survives a restore.
pveam update
tpl=$(pveam available --section system | awk '$2 ~ /^alpine-3.*_amd64\.tar/ {print $2}' | sort -V | tail -n1)
[ -f "/var/lib/vz/template/cache/$tpl" ] || pveam download local "$tpl"
pct status 200 >/dev/null 2>&1 || pct create 200 "local:vztmpl/$tpl" --hostname e2e-ct --rootfs local-lvm:1 \
    --memory 128 --unprivileged 1 --tags offsite
pct status 201 >/dev/null 2>&1 || pct create 201 "local:vztmpl/$tpl" --hostname e2e-ct-priv --rootfs local-lvm:1 \
    --memory 128 --unprivileged 0
for ct in 200 201; do
    pct status "$ct" | grep -q running || pct start "$ct"
    pct exec "$ct" -- sh -c 'head -c 4194304 /dev/urandom >/root/payload && sha256sum /root/payload >/root/payload.sha256'
    pct stop "$ct"
done

mkdir -p /srv/remote-sim
fstrim -av || true
