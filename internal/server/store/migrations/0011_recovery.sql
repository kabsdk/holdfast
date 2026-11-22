-- M6 (plan-m6): what the agent last reported of its dumps (names only, decision 037) and of a pause.
ALTER TABLE agents
  ADD COLUMN dumps          text[]      NOT NULL DEFAULT '{}',
  ADD COLUMN paused_command text        NOT NULL DEFAULT '',
  ADD COLUMN paused_until   timestamptz;

-- A command's secrets (a restore from another host carries that host's password and a read-only key): sealed
-- like the credentials, and cleared once the agent has acknowledged the command. Pause and resume, for a
-- rollback.
ALTER TABLE commands ADD COLUMN sealed bytea;
ALTER TABLE commands DROP CONSTRAINT commands_kind_check;
ALTER TABLE commands ADD CONSTRAINT commands_kind_check CHECK (kind IN ('backup_now','check_now','restore','pause','resume'));

-- A repository rollback (decision 038): asked for, run in the background, and its outcome.
CREATE TABLE rollbacks (
  id                  bigserial   PRIMARY KEY,
  fqdn                text        NOT NULL REFERENCES hosts(fqdn),
  actor               text        NOT NULL,
  to_time             timestamptz NOT NULL,
  requested_at        timestamptz NOT NULL,
  finished_at         timestamptz,
  status              text        NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'success', 'failed')),
  agent_down          boolean     NOT NULL DEFAULT false,
  message             text        NOT NULL DEFAULT '',
  brought_back        integer     NOT NULL DEFAULT 0,
  set_aside           integer     NOT NULL DEFAULT 0,
  snapshots_back      text[]      NOT NULL DEFAULT '{}',
  snapshots_set_aside text[]      NOT NULL DEFAULT '{}',
  check_passed        boolean,
  error               text        NOT NULL DEFAULT ''
);
CREATE INDEX rollbacks_host ON rollbacks (fqdn, requested_at DESC);
