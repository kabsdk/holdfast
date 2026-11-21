-- Decision 030: the repository check runs on the agent. `holdfast check` queues it as a command, and its
-- result arrives as an event of kind 'check'.
ALTER TABLE commands DROP CONSTRAINT commands_kind_check;
ALTER TABLE commands ADD CONSTRAINT commands_kind_check CHECK (kind IN ('backup_now','check_now'));
