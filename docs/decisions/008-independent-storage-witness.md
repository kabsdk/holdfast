# 008: An independent storage witness

Status: accepted, 2026-11-21

## Decision

`holdfast-freshness` lists the bucket directly. It does not go through the agents or the server. It holds
list-only access and no repository password. It exports, per host, the write time of the newest snapshot
object.

## Why

"The agent says the backup is OK" and "the bucket has a fresh snapshot" must be judged separately. If an
agent or the server has a bug or lies, missing backups must still be visible. When the two disagree,
storage wins.

The PoC test `TestStalenessAndIndependentStorageWitness` demonstrated this: when the newest snapshot object
was deleted, the injected disagreement showed as a gap of 7194 seconds.

## Consequences

- The witness can only judge per host, not per job. It cannot decrypt snapshots, so it cannot read the
  `job=` tag (plan-m1, later-milestone question L6).
- The alerts compare the server's metrics with the witness's metrics (`HoldfastWitnessDisagreement`).
