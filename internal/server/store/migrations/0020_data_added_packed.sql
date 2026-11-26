-- What a backup added to the repository as it is stored, compressed: restic's data_added_packed, from the snapshot's
-- summary, beside data_added, which is uncompressed. Snapshots already in the catalogue get it at its next refresh.
ALTER TABLE snapshots ADD COLUMN data_added_packed bigint;
