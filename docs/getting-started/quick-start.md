# Quick start

This page takes you from a fresh installation to your first offsite backup. Run every command as
root on a Proxmox VE node where the package is [installed](installation.md).

The example uses these names; choose your own:

| Name | Example | What it is |
|---|---|---|
| Remote | `onedrive-main` | The connection to your OneDrive account. |
| Storage | `offsite` | The new Proxmox VE storage that lists your offsite backups. |
| Source | `homelab` | A name for this Proxmox VE installation or cluster inside the repository. |
| Local storage | `local` | The storage your backup jobs already write to. |

## 1. Connect OneDrive

```sh
pve-rclone-backup remote add onedrive-main
```

1. The command prints a Microsoft sign-in address. Open it in a browser on any device, such as your
   laptop, and sign in.
2. After you approve access, the browser tries to open `http://localhost:53682/...` and shows an
   error page. That is expected.
3. Copy the complete address from the browser's address bar and paste it into the terminal.
4. For the type of connection, choose `onedrive`.
5. For the drive, choose the one named **OneDrive**. Do not just press Enter: the suggested drive
   is often a hidden system drive, such as `ODCMetadataArchive`.
6. Confirm the drive it found.

Then check that the remote works. This writes, reads back and deletes a small test file, and shows
your quota:

```sh
pve-rclone-backup remote test onedrive-main
```

!!! tip
    If you have registered your own Microsoft app, add `--client-id <id>`; the command then asks
    for the app's secret. See [Remotes](../guide/remotes.md#your-own-microsoft-app).

## 2. Create the offsite storage

```sh
pve-rclone-backup storage init offsite --remote onedrive-main --source homelab \
    --replicate-from local
```

This:

1. creates an encrypted repository in the folder `pve-backups` of your OneDrive, with new random
   encryption keys;
2. writes a **recovery kit** to `offsite-recovery-kit.txt` in the current directory;
3. waits for you to confirm that you have stored the kit (see below);
4. adds the storage `offsite` to Proxmox VE.

While it waits, copy the kit off the host, for example from your computer with
`scp root@<host>:/root/offsite-recovery-kit.txt .`, and keep it in a password manager or print it.
Then type the kit's checksum, which the command printed above the prompt in the form
`xxxx-xxxx-xxxx-xxxx`, and press Enter.

!!! danger "Store the recovery kit before you go on"
    The recovery kit holds the encryption keys. Without it, your offsite backups **cannot be
    restored** if this host is lost: the keys exist nowhere else. Copy it somewhere off this host,
    such as a password manager or a printout, then delete the local file. Treat it like a
    password: do not paste it into a terminal, a chat or an issue.

    To protect the kit with a passphrase, put the passphrase in a file and add
    `--kit-passphrase-file <file>`.

Replication starts once the kit is confirmed.

## 3. Back up as usual

Your backup jobs need no changes. When a job writes an archive to `local`, the daemon notices it and
queues the upload. It waits until the archive has been unchanged for a minute, then uploads it.

When you enable replication, the newest existing archive of each guest is also uploaded
([`rclone-backfill`](../reference/storage-properties.md#rclone-backfill)). To try it straight away,
back up a guest:

```sh
vzdump 100 --storage local
```

## 4. Watch it go offsite

```sh
pve-rclone-backup status         # daemon health, storages and queue counts
pve-rclone-backup queue list     # uploads and their progress
pve-rclone-backup backup list    # offsite backups
```

The backups also appear in the Proxmox VE GUI: select the storage `offsite` and open
**Backups**.

## 5. Choose an offsite retention policy

By default, offsite backups are kept forever. To thin them out, give the storage a retention policy
in Proxmox VE's usual format. Local retention stays separate: deleting or pruning a local backup
never removes the offsite copy.

```sh
pve-rclone-backup storage set offsite prune-backups=keep-last=3,keep-weekly=4,keep-monthly=6
pve-rclone-backup retention preview offsite
```

Deleted offsite backups are kept for a grace period (7 days by default) and can be brought back.
See [Retention and deletion](../guide/retention.md).

## 6. Test a restore

A backup you have never restored is a hope, not a backup. Restore a VM to a new, unused VMID:

```sh
pve-rclone-backup backup list --vmid 100
pve-rclone-backup restore offsite:backup/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst \
    --vmid 900 --target-storage local-lvm
```

See [Restoring](../guide/restore.md) for containers, other modes, and restoring from the Proxmox VE
GUI.

## Next steps

- Practise [disaster recovery](../dr-runbook.md) on a spare machine, using your recovery kit.
- Run `pve-rclone-backup doctor` from your monitoring, or now and then; it exits with status 5
  when it finds problems. See [Monitoring](../guide/monitoring.md).
- Try `pve-rclone-backup tui`, the [terminal interface](../guide/tui.md).
