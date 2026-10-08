# pve-rclone-backup

Offsite replication of Proxmox VE backups to rclone-supported cloud storage.
Microsoft OneDrive Personal is the first target.

**Documentation: <https://ariedotcodotnz.github.io/pve-rclone-backup/>**

> **Status: pre-alpha, under active development.** Replication, verification, retention, fetch and
> restore work through the CLI and the TUI, and `make deb` builds an installable package. It passes
> an end-to-end suite on a nested Proxmox VE 9.2 node (`make e2e`). It has not yet been tested
> against a real OneDrive account.

## How it works

- **Backups stay native.** Proxmox VE keeps writing its normal vzdump backups to local storage.
  - `pve-rclone-backupd` notices each finished archive.
  - It uploads the archive asynchronously and unchanged, as encrypted segments (rclone crypt) that can be resumed.
  - A cloud outage never fails or blocks a local backup.
  - No second local copy is made.
- **Native PVE integration.** A storage plugin of type `rclone-backup` lists the offsite backups inside Proxmox VE: Backups tab, quota, notes, protection, offsite retention and "Show Configuration".
- **Disaster recovery without the original host.** Every offsite backup has a self-describing manifest. With the recovery kit, a fresh PVE host can rebuild the catalogue straight from the cloud. Even without this tool, the archives can be reassembled using stock rclone.
- **One interface for everything.** Configuration and operations go through the `pve-rclone-backup` CLI and TUI. Both talk to the daemon over a root-only local socket.

Architecture decisions are recorded in [`docs/adr/`](docs/adr/).

## Getting started

Install the package from the [Releases page](https://github.com/ariedotcodotnz/pve-rclone-backup/releases)
on each Proxmox VE node (on the host, not in a container): `apt install ./pve-rclone-backup_<version>_amd64.deb`.
These commands then run as root on the node.

1. Connect a OneDrive account. The command prints a Microsoft sign-in address you can open on
   any device. After you approve access, the browser lands on an error page at
   `http://localhost:53682/...`: copy that address and paste it back.

   ```sh
   pve-rclone-backup remote add onedrive-main
   pve-rclone-backup remote test onedrive-main
   ```

2. Create the encrypted repository and the storage. The command writes a recovery kit holding
   the encryption keys. Store the kit offline and confirm it by typing its checksum.
   Replication starts only after that.

   ```sh
   pve-rclone-backup storage init offsite --remote onedrive-main --source homelab \
       --replicate-from local --kit-file /root/offsite-recovery-kit.txt
   ```

3. Watch archives go offsite after each backup job:

   ```sh
   pve-rclone-backup status
   pve-rclone-backup queue list
   pve-rclone-backup backup list
   ```

4. Restore when needed: stream a VM straight into `qmrestore`, stage a container for `pct restore`,
   or fetch an offsite backup into a local storage so PVE can restore it natively:

   ```sh
   pve-rclone-backup restore offsite:backup/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst --vmid 900
   pve-rclone-backup backup fetch offsite:backup/vzdump-lxc-200-2026_10_04-02_00_01.tar.zst --to-storage local
   ```

`pve-rclone-backup tui` offers the same in an interactive terminal interface, and
`pve-rclone-backup doctor` checks the installation. Every command accepts `-o json`.

The [documentation](https://ariedotcodotnz.github.io/pve-rclone-backup/) covers installation, the
user guide, and a reference of every command and setting. Its sources are in [`docs/`](docs/).

## Building

You need Go 1.26 or newer; the package and Perl tests also need Docker.

```sh
make build      # bin/pve-rclone-backupd, bin/pve-rclone-backup
make test       # unit tests; make integration, make perl-test for more
make lint
make deb        # dist/pve-rclone-backup_<version>_amd64.deb
make deb-test   # install, use and purge the package in a PVE storage library container
make e2e        # end-to-end on a nested Proxmox VE 9.2 node (see below)
```

`make e2e` installs Proxmox VE 9.2 unattended from the official ISO into a nested VM, then installs
the package and runs backups, replication, the GUI's API calls, restores and failure scenarios
against it (`test/e2e`). QEMU runs in a container with `/dev/kvm` passed through, so the host needs
only Docker and hardware virtualisation. The first run builds a cached base image in
`~/.cache/pve-rclone-backup/e2e` (about 15 minutes); later runs boot a throwaway copy of it.
`test/e2e/vm.sh` starts, stops and opens a shell on the VM.

Recovering without the original host is described in [docs/dr-runbook.md](docs/dr-runbook.md), and
recovering with stock rclone only in [docs/manual-recovery.md](docs/manual-recovery.md).

`make docs` regenerates the reference pages from the command tree and `schema/*.yaml`; see
[Development](docs/development/index.md) to preview the site.

## License

AGPL-3.0-or-later. See [LICENSE](LICENSE).
