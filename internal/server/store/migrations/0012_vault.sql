-- M7: holdfast-vault (decisions 027 and 039).

-- On holdfast-server: the vaults that copy its hosts, each with its token, the read-only key it reads with, and
-- its latest report.
CREATE TABLE vaults (
  name             text        PRIMARY KEY CHECK (name ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
  token_sha256     text        NOT NULL UNIQUE,
  s3_access_key_id text        NOT NULL,
  s3_secret        bytea       NOT NULL,
  created_at       timestamptz NOT NULL,
  created_by       text        NOT NULL,
  last_seen_at     timestamptz,
  reported_at      timestamptz,
  report           jsonb
);

-- On holdfast-server: a restore from a vault, asked here and granted by the vault on its next sync. Its ID is the
-- restore command's.
CREATE TABLE vault_requests (
  id           text        PRIMARY KEY,
  vault        text        NOT NULL REFERENCES vaults(name) ON DELETE CASCADE,
  fqdn         text        NOT NULL REFERENCES hosts(fqdn),
  snapshot     text        NOT NULL, -- the copy, in the vault
  original     text        NOT NULL, -- the snapshot here
  spec         jsonb       NOT NULL, -- the restore as asked
  requested_by text        NOT NULL,
  requested_at timestamptz NOT NULL,
  state        text        NOT NULL DEFAULT 'waiting' CHECK (state IN ('waiting','granted','refused','expired')),
  answered_at  timestamptz,
  error        text        NOT NULL DEFAULT ''
);

-- On holdfast-server: when Holdfast deleted a snapshot because an admin asked (decision 031), which a vault shows
-- as "Deleted in primary". A forget by retention is not one.
ALTER TABLE snapshots ADD COLUMN deleted_on_request_at timestamptz;

-- In the vault: a copy's source snapshot; the snapshots deleted in primary (decision 031); the source's last feed,
-- sealed, so copies go on while the source server is down; and each host's last copy.
ALTER TABLE snapshots ADD COLUMN original text NOT NULL DEFAULT '';
CREATE TABLE primary_deleted (
  fqdn       text        NOT NULL REFERENCES hosts(fqdn),
  original   text        NOT NULL,
  deleted_at timestamptz NOT NULL,
  PRIMARY KEY (fqdn, original)
);
CREATE TABLE vault_feed (
  id         integer     PRIMARY KEY CHECK (id = 1),
  sealed     bytea       NOT NULL,
  fetched_at timestamptz NOT NULL
);
ALTER TABLE hosts
  ADD COLUMN copied_at     timestamptz,
  ADD COLUMN copy_tried_at timestamptz,
  ADD COLUMN copy_error    text NOT NULL DEFAULT '';
