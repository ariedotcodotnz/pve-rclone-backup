-- Repositories this node has initialized, imported or opened. Storage
-- entries in storage.cfg may only be bound to a known repository, which
-- lets the storage configuration hooks validate without network access.
CREATE TABLE repositories (
    uuid TEXT PRIMARY KEY,
    remote TEXT NOT NULL,
    base_path TEXT NOT NULL,
    encryption TEXT NOT NULL CHECK (encryption IN ('crypt', 'none')),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (remote, base_path)
);
