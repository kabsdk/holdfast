# Changelog

The collection is released with Holdfast and has its version; Holdfast's own changes are in its release notes.

## 0.1-dev

The first release, as a development release of Holdfast, subject to change: the roles `server`, `agent` and `cli`,
and the playbook `site`, which gathers no facts. Until 0.1.0, `holdfast_version: 0.1-dev` installs the newest build,
and each build upgrades the one before.

- The server role needs `holdfast_fqdn`, the server's DNS name, and the agent role needs `holdfast_agent_name`, the
  agent's name in Holdfast (decision 056). The roles read no name from the machine. `holdfast_fqdn` on the agent
  role, and `holdfast_hostname_autodetect`, are gone: the role stops and says to give `holdfast_agent_name`.
- An enrolled agent keeps its name: the agent role stops when `holdfast_agent_name` differs from the name in
  `agent.yaml`. It writes an `agent.yaml` that names the agent as `fqdn:` as `agent_name:`.
- The roles use `holdfast agent …`, which replaces `holdfast host …`.
- Each job in `holdfast_jobs` is a mapping of `holdfast job set`'s flags, with an underscore for a dash, such as
  `{path: [/etc, /home], exclude: ["*.tmp"], daily: "02:00", keep_monthly: 6mo}`, or for a PostgreSQL database
  `{connection: main, database: app, every: 6h}`.
- `holdfast_postgresql_bin_dir` and `holdfast_postgresql_allow_in_place_restore` set the agent's PostgreSQL client in
  `agent.yaml` (decision 055). The connections, with their passwords, are set in Holdfast, not in the inventory.
