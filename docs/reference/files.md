# Files and paths

## Installed by the package

| Path | Contents |
|---|---|
| `/usr/sbin/pve-rclone-backupd` | The daemon. |
| `/usr/bin/pve-rclone-backup` | The command line tool and terminal interface. |
| `/usr/lib/systemd/system/pve-rclone-backupd.service` | The daemon's systemd unit. |
| `/usr/share/perl5/PVE/Storage/Custom/RcloneBackupPlugin.pm` | The Proxmox VE storage plugin. |
| `/usr/share/perl5/PVE/Storage/Custom/RcloneBackup/` | Helper modules of the plugin. |
| `/usr/share/perl5/PVE/BackupProvider/Plugin/Rclone.pm` | The plugin's backup provider. |
| `/usr/share/pve-rclone-backup/vzdump-hook` | The optional [vzdump hook](../guide/replication.md#the-vzdump-hook). |
| `/usr/share/doc/pve-rclone-backup/` | The README, the [disaster recovery runbook](../dr-runbook.md) and [manual recovery](../manual-recovery.md). |
| `/usr/share/man/man1/pve-rclone-backup*.1.gz` | Manual pages of the command line tool. |
| `/usr/share/bash-completion/completions/pve-rclone-backup`, `/usr/share/zsh/vendor-completions/_pve-rclone-backup` | Shell completion. |

## Shared by the cluster

These live in the Proxmox VE cluster file system, which every node of a cluster shares. The package
never removes them.

| Path | Contents |
|---|---|
| `/etc/pve/storage.cfg` | The `rclone-backup` storages, with all other storages. See [Storage properties](storage-properties.md). |
| `/etc/pve/pve-rclone-backup/daemon.cfg` | [Daemon settings](daemon-settings.md) (optional). |
| `/etc/pve/nodes/<node>/pve-rclone-backup.cfg` | [Node settings](node-settings.md) of one node (optional). |
| `/etc/pve/pve-rclone-backup/source.json` | The installation's identity in repositories. |
| `/etc/pve/pve-rclone-backup/kits.json` | Which recovery kits were exported and confirmed (no secrets). |
| `/etc/pve/priv/pve-rclone-backup/remotes.conf` | Remote credentials (OAuth tokens). Root only. |
| `/etc/pve/priv/pve-rclone-backup/keys/<repository>.json` | Encryption keys of each repository. Root only. |

## Local to each node

| Path | Contents |
|---|---|
| `/var/lib/pve-rclone-backup/state.db` | The queue, job history and catalogue (SQLite). A cache: it can be rebuilt from the cloud. Removed by `apt purge`. |
| `/var/lib/pve-rclone-backup/state.db.schema-<n>` | A copy of the database taken before an upgrade changed its format. |
| `/var/lib/pve-rclone-backup/state.db.<reason>-<time>` | A database that could not be used, moved aside. |
| `/var/lib/pve-rclone-backup/staging/` | Archives staged for restores (the default [`staging-dir`](node-settings.md#staging-dir)). |
| `/var/lib/pve-rclone-backup/vzdump-hook.chain` | The hook script that the vzdump hook runs after its own work, if any. |
| `/run/pve-rclone-backup/api.sock` | The daemon's API socket. Root only. |

The storage plugin reports paths under `/run/pve-rclone-backup/virtual/` for offsite backups. They
never exist as files.

## In the cloud

A repository is a folder in the remote (`pve-backups` by default):

| Path | Contents |
|---|---|
| `pve-rclone-backup.repo.json` | Repository marker: format version and encryption settings. No secrets. |
| `g1/` | Everything encrypted with the repository's first set of keys. Below it, names are encrypted. |

Inside `g1/`, after decryption, each backup has a folder
`v1/<source>/<qemu|lxc>/<vmid>/<time>/` with the archive's segments (`part.000000`, ...), the
backup log, `meta.json` (notes, protection, deletion mark) and `manifest.json`. See
[Manual recovery](../manual-recovery.md#repository-layout) for the details.
