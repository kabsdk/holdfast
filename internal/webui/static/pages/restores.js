import { html, useState, usePoll, link, fmtTime, Pill, Snap, ErrorBox, Loading } from '../lib.js';
import { api } from '../api.js';

export function Restores({ hosts }) {
  const r = usePoll(() => api.restores(), []);
  const [fqdn, setFqdn] = useState('');
  const [job, setJob] = useState('');
  const host = hosts.find((h) => h.fqdn === fqdn);
  return html`<h1>Restores</h1>
    <p class="muted">Every restore on the hosts you can see. A restore always starts from a snapshot: host → job → snapshot → Restore.</p>
    <div class="panel row">
      <label>Host <select value=${fqdn} onChange=${(e) => { setFqdn(e.target.value); setJob(''); }}>
        <option value="">choose…</option>${hosts.filter((h) => h.jobs.length).map((h) => html`<option>${h.fqdn}</option>`)}</select></label>
      <label>Job <select value=${job} onChange=${(e) => setJob(e.target.value)} disabled=${!host}>
        <option value="">choose…</option>${(host?.jobs || []).map((j) => html`<option>${j.id}</option>`)}</select></label>
      <a class="button primary" href=${fqdn && job ? link('hosts', fqdn, 'jobs', job, 'snapshots') : '#/restores'} disabled=${!(fqdn && job)}>Choose a snapshot</a>
    </div>
    <${ErrorBox} error=${r.error} />
    ${!r.data ? html`<${Loading} />` : html`
      ${r.data.queued.length > 0 && html`<h2>Queued</h2><table>
        <thead><tr><th>Asked</th><th>Host · job</th><th>By</th><th>Expires</th></tr></thead>
        <tbody>${r.data.queued.map((c) => html`<tr><td>${fmtTime(c.created_at)}</td><td>${c.fqdn} · ${c.job_id}</td><td>${c.created_by}</td><td>${fmtTime(c.expires_at)}</td></tr>`)}</tbody></table>`}
      <h2>History</h2>
      ${r.data.done.length === 0 ? html`<p class="muted">No restores yet.</p>` : html`<table>
        <thead><tr><th>When</th><th>Host · job</th><th>Snapshot</th><th>What → where</th><th>Result</th></tr></thead>
        <tbody>${r.data.done.map((e) => html`<tr>
          <td>${fmtTime(e.finished)}</td><td>${e.fqdn} · ${e.job_id}</td>
          <td>${e.detail?.snapshot && html`<${Snap} fqdn=${e.fqdn} id=${e.detail.snapshot} />`}</td>
          <td>${e.detail?.target || ''}${e.detail?.directory ? html` → <code>${e.detail.directory}</code>` : ''}${e.detail?.dry_run === 'true' ? ' (dry run)' : ''}</td>
          <td><${Pill} kind=${e.status === 'success' ? 'ok' : 'error'}>${e.status}<//> ${e.detail?.files_restored ?? ''} files${e.error ? ' · ' + e.error : ''}</td>
        </tr>`)}</tbody></table>`}`}`;
}
