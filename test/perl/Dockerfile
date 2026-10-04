# Test image with the real Proxmox VE storage library (no PVE services).
# Used to load our Perl plugin through genuine PVE::Storage code.
FROM debian:trixie-slim

ARG PVE_REPO=pve-no-subscription

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl; \
    curl -fsSL https://enterprise.proxmox.com/debian/proxmox-archive-keyring-trixie.gpg \
        -o /usr/share/keyrings/proxmox-archive-keyring.gpg; \
    printf 'Types: deb\nURIs: http://download.proxmox.com/debian/pve\nSuites: trixie\nComponents: %s\nSigned-By: /usr/share/keyrings/proxmox-archive-keyring.gpg\n' \
        "$PVE_REPO" > /etc/apt/sources.list.d/pve.sources; \
    printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d; chmod +x /usr/sbin/policy-rc.d; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        libpve-storage-perl libjson-perl perl; \
    rm -rf /var/lib/apt/lists/*

WORKDIR /src
