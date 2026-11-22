-- M8 (decision 041): hosts back up through the server's gateway, and hold no S3 key. Besides its own repository, a
-- host may read another host's through the gateway only under a grant: for one restore onto it. The grant ends when
-- the host reports the restore, when the command expires, or at expires_at, whichever comes first.
CREATE TABLE gateway_grants (
  command_id text        PRIMARY KEY,
  reader     text        NOT NULL REFERENCES hosts (fqdn),
  repo       text        NOT NULL REFERENCES hosts (fqdn),
  created_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL,
  ended_at   timestamptz
);
CREATE INDEX gateway_grants_open ON gateway_grants (reader, repo) WHERE ended_at IS NULL;
