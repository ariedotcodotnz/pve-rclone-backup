-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Operational state and catalogue cache of pve-rclone-backupd.
-- Remote manifests are authoritative for what exists offsite; everything in
-- the catalogue tables can be rebuilt from them.

CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

-- Cache of rclone-backup storage.cfg entries and their repository binding.
CREATE TABLE storages (
  storeid        TEXT PRIMARY KEY,
  remote         TEXT NOT NULL,
  base_path      TEXT NOT NULL,
  source         TEXT NOT NULL,
  repo_uuid      TEXT,
  source_uuid    TEXT,
  generation     INTEGER,
  config_hash    TEXT NOT NULL,
  last_resync_at INTEGER,
  about_json     TEXT,
  health         TEXT,
  health_detail  TEXT,
  updated_at     INTEGER NOT NULL
);

-- Catalogue: cache of remote manifests and meta documents.
CREATE TABLE backups (
  id              INTEGER PRIMARY KEY,
  storeid         TEXT NOT NULL REFERENCES storages ON DELETE CASCADE,
  volname         TEXT NOT NULL,
  vmtype          TEXT NOT NULL CHECK (vmtype IN ('qemu', 'lxc')),
  vmid            INTEGER NOT NULL,
  backup_time     INTEGER NOT NULL,
  ts_label        TEXT NOT NULL,
  collision_index INTEGER NOT NULL DEFAULT 0,
  remote_dir      TEXT NOT NULL,
  generation      INTEGER NOT NULL,
  state           TEXT NOT NULL CHECK (state IN ('complete', 'tombstoned', 'deleting', 'damaged')),
  archive_size    INTEGER NOT NULL,
  archive_sha256  TEXT NOT NULL,
  archive_format  TEXT NOT NULL,
  compression     TEXT,
  segment_size    INTEGER NOT NULL,
  segment_count   INTEGER NOT NULL,
  guest_name      TEXT,
  notes           TEXT,
  protected       INTEGER NOT NULL DEFAULT 0,
  meta_dirty      INTEGER NOT NULL DEFAULT 0,
  delete_after    INTEGER,
  uploaded_at     INTEGER NOT NULL,
  manifest_json   TEXT NOT NULL,
  verify_level    INTEGER NOT NULL DEFAULT 0,
  verified_at     INTEGER,
  verify_result   TEXT,
  UNIQUE (storeid, volname),
  UNIQUE (storeid, remote_dir)
);
CREATE INDEX backups_guest ON backups (storeid, vmtype, vmid, backup_time);

-- Work queue: replication, verification, deletion, fetch/restore, resync.
CREATE TABLE jobs (
  id              INTEGER PRIMARY KEY,
  kind            TEXT NOT NULL CHECK (kind IN ('replicate', 'verify', 'delete', 'fetch', 'restore', 'resync', 'meta-sync')),
  storeid         TEXT NOT NULL,
  state           TEXT NOT NULL,
  priority        INTEGER NOT NULL DEFAULT 0,
  dedupe_key      TEXT NOT NULL UNIQUE,
  backup_volname  TEXT,
  source_storage  TEXT,
  source_path     TEXT,
  source_dev      INTEGER,
  source_ino      INTEGER,
  source_size     INTEGER,
  source_mtime_ns INTEGER,
  attempts        INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER,
  error_class     TEXT,
  last_error      TEXT,
  progress_bytes  INTEGER NOT NULL DEFAULT 0,
  total_bytes     INTEGER,
  next_segment    INTEGER NOT NULL DEFAULT 0,
  hash_state      BLOB,
  owner_node      TEXT NOT NULL,
  lease_until     INTEGER,
  params_json     TEXT NOT NULL DEFAULT '{}',
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  started_at      INTEGER,
  finished_at     INTEGER
);
CREATE INDEX jobs_runnable ON jobs (state, next_attempt_at, priority);

CREATE TABLE job_segments (
  job_id           INTEGER NOT NULL REFERENCES jobs ON DELETE CASCADE,
  idx              INTEGER NOT NULL,
  start_offset     INTEGER NOT NULL,
  size             INTEGER NOT NULL,
  sha256           TEXT,
  stored_size      INTEGER,
  stored_hash_type TEXT,
  stored_hash      TEXT,
  state            TEXT NOT NULL CHECK (state IN ('pending', 'uploaded', 'verified')),
  PRIMARY KEY (job_id, idx)
);

CREATE TABLE job_events (
  id         INTEGER PRIMARY KEY,
  job_id     INTEGER NOT NULL REFERENCES jobs ON DELETE CASCADE,
  ts         INTEGER NOT NULL,
  level      TEXT NOT NULL,
  from_state TEXT,
  to_state   TEXT,
  message    TEXT NOT NULL
);
CREATE INDEX job_events_job ON job_events (job_id, id);

CREATE TABLE verifications (
  id           INTEGER PRIMARY KEY,
  storeid      TEXT NOT NULL,
  volname      TEXT NOT NULL,
  level        INTEGER NOT NULL,
  started_at   INTEGER NOT NULL,
  finished_at  INTEGER,
  result       TEXT,
  details_json TEXT
);
CREATE INDEX verifications_backup ON verifications (storeid, volname, id);

CREATE TABLE idempotency (
  key           TEXT PRIMARY KEY,
  response_json TEXT NOT NULL,
  created_at    INTEGER NOT NULL
);

-- Archives placed into local storages by fetch; never replicated again.
CREATE TABLE fetched_archives (
  path       TEXT PRIMARY KEY,
  dev        INTEGER,
  ino        INTEGER,
  storeid    TEXT,
  volname    TEXT,
  sha256     TEXT,
  created_at INTEGER NOT NULL
);

CREATE TABLE alerts (
  id         TEXT PRIMARY KEY,
  severity   TEXT NOT NULL,
  storeid    TEXT,
  message    TEXT NOT NULL,
  raised_at  INTEGER NOT NULL,
  cleared_at INTEGER
);
