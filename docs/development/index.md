# Development

## Layout

| Path | Contents |
|---|---|
| `cmd/pve-rclone-backupd`, `cmd/pve-rclone-backup` | The daemon and the command line tool. |
| `cmd/schemagen` | Generates configuration code and reference pages from `schema/*.yaml`. |
| `cmd/docgen` | Generates the command line reference and the manual pages. |
| `internal/` | The Go packages: API, catalogue, discovery, jobs, replication, restore, retention, transport (embedded rclone) and more. |
| `perl/` | The Proxmox VE storage plugin and backup provider. |
| `schema/` | The configuration schemas, the single source of truth for every setting. |
| `packaging/`, `systemd/` | The Debian package and the systemd unit. |
| `test/perl`, `test/deb`, `test/e2e` | Plugin, package and end-to-end tests. |
| `docs/` | This documentation. |

Architecture decisions are recorded in [ADRs](../adr/0001-architecture.md). The spikes record
facts about Proxmox VE that the design depends on, as verified on a real node.

## Build and test

You need Go 1.26 or newer. The Perl, package and end-to-end tests also need Docker.

```sh
make build         # bin/pve-rclone-backupd, bin/pve-rclone-backup
make test          # unit tests
make race          # unit tests with the race detector
make integration   # the embedded rclone engine against local backends; no network
make perl-test     # the plugin against Proxmox VE's real storage library, in a container
make lint          # go vet, gofmt, golangci-lint, and generated files up to date
make deb           # dist/pve-rclone-backup_<version>_amd64.deb
make deb-test      # install, use and purge the package in a container
make e2e           # end-to-end tests on a nested Proxmox VE node
```

### End-to-end tests

`make e2e` installs Proxmox VE 9.2 unattended from the official ISO into a nested VM. It then
installs the package and runs backups, replication, the GUI's API calls, restores and failure
scenarios against it (`test/e2e`). QEMU runs in a container with `/dev/kvm` passed through, so the
host needs only Docker and hardware virtualisation.

The first run builds a cached base image in `~/.cache/pve-rclone-backup/e2e`, which takes about
15 minutes. Later runs boot a throwaway copy of it. `test/e2e/vm.sh` starts, stops and opens a
shell on the VM; `E2E_KEEP=1 make e2e` leaves it running afterwards.

## Generated files

Several files are generated, and CI fails if they are out of date:

| Source | Generated | Command |
|---|---|---|
| `schema/*.yaml` | Go configuration types, JSON schemas, the plugin's `Schema.pm`, the [configuration reference](../reference/storage-properties.md) | `make generate` |
| The command tree in `internal/cli` | The [command line reference](../reference/cli/index.md) | `make docs` |

`make docs` runs both generators.

## Documentation

The site is built with [MkDocs](https://www.mkdocs.org/) and
[Material for MkDocs](https://squidfunk.github.io/mkdocs-material/). It is published to GitHub
Pages from the `master` branch by `.github/workflows/docs.yml`. To preview it locally:

```sh
python3 -m venv .venv
.venv/bin/pip install -r docs/requirements.txt
make docs
.venv/bin/mkdocs serve      # http://127.0.0.1:8000
```

`mkdocs build --strict` fails on broken links, which CI checks on every pull request.

## Releases

A version tag publishes a release:

```sh
git tag -a v0.1.0 -m "pve-rclone-backup 0.1.0"
git push origin v0.1.0
```

`.github/workflows/release.yml` then:

1. runs the whole CI suite on the tagged commit, which builds and tests the package with the tag's
   version;
2. publishes the package on GitHub Releases with a `SHA256SUMS` file and a build provenance
   attestation.

Tags look like `v1.2.3`. A tag with a suffix such as `v0.1.0-alpha.1` makes a pre-release,
whose Debian version is `0.1.0~alpha.1`; Debian sorts that before `0.1.0`. Other tags fail the
workflow before anything is built.

## License

AGPL-3.0-or-later. Each source file carries an SPDX header.
