-- PostgreSQL connections' passwords (decision 055). The rest of a connection is in the config, in every revision; the
-- passwords are kept here, sealed with the credentials key as a repository password is, and sent only to the agent.
-- kind is backup (the user the agent backs up as) or restore (the user it restores as).
CREATE TABLE pg_passwords (
  fqdn       text        NOT NULL,
  connection text        NOT NULL,
  kind       text        NOT NULL CHECK (kind IN ('backup', 'restore')),
  sealed     bytea       NOT NULL,
  updated_at timestamptz NOT NULL,
  PRIMARY KEY (fqdn, connection, kind)
);

-- A deletion always says why (decision 055): who asked, and the reason, stay on the snapshot after Holdfast deleted it.
ALTER TABLE snapshots ADD COLUMN deletion_requested_by text NOT NULL DEFAULT '';
ALTER TABLE snapshots ADD COLUMN deletion_reason text NOT NULL DEFAULT '';

-- Test connection: the agent connects as it would to back up, and reports what it finds.
ALTER TABLE commands DROP CONSTRAINT commands_kind_check;
ALTER TABLE commands ADD CONSTRAINT commands_kind_check CHECK (kind IN ('backup_now','check_now','restore','pause','resume','postgresql_test'));

-- The address an agent last connected from (decision 056): Holdfast knows an agent by its name and this, and no longer
-- reads the machine's hostname.
ALTER TABLE agents ADD COLUMN remote_addr text NOT NULL DEFAULT '';

-- What an agent has for PostgreSQL jobs, as it reports it: its pg_dump's version, and its own limit on in-place
-- restores of a database (proto.PGFacts). NULL: no postgresql.bin_dir in its agent.yaml.
ALTER TABLE agents ADD COLUMN postgresql jsonb;
