-- M4: login sessions (decision 034). The token is stored only as its sha256. Groups are the ones the
-- provider gave at login.
CREATE TABLE sessions (
  token_sha256 text        PRIMARY KEY,
  kind         text        NOT NULL CHECK (kind IN ('browser','cli')),
  subject      text        NOT NULL,
  username     text        NOT NULL,
  groups       text[]      NOT NULL DEFAULT '{}',
  created_at   timestamptz NOT NULL,
  expires_at   timestamptz NOT NULL,
  idle_seconds integer     NOT NULL DEFAULT 0, -- a browser session ends after this long without use; 0: never
  last_used_at timestamptz NOT NULL
);
CREATE INDEX sessions_username ON sessions (username);
