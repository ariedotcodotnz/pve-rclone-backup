# Runner for the nested Proxmox VE test VM: QEMU (started with /dev/kvm
# passed through, so the host needs no QEMU install or kvm group) and
# Proxmox's unattended-installer tooling.
FROM debian:trixie-slim

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl; \
    curl -fsSL https://enterprise.proxmox.com/debian/proxmox-archive-keyring-trixie.gpg \
        -o /usr/share/keyrings/proxmox-archive-keyring.gpg; \
    printf 'Types: deb\nURIs: http://download.proxmox.com/debian/pve\nSuites: trixie\nComponents: pve-no-subscription\nSigned-By: /usr/share/keyrings/proxmox-archive-keyring.gpg\n' \
        > /etc/apt/sources.list.d/pve.sources; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        qemu-system-x86 qemu-utils proxmox-auto-install-assistant xorriso socat; \
    rm -rf /var/lib/apt/lists/*
