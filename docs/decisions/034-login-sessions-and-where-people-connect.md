# 034: Login, sessions, and where people connect

Status: accepted, 2026-11-22. It builds on decision
024.

## Decision

- **The server is the only OIDC client.** It runs the authorization code flow, with PKCE, for browsers, and
  the device flow (RFC 8628) for the CLI. It checks the ID token, reads the groups claim, and issues its own
  session: a cookie for a browser, a token for the CLI. Any provider with both flows works, such as Authelia
  4.39 or later, Keycloak, Dex or Okta.
- **Sessions live in PostgreSQL,** with the token stored hashed. A browser session lasts 12 hours and ends
  after an hour without use. A CLI session lasts 7 days. Groups are read at login. An admin can end a user's
  sessions at once.
- **People connect on a listener of their own,** `ui.listen`, with a certificate browsers trust. Agents keep
  the agent listener and the certificate they pin. The UI's listener is off until it is configured.
- **Root on the server keeps the Unix socket,** as admin on every host. Escrow export stays there only.
- **No automation tokens yet.** Scripts on the server use the root socket.
- **`auth.bootstrap_admins`** names users who are admin on every host whatever their groups, for the first
  login. Each login that relies on it is audited.
- **The tests log in through a real provider:** a pinned Authelia, in the e2e tests and in the lab.

## Why

- **One relying party is one thing to secure.** The CLI holds no client secret, and the API checks only its
  own sessions, never tokens from the provider.
- **The device flow lets the CLI log in from any machine**, with the provider's own second factor, without a
  browser on that machine.
- **Two listeners keep two trust models apart.** Agents pin Holdfast's CA; browsers trust the organisation's
  CA. Every host must reach the agent port; only admins need the UI.
- **Root can already read the database and the sealing key,** so giving it less than admin would be pretend
  security.
