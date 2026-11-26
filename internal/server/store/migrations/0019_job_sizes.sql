-- What each job's restore points take in this site's repository: restic stats --mode raw-data of the job's snapshots,
-- each piece of data once, as stored. Measured once a day with the repository's size (spec §3.5).
CREATE TABLE job_sizes (
  fqdn        text        NOT NULL,
  job_id      text        NOT NULL,
  bytes       bigint      NOT NULL,
  measured_at timestamptz NOT NULL,
  PRIMARY KEY (fqdn, job_id)
);
