-- M5: the escrow file in the bucket (decision 036), the repository's size, a rotated password's
-- old key, and decommission.
ALTER TABLE hosts
  -- escrow/<fqdn>.age: the credentials version it holds (NULL: none written), when, and the last failure.
  ADD COLUMN escrowed_version          integer,
  ADD COLUMN escrowed_at               timestamptz,
  ADD COLUMN escrow_tried_at           timestamptz,
  ADD COLUMN escrow_error              text        NOT NULL DEFAULT '',
  -- The repository's size in the bucket, from the daily listing with versions.
  ADD COLUMN size_at                   timestamptz,
  ADD COLUMN size_tried_at             timestamptz,
  ADD COLUMN size_current_bytes        bigint      NOT NULL DEFAULT 0,
  ADD COLUMN size_noncurrent_bytes     bigint      NOT NULL DEFAULT 0,
  ADD COLUMN size_objects              integer     NOT NULL DEFAULT 0,
  ADD COLUMN size_error                text        NOT NULL DEFAULT '',
  -- The repository key of the password a rotation replaced, until it is removed from the repository.
  ADD COLUMN repo_key_retiring         text        NOT NULL DEFAULT '',
  -- Decommission: asked for, due after 7 days, cancellable until done.
  ADD COLUMN decommission_requested_at timestamptz,
  ADD COLUMN decommission_requested_by text        NOT NULL DEFAULT '',
  ADD COLUMN decommission_due_at       timestamptz,
  ADD COLUMN decommission_tried_at     timestamptz,
  ADD COLUMN decommission_error        text        NOT NULL DEFAULT '',
  ADD COLUMN decommissioned_at         timestamptz,
  ADD COLUMN decommissioned_objects    integer     NOT NULL DEFAULT 0;
