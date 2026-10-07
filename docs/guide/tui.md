# Terminal interface

```sh
pve-rclone-backup tui
```

The terminal interface offers most of what the command line does, in six views. It works over SSH
and in the Proxmox VE web shell (**Node → Shell**), and needs no mouse. Lists update by themselves
as jobs progress.

## Views

| Key | View | Shows |
|---|---|---|
| `1` | Dashboard | Daemon health, queue counts and alerts; each storage's health, usage and newest offsite backup; running jobs. |
| `2` | Backups | Offsite backups of all storages. |
| `3` | Queue | Uploads, fetches, restores, verifications and deletions. |
| `4` | Remotes | Cloud accounts and whether they are authorized. |
| `5` | Storages | Offsite storages, their health and recovery kit status. |
| `6` | Diagnostics | The checks of `pve-rclone-backup doctor`. |

## Keys

Everywhere:

| Key | Action |
|---|---|
| `tab`, `shift+tab` (or `→`, `←`, `l`, `h`), `1` to `6` | Switch views. |
| `↑`, `↓` | Select a row. |
| `ctrl+r`, `F5` | Refresh. |
| `?` | Show the keys. |
| `q`, `ctrl+c` | Quit. |

In each view:

| View | Key | Action |
|---|---|---|
| Backups | `enter` | Details of the selected backup. |
| | `v` | Verify its content (level 3). |
| | `f` | Fetch it into a local backup storage. |
| | `R` | Restore it. |
| | `p` | Protect or unprotect it. |
| | `d` | Delete it (after the grace period). |
| | `u` | Undelete it. |
| Queue | `enter` | Details and history of the selected job. |
| | `r` | Retry it. |
| | `c` | Cancel it. |
| Remotes | `a` | Add a OneDrive remote. |
| | `t` | Test the selected remote. |
| | `c` | Reconnect it (sign in again). |
| | `x` | Remove it. |
| Storages | `enter` | Details of the selected storage. |
| | `i` | Create a new offsite storage. |
| | `k` | Export a recovery kit for it. |
| | `s` | Rebuild its catalogue. |
| Diagnostics | `enter` | Run the checks again. |

Actions that delete or overwrite something ask for confirmation first.

## Setting up from the interface

You can go from nothing to a replicating storage without the command line:

1. In **Remotes**, press `a` and sign in to OneDrive. The interface shows the sign-in address
   and waits for you to paste the redirect address, as on the
   [command line](remotes.md#add-a-onedrive-remote).
2. In **Storages**, press `i`, and fill in the storage name, remote, source name and local
   storages to replicate from.
3. Export the recovery kit, store it, and type its checksum to confirm it.

Changing storage settings is not part of the interface; use `pve-rclone-backup storage set`.
