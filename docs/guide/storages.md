# Storages

An offsite storage is a Proxmox VE storage of type `rclone-backup`. It has three jobs:

- it says **where** offsite backups go: a remote, a repository path, and a source name;
- it says **what** to replicate: which local storages, and which guests;
- it **shows** the offsite backups in Proxmox VE.

## Create a storage

Use `storage init`, not the Proxmox VE GUI or `pvesm add`. Besides adding the storage, it creates
the repository and its keys, and makes sure you have a recovery kit:

```sh
pve-rclone-backup storage init offsite --remote onedrive-main --source homelab \
    --replicate-from local
```

| Option | Meaning |
|---|---|
| `--remote` | The [remote](remotes.md) to use. Required. |
| `--source` | The name of this installation inside the repository: lowercase letters, digits and `-`, up to 32 characters. Your cluster's name is a good choice. Required. |
| `--path` | The repository folder in the remote. Default: `pve-backups`. |
| `--replicate-from` | The local backup storages to replicate, separated by commas. Without it, the storage replicates nothing until you set [`rclone-replicate-from`](../reference/storage-properties.md#rclone-replicate-from). |
| `--encryption` | `crypt` (default) or `none`. |

`storage init` then:

1. creates the repository, or uses the existing one at that path (for example, one that another
   cluster already uses);
2. registers the source name, and refuses a name that another installation already uses;
3. for an encrypted repository, writes a [recovery kit](recovery-kit.md) and asks you to confirm
   it;
4. adds the storage to `/etc/pve/storage.cfg` through the Proxmox VE API.

The result in `/etc/pve/storage.cfg` looks like this:

```
rclone-backup: offsite
	content backup
	rclone-encryption crypt
	rclone-path pve-backups
	rclone-remote onedrive-main
	rclone-replicate-from local
	rclone-source homelab
	shared 1
```

!!! note "Settings that cannot change"
    `rclone-path`, `rclone-encryption` and `rclone-source` are fixed when the storage is created.
    To change them, create a new storage.

## Change settings

```sh
pve-rclone-backup storage set offsite rclone-transfers=1 rclone-bwlimit=4M
pvesm set offsite --rclone-transfers 1          # the same, with Proxmox VE's own tool
pvesm set offsite --delete rclone-bwlimit       # back to the default
```

The daemon notices changes within a few seconds; it needs no restart. All settings are described in
[Storage properties](../reference/storage-properties.md).

## Choose what to replicate

### Source storages

[`rclone-replicate-from`](../reference/storage-properties.md#rclone-replicate-from) lists the
local storages whose finished backups are replicated. They must be storages with a directory, such
as Directory, NFS, CIFS or CephFS storages, that hold backups (content `backup`). Their backups are
found in their `dump` folder, or the folder set for backups in `content-dirs`.

A Proxmox Backup Server storage cannot be a source: its backups are not vzdump archive files.

### Guests

By default, every guest's backups are replicated. To choose:

| Setting | Effect |
|---|---|
| `rclone-guests=all` | Every guest (default). |
| `rclone-guests=tagged` | Only guests with one of the tags in `rclone-tags` (default tag: `offsite`). Add the tag to a guest in the GUI under **Summary → Tags**. |
| `rclone-guests=listed` | Only the guests in `rclone-vmids`, such as `rclone-vmids=100,101,200`. |
| `rclone-exclude-vmids=...` | Never these guests, whatever `rclone-guests` says. |

For example, to replicate only guests tagged `offsite` or `important`:

```sh
pve-rclone-backup storage set offsite rclone-guests=tagged 'rclone-tags=offsite;important'
```

### Fewer offsite backups than local ones

Uploading every daily backup may take more bandwidth than you have.
[`rclone-min-interval`](../reference/storage-properties.md#rclone-min-interval) skips a backup if
the guest already has an offsite backup, or a queued upload, that is newer than the interval. For
example, with daily local backups:

```sh
pve-rclone-backup storage set offsite rclone-min-interval=7d   # about one offsite backup a week
```

### Existing backups

When replication is first enabled for a storage, existing archives are handled according to
[`rclone-backfill`](../reference/storage-properties.md#rclone-backfill):

- `latest` (default): upload the newest existing archive of each guest;
- `all`: upload every existing archive;
- `none`: upload only backups made from now on.

## Several storages

- **Several storages on one remote**: give them different `--path` values to keep separate
  repositories with their own keys, or the same path with different `--source` names.
- **Several clusters on one account**: use the same remote path, and give each cluster its own
  source name. Each cluster sees only its own backups.
- **Two offsite copies**: create two storages, on two remotes, that both replicate from `local`.

## Browse another installation's backups

To list and restore the backups that another installation stored in a repository, add a read-only
storage for its source. This is how [disaster recovery](../dr-runbook.md) works:

```sh
pve-rclone-backup storage init office-dr --remote onedrive-main --source office --read-only
```

A read-only storage uploads nothing and is immutable: none of its backups can be deleted or pruned
through it.

## Rebuild the catalogue

The daemon keeps each storage's list of backups in sync with the cloud on its own. To refresh it now,
for example after changes made from another installation:

```sh
pve-rclone-backup storage resync offsite
```

## Show and remove storages

```sh
pve-rclone-backup storage list
pve-rclone-backup storage show offsite
pvesm remove offsite
```

Removing a storage only removes it from Proxmox VE. Its offsite backups, the repository and its keys
stay, so you can add the storage again later with `storage init` and the same remote, path and
source.
