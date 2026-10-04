# Spike: vzdump file events (M3)

Discovery decides when an archive is finished from what appears in a dump directory, so the
order in which vzdump creates, renames and removes files has to be known, not guessed. These
sequences were recorded on Proxmox VE 9.2.21 (pve-manager 9.2.21, qemu-server 9.2.10,
pve-container 6.1.14) on the nested test node, with `test/e2e/evlog` watching `/var/lib/vz/dump`.
They are committed as fixtures in `internal/discovery/testdata/vzdump/`.

To record them again (for example after a PVE upgrade):

```sh
test/e2e/vm.sh up && eval "$(test/e2e/vm.sh env)"
E2E_SSH=$E2E_SSH E2E_SSH_KEY=$E2E_SSH_KEY E2E_RECORD=$PWD/internal/discovery/testdata/vzdump \
    go test -tags e2e -run TestVzdumpEvents -v ./test/e2e/
```

`TestVzdumpEvents` also asserts the properties below on every run, so the nightly e2e job notices
if a PVE update changes them. `TestReplayVzdumpEvents` in `internal/discovery` replays each
recording against the real inotify watcher and checks that exactly the finished archives are
queued, once each.

## Scenarios

| Fixture | Command | Result |
|---|---|---|
| `qemu-snapshot` | `vzdump 100 --mode snapshot --compress zstd` (VM running) | archive |
| `qemu-stop` | `vzdump 100 --mode stop --compress zstd` | archive |
| `lxc-snapshot` | `vzdump 200 --mode snapshot --compress zstd` (CT running, LVM-thin) | archive |
| `lxc-suspend` | `vzdump 200 --mode suspend --compress zstd` (CT running) | archive |
| `lxc-stop-uncompressed` | `vzdump 200 --mode stop --compress 0` | `.tar` archive |
| `lxc-notes-protected` | `--notes-template '{{guestname}} offsite' --protected 1` | archive with sidecars |
| `lxc-prune` | `--prune-backups keep-last=1` | archive, older ones pruned |
| `qemu-enospc` | backup to a 24 MiB tmpfs storage | failure: out of space |
| `qemu-abort` | `kill -TERM` vzdump while it writes | failure: aborted |

## Findings

- **A finished archive appears only through `rename()`.**
  - vzdump writes `vzdump-<type>-<vmid>-<time>.<vma|tar>.dat` and renames it to the final name
    (`IN_MOVED_FROM` and `IN_MOVED_TO` with one cookie) after the data is complete.
  - No create, modify or close-write event ever names a final archive. Watching `IN_MOVED_TO`
    is enough, and `IN_CLOSE_WRITE` on a final name never happens.
- **Failed and aborted backups never produce a final name.**
  - On ENOSPC or SIGTERM, the `.dat` file is deleted.
  - The `.log` file is still written.
- **The temporary directory lives in the dump directory.** vzdump creates
  `vzdump-<type>-<vmid>-<time>.tmp` (a directory) first and removes it after the rename.
- **Sidecars follow the rename within milliseconds.**
  - `.notes` is written as `<archive>.notes.tmp.<pid>` and renamed.
  - `.protected` is created empty.
  - The task log `<base>.log` is created and written last (10–30 ms after the archive's rename).
  - Discovery's settle rule (archive at least 60 s old, and its `.log` present or 10 minutes
    passed) therefore waits far longer than necessary. It stays because it costs little and also
    covers storages whose events arrive late or not at all (NFS, CIFS).
- **Local prune deletes archives and their logs.**
  - `--prune-backups` removes older archives and their `.log` after the new archive is renamed into
    place, and before the new log is written.
  - A protected archive is kept.
  - Discovery never mirrors these deletions offsite.
- **Archives are written once.** The modify events of an archive all happen before its rename,
  so the size and mtime recorded at discovery stay valid unless the file is replaced.

## Not covered yet

- **Network storages.**
  - inotify does not report changes made by other NFS or CIFS clients.
  - The periodic reconcile scan and the optional vzdump hook cover them.
  - A recording on an NFS-backed storage of the same node (local writes, so events are reported)
    would only repeat the findings above.
- **Backups to PBS** are out of scope, since they produce no files in a dump directory.
