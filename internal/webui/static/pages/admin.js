import { html, useState, useEffect, usePoll, useAction, fmtTime, ErrorBox, Loading, Pill } from '../lib.js';
import { api } from '../api.js';

export function Audit({ hosts }) {
  const [host, setHost] = useState('');
  const [action, setAction] = useState('');
  const [pages, setPages] = useState([]);
  const first = usePoll(() => api.audit({ host, action, limit: 100 }), [host, action]);
  useEffect(() => setPages([]), [host, action]);
  const older = useAction();
  const rows = [...(first.data || []), ...pages];
  return html`<h1>Audit log</h1>
    <p class="muted">Every action, with who did it, when, and the warnings they accepted. It is append-only. You see the entries of your hosts.</p>
    <div class="row panel">
      <label>Host <select value=${host} onChange=${(e) => setHost(e.target.value)}><option value="">all you can see</option>${hosts.map((h) => html`<option>${h.fqdn}</option>`)}</select></label>
      <label>Action <input placeholder="such as restore.requested" value=${action} onInput=${(e) => setAction(e.target.value.trim())} /></label>
    </div>
    <${ErrorBox} error=${first.error || older.error} />
    ${!first.data ? html`<${Loading} />` : html`<table>
      <thead><tr><th>When</th><th>Who</th><th>Action</th><th>Host</th><th>Reason and details</th></tr></thead>
      <tbody>${rows.map((a) => html`<tr><td>${fmtTime(a.at)}</td><td>${a.actor} <span class="muted">${a.source}</span></td><td><code>${a.action}</code></td>
        <td>${a.fqdn}${a.job_id ? ' · ' + a.job_id : ''}</td>
        <td>${a.detail?.message && html`<div>${a.detail.message}</div>`}<span class="muted mono wrap">${JSON.stringify(Object.fromEntries(Object.entries(a.detail || {}).filter(([k]) => k !== 'message')))}</span></td></tr>`)}</tbody>
    </table>
    ${rows.length >= 100 && html`<button disabled=${older.busy} onClick=${async () => {
      const more = await older.run(() => api.audit({ host, action, limit: 100, before: rows[rows.length - 1].id }));
      if (more) setPages([...pages, ...more]);
    }}>Older</button>`}`}`;
}

