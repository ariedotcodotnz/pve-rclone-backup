# Verification

Verification checks that offsite backups are still there and unchanged. There are four levels:

| Level | What is checked | Cost | When |
|---|---|---|---|
| 1 | Every segment exists, with the expected size. | A folder listing. | After every upload, and at every catalogue rebuild. |
| 2 | The checksum the cloud reports for each segment still matches the one recorded at upload. | A folder listing; nothing is downloaded. | After every upload, and at every catalogue rebuild. |
| 3 | The whole archive is downloaded and decrypted. Each block is authenticated, and every SHA-256 digest is checked, as well as the archive's structure (`vma verify` or `tar -t`). | Downloads the archive. | When you ask, or on a rolling schedule within a daily budget. |
| 4 | The backup is restored into a scratch guest, which is then removed. | A full restore. | When you ask. |

## Scheduled verification

Levels 1 and 2 run whenever the daemon rebuilds a storage's catalogue: every
[`rclone-verify-interval`](../reference/storage-properties.md#rclone-verify-interval) (1 day by
default), and at least every
[`catalog-resync-interval`](../reference/daemon-settings.md#catalog-resync-interval) (6 hours).

Level 3 is off by default, because it downloads backups. To verify each backup's content regularly,
set an interval and a daily budget:

```sh
pve-rclone-backup storage set offsite rclone-verify-content=30d rclone-verify-budget=50G
```

Every hour, the daemon then queues level 3 verifications of backups that were not content-verified
within the interval: first those never content-verified, then the least recently verified. It stops when the bytes verified in
the last 24 hours reach the budget. A backup larger than the whole budget is still verified, on a
day when nothing else was.

In a cluster, one node plans the scheduled verifications for all.

## Verify now

```sh
pve-rclone-backup backup verify offsite:backup/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst            # level 3
pve-rclone-backup backup verify offsite:backup/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst --level 2  # the storage's listing checks
pve-rclone-backup backup verify offsite:backup/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst --level 4 \
    --scratch-vmid 9999 --scratch-storage local-lvm
```

Level 4 needs an unused VMID. The scratch guest gets new MAC addresses so that it cannot clash with
the original, and is removed afterwards. It is not started.

See past results:

```sh
pve-rclone-backup verify history
```

`backup list` and `backup inspect` show the highest level each backup passed, and when.

## Damaged backups

When a check finds segments missing, of the wrong size or altered, the backup is marked
**damaged**. Then:

- an alert is raised, which `status` and `doctor` report;
- the backup is not restored unless you pass `--allow-damaged`;
- while the guest's newest backup is damaged, retention keeps its newest good backup;
- the damaged backup is never deleted automatically.

The next backup of the guest is uploaded as usual. If a later check finds the backup intact, for
example after a temporary problem at the cloud provider, the alert is cleared.

```sh
pve-rclone-backup backup list --state damaged
```
