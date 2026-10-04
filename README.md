# pve-rclone-backup

Offsite replication of Proxmox VE backups to rclone-supported cloud storage.
Microsoft OneDrive Personal is the first target.

> **Status: pre-alpha, under active development.** Nothing here is usable yet.

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

## Building

You need Go 1.26 or newer.

```sh
make build   # bin/pve-rclone-backupd, bin/pve-rclone-backup
make test
make lint
```

## License

AGPL-3.0-or-later. See [LICENSE](LICENSE).
