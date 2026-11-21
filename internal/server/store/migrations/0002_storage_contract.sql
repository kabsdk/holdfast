-- Decision 029: the admin makes each host's S3 key, and Holdfast tests the storage contract.

-- Who made the host's key: 'admin' (given at host add or set-key) or 'holdfast' (generated, then made
-- by the optional provisioner command). The provisioner only ever runs for keys Holdfast generated.
ALTER TABLE host_credentials
  ADD COLUMN s3_key_source text NOT NULL DEFAULT 'holdfast' CHECK (s3_key_source IN ('holdfast','admin'));

-- The last storage contract test of each host: when, and what failed (an empty array when all passed).
ALTER TABLE hosts
  ADD COLUMN storage_checked_at timestamptz,
  ADD COLUMN storage_findings   jsonb;
