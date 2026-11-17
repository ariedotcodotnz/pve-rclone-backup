-- Backup directories without a manifest, as found by the last resync of
-- each storage: uploads in progress or abandoned, and deletions that
-- stopped half-way. What they hold takes quota but is never listed.
CREATE TABLE incomplete_uploads (
  storeid       TEXT NOT NULL REFERENCES storages ON DELETE CASCADE,
  remote_dir    TEXT NOT NULL, -- g<N>/v1/<source>/<type>/<vmid>/<timestamp>[.<n>]
  state         TEXT NOT NULL CHECK (state IN ('incomplete', 'deleting')),
  vmtype        TEXT NOT NULL,
  vmid          INTEGER NOT NULL,
  backup_time   INTEGER NOT NULL,
  files         INTEGER NOT NULL,
  stored_bytes  INTEGER NOT NULL,
  fingerprint   TEXT NOT NULL, -- names and sizes of the files
  first_seen_at INTEGER NOT NULL,
  changed_at    INTEGER NOT NULL, -- when the files were last seen to change
  PRIMARY KEY (storeid, remote_dir)
);
