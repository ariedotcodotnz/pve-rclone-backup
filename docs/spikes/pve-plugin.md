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

## Still to verify on a real PVE 9.2 node (nested VM, M2 harness)

- Backups tab rendering: size, notes and the protected flag; restore dialog type detection via `subtype`; "Show Configuration" button.
- The GUI delete flow: `path()`, then `free_image()`, then `archive_auxiliaries_remove` on the virtual path.
- Prune dialog dry-run and apply.
- How the GUI shows `status()` when the total is unknown.
- pvestatd behaviour and latency with the daemon up and down.
- The vzdump task error text when a job targets this storage.
- Package installation triggering the pve-manager reload.
