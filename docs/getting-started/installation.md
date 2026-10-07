# Installation

pve-rclone-backup ships as one Debian package that contains:

- the daemon `pve-rclone-backupd`;
- the command line and terminal interface `pve-rclone-backup`;
- the Proxmox VE storage plugin.

Install it on every node that should replicate backups or list offsite backups.

## Get the package

No release packages are published yet. Either build the package yourself, or download the `deb`
artifact of a successful [CI run](https://github.com/ariedotcodotnz/pve-rclone-backup/actions/workflows/ci.yml)
on the `master` branch.

To build it you need Go 1.26 or newer, `make` and `dpkg-deb`. Any Linux machine works; it does not
have to be the Proxmox VE host.

```sh
git clone https://github.com/ariedotcodotnz/pve-rclone-backup.git
cd pve-rclone-backup
make deb
```

The package is written to `dist/pve-rclone-backup_<version>_amd64.deb`.

## Install

Copy the package to the node and install it with `apt`, which also installs any missing
dependencies:

```sh
apt install ./pve-rclone-backup_<version>_amd64.deb
```

The installation:

- starts `pve-rclone-backupd` and enables it at boot;
- reloads the Proxmox VE API services, so that they pick up the new `rclone-backup` storage type.

Check that everything is in place:

```sh
systemctl status pve-rclone-backupd
pve-rclone-backup doctor
```

`doctor` reports a missing recovery kit or storage until you finish the
[quick start](quick-start.md). That is expected.

Shell completion for bash and zsh is installed with the package and works in a new shell.

## Upgrade

Install the newer package the same way. The daemon restarts. Uploads that were interrupted resume
from their last complete segment.

## Uninstall

!!! warning "Remove the storages first"
    Before you uninstall, remove the `rclone-backup` storages from **Datacenter → Storage** (or
    with `pvesm remove <storage>`). Without the plugin, Proxmox VE cannot read these storage
    entries, and a later change to the storage configuration can drop them.

```sh
apt remove pve-rclone-backup    # keeps the local state in /var/lib/pve-rclone-backup
apt purge pve-rclone-backup     # also removes the local state
```

Neither removes anything in the cloud. Settings, keys and credentials under `/etc/pve` are also
kept, because they are shared by every node of a cluster. To remove them as well, once no node
uses them:

```sh
rm -r /etc/pve/pve-rclone-backup /etc/pve/priv/pve-rclone-backup
```

!!! danger
    `/etc/pve/priv/pve-rclone-backup` holds the encryption keys. Delete it only if you have a
    recovery kit, or no longer need the offsite backups.
