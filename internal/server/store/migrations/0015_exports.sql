-- M9 (decision 040, plan-m9): the nightly export of the server's tables to meta/<date>.age in the bucket, and with
-- off-site copies (decision 041) to the off-site bucket too. One row per attempt: when, by whom, what was written,
-- and how each write went. A failed off-site write leaves the primary export counted (plan-m9 Q4).
CREATE TABLE exports (
  id             bigserial   PRIMARY KEY,
  started_at     timestamptz NOT NULL,
  finished_at    timestamptz NOT NULL,
  actor          text        NOT NULL,
  name           text        NOT NULL,           -- meta/<name>, the UTC date
  bytes          bigint      NOT NULL DEFAULT 0,
  error          text        NOT NULL DEFAULT '', -- the primary write; '' when it was written
  offsite        boolean     NOT NULL DEFAULT false, -- whether it was to be written off-site too
  offsite_error  text        NOT NULL DEFAULT ''
);
CREATE INDEX exports_started ON exports (started_at DESC);

-- A server restored from an export (plan-m9 Q6): the head revision it was restored at. An agent that reports a newer
-- revision ran a change the export does not have.
CREATE TABLE restores_from_export (
  id          bigserial   PRIMARY KEY,
  restored_at timestamptz NOT NULL,
  export_name text        NOT NULL,
  exported_at timestamptz NOT NULL,
  head        bigint      NOT NULL
);
