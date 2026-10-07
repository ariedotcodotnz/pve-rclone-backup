# Security model

This page explains what pve-rclone-backup protects against, and what it does not.

## What the cloud provider sees

With encryption (the default), the provider stores:

- the contents of your backups, encrypted;
- file and folder names, encrypted;
- the repository marker `pve-rclone-backup.repo.json`, which holds the format version and the
  encryption settings, but no keys.

The provider can still see how many backups you store, their approximate sizes, and when they were
uploaded.

Encryption uses [rclone crypt](https://rclone.org/crypt/): XSalsa20-Poly1305 in 64 KiB blocks, with
random 256-bit keys for each repository. Each block is authenticated. Each backup's manifest also
records the size and SHA-256 of every segment and of the whole archive, which are checked on every
restore. Altered, truncated or missing data is therefore detected.

One change is not detected: someone with access to the cloud account can put back an older copy of
a backup's notes, protection and deletion mark, which are stored next to it in `meta.json`.

## Threats

| Threat | What happens | Protection |
|---|---|---|
| Someone gets your cloud account, but not your keys | They cannot read your backups. They can delete them, or alter them. | Alterations are detected; see above. On OneDrive, deleted files stay in the recycle bin for 30 days, and Microsoft 365 subscribers can restore the whole OneDrive to an earlier point within the last 30 days. Keep local backups too. |
| Someone gets root on a Proxmox VE node | They can read the keys and the cloud credentials, and so read or delete offsite backups. | This holds for any backup that a host pushes to the cloud. The protections against a lost account still apply. The recovery kit keeps your keys if the host's copy is destroyed. |
| Ransomware on the host | Local backups may be encrypted; offsite backups may be deleted if the attacker has root. | Archives are uploaded unchanged and never changed afterwards. Deletions wait for a grace period and are capped per run; `rclone-immutable` refuses deletions made through pve-rclone-backup. A root attacker can turn these off, so the cloud provider's recycle bin and restore features are the last line. |
| You delete the wrong backup | The backup would be lost. | Deletions wait for a grace period and can be undone; protected backups cannot be deleted; deleting local backups never deletes offsite ones. |
| The OAuth token is stolen, for example from a recovery kit | Access to the cloud account, but not to your backups' contents. | Revoke the app's access in your Microsoft account, then run `remote reconnect`. Protect kits that hold credentials with a passphrase. |
| The recovery kit is stolen | Without a passphrase, the thief has your keys. | Protect the kit with a passphrase (scrypt and XChaCha20-Poly1305), and keep the passphrase separately. |
| A guest is malicious | It controls its own disk contents and configuration. | Only Proxmox VE's own tools read archive contents. Guest configurations are stored as text and shown without control characters. Paths are built from VMIDs and times only. |

## On the node

- The daemon runs as root, because it reads backup storages and runs `qmrestore` and `pct`. Its
  systemd unit adds restrictions that do not break those tools.
- The daemon opens **no network ports**. The command line tool and the storage plugin reach it
  through `/run/pve-rclone-backup/api.sock`, which only root can open; the daemon also checks that
  each caller is root.
- While you sign in to a remote, rclone listens on `127.0.0.1:53682` on the node, for the
  sign-in to complete.
- Credentials and keys live under `/etc/pve/priv`, which only root can read. They never appear in
  `/etc/pve/storage.cfg`, in the Proxmox VE API, or in logs: the daemon removes tokens, passwords and
  keys from everything it logs, including rclone's own messages.
- Commands are run with explicit arguments, never through a shell.

## Reporting a vulnerability

Please report security problems privately, through the
[repository's Security tab](https://github.com/ariedotcodotnz/pve-rclone-backup/security), rather
than in a public issue.
