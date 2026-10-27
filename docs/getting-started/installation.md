# Installation

pve-rclone-backup ships as one Debian package that contains:

- the daemon `pve-rclone-backupd`;
- the command line and terminal interface `pve-rclone-backup`;
- the Proxmox VE storage plugin.

Install it on every node that should replicate backups or list offsite backups.

## Get the package

### A release

Each version is published on the
[Releases page](https://github.com/ariedotcodotnz/pve-rclone-backup/releases), with a checksum
file. Download both on the node and check the package:

```sh
wget https://github.com/ariedotcodotnz/pve-rclone-backup/releases/download/<tag>/pve-rclone-backup_<version>_amd64.deb \
     https://github.com/ariedotcodotnz/pve-rclone-backup/releases/download/<tag>/SHA256SUMS
sha256sum -c SHA256SUMS
```

Each release lists these commands with the right names filled in. Release packages are built by
GitHub Actions from the tagged source. To confirm that a package came from there, use the
[GitHub CLI](https://cli.github.com/) on any machine:

```sh
gh attestation verify pve-rclone-backup_<version>_amd64.deb --repo ariedotcodotnz/pve-rclone-backup
```

### A development build

Every commit to `master` is built and tested by CI, which keeps the package for 90 days as the
`deb` artifact of the [CI run](https://github.com/ariedotcodotnz/pve-rclone-backup/actions/workflows/ci.yml).
Downloading artifacts needs a GitHub login, for example with the GitHub CLI:

```sh
run=$(gh run list --repo ariedotcodotnz/pve-rclone-backup --workflow ci.yml --branch master \
    --status success --limit 1 --json databaseId --jq '.[0].databaseId')
gh run download "$run" --repo ariedotcodotnz/pve-rclone-backup --name deb
```

`--status success` skips runs where any check failed, even a check that does not affect the
package, such as lint. To take the package of one commit regardless, look up its run by commit:

```sh
gh run list --repo ariedotcodotnz/pve-rclone-backup --workflow ci.yml --commit <commit> \
    --json databaseId,conclusion
gh run download <run> --repo ariedotcodotnz/pve-rclone-backup --name deb
```

The run's **package** job must have succeeded; otherwise the run has no `deb` artifact.

### Build it yourself

You need Go 1.26 or newer, `make` and `dpkg-deb`. Any Linux machine works; it does not have to be
the Proxmox VE host.

```sh
git clone https://github.com/ariedotcodotnz/pve-rclone-backup.git
cd pve-rclone-backup
make deb
```

The package is written to `dist/pve-rclone-backup_<version>_amd64.deb`.

To build on the Proxmox VE node itself, install Go from [go.dev](https://go.dev/dl/): the Go
packaged by Debian is too old.

```sh
apt install git make
wget https://go.dev/dl/go<version>.linux-amd64.tar.gz
rm -rf /usr/local/go && tar -C /usr/local -xzf go<version>.linux-amd64.tar.gz
export PATH=/usr/local/go/bin:$PATH
git clone https://github.com/ariedotcodotnz/pve-rclone-backup.git
cd pve-rclone-backup
make deb
apt install ./dist/pve-rclone-backup_*_amd64.deb
```

Later, `git pull` and `make deb` again to build a newer version.

Development builds are versioned `0.1.0~dev.<date>.g<commit>`. Two builds of the same commit on
the same day get the same version, and `apt install` skips a version that is already installed.
Use `apt install --reinstall` for such a build, or give your own build a higher version:

```sh
make deb DEB_VERSION=0.1.0~dev.$(date -u +%Y%m%d).g$(git rev-parse --short HEAD)+local1
```

A newer day's build still sorts above it.

## Install

Install the package on the Proxmox VE host itself, not in a container or VM; see
[Requirements](requirements.md#where-it-runs). Copy it to the node if you downloaded or built it
elsewhere, and install it with `apt`, which also installs any missing dependencies:

```sh
apt install ./pve-rclone-backup_<version>_amd64.deb
```

The installation:

- starts `pve-rclone-backupd` and enables it at boot;
- reloads the Proxmox VE API services, so that they pick up the new `rclone-backup` storage type.

Check that everything is in place:

```sh
systemctl status pve-rclone-backupd
pve-rclone-backup doctor
```

`doctor` reports a missing recovery kit or storage until you finish the
[quick start](quick-start.md). That is expected.

Shell completion for bash and zsh is installed with the package and works in a new shell.

## Upgrade

Install the newer package the same way. The daemon restarts. Uploads that were interrupted resume
from their last complete segment.

In a cluster, upgrade every node before you use a storage property that the new version adds:
nodes with the older plugin do not know it.

`dpkg -l pve-rclone-backup` shows the installed version; `pve-rclone-backup doctor` checks the
installation afterwards.

!!! note "Upgrading to a version with `rclone-recycle-bin`"
    Earlier versions sent deleted offsite backups to the OneDrive recycle bin, where they kept
    using quota for up to 30 days. Newer versions delete them permanently. Backups that are already
    in the recycle bin stay there: empty it once on the OneDrive website after upgrading. See
    [The cloud storage is full](../guide/troubleshooting.md#the-cloud-storage-is-full).

## Uninstall

!!! warning "Remove the storages first"
    Before you uninstall, remove the `rclone-backup` storages from **Datacenter → Storage** (or
    with `pvesm remove <storage>`). Without the plugin, Proxmox VE cannot read these storage
    entries, and a later change to the storage configuration can drop them.

```sh
apt remove pve-rclone-backup    # keeps the local state in /var/lib/pve-rclone-backup
apt purge pve-rclone-backup     # also removes the local state
```

Neither removes anything in the cloud. Settings, keys and credentials under `/etc/pve` are also
kept, because they are shared by every node of a cluster. To remove them as well, once no node
uses them:

```sh
rm -r /etc/pve/pve-rclone-backup /etc/pve/priv/pve-rclone-backup
```

!!! danger
    `/etc/pve/priv/pve-rclone-backup` holds the encryption keys. Delete it only if you have a
    recovery kit, or no longer need the offsite backups.
