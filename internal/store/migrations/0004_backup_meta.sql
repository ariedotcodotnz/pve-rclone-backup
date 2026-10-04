-- Local changes to notes, protection and tombstones are applied to the
-- catalogue at once and pushed to the remote meta document in the
-- background; meta_rev tells a push whether the row changed meanwhile.
ALTER TABLE backups ADD COLUMN meta_rev INTEGER NOT NULL DEFAULT 0;
ALTER TABLE backups ADD COLUMN tombstone_at INTEGER;
ALTER TABLE backups ADD COLUMN tombstone_reason TEXT;
ALTER TABLE backups ADD COLUMN tombstone_by TEXT;
CREATE INDEX backups_dirty ON backups (meta_dirty) WHERE meta_dirty = 1;
