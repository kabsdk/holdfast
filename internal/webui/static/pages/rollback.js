// Roll an agent's repository back from S3's old versions (decisions 038 and 058): pick something that was deleted or
// overwritten, and bring it back. The server lists those moments, works out the preview and refuses what it must; this
// page only shows them and asks for the agent's name.
import { html, useState, usePoll, useAction, link, fmtTime, ErrorBox, Loading, Pill, can } from '../lib.js';
import { api } from '../api.js';

const KINDS = ['snapshots', 'index', 'data', 'keys', 'config', 'other'];
const NAMES = { snapshots: 'snapshot files', index: 'index files', data: 'data files', keys: 'key files', config: 'config', other: 'other files' };
const count = (m) => KINDS.reduce((n, k) => n + (m[k] || 0), 0);
const short = (ids) => ids.map((id) => id.slice(0, 8)).join(' ');

// what says what one moment deleted or overwrote: its snapshots and their jobs first, then the rest by kind.
function what(p) {
  const parts = [];
  const snaps = p.deleted.snapshots || 0;
  if (snaps) parts.push(`${snaps} ${snaps === 1 ? 'snapshot' : 'snapshots'}${p.jobs.length ? ' of ' + p.jobs.join(' and ') : ''}`);
  const rest = KINDS.filter((k) => k !== 'snapshots' && p.deleted[k]).map((k) => `${p.deleted[k]} ${NAMES[k]}`);
  if (rest.length) parts.push((snaps ? 'and ' : '') + rest.join(', '));
  const del = parts.length ? parts.join(', ') + ' deleted' : '';
  const over = KINDS.filter((k) => p.overwritten[k]).map((k) => `${p.overwritten[k]} ${NAMES[k]}`).join(', ');
  return [del, over && over + ' overwritten'].filter(Boolean).join('; ');
}

// who says who made one moment's changes: one of Holdfast's runs, or not Holdfast, in the CLI's words.
function who(p) {
  const name = { scheduled: 'the daily run’s retention', run_now: 'Run retention now', prune_now: 'Prune now', delete_now: 'Delete now', rollback: 'a rollback' }[p.by];
  if (!name) return html`<span class="errtext">not Holdfast: ${count(p.overwritten) ? 'changed' : 'deleted'} in the bucket</span>`;
  return p.by === 'scheduled' ? name : `${name}, by ${p.actor}`;
}

// Preview is what bringing one moment back changes, and the confirmation.
function Preview({ fqdn, host, point, onDone, onCancel }) {
  const pv = usePoll(() => api.rollbackPreview(fqdn, point.to), [fqdn, point.to], 0);
  const [typed, setTyped] = useState('');
  const [msg, setMsg] = useState('');
  const run = useAction();
  const p = pv.data;
  const sum = (m) => KINDS.reduce((n, k) => n + (m?.[k] || 0), 0);
  return html`<div class="panel" style="margin:.8rem 0 0">
    <h3 style="margin:0 0 .5rem">Just before ${fmtTime(point.at)}, ${who(point)}</h3>
    <${ErrorBox} error=${pv.error || run.error} />
    ${!p ? html`<${Loading} what="what would change" />` : p.refused ? html`<div class="banner error">Refused: ${p.refused}</div>` : html`
      <dl class="facts">
        <dt>Comes back</dt><dd>${p.snapshots_back.length ? html`${p.snapshots_back.length} ${p.snapshots_back.length === 1 ? 'snapshot' : 'snapshots'}: <span class="mono">${short(p.snapshots_back)}</span>` : 'no snapshot'}</dd>
        <dt>Set aside</dt><dd>${p.snapshots_set_aside.length ? html`${p.snapshots_set_aside.length} ${p.snapshots_set_aside.length === 1 ? 'backup' : 'backups'} made since, not deleted: <span class="mono">${short(p.snapshots_set_aside)}</span>` : 'nothing made since'}</dd>
        <dt>Objects</dt><dd>${sum(p.bring_back)} brought back, ${sum(p.set_aside)} set aside, ${p.unchanged} unchanged</dd>
      </dl>
      ${p.forgotten_before.length > 0 && html`<div class="banner warning">${p.forgotten_before.length} of the snapshots that come back had been forgotten by Holdfast: the daily run forgets them again unless a retention keeps them.</div>`}
      ${can(host, 'repository.rollback') && html`
        <label class="field"><span>Type the agent's name to confirm</span><input value=${typed} onInput=${(e) => setTyped(e.target.value)} autocomplete="off" /></label>
        <label class="field"><span>Why (audit log)</span><input value=${msg} onInput=${(e) => setMsg(e.target.value)} /></label>
        <div class="row" style="margin-top:.5rem">
          <button class="danger" disabled=${run.busy || typed !== fqdn} onClick=${() => run.run(() => api.rollback(fqdn, { to: p.to, confirm: typed, message: msg })).then((r) => r && onDone(r))}>Roll back</button>
          <button type="button" class="link" onClick=${onCancel}>Cancel</button></div>`}`}
  </div>`;
}

