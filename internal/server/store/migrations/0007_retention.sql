-- M3: retention, Prune now and Delete snapshot.

-- M1's maintenance_runs recorded the server's own restic checks. Those moved to the agent (decision 030)
-- and nothing writes the table any more; it keeps its old rows under another name.
ALTER TABLE maintenance_runs RENAME TO m1_check_runs;
ALTER SEQUENCE maintenance_runs_id_seq RENAME TO m1_check_runs_id_seq;
ALTER INDEX maintenance_runs_pkey RENAME TO m1_check_runs_pkey;

-- One row per forget-and-prune on a host: the daily run, Prune now, or Delete now. It is also the list of
-- past prunes (OQ2). forgotten holds the snapshot IDs restic forgot; freed is what restic said
-- the prune freed, as it printed it.
CREATE TABLE maintenance_runs (
  id          bigserial   PRIMARY KEY,
  fqdn        text        NOT NULL REFERENCES hosts(fqdn),
  kind        text        NOT NULL CHECK (kind IN ('scheduled','run_now','prune_now','delete_now')),
  actor       text        NOT NULL,
  started_at  timestamptz NOT NULL,
  finished_at timestamptz,
  status      text        NOT NULL DEFAULT 'running' CHECK (status IN ('running','success','failed')),
  forgotten   text[]      NOT NULL DEFAULT '{}',
  pruned      boolean     NOT NULL DEFAULT false,
  freed       text        NOT NULL DEFAULT '',
  error       text        NOT NULL DEFAULT ''
);
CREATE INDEX maintenance_runs_host ON maintenance_runs (fqdn, started_at DESC);

-- A snapshot an admin asked to delete (decision 031). The daily run forgets it 7 days after the request;
-- cancelling removes the row.
CREATE TABLE pending_deletions (
  fqdn         text        NOT NULL REFERENCES hosts(fqdn),
  snapshot_id  text        NOT NULL,
  requested_by text        NOT NULL,
  requested_at timestamptz NOT NULL,
  message      text        NOT NULL DEFAULT '',
  PRIMARY KEY (fqdn, snapshot_id)
);

ALTER TABLE hosts
  -- Something was forgotten and not pruned yet.
  ADD COLUMN prune_needed boolean NOT NULL DEFAULT false,
  -- The bucket's lifecycle rule: how many days S3 keeps a deleted file's old version (B011). It dates
  -- when deleted data is really gone (decision 031).
  ADD COLUMN storage_noncurrent_days integer NOT NULL DEFAULT 0;
