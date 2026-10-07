# Restoring

There are two ways to get a guest back from an offsite backup:

- **Restore** straight into a guest, with `pve-rclone-backup restore`.
- **Fetch** the backup into a local backup storage, then restore it like any local backup: in the
  Proxmox VE GUI, or with `qmrestore` or `pct restore`.

Both check every segment and the whole archive against the SHA-256 digests recorded at upload.

!!! note "Restoring from the GUI"
    The Proxmox VE GUI cannot yet restore straight from an offsite storage. If you try, the task
    fails and points you to `restore` or `backup fetch`. A fetched backup can be restored from
    the GUI.

## Restore a guest

```sh
pve-rclone-backup restore offsite:backup/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst --vmid 900
```

| Option | Meaning |
|---|---|
| `--vmid` | The ID of the restored guest. Required. |
| `--target-storage` | Storage for the guest's disks. Without it, `qmrestore` and `pct restore` use their defaults; a VM's disks go back to the storages recorded in the backup. |
| `--unique` | Give the guest new MAC addresses and other unique identifiers, for a copy that runs next to the original. |
| `--force` | Overwrite an existing guest with that ID. You are asked to confirm. |
| `--mode` | `stream` or `stage`; see below. |
| `--no-wait` | Return once the restore is queued. Follow it with `pve-rclone-backup queue show`. |

The command shows progress until the restore has finished.

### Virtual machines

By default, a VM is **streamed**: the archive is downloaded, decrypted and decompressed straight
into `qmrestore`, without being stored on local disk. If the archive's digest turns out wrong at the
end, the restore fails and the new VM is removed.

With `--mode stage`, the archive is first downloaded to the staging directory and fully checked,
and only then restored. This needs local space for the archive, but nothing is written to the
target storage unless the archive is good.

### Containers

Containers are always staged: the archive is downloaded to the staging directory, checked, and
restored with `pct restore`. This also works for privileged containers.

### Staging space

Staging uses [`staging-dir`](../reference/node-settings.md#staging-dir)
(`/var/lib/pve-rclone-backup/staging` by default). A restore starts only if the archive fits while
leaving [`staging-reserve`](../reference/node-settings.md#staging-reserve) (10 GiB) free. Staged
archives are removed after the restore, and leftovers are removed when the daemon starts.

Restores run with the I/O priority set by
[`restore-ionice`](../reference/node-settings.md#restore-ionice).

## Fetch into a local storage

```sh
pve-rclone-backup backup fetch offsite:backup/vzdump-lxc-200-2026_10_04-02_00_01.tar.zst --to-storage local
```

The archive is downloaded into the storage's backup folder under a temporary name, checked, and
then given its original name. It then appears as an ordinary local backup, with:

- notes that say where and when it was fetched from, followed by the original notes;
- the backup log, if one was uploaded;
- **protection**, so that the next local prune does not delete it. Pass `--no-protect` to skip this.

A fetched archive is never uploaded again.

The target storage needs room for the archive plus `staging-reserve`.

## Damaged backups

A backup that verification found [damaged](verification.md#damaged-backups) is not restored. Restore
an older backup instead. If no good backup is left, `--allow-damaged` tries anyway; expect the
restore to fail where data is missing or altered.

## Restore without this tool

The archives can also be reassembled with stock rclone; see
[Manual recovery](../manual-recovery.md).