export function Access({ me }) {
  const acc = usePoll(() => api.access(), [], 0);
  const sessions = usePoll(() => api.sessions(), [], 30000);
  const [rows, setRows] = useState(null);
  const [msg, setMsg] = useState('');
  const save = useAction();
  const end = useAction();
  useEffect(() => { if (acc.data) setRows(acc.data.bindings.map((b) => ({ ...b, hosts: b.hosts.join(' ') }))); }, [acc.data]);
  if (!me.admin_everywhere) return html`<div class="banner warning">The bindings are for admins on every host.</div>`;
  if (acc.error) return html`<${ErrorBox} error=${acc.error} />`;
  if (!rows) return html`<${Loading} />`;
  const set = (i, k, v) => setRows(rows.map((r, j) => (j === i ? { ...r, [k]: v } : r)));
  return html`<h1>Access</h1>
    <p class="muted">A binding gives a group from the login provider one role on the hosts its patterns match (* for any characters). Several bindings combine per host. A host no binding of yours covers does not exist for you. There are three roles and no custom ones (decision 035).</p>
    <table>
      <thead><tr><th>Group</th><th>Role</th><th>Hosts</th><th></th></tr></thead>
      <tbody>${rows.map((r, i) => html`<tr>
        <td><input value=${r.group} onInput=${(e) => set(i, 'group', e.target.value)} /></td>
        <td><select value=${r.role} onChange=${(e) => set(i, 'role', e.target.value)}>${Object.keys(acc.data.roles).map((x) => html`<option>${x}</option>`)}</select></td>
        <td><input style="width:100%" value=${r.hosts} onInput=${(e) => set(i, 'hosts', e.target.value)} placeholder="* or web*.example.com db01.example.com" /></td>
        <td><button class="small" onClick=${() => setRows(rows.filter((_, j) => j !== i))}>Remove</button></td></tr>`)}</tbody>
    </table>
    <p><button class="small" onClick=${() => setRows([...rows, { group: '', role: 'viewer', hosts: '*' }])}>+ Add binding</button></p>
    <div class="row"><input style="flex:1" placeholder="Message" value=${msg} onInput=${(e) => setMsg(e.target.value)} />
      <button class="primary" disabled=${save.busy} onClick=${() => save.run(() => api.setAccess({ base_revision: acc.data.revision, message: msg,
        bindings: rows.map((r) => ({ group: r.group.trim(), role: r.role, hosts: r.hosts.split(/[\s,]+/).filter(Boolean) })) })).then((r) => r && acc.reload())}>Save as a new revision</button></div>
    <${ErrorBox} error=${save.error} />
    ${save.done && html`<div class="banner ok">Saved as r${save.done.revision}. Group changes at the provider take effect at each user's next login; end their sessions below to make it at once.</div>`}
    <h2>What each role may do</h2>
    <table><thead><tr><th>Role</th><th>Permissions</th></tr></thead>
      <tbody>${Object.entries(acc.data.roles).map(([r, perms]) => html`<tr><td>${r}</td><td>${perms.map((p) => html`<${Pill}>${p}<//>`)}</td></tr>`)}</tbody></table>
    <h2>Sessions</h2>
    <${ErrorBox} error=${sessions.error || end.error} />
    ${sessions.data && html`<table><thead><tr><th>User</th><th>Kind</th><th>Groups</th><th>Since</th><th>Last used</th><th>Ends</th><th></th></tr></thead>
      <tbody>${sessions.data.map((s) => html`<tr><td>${s.username}</td><td>${s.kind}</td><td>${s.groups.join(', ')}</td><td>${fmtTime(s.created_at)}</td><td>${fmtTime(s.last_used_at)}</td><td>${fmtTime(s.expires_at)}</td>
        <td><button class="small" onClick=${() => { if (confirm(`End every session of ${s.username}?`)) end.run(() => api.endSessions(s.username)).then(sessions.reload); }}>End their sessions</button></td></tr>`)}</tbody></table>`}`;
}

// The nightly export (decision 040): what a new server is restored from if this one is lost (decision 042).
export function Exports({ me }) {
  const ex = usePoll(() => api.exports(), [], 30000);
  const now = useAction();
  if (ex.error) return html`<${ErrorBox} error=${ex.error} />`;
  if (!ex.data) return html`<${Loading} />`;
  const v = ex.data;
  const size = (n) => (n >= 1 << 20 ? (n / (1 << 20)).toFixed(1) + ' MiB' : (n / 1024).toFixed(1) + ' KiB');
  return html`<h1>Exports</h1>
    <p class="muted">Every night at 00:30 the server writes its database, encrypted to the escrow's age key, to <code>meta/</code> in the bucket${v.offsite ? ', and to the off-site bucket too' : ''}.
      If this server is lost, a new one on the same bucket is restored from the newest (<code>holdfast-server restore-export</code>, docs/runbooks/rebuild-holdfast.md). The newest ${v.keep} are kept.</p>
    ${!v.on && html`<div class="banner warning">No export is written: server.yaml names no <code>escrow.age_recipient</code>.</div>`}
    ${v.restored && html`<div class="banner">This server was restored from ${v.restored.export_name} on ${fmtTime(v.restored.restored_at)}, at revision ${v.restored.head}.</div>`}
    ${v.on && html`<div class="panel row">
      <span>Last written: ${v.last_good ? fmtTime(v.last_good.started_at) : 'never'}${v.offsite ? ` · off-site: ${v.last_offsite ? fmtTime(v.last_offsite.started_at) : 'never'}` : ''} · next: ${fmtTime(v.next_at)}</span>
      ${me.admin_everywhere && html`<button class="primary" disabled=${now.busy} onClick=${() => now.run(() => api.exportNow()).then(ex.reload, ex.reload)}>Export now</button>`}
    </div>`}
    <${ErrorBox} error=${now.error} />
    ${now.done && html`<div class=${'banner ' + (now.done.offsite_error ? 'warning' : 'ok')}>Wrote meta/${now.done.name}, ${size(now.done.bytes)}${now.done.offsite ? (now.done.offsite_error ? '; not off-site: ' + now.done.offsite_error : ', off-site too') : ''}.</div>`}
    <table><thead><tr><th>When</th><th>Object</th><th>Size</th><th>By</th><th>Result</th></tr></thead>
      <tbody>${v.recent.map((e) => html`<tr><td>${fmtTime(e.started_at)}</td><td class="mono">meta/${e.name}</td><td>${e.error ? '' : size(e.bytes)}</td><td>${e.actor}</td>
        <td>${e.error ? html`<${Pill} kind="error">failed<//> ${e.error}` : e.offsite && e.offsite_error ? html`<${Pill} kind="warning">not off-site<//> ${e.offsite_error}` : html`<${Pill} kind="ok">written<//>${e.offsite ? ' · off-site too' : ''}`}</td></tr>`)}</tbody></table>
    <p class="muted">Same: <code>holdfast export list</code>, <code>holdfast export now</code>.</p>`;
}

export function Settings({ settings, me }) {
  const s = settings;
  const test = useAction();
  const n = s.notify || {};
  const alerts = [n.mail_to?.length && `mail to ${n.mail_to.join(', ')} through ${n.relay}`, n.webhook && 'a webhook', n.heartbeat && 'a heartbeat for a dead man\'s switch'].filter(Boolean);
  return html`<h1>Settings</h1>
    <p class="muted">What <code>server.yaml</code> says, without secrets. Changing it needs root on the server (spec §3.3).</p>
    <div class="panel"><dl class="facts">
      ${s.role === 'vault' ? html`<dt>Off-site copies</dt><dd>made by ${s.node}, every host's snapshots copied with restic copy, under their own retention</dd>`
        : html`<dt>Server</dt><dd>${s.node} · agents connect to ${s.agent_url}${s.ui_url ? ` · people to ${s.ui_url}` : ''}</dd>
          <dt>Off-site copies</dt><dd>${s.offsite ? 'on: see the Off-site copies pages' : 'off: server.yaml has no offsite: section'}</dd>`}
      <dt>Default time zone</dt><dd>${s.default_zone}</dd>
      <dt>restic</dt><dd>${s.restic_version}, which the packages ship</dd>
      <dt>Storage</dt><dd>${s.storage.endpoint} · bucket ${s.storage.bucket}${s.storage.region ? ' · ' + s.storage.region : ''}</dd>
      <dt>Repository check</dt><dd>${s.maintenance.check}, reading 1/${s.maintenance.check_cycle_days} of the data each day</dd>
      <dt>Retention</dt><dd>${s.maintenance.retention}</dd>
      <dt>Login</dt><dd>${s.login.issuer ? `${s.login.issuer} · groups from the ${s.login.groups_claim} claim, usernames from ${s.login.username_claim}` : 'no identity provider: root logs in with holdfast ui-login on the server'}</dd>
      ${s.login.bootstrap_admins && html`<dt>Bootstrap admins</dt><dd>${s.login.bootstrap_admins.join(', ') || 'none'}</dd>`}
      ${s.role !== 'vault' && html`<dt>Alerts</dt><dd>${alerts.length ? `${alerts.join('; ')}, once a problem has lasted ${n.after}` : 'none: set notify: in server.yaml, or use the Prometheus rules'}</dd>`}
    </dl>
    ${s.role !== 'vault' && me.admin_everywhere && alerts.length > 0 && html`<p><button disabled=${test.busy} onClick=${() => test.run(() => api.notifyTest())}>Send a test alert</button>
      ${test.done && html` <span class="muted">${['mail', 'webhook', 'heartbeat'].filter((c) => test.done[c]).map((c) => `${c}: ${test.done[c]}`).join(' · ')}</span>`}</p>
      <${ErrorBox} error=${test.error} />`}
    </div>`;
}
