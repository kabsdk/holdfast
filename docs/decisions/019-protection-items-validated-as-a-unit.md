# 019: A protection item is schedule plus retention, validated as a unit

Status: accepted, 2026-11-21

## Decision

A protection item bundles a schedule and a retention, and the checks validate the two together.

## Why

A retention policy only means something relative to its schedule:
- An hourly tier on a daily schedule adds nothing (R001).
- The worst-case loss and the oldest restore point depend on both (R101, R103).

If the two were validated separately, each half could pass while the combination is wrong.

## Consequences

- The checks engine takes the pair as its input, never a retention alone.
