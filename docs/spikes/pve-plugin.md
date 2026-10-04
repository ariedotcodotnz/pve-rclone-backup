# Spike: PVE storage plugin and backup provider (M2)

## Verified without a PVE host

These checks ran in `test/perl`: Debian trixie plus Proxmox's `libpve-storage-perl` 9.1.11 (APIVER 15, APIAGE 6), under `perl -T`.

- **Loading and registration**
  - The real `PVE::Storage` custom-plugin loader loads `PVE::Storage::Custom::RcloneBackupPlugin` without warnings.
  - It registers type `rclone-backup`.
  - `api()` reports the running APIVER inside our tested range of 12–15.
- **Properties**
  - Every defined property carries the `rclone-` prefix, plus a `title` and a `description`.
  - There are no collisions with built-in properties or the fixture list of third-party properties.
  - All `options()` refer to known properties.
- **Features**
  - `storage_has_feature('rclone-backup', 'backup-provider')` is true.
  - No sensitive properties are declared.
- **storage.cfg parsing** works through `PVE::Storage::Plugin->parse_config`, with other storage types alongside.
  - SectionConfig validation rejects bad values for every constrained property.
  - The formats `pve-tag-list`, `pve-vmid-list`, `pve-storage-id-list` and `prune-backups` all work.
  - `check_config` forces `shared` on create.
- **Dispatch from PVE entry points**
  - `PVE::Storage::volume_list` reaches `list_volumes`. Invalid volume names coming from the daemon are dropped. `subtype` is set to `qemu` or `lxc`.
  - `PVE::Storage::prune_backups` reaches `prune_backups`. Removals are logged as tombstoning.
  - `PVE::Storage::extract_vzdump_config` reaches the provider's `archive_get_guest_config`, served by the daemon. This is the GUI's "Show Configuration".
  - The provider refuses direct backups (`job_init`) and GUI restores with guidance messages.
- **Daemon failure**
  - Daemon down: `status` returns inactive in under 1 s, and `activate_storage` fails with an explanatory message.
  - Daemon hung: client timeouts are honoured.

## Verified on a real PVE 9.2 node

`make e2e` (`test/e2e`) installs Proxmox VE 9.2 from the official ISO into a nested VM and runs
these checks there. The last run used pve-manager 9.2.21, libpve-storage-perl 9.1.11, qemu-server
9.2.10 and pve-container 6.1.14. The GUI's requests are sent through pveproxy and pvedaemon with an
API token, exactly as the browser sends them.

- **Package installation**
  - `apt install ./pve-rclone-backup_*.deb` pulls in its dependencies.
  - The `pve-api-updates` trigger reloads pvedaemon and pveproxy, so the API serves the new storage
    type right after installation.
  - Purging stops the daemon, keeps `/etc/pve/pve-rclone-backup` and leaves PVE working.
- **Adding the storage** with `pvesh create /storage`.
  - This found a bug, now fixed: PVE activates a storage *before* writing storage.cfg, so the
    daemon does not know it yet. The plugin now validates the configuration instead.
- **Backups tab data** (`GET /nodes/{node}/storage/{storage}/content`): volid, `subtype` (qemu or
  lxc), size, ctime and VMID.
  - Notes and the protected flag round-trip through `PUT .../content/{volume}`.
- **Show Configuration** (`GET /nodes/{node}/vzdump/extractconfig`) and `pvesm extractconfig` are
  served from the manifest.
- **GUI delete** (`DELETE .../content/{volume}`)
  - A protected backup is refused.
  - Otherwise the delete task succeeds. `path()` and `archive_auxiliaries_remove` on the virtual
    path cause no errors, and the backup becomes a tombstone, hidden from the list.
  - `backup undelete` brings it back.
- **Prune dialog**
  - `GET .../prunebackups` previews the marks; `DELETE .../prunebackups` applies them as tombstones.
  - This found a bug, now fixed: PVE passes the VMID as a string.
- **Storage status**
  - Active while the daemon runs.
  - With the daemon stopped, `pvesm status` returns promptly and shows the storage `inactive`.
  - Local backups keep working, and their archives are replicated once the daemon is back.
- **Direct backups are refused.** A vzdump job aimed at the storage fails with:
  `ERROR: Backup job failed - direct backups to an rclone-backup storage are not supported: back up
  to a local storage listed in 'rclone-replicate-from' and pve-rclone-backupd replicates finished
  archives offsite`.
- **Restores**
  - **Fetch, then native restore.** A fetched archive restores with stock `qmrestore`, and the disk's
    SHA-256 matches the original. The fetched copy is never uploaded again.
  - **VM stream restore** gives a matching disk checksum.
  - **Container restores**, privileged and unprivileged, keep payload checksums and setuid files.
  - This found two bugs, now fixed:
    - `pct restore` inherited the daemon's umask 077, which locks out an unprivileged container's
      mapped root.
    - The unit's `RestrictSUIDSGID` blocked setuid files.

## Not verified yet

- **Visual rendering of the GUI**: columns of the Backups tab, the restore dialog chosen from
  `subtype`, and the Show Configuration window. The data these views are built from is verified
  above.
- **A remote that reports no total size.** The test remote is a local file system, which always
  reports one.
