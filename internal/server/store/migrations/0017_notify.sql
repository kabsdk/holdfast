-- The server sends its own alerts (decision 046). One row per problem it has seen, until the problem ends: whether it
-- was sent, so it is sent once when it starts and once when it ends, and a restart sends nothing twice.
CREATE TABLE notify_state (
  key        text PRIMARY KEY,           -- code, host and job
  severity   text NOT NULL,
  message    text NOT NULL,
  first_seen timestamptz NOT NULL,
  notified   boolean NOT NULL DEFAULT false
);
