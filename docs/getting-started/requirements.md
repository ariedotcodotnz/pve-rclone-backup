# Requirements

## Where it runs

pve-rclone-backup is installed **on the Proxmox VE host itself**, on every node whose backups
should go offsite, like other Proxmox VE storage plugins. It does not run in a container or a VM,
because it has to work with Proxmox VE directly:

- Proxmox VE's own services load its storage plugin, so that offsite backups appear in the GUI.
- The daemon reads the archives in your backup storages as vzdump finishes them.
- It keeps its settings, keys and credentials in the cluster file system, `/etc/pve`.
- Restores run Proxmox VE's own tools, `qmrestore` and `pct`.

The daemon runs as a systemd service with restrictions that do not get in the way of these tools.
It opens no network ports. See the [security model](../reference/security.md).

## Proxmox VE

- **Proxmox VE 9.0 or newer**, on amd64. The storage plugin supports storage plugin API versions
  12 to 16 (Proxmox VE 9.0 to 9.2).
- A **local backup storage** that your backup jobs write to, such as `local` or an NFS or CIFS
  share. pve-rclone-backup replicates the archives that appear there; it never writes backups
  itself.
- Root access to every node that should replicate. All commands run as root.

The package depends on `libpve-storage-perl`, `libjson-perl`, `zstd`, `tar` and `gzip`, which every
Proxmox VE installation has. Restores also use the Proxmox VE tools `qmrestore`, `pct` and `vma`.

## Cloud storage

- A **Microsoft OneDrive** account. pve-rclone-backup is developed for OneDrive Personal; it uses
  rclone's OneDrive support, which also covers OneDrive for Business and SharePoint document
  libraries.
- Enough free space for your offsite backups. Each backup is stored once, at the size of its
  compressed archive plus less than 0.03% for encryption.
- **Optional, but recommended:** your own Microsoft app registration (a client ID and secret).
  Without one, the account uses rclone's shared app, which Microsoft throttles more often.

!!! note "OneDrive limits"
    - A single file in OneDrive can be at most 250 GiB. Archives are uploaded in segments of
      `rclone-segment-size` (1 GiB by default), so archives of any size fit.
    - Deleted files go to the OneDrive recycle bin and still count towards your quota until it is
      emptied (after 30 days, or by hand).
    - OneDrive refresh tokens expire after 90 days without use. A daemon that runs regularly
      renews its token on its own; a token stored only in a recovery kit may expire.

## Network

The daemon needs outbound HTTPS to the cloud provider. It opens no network ports: the command line
tool, the terminal interface and the Proxmox VE plugin talk to it through a local socket that only
root can use.

Signing in to OneDrive does not need a browser on the Proxmox VE host. You open a sign-in address on
any device and paste the result back. See [Remotes](../guide/remotes.md).

## Local disk

Replication reads the archives where vzdump wrote them and needs no extra space. Some restores need
space to stage an archive:

| Operation | Space needed on the node |
|---|---|
| VM restore (default stream mode) | None. |
| VM restore in stage mode, container restore | The archive's size, in the staging directory. |
| Fetch into a local backup storage | The archive's size, in that storage. |

The staging directory is `/var/lib/pve-rclone-backup/staging` unless you change
[`staging-dir`](../reference/node-settings.md#staging-dir).
