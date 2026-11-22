import { html, useState, link, ago, fmtTime, Pill, useAction, ErrorBox } from '../lib.js';
import { api } from '../api.js';

// jobState is how a job's pill reads: the server's problem for it, or OK.
export function jobState(host, job) {
  const p = (host.problems || []).find((x) => x.job_id === job.id);
  if (p) return { kind: p.severity, text: { stale: 'stale', failed: job.status?.last_status || 'failed', incomplete: 'incomplete' }[p.code] || p.code };
  if (!job.status) return { kind: '', text: host.state === 'enrolled' ? 'waiting for first backup' : 'not enrolled' };
  return { kind: 'ok', text: 'OK' };
}

export function Fleet({ me, hosts }) {
  const [filter, setFilter] = useState('attention');
  const check = useAction();
  const jobs = hosts.flatMap((h) => h.jobs.map((j) => ({ h, j, s: jobState(h, j) })));
  const count = (code) => hosts.flatMap((h) => h.problems || []).filter((p) => p.code === code).length;
  const attention = hosts.filter((h) => (h.problems || []).length > 0);
  const shown = filter === 'attention' ? attention : hosts;
  const canCheck = hosts.some((h) => (h.actions || []).includes('maintenance'));
  return html`
    <div class="spread">
      <div><h1>Fleet</h1>
        <p class="muted">${hosts.length} hosts · ${jobs.length} jobs. Problems are the server's own view; the storage witness is judged in Prometheus.</p></div>
      <div class="row">
        ${canCheck && html`<button disabled=${check.busy} onClick=${() => check.run(() => api.checkAll())}>Check all repositories now</button>`}
        ${me.add_hosts && html`<a class="button primary" href="#/add-host">Add host</a>`}
      </div>
    </div>
    <${ErrorBox} error=${check.error} />
    ${check.done && html`<div class="banner ok">Checks asked for on ${check.done.length} hosts. Each agent runs restic check on its next poll; its result is in the host's runs.</div>`}
    <div class="cards">
      <div class="card"><div class="muted">Jobs OK</div><div class="n">${jobs.filter((x) => x.s.kind === 'ok').length} / ${jobs.length}</div></div>
      <div class="card"><div class="muted">Stale</div><div class="n">${count('stale')}</div></div>
      <div class="card"><div class="muted">Failed or interrupted</div><div class="n">${count('failed')}</div></div>
      <div class="card"><div class="muted">Incomplete snapshots</div><div class="n">${count('incomplete')}</div></div>
    </div>
    <div class="chips">
      <span class=${'chip ' + (filter === 'attention' ? 'on' : '')} onClick=${() => setFilter('attention')}>Needs attention · ${attention.length}</span>
      <span class=${'chip ' + (filter === 'all' ? 'on' : '')} onClick=${() => setFilter('all')}>All hosts · ${hosts.length}</span>
    </div>
    ${shown.length === 0 ? html`<p class="muted">${filter === 'attention' ? 'Nothing needs attention.' : 'No hosts yet.'}</p>` : html`
    <table>
      <thead><tr><th>Host (FQDN)</th><th>Jobs · status</th><th>Last success (worst job)</th><th>Agent seen</th></tr></thead>
      <tbody>${shown.map((h) => {
        const worst = h.jobs.map((j) => j.status?.last_success_at).reduce((a, b) => (!a || (b && b < a) ? b : a), null);
        return html`<tr>
          <td><a href=${link('hosts', h.fqdn)}>${h.fqdn}</a><div class="muted">${h.agent ? `${h.agent.os_id} ${h.agent.os_version}` : h.state.replace('_', ' ')}</div></td>
          <td>${h.jobs.map((j) => { const s = jobState(h, j); return html`<${Pill} kind=${s.kind}>${j.id} · ${s.text}<//>`; })}
            ${(h.problems || []).filter((p) => !p.job_id).map((p) => html`<${Pill} kind=${p.severity}>${p.code.replace(/_/g, ' ')}<//>`)}</td>
          <td>${fmtTime(worst)}</td>
          <td>${h.agent ? ago(h.agent.last_seen_at) : '—'}</td>
        </tr>`;
      })}</tbody>
    </table>`}`;
}

export function Problems({ hosts }) {
  const all = hosts.flatMap((h) => (h.problems || []).map((p) => ({ ...p, fqdn: h.fqdn })));
  const order = { error: 0, warning: 1 };
  all.sort((a, b) => order[a.severity] - order[b.severity] || a.fqdn.localeCompare(b.fqdn));
  return html`<h1>Problems</h1>
    <p class="muted">What the server knows is wrong, on the hosts you can see. Paging is Prometheus's job: its alert rules ship with Holdfast.</p>
    ${all.length === 0 ? html`<div class="banner ok">No problems.</div>` : html`<table>
      <thead><tr><th>Severity</th><th>Host</th><th>Job</th><th>Problem</th><th></th></tr></thead>
      <tbody>${all.map((p) => html`<tr>
        <td><${Pill} kind=${p.severity}>${p.severity}<//></td><td>${p.fqdn}</td><td>${p.job_id || ''}</td>
        <td><code>${p.code}</code> ${p.message}</td><td><a href=${link('hosts', p.fqdn)}>Open host</a></td>
      </tr>`)}</tbody></table>`}`;
}
