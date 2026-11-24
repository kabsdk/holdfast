# Changelog

The collection is released with Holdfast and has its version; Holdfast's own changes are in its release notes.

## 0.1.0

The first release: the roles `server`, `agent` and `cli`, and the playbook `site`, which gathers no facts.

- The server and every host need `holdfast_fqdn`: the roles read no host name from the machine, unless
  `holdfast_hostname_autodetect` is true on the agent role.
- Each job in `holdfast_jobs` is a mapping of `holdfast job set`'s flags, with an underscore for a dash, such as
  `{path: [/etc, /home], exclude: ["*.tmp"], daily: "02:00", keep_monthly: 6mo}`.

Two development releases before it, 0.5.0 and 0.6.0, were withdrawn when the numbering started again at 0.1.0.
