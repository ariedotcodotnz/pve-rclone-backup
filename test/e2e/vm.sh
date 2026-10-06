#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Nested Proxmox VE test VM. QEMU runs in a container with /dev/kvm passed
# through, so the host needs only docker. Commands:
#
#   base   install PVE from the official ISO (unattended) and provision
#          the test guests; cached, so it only runs once
#   up     boot a throwaway overlay of the base image and wait for SSH
#   down   stop the VM and discard the overlay
#   ssh    run a command in the VM (or open a shell)
#   env    print the settings the Go e2e tests read
#
# Settings (environment):
#   E2E_DIR        cache directory     (~/.cache/pve-rclone-backup/e2e)
#   E2E_SSH_PORT   SSH forward         (10022, bound to 127.0.0.1)
#   E2E_GUI_PORT   web GUI forward     (18006, bound to 127.0.0.1)
#   E2E_MEMORY     VM memory in MiB    (4096)
#   E2E_CPUS       VM vCPUs            (4)
set -eu

here=$(cd "$(dirname "$0")" && pwd)
dir=${E2E_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/pve-rclone-backup/e2e}
ssh_port=${E2E_SSH_PORT:-10022}
gui_port=${E2E_GUI_PORT:-18006}
memory=${E2E_MEMORY:-4096}
cpus=${E2E_CPUS:-4}
image=${E2E_IMAGE:-pve-rclone-backup-e2e:trixie}
name=pve-rclone-backup-e2e

iso_name=proxmox-ve_9.2-1.iso
iso_sha256=4e88fe416df9b527624a175f24c9aa07c714d3332afb1ee3dbf3879573ef2c6c
iso_url=https://enterprise.proxmox.com/iso/$iso_name

log() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
die() { log "error: $*"; exit 1; }

ensure_image() {
    docker image inspect "$image" >/dev/null 2>&1 || docker build -t "$image" "$here"
}

# run_qemu <docker options> -- <qemu options>: QEMU as the invoking user,
# allowed to open /dev/kvm through the device's group.
run_qemu() {
    opts=""
    while [ "$1" != "--" ]; do opts="$opts $1"; shift; done
    shift
    [ -c /dev/kvm ] || die "/dev/kvm is missing (enable virtualisation)"
    # shellcheck disable=SC2086 # opts is a list of options
    docker run --name "$name" $opts --device /dev/kvm --group-add "$(stat -c %g /dev/kvm)" \
        --user "$(id -u):$(id -g)" --network host -v "$dir:/work" "$image" \
        qemu-system-x86_64 -accel kvm -cpu host -machine q35 -smp "$cpus" -m "$memory" \
        -display none -vga std -monitor unix:/work/monitor.sock,server,nowait \
        -netdev "user,id=n0,hostfwd=tcp:127.0.0.1:$ssh_port-:22,hostfwd=tcp:127.0.0.1:$gui_port-:8006" \
        -device virtio-net-pci,netdev=n0 "$@"
}

monitor() {
    printf '%s\n' "$*" | docker exec -i "$name" socat - UNIX-CONNECT:/work/monitor.sock >/dev/null 2>&1 || true
}

# screenshot saves the VM console as PNG, to see why something hangs.
screenshot() {
    rm -f "$dir/screen.png"
    monitor "screendump /work/screen.png -f png"
    sleep 1
    [ -f "$dir/screen.png" ] && log "console screenshot: $dir/screen.png"
}

ssh_vm() {
    ssh -i "$dir/id_ed25519" -o IdentitiesOnly=yes -o IdentityAgent=none -p "$ssh_port" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o LogLevel=ERROR -o ConnectTimeout=5 -o BatchMode=yes root@127.0.0.1 "$@"
}

# The VM's own key only: an SSH agent in the caller's session (which may
# not answer at all) is never asked.
ssh_probe() {
    timeout 20 ssh -i "$dir/id_ed25519" -o IdentitiesOnly=yes -o IdentityAgent=none -p "$ssh_port" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o LogLevel=ERROR -o ConnectTimeout=5 -o BatchMode=yes root@127.0.0.1 true 2>/dev/null
}

wait_ssh() {
    deadline=$(($(date +%s) + ${1:-600}))
    # Each probe is bounded: QEMU accepts forwarded connections before the
    # guest's sshd runs, and ssh then waits for a greeting indefinitely
    # (ConnectTimeout does not cover that).
    until ssh_probe; do
        if ! docker inspect -f '{{.State.Running}}' "$name" 2>/dev/null | grep -q true; then
            docker logs "$name" 2>&1 | tail -20 >&2
            die "VM exited before SSH came up"
        fi
        [ "$(date +%s)" -lt "$deadline" ] || { screenshot; die "timed out waiting for SSH"; }
        sleep 5
    done
}

