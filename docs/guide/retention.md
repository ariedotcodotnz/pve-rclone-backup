# Retention and deletion

## Local and offsite retention are separate

Local backups follow the retention settings of your backup jobs and local storages, as before.
Deleting or pruning a local backup **never** deletes its offsite copy. That is what lets you keep a
few days locally and months offsite.

Offsite backups follow the retention policy of the offsite storage. Without one, every offsite
backup is kept forever.

## Set a policy

Offsite retention uses the same `prune-backups` setting as every Proxmox VE storage, and the same
rules: `keep-last`, `keep-hourly`, `keep-daily`, `keep-weekly`, `keep-monthly` and `keep-yearly`.

```sh
pve-rclone-backup storage set offsite prune-backups=keep-daily=7,keep-weekly=8,keep-monthly=12
```

You can also set it in the Proxmox VE GUI: **Datacenter → Storage → offsite → Edit → Backup
Retention**.

Check what the policy would remove before it does:

```sh
pve-rclone-backup retention preview offsite
pve-rclone-backup retention preview offsite --vmid 100
```

## When retention runs

Every day after [`retention-time`](../reference/daemon-settings.md#retention-time) (05:00 local
time by default), the daemon applies each storage's policy. To apply it now:

```sh
pve-rclone-backup retention run offsite
```

The prune dialog of the Proxmox VE GUI (**Backups → Prune group**) uses the same rules: its preview
matches `retention preview`, and pruning there works like `retention run`.

## Safety rails

On top of Proxmox VE's rules, offsite retention keeps a backup that would otherwise be removed when:

| Rail | Rule |
|---|---|
| [`rclone-min-age`](../reference/storage-properties.md#rclone-min-age) | It is younger than this (7 days by default). |
| [`rclone-keep-min`](../reference/storage-properties.md#rclone-keep-min) | Fewer than this many verified backups of the guest would be left (default 1). |
| Damaged newest backup | It is the guest's newest good backup, and a newer one is [damaged](verification.md#damaged-backups). |
| [`rclone-max-deletes`](../reference/storage-properties.md#rclone-max-deletes) | This many backups were already removed in this run (default 25; the oldest go first). `0` means no limit. |

Protected backups are never removed. `retention preview` shows which rail kept a backup.

## Deletion is delayed

Retention, like deleting a backup by hand, only marks a backup for deletion. It is deleted from the
cloud once the storage's grace period has passed
([`rclone-delete-grace`](../reference/storage-properties.md#rclone-delete-grace), 7 days by
default). Until then, `backup undelete` brings it back:

```sh
pve-rclone-backup backup list --storage offsite --state tombstoned
pve-rclone-backup backup undelete offsite:backup/vzdump-qemu-100-2026_09_01-02_00_01.vma.zst
```

The mark is stored with the backup in the cloud, so the grace period survives the loss of the host.

## Deleted backups skip the recycle bin

OneDrive keeps deleted files in its recycle bin for up to 30 days, and they count towards your quota
all that time. The grace period already gives you a window to bring a backup back, so once it has
passed the daemon deletes the backup permanently. It does not end up in the recycle bin.

To keep deleted backups in the recycle bin as well, set
[`rclone-recycle-bin`](../reference/storage-properties.md#rclone-recycle-bin):

```sh
pve-rclone-backup storage set offsite rclone-recycle-bin=1
```

This setting decides on its own: the remote's `hard_delete` option has no effect on deleted backups.
If OneDrive refuses a permanent deletion, the daemon uses the recycle bin instead and logs a notice.

## How much space offsite backups use

Every vzdump archive is a full backup, so each offsite backup takes about as much space as its
archive. A backup stays in the cloud until retention removes it, which is at least
[`rclone-min-age`](../reference/storage-properties.md#rclone-min-age) after it was made (7 days by
default), and then for the grace period (another 7 days by default). With daily backups that is at
least 14 backups of each guest, whatever `prune-backups` says. Lower both to free space sooner.

## Immutable storages

With [`rclone-immutable`](../reference/storage-properties.md#rclone-immutable) set to `1`, the
storage refuses every deletion and does not apply retention. Storages added by `recover` and
`storage init --read-only` are immutable.

!!! note "What rails cannot protect against"
    Anyone with root access to a node, or with your cloud credentials, can still delete offsite
    backups, for example by changing these settings first, and OneDrive lets them delete files
    permanently. Microsoft 365 subscribers can restore a whole OneDrive to an earlier point within
    the last 30 days. See the [security model](../reference/security.md).
