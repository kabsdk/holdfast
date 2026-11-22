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
        <td>${a.detail?.message && html`<div>${a.detail.message}</div>`}<span class="muted mono">${JSON.stringify(Object.fromEntries(Object.entries(a.detail || {}).filter(([k]) => k !== 'message')))}</span></td></tr>`)}</tbody>
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

// The vaults that copy this site's hosts to another site (decisions 027 and 039). Their settings are theirs: here
// they are registered, listed and removed.
export function Vaults({ me }) {
  const list = usePoll(() => api.vaults(), [], 30000);
  const [name, setName] = useState('');
  const [keyID, setKeyID] = useState('');
  const [secret, setSecret] = useState('');
  const [msg, setMsg] = useState('');
  const add = useAction();
  const rm = useAction();
  if (!me.admin_everywhere) return html`<div class="banner warning">Vaults are for admins on every host.</div>`;
  return html`<h1>Vaults</h1>
    <p class="muted">A vault, in another site, copies this site's hosts with restic copy and keeps them under its own retention. It connects here; nothing here connects to it or deletes anything there. While it has not copied a snapshot, the daily run here keeps it for up to 7 days.</p>
    <${ErrorBox} error=${list.error || rm.error} />
    ${list.data && (list.data.length === 0 ? html`<p class="muted">No vault is registered.</p>` : html`<table>
      <thead><tr><th>Vault</th><th>Last seen</th><th>Hosts</th><th>Waiting to be copied</th><th>Copy errors</th><th></th></tr></thead>
      <tbody>${list.data.map((v) => html`<tr><td>${v.name}</td><td>${v.last_seen_at ? fmtTime(v.last_seen_at) : 'never'}</td><td>${v.hosts}</td><td>${v.pending}</td>
        <td>${v.copy_errors ? html`<${Pill} kind="error">${v.copy_errors}<//>` : '0'}</td>
        <td><button class="small" onClick=${() => { if (confirm(`Remove vault ${v.name}? Its token stops working; its copies stay in its site.`)) rm.run(() => api.removeVault(v.name, {})).then(list.reload); }}>Remove</button></td></tr>`)}</tbody></table>`)}
    <div class="panel"><h3>Register a vault</h3>
      <p class="muted">Make a key at this site's S3 server that may only read <code>repos/</code> (docs/storage.md). It is tested on a host first. The vault's token is shown once.</p>
      <label class="field"><span>Name</span><input value=${name} onInput=${(e) => setName(e.target.value)} placeholder="site-b" /></label>
      <label class="field"><span>Read-only key: access key ID</span><input value=${keyID} onInput=${(e) => setKeyID(e.target.value)} autocomplete="off" /></label>
      <label class="field"><span>Secret access key</span><input type="password" value=${secret} onInput=${(e) => setSecret(e.target.value)} autocomplete="new-password" /></label>
      <label class="field"><span>Why (audit log)</span><input value=${msg} onInput=${(e) => setMsg(e.target.value)} /></label>
      <button class="primary" disabled=${add.busy || !name || !keyID || !secret} onClick=${() => add.run(() => api.addVault({ name, s3_key: { access_key_id: keyID, secret_access_key: secret }, message: msg })).then(list.reload)}>Test the key and register</button>
      <${ErrorBox} error=${add.error} />
      ${add.done && html`<div class="banner ok">Vault ${add.done.name} is registered. Its token, shown only now: put it alone in the vault's <code>source.token_file</code> (root-only).
        <pre>${add.done.token}</pre></div>`}
    </div>`;
}

export function Settings({ settings }) {
  const s = settings;
  return html`<h1>Settings</h1>
    <p class="muted">What <code>server.yaml</code> says, without secrets. Changing it needs root on the server (spec §3.3).</p>
    <div class="panel"><dl class="facts">
      ${s.role === 'vault' ? html`<dt>Vault</dt><dd>${s.node} · copies from ${s.source}${s.ui_url ? ` · people use ${s.ui_url}` : ''}</dd>`
        : html`<dt>Server</dt><dd>${s.node} · agents connect to ${s.agent_url}${s.ui_url ? ` · people to ${s.ui_url}` : ''}</dd>`}
      <dt>Default time zone</dt><dd>${s.default_zone}</dd>
      <dt>restic</dt><dd>${s.restic_version}, which the packages ship</dd>
      <dt>Storage</dt><dd>${s.storage.endpoint} · bucket ${s.storage.bucket}${s.storage.region ? ' · ' + s.storage.region : ''}</dd>
      <dt>Repository check</dt><dd>${s.maintenance.check}, reading 1/${s.maintenance.check_cycle_days} of the data each day</dd>
      <dt>Retention</dt><dd>${s.maintenance.retention}</dd>
      <dt>Login</dt><dd>${s.login.issuer || 'not set up: only root on the server can use Holdfast'} · groups from the ${s.login.groups_claim} claim, usernames from ${s.login.username_claim}</dd>
      ${s.login.bootstrap_admins && html`<dt>Bootstrap admins</dt><dd>${s.login.bootstrap_admins.join(', ') || 'none'}</dd>`}
    </dl></div>`;
}
