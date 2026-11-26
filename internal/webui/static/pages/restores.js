import { html, useState, usePoll, link, fmtTime, Pill, Snap, ErrorBox, Loading, isPG, dbIcon } from '../lib.js';
import { api } from '../api.js';

// intoText says where a restore went: a new database or one in place (decision 055), a folder, or the original paths.
function intoText(d) {
  if (d.database) return d.in_place === 'true' ? `${d.database} on ${d.connection}, in place${d.pre_restore ? `; safety snapshot ${d.pre_restore.slice(0, 8)}` : ''}` : `new database ${d.database} on ${d.connection}`;
  if (d.directory) return d.directory;
  return d.target === 'in_place' ? 'the original location' : d.target || '';
}

function resultText(e) {
  const d = e.detail || {};
  const parts = [e.status];
  if (d.tables) parts.push(`${d.tables} tables`);
  else if (d.files_restored != null) parts.push(`${d.files_restored} files`);
  if (e.error) parts.push(e.error);
  return parts.join(' · ');
}

export function Restores({ hosts }) {
  const r = usePoll(() => api.restores(), []);
  const [fqdn, setFqdn] = useState('');
  const [job, setJob] = useState('');
  const host = hosts.find((h) => h.fqdn === fqdn);
  const jobOf = (e) => hosts.find((h) => h.fqdn === e.fqdn)?.jobs.find((j) => j.id === e.job_id);
  return html`<h1>Restores</h1>
    <p class="muted">Every restore on the agents you can see: files, and PostgreSQL dumps into a database or as their file. A restore always starts from a restore point: agent → job → its calendar → Restore.</p>
    <div class="panel row">
      <label>Agent <select value=${fqdn} onChange=${(e) => { setFqdn(e.target.value); setJob(''); }}>
        <option value="">choose…</option>${hosts.filter((h) => h.jobs.length).map((h) => html`<option value=${h.fqdn}>${h.fqdn}</option>`)}</select></label>
      <label>Job <select value=${job} onChange=${(e) => setJob(e.target.value)} disabled=${!host}>
        <option value="">choose…</option>${(host?.jobs || []).map((j) => html`<option value=${j.id}>${j.id}</option>`)}</select></label>
      <a class="button primary" href=${fqdn && job ? link('agents', fqdn, 'jobs', job) : '#/restores'} disabled=${!(fqdn && job)}>Choose a snapshot</a>
    </div>
    <${ErrorBox} error=${r.error} />
    ${!r.data ? html`<${Loading} />` : html`
      ${r.data.queued.length > 0 && html`<h2>Queued</h2><table>
        <thead><tr><th>Asked</th><th>Agent · job</th><th>By</th><th>Expires</th></tr></thead>
        <tbody>${r.data.queued.map((c) => html`<tr><td>${fmtTime(c.created_at)}</td><td>${c.fqdn} · ${c.job_id}</td><td>${c.created_by}</td><td>${fmtTime(c.expires_at)}</td></tr>`)}</tbody></table>`}
      <h2>History</h2>
      ${r.data.done.length === 0 ? html`<p class="muted">No restores yet.</p>` : html`<table>
        <thead><tr><th>When</th><th>Agent · job</th><th>Snapshot</th><th>What → where</th><th>Result</th></tr></thead>
        <tbody>${r.data.done.map((e) => {
          const d = e.detail || {};
          const db = !!d.database || isPG(jobOf(e));
          return html`<tr>
          <td>${fmtTime(e.finished)}</td><td>${e.fqdn} · ${e.job_id}</td>
          <td>${d.snapshot && html`<${Snap} fqdn=${e.fqdn} id=${d.snapshot} />`}</td>
          <td><span class="kindline">${db && dbIcon(13)}<span class="wrap">${d.database ? (d.in_place === 'true' ? 'the dump, in place' : 'the dump, into a new database') : db ? 'the dump file' : (d.target || '')}${intoText(d) && intoText(d) !== d.target ? html` → <code>${intoText(d)}</code>` : ''}${d.dry_run === 'true' ? ' (dry run)' : ''}</span></span></td>
          <td><${Pill} kind=${e.status === 'success' ? 'ok' : 'error'}>${e.status}<//> <span class="wrap">${resultText(e).split(' · ').slice(1).join(' · ')}</span></td>
        </tr>`;
        })}</tbody></table>`}`}`;
}
