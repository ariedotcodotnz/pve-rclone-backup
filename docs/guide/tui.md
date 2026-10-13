# Terminal interface

```sh
pve-rclone-backup tui
```

The terminal interface covers every CLI operation and its options, in seven views. It works over
SSH and in the Proxmox VE web shell (**Node → Shell**), and needs no mouse. Lists update as jobs
progress. Tables, forms and output adapt to the terminal size; long content scrolls with the keyboard.

Run it as root on the Proxmox node. When starting it directly from another machine, allocate a
terminal with SSH:

```sh
ssh -t root@pve-node pve-rclone-backup tui
```

For a different daemon socket, use `pve-rclone-backup --socket /path/to/api.sock tui`.

## Views

| Key | View | Shows |
|---|---|---|
| `1` | Dashboard | Daemon health, queue counts and alerts; each storage's health, usage and newest offsite backup; running jobs. |
| `2` | Backups | Offsite backups of all storages. |
| `3` | Queue | Uploads, fetches, restores, verifications and deletions. |
| `4` | Remotes | Cloud accounts and whether they are authorized. |
| `5` | Storages | Offsite storages, their health and recovery kit status. |
| `6` | Diagnostics | The checks of `pve-rclone-backup doctor`. |
| `7` | Actions | Searchable forms for every CLI command, including recovery, retention, configuration, hook management, version and completion utilities. |

## Keys

Everywhere:

| Key | Action |
|---|---|
| `tab`, `shift+tab` (or `→`, `←`, `l`, `h`), `1` to `7` | Switch views. |
| `↑`, `↓` | Select a row. |
| `page up`, `page down`, `home`, `end` | Move through lists or scroll output. |
| `A` | Actions for the selected backup, job, remote or storage, with its ID filled in. |
| `:` | Open the Actions view. |
| `ctrl+r`, `F5` | Refresh. |
| `?` | Show the keys. |
| `q`, `ctrl+c` | Quit from a view. Inside a running command, `ctrl+c` stops that command first. |

In each view:

| View | Key | Action |
|---|---|---|
| Backups | `enter` | Details of the selected backup. |
| | `v` | Verify it at level 2, 3 or 4, with scratch guest options for level 4. |
| | `f` | Fetch it into a local backup storage. |
| | `R` | Restore it. |
| | `p` | Protect or unprotect it. |
| | `d` | Delete it (after the grace period). |
| | `u` | Undelete it. |
| | `n` | View or edit notes, including multiline notes. |
| | `H` | Verification history for it. |
| Queue | `enter` | Details and history of the selected job. |
| | `r` | Retry it. |
| | `c` | Cancel it. |
| | `p` | Set its priority. |
| | `s` | Scan for finished archives, optionally restricting the storage. |
| Remotes | `enter` | Details of the selected remote. |
| Remotes | `a` | Add a OneDrive remote. |
| | `t` | Test the selected remote. |
| | `c` | Reconnect it (sign in again). |
| | `x` | Remove it. |
| | `b` | Provider quota. |
| Storages | `enter` | Details of the selected storage. |
| | `i` | Create a new offsite storage. |
| | `k` | Export a recovery kit for it. |
| | `s` | Rebuild its catalogue. |
| | `e` | Change storage properties. Enter one `property=value` per line. |
| | `p` | Preview retention, with optional guest and type filters. |
| | `P` | Apply retention after confirmation. |
| Diagnostics | `enter` | Run the checks again. |
| Actions | `/` | Search names and descriptions. Multiple words narrow the results. |
| | `enter` | Open the selected command's form. |
| | `esc` | Clear the search. |

## Command forms and output

Choose an action in **Actions**, or press `A` on a selected row. The form contains the command's
positional arguments and all its CLI options, with their descriptions and defaults. Existing
CLI validation and confirmations also apply here. Commands execute inside the interface;
you do not need to open a shell or type command-line syntax.

| Key | In a form |
|---|---|
| `tab`, `shift+tab` | Select the next or previous field. |
| `enter` | Advance a single-line field; insert a newline in a multiline field. |
| `space` | Toggle a boolean option. |
| `ctrl+s` | Submit from any field. |
| `ctrl+x` | Toggle the selected option between explicitly set and default. Resetting restores its original default value. |
| `page up`, `page down` | Scroll the form and instructions. |
| `esc` | Cancel the form. During a command's question, cancel that command. |

Editing an option sets it. An untouched optional field uses the CLI default. To explicitly pass
an empty value, use `ctrl+x` on that option: for example, an empty `--set` clears backup notes.
Repeated options such as remote `--param` and `--answer` accept one value per line; comma-separated
options such as `--replicate-from` use commas. Values containing spaces are entered as ordinary
text and need no shell quoting. Secret prompts mask the answer. Options that read secrets from
files, such as `--kit-passphrase-file`, retain their CLI behavior.

The output view streams command results, progress and errors. Questions open an answer form with
the recent output for context; this includes sign-in addresses and recovery-kit checksums.
Use `page up`/`page down` to read output, and `end` to follow the latest lines. When the command
finishes, its exit code appears in the hint; `s` saves the output to a new file with mode `0600`,
and `esc` or `enter` closes the output.

While a command runs, `esc` or `ctrl+c` stops it. When waiting for a queued daemon job, this stops
waiting; the job continues. Use **Queue**, `c`, to cancel that job. Choose `--no-wait=true` in a
fetch, restore or verification form to queue it immediately and follow it in **Queue**.

The action catalogue comes from the CLI command tree, so new commands and options appear
automatically. **Actions** remains available when the daemon is offline, allowing operations
that work locally, such as `recovery-kit show`, `hook status`, and completion generation.

The interface uses Bubble Tea, Bubbles and Lip Gloss. Mouse reporting is disabled, so normal
terminal text selection and SSH keyboard input work without special mouse support.

## Setting up from the interface

You can go from nothing to a replicating storage without the command line:

1. In **Remotes**, press `a` and sign in to OneDrive. The interface shows the sign-in address
   and waits for you to paste the redirect address, as on the
   [command line](remotes.md#add-a-onedrive-remote).
2. In **Storages**, press `i`, and fill in the storage name, remote, source name and local
   storages to replicate from. Optional flags include read-only recovery and source adoption.
3. Export the recovery kit, store it, and type its checksum to confirm it.

For disaster recovery, open **Actions**, search for `recover`, and supply the kit file and
recovery options. To verify, import, inspect or confirm an existing kit, search for `recovery-kit`.
Storage settings are available with **Storages**, `e`; configuration schemas are under
**Actions**, `config schema`.
