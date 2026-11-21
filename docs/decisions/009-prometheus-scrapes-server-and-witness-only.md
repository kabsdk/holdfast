# 009: Prometheus scrapes only the server and the witness

Status: accepted, 2026-11-21

## Decision

- Prometheus scrapes the server and the storage witness. It never scrapes individual agents.
- The alert rules ship with the product.

## Why

- **Agents only dial out.** Hosts need no inbound port, and there is no scrape config to maintain per
  host.
- **The server already has the agents' state.** It knows the last-seen time of every agent and the status
  of every job. Its staleness metrics catch an agent that is dead or unreachable. A per-agent scrape would
  simply go silent.
- **The alert rules are part of the product.** Their thresholds come from the product's own metrics, such
  as the longest gap, so they are tested and versioned with the code.

*The reasons were inferred when the record was written. You accepted them on 2026-11-21, leaving the call to me ("Whatever you recommend").*

## Consequences

- An agent's metrics reach Prometheus only through the server, and arrive late when the server is down. The
  witness covers that window.
