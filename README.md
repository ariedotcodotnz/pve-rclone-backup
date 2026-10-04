# pve-rclone-backup

Offsite replication of Proxmox VE backups to rclone-supported cloud storage.
Microsoft OneDrive Personal is the first target.

> **Status: pre-alpha, under active development.** Replication, the daemon API and the CLI work;
> there is no Debian package yet, and restores, retention and the TUI are still being built.

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

`pve-rclone-backup doctor` checks the installation. Every command accepts `-o json`.

## Building

You need Go 1.26 or newer.

```sh
make build   # bin/pve-rclone-backupd, bin/pve-rclone-backup
make test
make lint
```

## License

AGPL-3.0-or-later. See [LICENSE](LICENSE).
