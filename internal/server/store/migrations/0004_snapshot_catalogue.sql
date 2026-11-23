-- M2: the snapshot catalogue. It is a cache of each repository's `restic snapshots`, plus
-- tombstones: a row whose snapshot is no longer in the repository keeps its gone_at. Everything except the
-- tombstones can be rebuilt from the repositories.
CREATE TABLE snapshots (
  fqdn                text        NOT NULL REFERENCES hosts(fqdn),
  id                  text        NOT NULL,
  short_id            text        NOT NULL,
  time                timestamptz NOT NULL,
  hostname            text        NOT NULL,
  job_id              text        NOT NULL DEFAULT '', -- from the tag job=<id>; '' without one
  tags                text[]      NOT NULL DEFAULT '{}',
  paths               text[]      NOT NULL DEFAULT '{}',
  parent              text        NOT NULL DEFAULT '',
  total_files         bigint,
  total_bytes         bigint,
  data_added          bigint,
  first_seen          timestamptz NOT NULL,
  gone_at             timestamptz,                     -- a tombstone: no longer in the repository
  deleted_by_holdfast boolean     NOT NULL DEFAULT false, -- set by Holdfast's own deletions, from M3
  acknowledged_at     timestamptz,                     -- an admin has seen that it disappeared
  PRIMARY KEY (fqdn, id)
);
CREATE INDEX snapshots_short_id ON snapshots (short_id);

-- "Made by Holdfast" means an agent reported the snapshot's ID in an event (decision 032).
CREATE INDEX events_snapshot ON events (fqdn, snapshot_id) WHERE snapshot_id <> '';

ALTER TABLE hosts
  ADD COLUMN catalogue_refreshed_at timestamptz,
  ADD COLUMN catalogue_error        text NOT NULL DEFAULT '';
