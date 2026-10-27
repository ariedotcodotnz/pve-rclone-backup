# Remotes

A remote is a cloud account the daemon can reach: a Microsoft OneDrive or a Google Drive. Each
[storage](storages.md) uses one remote, and several storages can share a remote.

Remote names start with a lowercase letter and contain only lowercase letters, digits, `-` and
`_`, such as `onedrive-main` or `gdrive`.

## Add a OneDrive remote

```sh
pve-rclone-backup remote add onedrive-main
```

Signing in uses a **relay**, so the Proxmox VE host needs no browser:

1. The command prints a Microsoft sign-in address. Open it in a browser on any device and sign in.
2. After you approve access, Microsoft sends the browser to `http://localhost:53682/?code=...`.
   Nothing listens there on your device, so the browser shows an error page. That is expected.
3. Copy the complete address from the address bar and paste it into the terminal. The daemon
   hands the code to rclone, which exchanges it for a token. An address from an earlier attempt
   does not work for this one: the command says so and asks again.
4. For the type of connection, choose `onedrive`.
5. For the drive, choose the one named **OneDrive**. Do not just press Enter: the suggested drive
   is often a hidden system drive, such as `ODCMetadataArchive`. If rclone cannot use an answer,
   it says why and asks again.
6. Confirm the drive it found.

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

## Add a Google Drive remote

```sh
pve-rclone-backup remote add gdrive --provider drive --client-id <id>.apps.googleusercontent.com
```

Use [your own Google app](#your-own-google-app): rclone's shared app is heavily rate-limited for
everyone who uses it. Signing in uses the same relay as OneDrive:

1. The command prints a Google sign-in address. Open it in a browser on any device, sign in and
   allow access to Google Drive. With your own app that is not verified by Google, Google warns
   about it first; choose **Advanced** and continue to your app.
2. Google sends the browser to `http://127.0.0.1:53682/?state=...&code=...`, which shows an error
   page. Copy the complete address from the address bar and paste it into the terminal.
3. To *Configure this as a Shared Drive*, answer `false` to store backups in your own My Drive.
   Answer `true` to choose a shared drive of a Google Workspace account instead.

To sign in on another machine instead, run `rclone authorize "drive" "<client-id>" "<client-secret>"`
there and add the remote with `--auth token`.

### Your own Google app

1. In the [Google Cloud console](https://console.cloud.google.com/), create a project and enable the
   **Google Drive API** for it.
2. Under **Google Auth Platform**, set up the consent screen (the audience is *External* for a
   personal Google account) and add yourself as a test user.
3. Create an OAuth client of type **Desktop app**. Note its client ID and client secret.
4. **Publish the app** (*Audience → Publish app*). While an app is in testing, Google lets its
   sign-ins expire after 7 days, and the remote would need reconnecting every week. A published app
   for your own use does not need Google's verification; Google only shows the warning in step 1.

[rclone's instructions](https://rclone.org/drive/#making-your-own-client-id) show these steps with
screenshots. Then add the remote with `--client-id`; the command asks for the client secret.

### What to know about Google Drive

- **Full access.** The remote asks for access to all of Google Drive. Do not narrow it with
  `--param scope=drive.file`: Google then shows the backups only to this app, and
  [manual recovery](../manual-recovery.md) with stock rclone could not see them.
- **750 GB a day.** Google accepts at most 750 GB of uploads per account and day. Beyond that,
  uploads are throttled and wait until Google accepts them again, up to a day later. The first
  upload of many large backups can take several days.
- **Shared quota.** The storage is shared with Gmail and Google Photos. `remote about` shows how
  much is used.
- **Trash.** Files deleted in Google Drive stay in the trash for 30 days and count towards the
  quota. The daemon deletes offsite backups permanently instead, unless you set
  [`rclone-recycle-bin`](../reference/storage-properties.md#rclone-recycle-bin). In a shared drive,
  only managers may delete permanently; for other members, deleted backups go to the trash, and the
  daemon logs a notice.
- **Files of the same name.** Google Drive allows several files with the same name in one folder.
  Do not copy files into the backup folder by hand.

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
- you remove the app's access in your Microsoft or Google account;
- a Google sign-in expires after 6 months without use, or after 7 days while your Google app is
  in testing.

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
