# Changelog

The collection is released with Holdfast and has its version; Holdfast's own changes are in its release notes.

## 0.6.0

Breaking:

- `holdfast_fqdn` is required on the `server` and `agent` roles: the roles read no host name from the machine. On the
  server, `holdfast_public_url` (`https://<holdfast_fqdn>:8443`), `holdfast_ui_url` (`https://<holdfast_fqdn>`),
  `holdfast_mail_from` (`holdfast@<holdfast_fqdn>`) and the names in Holdfast's certificates default to it.
- Each job in `holdfast_jobs` is a mapping of `holdfast job set`'s flags, with an underscore for a dash, such as
  `{path: [/etc, /home], exclude: ["*.tmp"], daily: "02:00", keep_monthly: 6mo}`. A list repeats the flag, `true`
  gives a flag alone (`no_spread: true`), and `false` leaves it out. The flags string of 0.5.0 (`--path /etc`) is
  refused with a message.
- `holdfast_dumps` is removed, with Holdfast's dump jobs (decision 052): the agent role stops with a message while it
  is set, and takes out of `agent.yaml` a `dumps:` that 0.5.0 wrote there.

Other changes:

- `holdfast_hostname_autodetect` (agent role, default `false`): when true and `holdfast_fqdn` is not given, the role
  takes the host's own FQDN, its `fqdn` fact (what `hostname -f` says).
- The playbook `site` gathers no facts; the roles gather the little they need themselves, so it works with
  `gathering = explicit` in `ansible.cfg`.
- The README says where the collection is installed, and shows it in a `requirements.yml`.
- `ansible-doc` of the server role points to the documentation's installer settings, and says that
  `holdfast_admins` applies at every login while listed.

## 0.5.0

The first release: the roles `server`, `agent` and `cli`, and the playbook `site`.
