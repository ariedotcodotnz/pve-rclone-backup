# pve-rclone-backup

Offsite replication of Proxmox VE backups to rclone-supported cloud storage.
Microsoft OneDrive Personal is the first target.

> **Status: pre-alpha, under active development.** Replication, verification, retention, fetch and
> restore work through the CLI and the TUI, and `make deb` builds an installable package. It has not
> yet been tested on a real PVE host or against a real OneDrive account.

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

These commands run as root on a PVE node with the daemon running.

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

## Building

You need Go 1.26 or newer; the package and Perl tests also need Docker.

```sh
make build      # bin/pve-rclone-backupd, bin/pve-rclone-backup
make test       # unit tests; make integration, make perl-test for more
make lint
make deb        # dist/pve-rclone-backup_<version>_amd64.deb
make deb-test   # install, use and purge the package in a PVE storage library container
```

Recovering without the original host is described in [docs/dr-runbook.md](docs/dr-runbook.md), and
recovering with stock rclone only in [docs/manual-recovery.md](docs/manual-recovery.md).

## License

AGPL-3.0-or-later. See [LICENSE](LICENSE).
