# Encryption and recovery kits

## Encryption

Repositories are encrypted by default with [rclone crypt](https://rclone.org/crypt/). Contents and
names are both encrypted, so the cloud provider cannot read your backups, guest configurations or
VMIDs. Each block of 64 KiB is authenticated, so altered data is detected when it is read.

Each repository has its own two random 256-bit keys, created by `storage init`. They are stored
on the node in `/etc/pve/priv/pve-rclone-backup/keys/`, which only root can read and which Proxmox
VE replicates to every node of a cluster.

A repository created with `--encryption none` is stored as plain files. Its backups can be read by
anyone with access to the cloud account. This setting cannot be changed later.

## The recovery kit

The keys exist only on your Proxmox VE nodes. If you lose those nodes, you need another copy: the
**recovery kit**. It is a small text file that contains, for each repository:

- the remote's name and the repository's path;
- the source names;
- the crypt keys and settings;
- optionally, the remote's credentials (`--include-token`).

Its header lists the repositories in plain text and never contains secrets. The keys follow in an
armored block. The kit also has a short **checksum**, which you type back to confirm that your
copy is complete.

!!! danger
    Without the keys, encrypted offsite backups cannot be restored by anyone, including you. Keep the
    recovery kit somewhere that survives the loss of your Proxmox VE hosts, such as a password manager
    or a printout in a safe place.

### Protect the kit with a passphrase

Without a passphrase, anyone who has the kit file can read your keys. To encrypt the kit, write a
passphrase to a file and pass it when exporting:

```sh
pve-rclone-backup recovery-kit export --kit-file /root/offsite-kit.txt \
    --kit-passphrase-file /root/kit-passphrase
```

The keys are then encrypted with a key derived from the passphrase (scrypt and
XChaCha20-Poly1305). Store the passphrase separately from the kit, and do not lose it.

### Include the credentials

With `--include-token`, the kit also holds the remote's OAuth token. After a disaster, you can then
restore without signing in to the cloud again. The kit then also grants access to the cloud account,
so passphrase-protect it. OneDrive tokens expire after 90 days without use, so a kit's token can be
too old to use; you can always sign in again instead.

## Export and confirm

`storage init` exports a kit for each new encrypted repository and asks you to confirm it.
**Replication to a repository starts only after its kit is confirmed.**

To export a kit later (for example, a fresh copy, or a kit for all storages at once):

```sh
pve-rclone-backup recovery-kit export --kit-file /root/kit.txt                  # all storages
pve-rclone-backup recovery-kit export --kit-file /root/kit.txt --storage offsite
```

The command shows the kit's checksum and asks you to type it. To confirm later, or from a script:

```sh
pve-rclone-backup recovery-kit confirm <checksum>
pve-rclone-backup recovery-kit export --kit-file /root/kit.txt --confirm-checksum <checksum>
```

See which repositories have a confirmed kit:

```sh
pve-rclone-backup recovery-kit status
```

## Check a kit

Check a stored kit now and then, and before you depend on it:

```sh
pve-rclone-backup recovery-kit show /root/kit.txt --passphrase-file /root/kit-passphrase
pve-rclone-backup recovery-kit verify /root/kit.txt --passphrase-file /root/kit-passphrase
```

- `show` decodes the kit and lists its repositories. It does not need the daemon, so it also works
  on a machine without one. With `--show-secrets`, it prints an rclone configuration for
  [manual recovery](../manual-recovery.md).
- `verify` checks the kit against the live repositories: it confirms that its keys open each
  repository.

## Use a kit

On a new host, `pve-rclone-backup recover <kit>` imports the keys and adds read-only storages for
the repositories in the kit. See the [disaster recovery runbook](../dr-runbook.md).

`recovery-kit import` imports only the keys (and, with `--import-token`, the credentials), without
adding storages.
