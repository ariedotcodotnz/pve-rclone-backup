# ADR 0001 — Overall architecture

- Status: accepted
- Date: 2026-10-04

## Context

We want offsite copies of Proxmox VE (PVE 9.x) backups in rclone-supported cloud
storage, starting with Microsoft OneDrive Personal. Requirements:

- a cloud outage must never fail or block a local backup;
- no second full local copy of an archive;
- client-side encryption;
- restores must work after total loss of the PVE host;
- native PVE integration, with no patches to PVE files.

Upstream facts that shaped the decision (all verified against current source):

- **Backup Provider API (`PVE::BackupProvider::Plugin::Base`, since PVE 8.4).**
  - VM backups through it require fleecing, and keep a QEMU snapshot-access open until the provider has finished reading.
  - Restores need a `stat()`-able `qemu-img-path`.
  - Privileged containers are refused for provider restores.
- **vzdump** renames `*.dat` to the final archive name only on success, then prunes the local storage and runs `backup-end`/`log-end`.
- **`qmrestore -`** streams an uncompressed VMA from stdin straight into the target volumes.
- **Custom storage plugin property names share one namespace.** A duplicate makes `PVE::Storage::Plugin->init()` die, outside the loader's `eval`.
- **No schema-driven GUI for custom storage plugins yet.** The upstream series (v3, 2026-07) is not merged.
- **OneDrive limits:** 250 GiB maximum object size; paths under 400 characters; no streaming (unknown-size) uploads; deletes go to the recycle bin.
- **rclone:**
  - Debian trixie ships rclone 1.60.1, which is too old.
  - The RC API is documented as equivalent to shell access.
  - rclone's crypt layer verifies the ciphertext hash during upload.

## Decision

1. **Replication-first.**
   - vzdump keeps writing to local storage unchanged.
   - `pve-rclone-backupd` discovers finished archives (inotify plus reconcile scans, with an optional vzdump hook) and uploads them bit-identical and asynchronously.
   - Uploads are fixed-size, independently encrypted segments that resume per segment.
2. **Provider-integrated catalogue.**
   - A storage type `rclone-backup` (Perl: `PVE::Storage::Custom::RcloneBackupPlugin` plus `PVE::BackupProvider::Plugin::Rclone`) declares `backup-provider`.
   - It lists offsite backups, reports quota, and supports notes/protected, tombstoned deletes, offsite prune, and config extraction from the manifest.
   - Direct backups to this storage are refused with guidance.
   - GUI restores via the provider API come later.
3. **Restores in the MVP** are driven by the CLI and TUI:
   - fetch into a local backup storage (the result is a native PVE backup);
   - streaming VM restore via `qmrestore -`;
   - staged CT restore via `pct restore`.
4. **rclone is embedded as a pinned Go library** (user decision): no RC listener, byte-range segment uploads with a known size, in-process crypt hash checks, and one process owning OAuth tokens.
5. **The remote manifests are the authority** on what exists offsite. SQLite holds operational state and a cache that can be rebuilt.
6. **Configuration is schema-first.** Go types, the Perl `properties()`, and JSON Schema are all generated from one source.
7. **Encryption with rclone crypt is on by default**, with random keys. A recovery kit must be exported before replication is enabled.
8. **Scope and project decisions** (user decisions):
   - the MVP is single-node first but cluster-safe;
   - PVE integration is tested on an automated nested PVE VM;
   - the license is AGPL-3.0-or-later.

## Consequences

- We own rclone version upgrades (pin, CI, a Renovate policy).
- Native GUI configuration of the storage waits for the upstream schema-driven GUI. Until then we ship `title`, `advanced-properties` and `hidden-properties`, and configure via the CLI, the TUI and `pvesm`/`pvesh`.
- Every property our plugin defines carries the `rclone-` prefix, and CI checks for collisions.
- Direct-to-cloud provider backups are deferred to a separate design (M24).

See the full plan, kept outside this repository, for detailed data models, the API, workflows and milestones. Later ADRs refine individual decisions.
