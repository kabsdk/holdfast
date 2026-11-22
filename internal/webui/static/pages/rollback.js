// Roll a host's repository back to a time, from S3's old versions (decision 038). The server works out the
// preview and refuses what it must; this page only shows it and asks for the FQDN.
import { html, useState, usePoll, useAction, link, fmtTime, can, ErrorBox, Loading, Pill } from '../lib.js';
import { api } from '../api.js';

const kinds = (m) => ['config', 'keys', 'snapshots', 'index', 'data', 'other'].filter((k) => m[k]).map((k) => `${m[k]} ${k}`).join(', ') || 'none';
const short = (ids) => ids.map((id) => id.slice(0, 8)).join(' ');

// toInput turns a time into a datetime-local value, in the browser's zone.
function toInput(d) {
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}

export function Rollback({ fqdn }) {
  const host = usePoll(() => api.host(fqdn), [fqdn], 0);
  const past = usePoll(() => api.rollbacks(fqdn), [fqdn], 5000);
  const [when, setWhen] = useState(toInput(new Date(Date.now() - 3600 * 1000)));
  const [typed, setTyped] = useState('');
  const [down, setDown] = useState(false);
  const [msg, setMsg] = useState('');
  const preview = useAction();
  const run = useAction();
  const h = host.data;
  const p = preview.done;
  const to = () => new Date(when).toISOString().replace(/\.\d+Z$/, 'Z');
  return html`<h1>Roll ${fqdn}'s repository back</h1>
    <p class="muted">Every object under <code>repos/${fqdn}/</code> goes back to the version it had at that time: a copy of that version
      is made current, and what was not there then gets a delete marker. No version is deleted, so this can be rolled back too. The host's
      agent is paused meanwhile; then <code>restic check</code> runs, and the snapshot list is read again.</p>
    <div class="panel">
      <label class="field"><span>Roll back to</span><input type="datetime-local" value=${when} onInput=${(e) => setWhen(e.target.value)} /></label>
      <button disabled=${preview.busy} onClick=${() => preview.run(() => api.rollbackPreview(fqdn, to()))}>Show what would change</button>
      <${ErrorBox} error=${preview.error} />
      ${p && html`<div style="margin-top:.6rem">
        ${p.refused ? html`<div class="banner error">Refused: ${p.refused}</div>` : html`
          <dl class="facts">
            <dt>Brought back</dt><dd>${kinds(p.bring_back)}</dd>
            <dt>Set aside</dt><dd>${kinds(p.set_aside)}</dd>
            <dt>Unchanged</dt><dd>${p.unchanged} objects</dd>
            <dt>Snapshots back</dt><dd class="mono">${p.snapshots_back.length ? short(p.snapshots_back) : '—'}</dd>
            <dt>Snapshots set aside</dt><dd class="mono">${p.snapshots_set_aside.length ? short(p.snapshots_set_aside) : '—'}</dd>
            ${p.snapshot_deletions.length > 0 && html`<dt>Snapshot files deleted</dt><dd>${p.snapshot_deletions.map((d) => fmtTime(d)).join(', ')}</dd>`}
          </dl>
          ${p.forgotten_before.length > 0 && html`<div class="banner warning">${p.forgotten_before.length} of the snapshots that come back had been forgotten by Holdfast: the daily run forgets them again unless a retention keeps them.</div>`}
          ${can(h, 'repository.rollback') && html`
            <label class="field"><span>Type the FQDN to confirm</span><input value=${typed} onInput=${(e) => setTyped(e.target.value)} autocomplete="off" /></label>
            <label class="field"><span>Why (audit log)</span><input value=${msg} onInput=${(e) => setMsg(e.target.value)} /></label>
            <label><input type="checkbox" checked=${down} onChange=${(e) => setDown(e.target.checked)} /> The host is down, so its agent cannot be paused</label>
            <div class="row" style="margin-top:.5rem">
              <button class="danger" disabled=${run.busy || typed !== fqdn} onClick=${() => run.run(() => api.rollback(fqdn, { to: p.to, confirm: typed, agent_down: down, message: msg })).then(past.reload)}>Roll back</button>
            </div>`}`}
      </div>`}
      <${ErrorBox} error=${run.error} />
      ${run.done && html`<div class="banner ok">Rollback ${run.done.id} started. Its progress is below.</div>`}
    </div>
    <h2>Rollbacks of this repository</h2>
    ${!past.data ? html`<${Loading} what="rollbacks" />` : past.data.length === 0 ? html`<p class="muted">None.</p>` : html`<table>
      <thead><tr><th>Asked</th><th>To</th><th>By</th><th>Status</th><th>Brought back</th><th>Set aside</th><th>restic check</th><th></th></tr></thead>
      <tbody>${past.data.map((r) => html`<tr>
        <td>${fmtTime(r.requested_at)}</td><td>${fmtTime(r.to)}</td><td>${r.actor}</td>
        <td><${Pill} kind=${r.status === 'success' ? 'ok' : r.status === 'running' ? 'warning' : 'error'}>${r.status}<//></td>
        <td>${r.brought_back} objects, ${r.snapshots_back.length} snapshots</td><td>${r.set_aside} objects, ${r.snapshots_set_aside.length} snapshots</td>
        <td>${r.check_passed === undefined || r.check_passed === null ? '—' : r.check_passed ? 'passed' : 'failed'}</td>
        <td class="muted">${r.error || ''}</td></tr>`)}</tbody></table>`}
    <p class="muted">Same from the CLI: <code>holdfast host rollback ${fqdn} --to &lt;time&gt;</code>, then <code>--confirm ${fqdn}</code>.</p>
    <p><a href=${link('hosts', fqdn)}>Back to ${fqdn}</a></p>`;
}