export function Rollback({ fqdn }) {
  const host = usePoll(() => api.host(fqdn), [fqdn], 0);
  const pts = usePoll(() => api.rollbackPoints(fqdn), [fqdn], 0);
  const past = usePoll(() => api.rollbacks(fqdn), [fqdn], 5000);
  const [pick, setPick] = useState(null);
  const [started, setStarted] = useState(null);
  const days = host.data?.storage?.noncurrent_days;
  const list = pts.data?.points || [];
  return html`<h1>Roll ${fqdn}'s repository back</h1>
    <p class="muted">What was deleted or overwritten in this repository${days ? ` in the last ${days} days` : ''}, newest first. S3 keeps the old versions that long,
      and bringing them back puts the whole repository back as it was just before: what went then and since comes back, and backups made since are set
      aside, not deleted, so this can be rolled back too. Meanwhile the gateway refuses the agent's writes; then <code>restic check</code> runs, and the
      snapshot list is read again.</p>
    <${ErrorBox} error=${pts.error} />
    ${started && html`<div class="banner ok">Rollback ${started.id} started. Its progress is below.</div>`}
    ${!pts.data ? html`<${Loading} what="what S3 keeps" />` : pts.data.refused ? html`<div class="banner error">Refused: ${pts.data.refused}</div>`
      : list.length === 0 ? html`<p class="muted">Nothing was deleted or overwritten since ${fmtTime(pts.data.oldest)}, so there is nothing to bring back.</p>`
      : html`<div class="scrollx"><table style="min-width:640px">
          <thead><tr><th>When</th><th>What</th><th>By</th><th></th></tr></thead>
          <tbody>${list.map((p) => html`<tr style=${pick === p ? 'background:var(--info-bg)' : ''}>
            <td style="white-space:nowrap">${fmtTime(p.at)}</td><td>${what(p)}</td><td>${who(p)}</td>
            <td style="text-align:right"><button class="small" aria-pressed=${pick === p ? 'true' : 'false'} onClick=${() => setPick(p)}>Bring back…</button></td></tr>`)}</tbody></table></div>`}
    ${pick && html`<${Preview} key=${pick.to} fqdn=${fqdn} host=${host.data} point=${pick} onCancel=${() => setPick(null)}
      onDone=${(r) => { setStarted(r); setPick(null); past.reload(); pts.reload(); }} />`}
    <h2>Rollbacks of this repository</h2>
    ${!past.data ? html`<${Loading} what="rollbacks" />` : past.data.length === 0 ? html`<p class="muted">None.</p>` : html`<div class="scrollx"><table>
      <thead><tr><th>Asked</th><th>To</th><th>By</th><th>Status</th><th>Brought back</th><th>Set aside</th><th>restic check</th><th></th></tr></thead>
      <tbody>${past.data.map((r) => html`<tr>
        <td>${fmtTime(r.requested_at)}</td><td>${fmtTime(r.to)}</td><td>${r.actor}</td>
        <td><${Pill} kind=${r.status === 'success' ? 'ok' : r.status === 'running' ? 'warning' : 'error'}>${r.status}<//></td>
        <td>${r.brought_back} objects, ${r.snapshots_back.length} snapshots</td><td>${r.set_aside} objects, ${r.snapshots_set_aside.length} snapshots</td>
        <td>${r.check_passed === undefined || r.check_passed === null ? '—' : r.check_passed ? 'passed' : 'failed'}</td>
        <td class="muted">${r.error || ''}</td></tr>`)}</tbody></table></div>`}
    <p class="muted">Same from the CLI: <code>holdfast agent rollback ${fqdn}</code> lists them; <code>${'--to <time>'}</code> shows what one changes, then <code>--confirm ${fqdn}</code> does it.</p>
    <p><a href=${link('agents', fqdn)}>Back to ${fqdn}</a></p>`;
}
