-- M2: restore through the agent. A restore is a command with a payload, and it expires when the
-- host does not pick it up in time. Its result is an event with a detail.
ALTER TABLE commands DROP CONSTRAINT commands_kind_check;
ALTER TABLE commands ADD CONSTRAINT commands_kind_check CHECK (kind IN ('backup_now','check_now','restore'));
ALTER TABLE commands
  ADD COLUMN payload      jsonb,
  ADD COLUMN expires_at   timestamptz,
  ADD COLUMN cancelled_at timestamptz;

ALTER TABLE events ADD COLUMN detail jsonb NOT NULL DEFAULT '{}';

-- The host's own restore limits, as its agent last reported them (decision 030).
ALTER TABLE agents ADD COLUMN restore_bounds jsonb;
