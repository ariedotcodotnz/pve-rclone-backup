# Offsite backups

## Names

An offsite backup keeps the name of the archive it was uploaded from. Its volume ID, as Proxmox VE
and the commands use it, is the storage name plus that file name:

```
offsite:backup/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst
```

In the rare case that two different archives have the same name, the second one gets a suffix
such as `.1`.

## List and inspect

```sh
pve-rclone-backup backup list                          # all storages
pve-rclone-backup backup list --storage offsite --vmid 100
pve-rclone-backup backup list --state all              # include deleted ones
pve-rclone-backup backup inspect offsite:backup/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst
```

`inspect` shows the archive's size and SHA-256, the segments, when and from where it was uploaded,
and how it was last verified.

The list comes from the daemon's catalogue and needs no network access. The same backups appear in
the Proxmox VE GUI on the storage's **Backups** tab, and in `pvesm list offsite`.

| State | Meaning |
|---|---|
| `complete` | A good offsite backup. |
| `damaged` | Verification found segments missing or altered. See [Verification](verification.md#damaged-backups). |
| `tombstoned` | Marked for deletion and kept until its grace period ends. It can still be brought back. |
| `deleting` | Being deleted. |

## Notes and protection

Offsite backups have notes and a protected flag, like local backups. These are stored with the backup
in the cloud, so they survive the loss of the host. Changes made in the Proxmox VE GUI and on the
command line are the same thing:

```sh
pve-rclone-backup backup notes offsite:backup/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst
pve-rclone-backup backup notes offsite:backup/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst --set "Before the upgrade"
pve-rclone-backup backup protect offsite:backup/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst
pve-rclone-backup backup unprotect offsite:backup/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst
```

When a backup is uploaded, its notes and protected flag are copied from the local backup. Later
changes to the local backup are not copied. A protected backup is never deleted, whether by you or by
retention.

## Delete and undelete

Deleting an offsite backup never removes it straight away:

```sh
pve-rclone-backup backup delete offsite:backup/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst
```

The backup is marked for deletion (a *tombstone*) and hidden from the list. After the storage's
grace period ([`rclone-delete-grace`](../reference/storage-properties.md#rclone-delete-grace), 7
days by default), the daemon deletes it from the cloud. Until then, you can bring it back:

```sh
pve-rclone-backup backup list --state tombstoned
pve-rclone-backup backup undelete offsite:backup/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst
```

Deleting a backup in the Proxmox VE GUI works the same way.

!!! note
    On OneDrive, deleted files go to the recycle bin and count towards your quota until it is
    emptied. `pve-rclone-backup remote about` shows the size of the recycle bin.

A storage with [`rclone-immutable`](../reference/storage-properties.md#rclone-immutable) set to `1`
refuses every deletion.

Deleting or pruning **local** backups never deletes offsite backups.
