-- Holdfast M1 schema (plan-m1 §6). Append-only tables refuse UPDATE and DELETE at the database level.

CREATE FUNCTION holdfast_append_only() RETURNS trigger LANGUAGE plpgsql AS
$$ BEGIN RAISE EXCEPTION '% is append-only', TG_TABLE_NAME; END $$;

-- Config revisions: linear and immutable; never credentials. A save inserts number = base + 1, so a
-- concurrent save from the same base fails on the primary key and is refused (plan-m1 Q8).
-- `config` is json, not jsonb, so the exact canonical bytes (and their sha256) are kept.
CREATE TABLE revisions (
  number         bigint      PRIMARY KEY CHECK (number > 0),
  created_at     timestamptz NOT NULL DEFAULT now(),
  actor          text        NOT NULL,
  source         text        NOT NULL CHECK (source IN ('cli','api','ui','system')),
  message        text        NOT NULL CHECK (message <> ''),
  accepted_codes text[]      NOT NULL DEFAULT '{}',
  config         json        NOT NULL,
  config_sha256  bytea       NOT NULL
);
CREATE TRIGGER revisions_append_only BEFORE UPDATE OR DELETE ON revisions
  FOR EACH ROW EXECUTE FUNCTION holdfast_append_only();

-- The revision in which each host's agent-facing config last changed. sha256 NULL: the host left the
-- config in that revision.
CREATE TABLE host_desired (
  fqdn      text   NOT NULL,
  revision  bigint NOT NULL REFERENCES revisions(number),
  sha256    text,
  PRIMARY KEY (fqdn, revision)
);

-- Lifecycle state; the config itself lives in revisions.
CREATE TABLE hosts (
  fqdn            text        PRIMARY KEY CHECK (fqdn = lower(fqdn) AND fqdn !~ '\.$'),
  state           text        NOT NULL CHECK (state IN ('awaiting_enrollment','enrolled','retired')),
  added_revision  bigint      NOT NULL REFERENCES revisions(number),
  added_at        timestamptz NOT NULL,
  connected_at    timestamptz,
  first_backup_at timestamptz,
  retired_at      timestamptz
);

CREATE TABLE enrollment_codes (
  id          uuid        PRIMARY KEY,
  fqdn        text        NOT NULL REFERENCES hosts(fqdn),
  code_sha256 text        NOT NULL UNIQUE,
  created_at  timestamptz NOT NULL,
  expires_at  timestamptz NOT NULL,
  used_at     timestamptz,
  revoked_at  timestamptz,
  created_by  text        NOT NULL
);
CREATE UNIQUE INDEX enrollment_codes_one_open ON enrollment_codes (fqdn) WHERE used_at IS NULL AND revoked_at IS NULL;

CREATE TABLE agents (
  fqdn                 text        PRIMARY KEY REFERENCES hosts(fqdn),
  credential_kind      text        NOT NULL CHECK (credential_kind IN ('token','certificate')),
  credential_sha256    text        NOT NULL UNIQUE,
  enrolled_at          timestamptz NOT NULL,
  last_seen_at         timestamptz,
  agent_version        text NOT NULL DEFAULT '',
  restic_version       text NOT NULL DEFAULT '',
  os_id                text NOT NULL DEFAULT '',
  os_version           text NOT NULL DEFAULT '',
  hostname_fqdn        text NOT NULL DEFAULT '',
  timezone             text NOT NULL DEFAULT '',
  clock_offset_ms      bigint NOT NULL DEFAULT 0,
  applied_revision     bigint NOT NULL DEFAULT 0,
  applied_sha256       text   NOT NULL DEFAULT '',
  applied_at           timestamptz,
  credentials_version  integer NOT NULL DEFAULT 0
);

CREATE TABLE escrow_exports (
  id            bigserial   PRIMARY KEY,
  created_at    timestamptz NOT NULL DEFAULT now(),
  actor         text        NOT NULL,
  host_count    integer     NOT NULL,
  bundle_sha256 text        NOT NULL
);

