# Remotes

A remote is a cloud account the daemon can reach. Each [storage](storages.md) uses one remote, and
several storages can share a remote.

Remote names start with a lowercase letter and contain only lowercase letters, digits, `-` and
`_`, such as `onedrive-main`.

## Add a OneDrive remote

```sh
pve-rclone-backup remote add onedrive-main
```

Signing in uses a **relay**, so the Proxmox VE host needs no browser:

1. The command prints a Microsoft sign-in address. Open it in a browser on any device and sign in.
2. After you approve access, Microsoft sends the browser to `http://localhost:53682/?code=...`.
   Nothing listens there on your device, so the browser shows an error page. That is expected.
3. Copy the complete address from the address bar and paste it into the terminal. The daemon
   hands the code to rclone, which exchanges it for a token.
4. Answer rclone's questions about which drive to use. For a personal OneDrive, the suggested
   answers are right.

### Sign in with rclone instead

If you prefer, sign in on another machine with rclone (any recent version) and paste its output:

```sh
# on a machine with a browser
rclone authorize "onedrive"

# on the Proxmox VE node
pve-rclone-backup remote add onedrive-main --auth token
```

### Without a terminal

Every question can be answered on the command line, which is useful for automation. Pass the pasted
redirect address as `--answer redirect=<url>`, and other answers as `--answer <name>=<value>`. A
value of `@<file>` reads the answer from a file. The command tells you which answer is missing.

### Your own Microsoft app

By default, the remote uses rclone's shared Microsoft app, which many people use and Microsoft
throttles more often. With your own app registration, you have your own limits. Follow
[rclone's instructions](https://rclone.org/onedrive/#getting-your-own-client-id-and-key) to
register an app, then pass its ID:

```sh
pve-rclone-backup remote add onedrive-main --client-id 01234567-89ab-cdef-0123-456789abcdef
```

The command asks for the app's client secret. To provide it without a prompt, put it in a file and
pass `--client-secret-file <file>`. The secret is never given on the command line.

## Check a remote

```sh
pve-rclone-backup remote list                  # all remotes
pve-rclone-backup remote show onedrive-main    # authorization, token expiry, storages using it
pve-rclone-backup remote test onedrive-main    # write, read back and delete a small file
pve-rclone-backup remote about onedrive-main   # quota: total, used, free, recycle bin
```

`remote test` creates and removes a folder `.pve-rclone-backup-probe` at the root of the account.

## Reconnect a remote

A remote stops working when its credentials expire or are revoked. For example:

- a OneDrive refresh token expires after 90 days without use;
- you change your Microsoft password;
- you remove the app's access in your Microsoft account.

The daemon then marks the remote **auth_required** and pauses every upload to it. `status` and
`doctor` report the problem. Uploads stay queued, and local backups are not affected. Sign in again:

```sh
pve-rclone-backup remote reconnect onedrive-main
```

The paused uploads continue once the remote works again.

## Remove a remote

```sh
pve-rclone-backup remote remove onedrive-main
```

This removes the credentials from the node. It is refused while a storage uses the remote, and it
deletes nothing in the cloud.

## Where credentials are kept

Credentials are stored in `/etc/pve/priv/pve-rclone-backup/remotes.conf`, in rclone's
configuration format. Proxmox VE replicates `/etc/pve/priv` to every node of a cluster and lets only
root read it. Only the daemon reads and writes this file: it renews the tokens there, so do not edit
it while the daemon runs. Credentials never appear in `/etc/pve/storage.cfg` or in the logs.

A [recovery kit](recovery-kit.md) can include a copy of the credentials (`--include-token`).
