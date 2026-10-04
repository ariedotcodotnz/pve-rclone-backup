# Manual recovery with stock rclone

Offsite backups can be restored without pve-rclone-backup. You need:

- rclone 1.62 or newer;
- the recovery kit;
- access to the cloud account.

## 1. Get the rclone configuration

On any machine with pve-rclone-backup installed, print the kit's contents as an rclone configuration
(this does not need the daemon):

```sh
pve-rclone-backup recovery-kit show kit.txt --show-secrets [--passphrase-file pass.txt]
```

Without the tool, the kit's header lists every repository's location and crypt settings. Its
armored block holds the keys. If the kit was not passphrase-protected, that block is two format
bytes followed by the kit as JSON:

```sh
sed -n '/^-----BEGIN/,/^-----END/p' kit.txt | sed '1d;$d' | base64 -d | tail -c +3
```

The crypt passwords in the JSON are plain text. Write them into `rclone.conf` with
`rclone obscure <password>`.

Add the printed sections to `~/.config/rclone/rclone.conf`. If the kit holds no credentials, create
the transport remote with `rclone config` under the name the kit shows. Each repository has one
crypt remote per key generation, named `<remote>-g<N>`:

```ini
[onedrive-main-g1]
type = crypt
remote = onedrive-main:pve-backups/g1
filename_encryption = standard
directory_name_encryption = true
filename_encoding = base32768
suffix = .bin
password = <obscured>
password2 = <obscured>
```

## 2. Find the backup

```sh
rclone lsd onedrive-main-g1:v1/                       # sources (installations)
rclone lsd onedrive-main-g1:v1/homelab/qemu/100/      # backups of VM 100, by time
rclone cat onedrive-main-g1:v1/homelab/qemu/100/2026_10_04-02_00_01/manifest.json
```

The manifest lists the archive's original file name, size, SHA-256 and the segments.

## 3. Reassemble and check the archive

Segments are numbered `part.000000`, `part.000001`, …; concatenated in order they are the original
vzdump archive, byte for byte:

```sh
dir=onedrive-main-g1:v1/homelab/qemu/100/2026_10_04-02_00_01
for p in $(rclone lsf "$dir" --include 'part.*' | sort); do rclone cat "$dir/$p"; done \
    > vzdump-qemu-100-2026_10_04-02_00_01.vma.zst
sha256sum vzdump-qemu-100-2026_10_04-02_00_01.vma.zst     # compare with archive.sha256 in the manifest
```

## 4. Restore

```sh
qmrestore vzdump-qemu-100-2026_10_04-02_00_01.vma.zst 100 --storage local-lvm
pct restore 200 vzdump-lxc-200-2026_10_04-02_00_01.tar.zst --storage local-lvm
```

## Repository layout

```
<remote>:<path>/
├── pve-rclone-backup.repo.json      plaintext marker (no secrets)
└── g1/                              crypt root of key generation 1
    ├── keycheck
    └── v1/<source>/
        ├── source.json
        └── <qemu|lxc>/<vmid>/<YYYY_MM_DD-HH_MM_SS>[.<n>]/
            ├── part.000000 …        the archive in fixed-size segments
            ├── vzdump.log
            ├── meta.json            notes, protection, deletion state
            └── manifest.json        written last: a backup without one is incomplete
```

A directory whose `meta.json` shows `"state": "deleting"` was being deleted and may be incomplete.
