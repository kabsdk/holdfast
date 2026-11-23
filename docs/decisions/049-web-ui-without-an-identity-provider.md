# 049: The web UI without an identity provider; no LDAP, no local users

Status: accepted, 2026-11-23. Amends decisions 024 and 044. The questions were whether the web UI should also support
LDAP, and whether it needs a break-glass user; a login stays mandatory.

## Decision

1. **The installer turns the UI on** at `https://<server>/`, with a certificate from Holdfast's own CA unless
   `HOLDFAST_UI_TLS_CERT` and `HOLDFAST_UI_TLS_KEY` are given. `HOLDFAST_UI_URL=off` turns it off.
2. **`holdfast ui-login`**, as root on the server, prints a link that works once, within 5 minutes, and opens a browser
   session of 12 hours as `local:root`, an admin on every host. It is audited with the `SUDO_USER` who asked. The API
   call, `POST /api/v1/ui-login`, answers only on the root socket (an exception in spec §3.3). This is the login on a
   server without an identity provider, and the way in when the provider is down.
3. **OIDC stays the way everyone else logs in**, as themselves, with roles from their groups. Its three settings are
   all or none.
4. **No LDAP in Holdfast.** Authelia, Dex and Keycloak put LDAP or Active Directory behind OIDC in a few lines.
5. **No local password users.** Break glass is root on the server: the CLI on its socket, and the browser through
   `ui-login`. Below that, plain restic with the recovery file works with Holdfast gone (decision 047).

Login stays mandatory: there is no anonymous UI, and every action is audited with who did it.

## What it costs

A browser warns about Holdfast's own CA until you trust it or give a certificate. The link is a bearer secret shown in a
terminal, worth no more than root on the server, which it needs.

## Why

- **Seeing the UI should not wait for an OIDC client and a browser-trusted certificate.** Those are for letting people
  in as themselves, which a small team may do later.
- **LDAP in Holdfast** would add bind secrets, group searches, trust for the directory's certificate, and no second
  factor. The providers above do it better and are a few lines of config.
- **A password store of our own** needs brute-force throttling, a second factor, and a shared login that survives
  offboarding. Root on the server is already the most trusted account there is, and it is audited.

## What changes

`holdfast ui-login`, `/auth/link` on the UI's listener, the login page without a provider, `auth.oidc` optional, the
installer's UI defaults, spec §3.3, §6.1 and §6.4.
