# Monitoring

## At a glance

```sh
pve-rclone-backup status
```

`status` shows:

- the daemon's version, node and uptime;
- queue counts;
- problems with the configuration files;
- open alerts;
- each storage with its health, usage and replication sources.

## Health checks

```sh
pve-rclone-backup doctor
```

`doctor` checks the installation and lists each check as `OK`, `WARN` or `FAIL`:

- the Proxmox VE plugin is installed;
- the daemon runs, and its API matches the command line tool;
- configuration problems and open alerts;
- failed uploads;
- each storage is healthy and has a confirmed recovery kit;
- each remote is authorized.

It exits with status **5** when something needs attention, so it fits monitoring systems and
cron. For example, to get an email from cron when something is wrong:

```sh
# /etc/cron.d/pve-rclone-backup-doctor
MAILTO=root
0 8 * * * root pve-rclone-backup doctor >/dev/null || pve-rclone-backup doctor
```

## Alerts

The daemon raises alerts for problems that need you, and clears them when the problem is gone.

| Alert | Raised when | What to do |
|---|---|---|
| `auth:<remote>` | The remote rejected its credentials. Uploads to it are paused. | `pve-rclone-backup remote reconnect <remote>` |
| `quota:<remote>` | The remote is full. Uploads to it are paused. | Free space, or empty the OneDrive recycle bin. |
| `stalled:<job>` | An upload has been failing for more than a day. | `pve-rclone-backup queue show <job>` shows the error. |
| `damaged:<storage>` | Verification found damaged backups. | See [damaged backups](verification.md#damaged-backups). |
| `damaged:<storage>:<backup>` | A content verification of a backup failed. | As above. |
| `state-db-rebuilt` | The local state database was unusable and was recreated. Offsite backups are not affected; job history is lost. | Nothing; the alert is informational. |

`status`, `doctor` and the terminal interface show open alerts.

## Machine-readable output

Every command accepts `-o json`, for scripts and monitoring:

```sh
pve-rclone-backup status -o json
pve-rclone-backup backup list -o json
pve-rclone-backup doctor -o json
```

## Logs

The daemon logs to the systemd journal:

```sh
journalctl -u pve-rclone-backupd           # everything
journalctl -u pve-rclone-backupd -f        # follow
journalctl -u pve-rclone-backupd -p warning
```

For more detail, set [`log-level`](../reference/daemon-settings.md#log-level) to `debug` in
`/etc/pve/pve-rclone-backup/daemon.cfg` and restart the daemon. Logs never contain credentials or
encryption keys.

Each job keeps its own history of state changes and errors:

```sh
pve-rclone-backup queue show 42
```
