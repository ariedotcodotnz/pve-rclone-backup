-- Guest identity of replication jobs, for superseding older queued uploads
-- and for rclone-min-interval decisions.
ALTER TABLE jobs ADD COLUMN vmtype TEXT;
ALTER TABLE jobs ADD COLUMN vmid INTEGER;
ALTER TABLE jobs ADD COLUMN backup_time INTEGER;
CREATE INDEX jobs_guest ON jobs (storeid, vmtype, vmid, backup_time);
