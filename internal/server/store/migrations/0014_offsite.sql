-- M8 (decision 041): the off-site copies are made by holdfast-server itself, by a second instance of its own code with
-- its tables in the schema "offsite" of the same database. The two talk in-process: no vault registers, no token, no
-- read key, and a restore from off-site is granted at once, through the gateway.
DROP TABLE vault_requests;
DROP TABLE vault_feed;
ALTER TABLE vaults DROP COLUMN token_sha256, DROP COLUMN s3_access_key_id, DROP COLUMN s3_secret;

-- No host has an S3 key: it backs up through the gateway, and restores from off-site through it too.
ALTER TABLE host_credentials DROP COLUMN s3_access_key_id, DROP COLUMN s3_secret, DROP COLUMN s3_key_source;

-- A grant to read a repository through the gateway is for the primary site's repositories or the off-site ones.
ALTER TABLE gateway_grants ADD COLUMN site text NOT NULL DEFAULT 'primary' CHECK (site IN ('primary', 'offsite'));
DROP INDEX gateway_grants_open;
CREATE INDEX gateway_grants_open ON gateway_grants (reader, repo, site) WHERE ended_at IS NULL;
