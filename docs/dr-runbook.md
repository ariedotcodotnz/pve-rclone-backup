# Disaster recovery runbook

This runbook restores guests from offsite backups after the original Proxmox VE host or cluster is
lost. It needs:

- a fresh Proxmox VE 9 installation;
- the **recovery kit** of the lost installation, and its passphrase if it has one;
- access to the cloud account, either through credentials in the kit or by signing in again.

If you have no recovery kit, encrypted backups cannot be restored: the keys exist nowhere else.

## 1. Install

```sh
apt install ./pve-rclone-backup_<version>_amd64.deb
systemctl status pve-rclone-backupd
```

## 2. Import the recovery kit

Check the kit first. This works without the daemon:

```sh
pve-rclone-backup recovery-kit show /root/offsite-recovery-kit.txt --passphrase-file /root/kit-passphrase
```

Then import the keys. Add `--import-token` to also create the transport remote from credentials stored
in the kit:

```sh
pve-rclone-backup recovery-kit import /root/offsite-recovery-kit.txt \
    --passphrase-file /root/kit-passphrase --import-token
```

If the kit holds no credentials, or they have expired (OneDrive refresh tokens expire after 90 days of
non-use), connect the account again under **the same remote name** that the kit shows:

```sh
pve-rclone-backup remote add onedrive-main     # or: remote reconnect onedrive-main
```

Then confirm that the keys open the repository:

```sh
pve-rclone-backup recovery-kit verify /root/offsite-recovery-kit.txt --passphrase-file /root/kit-passphrase
```

## 3. Add the storage, read-only

Add a storage for the lost installation's source name, without `--replicate-from`, so nothing is
uploaded into it from here. `storage init` finds the existing repository and adopts it. Nothing is
overwritten.

```sh
pve-rclone-backup storage init offsite-dr --remote onedrive-main --path pve-backups --source homelab
pve-rclone-backup storage show offsite-dr
pve-rclone-backup backup list --storage offsite-dr
```

The catalogue is rebuilt from the backups' manifests in the cloud, so no database from the old host
is needed. Offsite backups also appear in the PVE GUI under the storage `offsite-dr`.

## 4. Restore

VMs stream straight into `qmrestore`; nothing is staged on local disk:

```sh
pve-rclone-backup restore offsite-dr:backup/vzdump-qemu-100-<time>.vma.zst --vmid 100 --target-storage local-lvm
```

Containers are staged and restored with `pct restore`. This also works for privileged containers:

```sh
pve-rclone-backup restore offsite-dr:backup/vzdump-lxc-200-<time>.tar.zst --vmid 200 --target-storage local-lvm
```

Alternatively, fetch an archive into a local backup storage and restore it from the PVE GUI:

```sh
pve-rclone-backup backup fetch offsite-dr:backup/vzdump-qemu-100-<time>.vma.zst --to-storage local
```

Every restore checks every segment and the whole archive against the digests recorded at upload.
If a stream restore finds a mismatch only after `qmrestore` has finished, the restore fails and the
VM it created is removed.

## 5. Resume offsite backups

Choose one:

- **Continue in the old namespace.** The new installation takes over the lost one's backups and
  retention. Add `--adopt-source` and replication sources:

  ```sh
  pve-rclone-backup storage init offsite --remote onedrive-main --source homelab --adopt-source \
      --replicate-from local
  ```

- **Start a new namespace.** Use a new `--source` name. The lost installation's backups stay
  readable through `offsite-dr` until you remove that storage.

Export and store a new recovery kit afterwards (`pve-rclone-backup recovery-kit export`).

## Without this tool

See [manual-recovery.md](manual-recovery.md): archives can be reassembled with stock rclone and
restored with `qmrestore` or `pct restore`.
