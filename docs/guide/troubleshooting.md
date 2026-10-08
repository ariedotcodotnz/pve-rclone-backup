# Troubleshooting

Start with these two commands; most problems show up in them:

```sh
pve-rclone-backup doctor
pve-rclone-backup status
```

## Common problems

### The offsite storage shows as inactive

The daemon is not running, or not reachable. Commands fail with exit status 3.

```sh
systemctl status pve-rclone-backupd
journalctl -u pve-rclone-backupd -n 50
systemctl restart pve-rclone-backupd
```

### Nothing is uploaded

Work through these in order:

1. **Is the recovery kit confirmed?** Replication to an encrypted repository starts only then.
   `pve-rclone-backup recovery-kit status` shows it; `doctor` reports it.
2. **Does the storage replicate from the right local storage?** Check `rclone-replicate-from` in
   `pve-rclone-backup storage show offsite`.
3. **Were the archives skipped?** `pve-rclone-backup queue list --state skipped` lists them, and
   `queue show <job>` gives the reason, such as the guest selection, `rclone-min-interval` or
   `rclone-backfill`.
4. **Can the daemon read the local storage?** If a network storage is not mounted, `status` and
   the daemon's log say so.
5. **Is the archive new?** Uploads start once an archive has been unchanged for a minute. Run
   `pve-rclone-backup queue scan` to look for new archives now.

### Uploads are paused

- **"rejected its credentials"**: the OneDrive token expired or was revoked. Run
  `pve-rclone-backup remote reconnect <remote>`.
- **"is full"**: OneDrive is out of space. Deleted files stay in the OneDrive recycle bin and
  count towards the quota; empty it, or delete backups. `pve-rclone-backup remote about <remote>`
  shows the usage.

### Uploads are slow

- OneDrive throttles busy apps. [Register your own app](remotes.md#your-own-microsoft-app) and
  reconnect the remote with it.
- Check `rclone-bwlimit`, `rclone-transfers` and `upload-workers`; see
  [Bandwidth and load](replication.md#bandwidth-and-load).
- If uploads cannot keep up with your backups, upload fewer of them with `rclone-min-interval`.

### A job failed

```sh
pve-rclone-backup queue list --state failed
pve-rclone-backup queue show <job>      # the error and the job's history
pve-rclone-backup queue retry <job>
```

### A restore or fetch fails

| Message | Cause |
|---|---|
| `... bytes free, ... needed` | Not enough space for staging, or in the target storage. Free space, change [`staging-dir`](../reference/node-settings.md#staging-dir), or stream a VM restore instead of staging it. |
| `... already exists` | A fetched archive with that name is already in the target storage. |
| `guest ... exists` | The VMID is in use. Choose a free VMID, or pass `--force` to overwrite the guest. |
| `the backup is damaged` | See [damaged backups](verification.md#damaged-backups). |

### A backup job fails with "direct backups to an rclone-backup storage are not supported"

The job writes to the offsite storage. Point it at a local storage that the offsite storage
replicates from.

### A backup uploaded on one node is missing on another

Each node refreshes its list regularly. To refresh it now, run
`pve-rclone-backup storage resync <storage>` on that node.

### The state database was lost or damaged

The daemon notices, moves the old file aside, and rebuilds the list of offsite backups from the
cloud. It raises the alert `state-db-rebuilt`. Job history is lost. Interrupted uploads start again;
segments that are already in the cloud are checked and not uploaded twice.

## Getting more detail

Set [`log-level`](../reference/daemon-settings.md#log-level) to `debug` in
`/etc/pve/pve-rclone-backup/daemon.cfg` and restart the daemon:

```sh
echo 'log-level: debug' >> /etc/pve/pve-rclone-backup/daemon.cfg
systemctl restart pve-rclone-backupd
journalctl -u pve-rclone-backupd -f
```

Remove the line again afterwards.

## Questions

**Can it run in an LXC container or a VM?**
No. It is installed on the Proxmox VE host; see [where it runs](../getting-started/requirements.md#where-it-runs).

**Do I need rclone installed?**
No. rclone is built into the daemon. You only need rclone itself for
[manual recovery](../manual-recovery.md) without this tool.

**Which cloud providers are supported?**
OneDrive. Other rclone backends may follow.

**Can it replicate from Proxmox Backup Server?**
No. It replicates the vzdump archive files that backup jobs write to directory-based storages.

**Does deleting a local backup delete the offsite copy?**
No. Offsite backups are removed only by offsite retention, or when you delete them yourself, and
then only after a grace period.

**What happens to backups taken while the cloud is unreachable?**
They are queued and uploaded later. Local backup jobs never wait for the cloud.

**Can I read the backups without this tool?**
Yes, with stock rclone and the recovery kit. See [Manual recovery](../manual-recovery.md).