-- Never part of a revision; never returned by the API. Secrets are ciphertext (internal/server/creds).
CREATE TABLE host_credentials (
  fqdn             text        PRIMARY KEY REFERENCES hosts(fqdn),
  version          integer     NOT NULL CHECK (version > 0),
  repository       text        NOT NULL,
  repo_password    bytea       NOT NULL,
  s3_access_key_id text        NOT NULL,
  s3_secret        bytea       NOT NULL,
  created_at       timestamptz NOT NULL,
  escrow_export_id bigint      REFERENCES escrow_exports(id)
);

-- Final results of runs: delivered at least once, stored exactly once.
CREATE TABLE events (
  fqdn           text        NOT NULL REFERENCES hosts(fqdn),
  id             text        NOT NULL,
  kind           text        NOT NULL,
  job_id         text        NOT NULL DEFAULT '',
  command_id     text        NOT NULL DEFAULT '',
  status         text        NOT NULL CHECK (status IN ('success','warning','failed','interrupted','skipped')),
  scheduled_for  timestamptz,
  spread_delay_s integer     NOT NULL DEFAULT 0,
  started_at     timestamptz NOT NULL,
  finished_at    timestamptz NOT NULL,
  revision       bigint      NOT NULL DEFAULT 0,
  snapshot_id    text        NOT NULL DEFAULT '',
  files_new      bigint      NOT NULL DEFAULT 0,
  files_changed  bigint      NOT NULL DEFAULT 0,
  bytes_added    bigint      NOT NULL DEFAULT 0,
  total_files    bigint      NOT NULL DEFAULT 0,
  total_bytes    bigint      NOT NULL DEFAULT 0,
  restic_exit    integer     NOT NULL DEFAULT 0,
  error          text        NOT NULL DEFAULT '',
  received_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (fqdn, id)
);
CREATE INDEX events_job ON events (fqdn, job_id, finished_at DESC);

-- Kept in the same transaction as each new event.
CREATE TABLE job_status (
  fqdn             text NOT NULL,
  job_id           text NOT NULL,
  last_run_at      timestamptz,
  last_status      text NOT NULL DEFAULT '',
  last_error       text NOT NULL DEFAULT '',
  last_success_at  timestamptz,
  last_snapshot_id text NOT NULL DEFAULT '',
  PRIMARY KEY (fqdn, job_id)
);
CREATE TABLE run_counters (
  fqdn   text   NOT NULL,
  kind   text   NOT NULL,
  job_id text   NOT NULL DEFAULT '',
  status text   NOT NULL,
  count  bigint NOT NULL,
  PRIMARY KEY (fqdn, kind, job_id, status)
);

-- Work queued for an agent; re-sent until acknowledged.
CREATE TABLE commands (
  id         text        PRIMARY KEY,
  fqdn       text        NOT NULL REFERENCES hosts(fqdn),
  kind       text        NOT NULL CHECK (kind IN ('backup_now')),
  job_id     text        NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  created_by text        NOT NULL,
  acked_at   timestamptz
);
CREATE INDEX commands_pending ON commands (fqdn) WHERE acked_at IS NULL;

CREATE TABLE maintenance_runs (
  id          bigserial   PRIMARY KEY,
  fqdn        text        NOT NULL REFERENCES hosts(fqdn),
  kind        text        NOT NULL CHECK (kind IN ('check')),
  subset      text        NOT NULL DEFAULT '',
  started_at  timestamptz NOT NULL,
  finished_at timestamptz,
  status      text        NOT NULL DEFAULT '',
  error       text        NOT NULL DEFAULT '',
  node        text        NOT NULL
);

-- Every action. Host history is this log for the host, merged with its events. Plain append-only;
-- hash chaining waits for OQ3.
CREATE TABLE audit_log (
  id        bigserial   PRIMARY KEY,
  at        timestamptz NOT NULL DEFAULT now(),
  actor     text        NOT NULL,
  source    text        NOT NULL,
  action    text        NOT NULL,
  fqdn      text        NOT NULL DEFAULT '',
  job_id    text        NOT NULL DEFAULT '',
  revision  bigint      NOT NULL DEFAULT 0,
  detail    jsonb       NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_log_host ON audit_log (fqdn, at DESC);
CREATE TRIGGER audit_log_append_only BEFORE UPDATE OR DELETE ON audit_log
  FOR EACH ROW EXECUTE FUNCTION holdfast_append_only();
