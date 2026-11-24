# Changelog

The collection is released with Holdfast and has its version; Holdfast's own changes are in its release notes.

## 0.1-dev

The first release, as a development release of Holdfast, subject to change: the roles `server`, `agent` and `cli`,
and the playbook `site`, which gathers no facts. Until 0.1.0, `holdfast_version: 0.1-dev` installs the newest build,
and each build upgrades the one before.

- The server and every host need `holdfast_fqdn`: the roles read no host name from the machine, unless
  `holdfast_hostname_autodetect` is true on the agent role.
- Each job in `holdfast_jobs` is a mapping of `holdfast job set`'s flags, with an underscore for a dash, such as
  `{path: [/etc, /home], exclude: ["*.tmp"], daily: "02:00", keep_monthly: 6mo}`.