# wait_exit <seconds>: wait for a foreground VM run (installer, power-off).
wait_exit() {
    deadline=$(($(date +%s) + $1))
    while docker inspect -f '{{.State.Running}}' "$name" 2>/dev/null | grep -q true; do
        if [ "$(date +%s)" -ge "$deadline" ]; then
            screenshot
            docker rm -f "$name" >/dev/null
            die "timed out waiting for the VM to power off"
        fi
        sleep 10
    done
    code=$(docker inspect -f '{{.State.ExitCode}}' "$name")
    docker rm "$name" >/dev/null
    [ "$code" = 0 ] || die "QEMU exited with status $code"
}

fetch_iso() {
    [ -f "$dir/$iso_name.ok" ] && return
    log "downloading $iso_name"
    curl -fL -C - -o "$dir/$iso_name" "$iso_url"
    echo "$iso_sha256  $dir/$iso_name" | sha256sum -c - >&2
    touch "$dir/$iso_name.ok"
}

install_pve() {
    [ -f "$dir/installed.qcow2" ] && return
    fetch_iso
    [ -f "$dir/id_ed25519" ] || ssh-keygen -q -t ed25519 -N '' -C pve-rclone-backup-e2e -f "$dir/id_ed25519"
    # The web GUI password, for manual checks; SSH uses the key.
    [ -f "$dir/root-password" ] || (umask 077 && head -c 18 /dev/urandom | base64 | tr -d '/+=' >"$dir/root-password")
    (umask 077 && sed -e "s|@PASSWORD@|$(cat "$dir/root-password")|" -e "s|@SSHKEY@|$(cat "$dir/id_ed25519.pub")|" \
        "$here/answer.toml" >"$dir/answer.toml")
    log "preparing the unattended installer ISO"
    rm -f "$dir/auto.iso"
    docker run --rm --user "$(id -u):$(id -g)" -v "$dir:/work" "$image" sh -ec '
        proxmox-auto-install-assistant validate-answer /work/answer.toml
        proxmox-auto-install-assistant prepare-iso /work/'"$iso_name"' --fetch-from iso \
            --answer-file /work/answer.toml --output /work/auto.iso --tmp /work' >&2
    log "installing Proxmox VE (about 5-10 minutes)"
    docker run --rm --user "$(id -u):$(id -g)" -v "$dir:/work" "$image" \
        qemu-img create -q -f qcow2 /work/installing.qcow2 48G
    run_qemu -d -- -no-reboot -boot d -cdrom /work/auto.iso \
        -drive file=/work/installing.qcow2,if=virtio,format=qcow2,cache=unsafe,discard=unmap >/dev/null
    wait_exit 2700
    mv "$dir/installing.qcow2" "$dir/installed.qcow2"
    rm -f "$dir/auto.iso"
}

provision() {
    [ -f "$dir/base.qcow2" ] && return
    install_pve
    log "provisioning the test guests"
    # A previous failed attempt is continued (provision.sh is idempotent).
    [ -f "$dir/provisioning.qcow2" ] || docker run --rm --user "$(id -u):$(id -g)" -v "$dir:/work" "$image" \
        qemu-img create -q -f qcow2 -b installed.qcow2 -F qcow2 /work/provisioning.qcow2
    run_qemu -d -- -drive file=/work/provisioning.qcow2,if=virtio,format=qcow2,discard=unmap >/dev/null
    wait_ssh 900
    ssh_vm 'cat >/root/provision.sh' <"$here/provision.sh"
    if ! ssh_vm sh /root/provision.sh; then
        ssh_vm poweroff || true
        wait_exit 300 || true
        die "provisioning failed; running vm.sh base again continues it"
    fi
    ssh_vm 'poweroff' || true
    wait_exit 300
    mv "$dir/provisioning.qcow2" "$dir/base.qcow2"
}

up() {
    if docker inspect "$name" >/dev/null 2>&1; then
        die "the test VM is already running (vm.sh down)"
    fi
    rm -f "$dir/run.qcow2"
    docker run --rm --user "$(id -u):$(id -g)" -v "$dir:/work" "$image" \
        qemu-img create -q -f qcow2 -b base.qcow2 -F qcow2 /work/run.qcow2
    log "booting the test VM (ssh 127.0.0.1:$ssh_port, GUI https://127.0.0.1:$gui_port)"
    run_qemu -d -- -drive file=/work/run.qcow2,if=virtio,format=qcow2,cache=unsafe,discard=unmap >/dev/null
    wait_ssh 600
    log "test VM is up"
}

down() {
    docker rm -f "$name" >/dev/null 2>&1 || true
    rm -f "$dir/run.qcow2" "$dir/monitor.sock"
}

mkdir -p "$dir"
case "${1:-}" in
base)
    ensure_image
    provision
    ;;
up)
    ensure_image
    [ -f "$dir/base.qcow2" ] || provision
    up
    ;;
down) down ;;
ssh)
    shift
    ssh_vm "$@"
    ;;
screenshot) screenshot ;;
env)
    echo "E2E_SSH=root@127.0.0.1:$ssh_port"
    echo "E2E_SSH_KEY=$dir/id_ed25519"
    echo "E2E_GUI=https://127.0.0.1:$gui_port"
    ;;
*)
    echo "usage: $0 base|up|down|ssh [command]|screenshot|env" >&2
    exit 2
    ;;
esac
