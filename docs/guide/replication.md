# Replication and the queue

## How archives are found

The daemon watches the backup folders of every storage listed in a storage's
`rclone-replicate-from`, in three ways:

- **inotify:** it sees an archive the moment vzdump gives it its final name. vzdump does that only
  when the backup succeeds, so failed and aborted backups are never uploaded.
- **Scans:** it looks through the folders every 15 minutes
  ([`scan-interval`](../reference/node-settings.md#scan-interval)) and at start-up. This catches
  archives written while the daemon was stopped, or by another node on a network share.
- **The vzdump hook (optional):** see [below](#the-vzdump-hook).

All three can report the same archive; it is queued only once. To scan now:

```sh
pve-rclone-backup queue scan
```

For each archive, the daemon checks the storage's rules: which guests to replicate,
`rclone-min-interval`, and `rclone-backfill` for archives that existed before replication was
enabled. A skipped archive is recorded with the reason, as a job in state `skipped`.

Archives that were [fetched back](restore.md#fetch-into-a-local-storage) from offsite are never
uploaded again.

## Uploads

A queued upload waits until the archive has been unchanged for 60 seconds, then:

1. **preparing:** opens the archive and reads the guest's configuration from it;
2. **uploading:** uploads it in segments, each encrypted on the fly and checked against the
   checksum the cloud reports;
3. **verifying:** lists the uploaded segments and compares their sizes and checksums;
4. **committing:** writes the manifest. The backup now exists offsite.

The daemon keeps the archive open during the upload. If local retention deletes the archive
meanwhile, the upload still finishes. If the archive is replaced or changed, the upload stops with
the state `source_lost`. The segments uploaded so far stay in the cloud as an incomplete backup,
which is never listed or restored; they are not yet removed automatically.

After a restart or reboot, an interrupted upload continues with the segment it was working on.

## The queue

```sh
pve-rclone-backup queue list                    # recent jobs
pve-rclone-backup queue list --state failed     # only failed ones
pve-rclone-backup queue show 42                 # one job, with its history
```

The queue also holds fetches, restores, verifications and deletions. Jobs run in order of priority,
then newest backup first.

| State | Meaning |
|---|---|
| `queued` | Waiting to run. |
| `preparing`, `uploading`, `verifying`, `committing` | Running. |
| `retry_wait` | Failed for now; it runs again later on its own. |
| `complete` | Done. |
| `skipped` | Not uploaded because of the storage's rules; the job shows why. |
| `superseded` | Not uploaded because a newer backup of the same guest was queued (`rclone-supersede`). |
| `source_lost` | The local archive disappeared or changed before the upload finished. |
| `failed` | Gave up. It needs your attention; see `queue show`. |
| `cancelled` | Cancelled by you. |

Fetches and restores also use `transferring` (downloading) and `applying` (placing the archive or
running the restore).

### Retries

| Problem | What happens |
|---|---|
| Network errors, timeouts, server errors, throttling | Retried after 30 seconds, then after doubling delays of up to 6 hours, for as long as it takes. If the cloud asks for a longer wait, that is respected. An alert is raised when a job has made no progress for 24 hours. |
| The remote rejects its credentials | Every upload to the remote is paused and an alert is raised. Run `pve-rclone-backup remote reconnect`. |
| The remote is full | Every upload to the remote is paused and retried every 30 minutes. Free space, or empty the recycle bin. |
| Data read back does not match | The job fails. |
| Other errors | Retried up to 10 times, then the job fails. |

### Control the queue

```sh
pve-rclone-backup queue retry 42          # run a failed, cancelled, skipped or superseded job again
pve-rclone-backup queue cancel 42
pve-rclone-backup queue priority 42 50    # -100 to 100; higher runs first
```

## Bandwidth and load

| Setting | Effect |
|---|---|
| [`rclone-bwlimit`](../reference/storage-properties.md#rclone-bwlimit) | Upload bandwidth of a storage, in rclone's format: `4M` is 4 MiB/s. A timetable such as `08:00,1M 23:00,off` limits uploads during the day only. |
| [`rclone-transfers`](../reference/storage-properties.md#rclone-transfers) | Archives a storage uploads at the same time (default 2). |
| [`upload-workers`](../reference/node-settings.md#upload-workers) | Archives a node uploads at the same time across all storages (default 2). |
| [`rclone-segment-size`](../reference/storage-properties.md#rclone-segment-size) | Size of the segments (default 1 GiB). An interrupted upload repeats at most one segment. |

The daemon runs with a low CPU and I/O priority, so uploads give way to guests and backup jobs.

## The vzdump hook

The daemon finds archives on its own, so the hook is optional. It reports each archive as soon as a
backup ends, which helps when another node writes backups to a network share and inotify does not
see the change.

```sh
pve-rclone-backup hook install            # sets 'script:' in /etc/vzdump.conf
pve-rclone-backup hook install --chain    # keeps an existing hook script and runs it after ours
pve-rclone-backup hook status
pve-rclone-backup hook uninstall          # restores a chained script
```

The hook never delays a backup by more than a few seconds, and never fails it, even when the
daemon is down. It is set node-wide; a backup job with its own `script` setting does not use it.
