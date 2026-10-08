# Proxmox VE integration

The package adds the storage type `rclone-backup` to Proxmox VE. An offsite storage then behaves
like a backup storage in the GUI, in `pvesm` and `pvesh`, and in the API. Every request is answered
from the daemon's catalogue, so the GUI stays fast even when the cloud is slow or unreachable.

## What works

| Where | What it does |
|---|---|
| Storage summary, `pvesm status` | Shows the cloud account's quota as the storage's size and usage. While the daemon is stopped, the storage shows as inactive. |
| **Backups** tab, `pvesm list` | Lists offsite backups with their guest, time, size, notes and protection. |
| **Show Configuration** | Shows the guest configuration recorded in the backup, without downloading it. `pvesm extractconfig` does the same. |
| **Edit Notes**, **Change Protection** | Change the backup's notes and protection, stored with the backup in the cloud. |
| **Remove** | Marks the backup for deletion, after a grace period; see [Retention and deletion](retention.md#deletion-is-delayed). Protected backups cannot be removed. |
| **Prune group**, the storage's **Backup Retention** | Offsite retention; see [Retention and deletion](retention.md). |

## What does not work (yet)

- **Restoring from the offsite storage in the GUI.** The restore task fails with a message pointing
  to `pve-rclone-backup restore` and `pve-rclone-backup backup fetch`. A fetched backup restores
  from the GUI as usual. See [Restoring](restore.md).
- **Backing up to the offsite storage.** A backup job that targets it fails at once with:

    ```
    direct backups to an rclone-backup storage are not supported: back up to a local storage
    listed in 'rclone-replicate-from' and pve-rclone-backupd replicates finished archives offsite
    ```

    Back up to a local storage instead; replication takes it from there.
- **Adding or editing the storage in the GUI.** Proxmox VE does not yet offer forms for storage
  types from plugins. Use `pve-rclone-backup storage init` to add a storage, and
  `pve-rclone-backup storage set` or `pvesm set` to change it. The GUI can still remove and disable
  it, and edit the settings every storage has, such as nodes and backup retention.

## Version compatibility

The plugin supports the storage plugin API of Proxmox VE 9.0 to 9.2 (API versions 12 to 16). On a
newer Proxmox VE, it reports the newest version it was tested with. Proxmox VE then logs a warning
and keeps using the plugin for as long as it supports that API version.

## If the daemon is not running

- Proxmox VE keeps working, and local backups are not affected.
- The offsite storage shows as inactive, and listing its backups fails with a message saying
  that the daemon cannot be reached.
- Archives written while the daemon was stopped are found and uploaded when it starts again.
