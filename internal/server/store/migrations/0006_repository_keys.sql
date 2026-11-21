-- Decision 032: a restic password (repository key) added outside Holdfast is shown and alerted.
-- repo_keys_known: the key IDs Holdfast made, or accepted (acknowledge); repo_keys_foreign: the others,
-- as last listed.
ALTER TABLE hosts
  ADD COLUMN repo_keys_known   text[] NOT NULL DEFAULT '{}',
  ADD COLUMN repo_keys_foreign text[] NOT NULL DEFAULT '{}';
