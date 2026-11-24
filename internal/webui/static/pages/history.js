import { html, useState, useEffect, usePoll, useAction, link, go, fmtTime, fmtDay, retentionText, Pill, Snap, ErrorBox, Loading, Paged } from '../lib.js';
import { api } from '../api.js';

function Compare({ revs }) {
  const [from, setFrom] = useState(revs.length > 1 ? revs[1].number : 0);
  const [to, setTo] = useState(revs.length ? revs[0].number : 0);
  const d = usePoll(() => (from && to ? api.diff(from, to) : Promise.resolve(null)), [from, to], 0);
  return html`<div class="panel"><h3>Compare</h3>
    <div class="row">From <select value=${from} onChange=${(e) => setFrom(+e.target.value)}>${revs.map((r) => html`<option value=${r.number}>r${r.number}</option>`)}</select>
      to <select value=${to} onChange=${(e) => setTo(+e.target.value)}>${revs.map((r) => html`<option value=${r.number}>r${r.number}</option>`)}</select></div>
    <${ErrorBox} error=${d.error} />
    ${d.data && (d.data.changes.length === 0 ? html`<p class="muted">(no differences you can see)</p>` : html`<pre>${d.data.changes.map((c) =>
      (c.from !== undefined ? `- ${c.path}: ${JSON.stringify(c.from)}\n` : '') + (c.to !== undefined ? `+ ${c.path}: ${JSON.stringify(c.to)}` : '')).join('\n')}</pre>`)}
  </div>`;
}

export function History({ me, hosts }) {
  const cuts = usePoll(() => api.retentionCuts(), [], 60000);
  const revs = usePoll(() => api.revisions(100), []);
  const prunes = usePoll(() => api.prunes(false), []);
  const act = useAction();
  const host = (f) => hosts.find((h) => h.fqdn === f);
  const may = (f, p) => (host(f)?.actions || []).includes(p);
  return html`<h1>Config history</h1>
    <p class="muted">Every save is a revision. There is deliberately no rollback: to undo, edit the job and save, which passes the same checks and the same 7-day protection as any change.</p>
    <h2>Retention cuts waiting out the 7 days · ${cuts.data?.length ?? '…'}</h2>
    <p class="muted">Nothing that a retention in force during the last 7 days keeps is forgotten. Prune now skips the wait, for an admin, after a second confirmation.</p>
    <${ErrorBox} error=${cuts.error || act.error} />
    ${cuts.data && (cuts.data.length === 0 ? html`<p class="muted">Nothing is waiting.</p>` : cuts.data.map((c) => html`<div class="banner warning">
      <strong>${c.fqdn} · ${c.job_id}</strong>: ${retentionText(c.from)} → ${retentionText(c.to)}. Cut in r${c.revision} by ${c.changed_by}, ${fmtTime(c.changed_at)}; what it drops may go from ${fmtTime(c.prunable_from)}.
      <div class="row" style="margin-top:.4rem">
        <a class="button small" href=${link('agents', c.fqdn, 'pending')}>What waits</a>
        ${may(c.fqdn, 'config.jobs') && html`<button class="small" onClick=${() => act.run(() => api.keepLonger(c.fqdn, c.job_id, {})).then(cuts.reload)}>Keep the old retention</button>`}
        ${may(c.fqdn, 'retention.cut') && html`<a class="button small" href=${link('agents', c.fqdn, 'prune-now')}>Prune now…</a>`}
      </div></div>`))}
    <h2>Revisions</h2>
    <${ErrorBox} error=${revs.error} />
    ${!revs.data ? html`<${Loading} />` : html`<${Paged} items=${revs.data} render=${(rows) => html`<table>
      <thead><tr><th>Revision</th><th>When</th><th>Who</th><th>Message</th><th>Accepted</th></tr></thead>
      <tbody>${rows.map((r, i) => html`<tr><td>r${r.number}${i === 0 && r.number === revs.data[0].number ? html` <${Pill}>current<//>` : ''}</td><td>${fmtTime(r.created_at)}</td><td>${r.actor}</td>
        <td>${r.message}</td><td>${(r.accepted_codes || []).join(', ')}</td></tr>`)}</tbody></table>`} />
      <${Compare} revs=${revs.data} />`}
    <h2>Past prunes</h2>
    <${ErrorBox} error=${prunes.error} />
    ${prunes.data && (prunes.data.length === 0 ? html`<p class="muted">Nothing was forgotten or pruned yet.</p>` : html`<table>
      <thead><tr><th>Started</th><th>Agent</th><th>Kind</th><th>Who</th><th>Status</th><th>Forgotten</th><th>Freed</th><th>Really gone by</th></tr></thead>
      <tbody>${prunes.data.map((p) => html`<tr><td>${fmtTime(p.started_at)}</td><td>${p.fqdn}</td><td>${p.kind.replace('_', ' ')}</td><td>${p.actor}</td>
        <td><${Pill} kind=${p.status === 'success' ? 'ok' : p.status === 'failed' ? 'error' : ''}>${p.status}<//>${p.error ? ' ' + p.error : ''}</td>
        <td>${p.forgotten.length}</td><td>${p.pruned ? p.freed : '—'}</td><td>${fmtDay(p.really_gone_by)}</td></tr>`)}</tbody></table>`)}`;
}

export function PruneNow({ fqdn }) {
  const plan = usePoll(() => api.pruneNowPlan(fqdn), [fqdn], 0);
  const [typed, setTyped] = useState('');
  const [msg, setMsg] = useState('');
  const [month, setMonth] = useState('');
  const [shown, setShown] = useState(false);
  const act = useAction();
  const [run, setRun] = useState(null);
  useEffect(() => {
    if (!act.done) return;
    const t = setInterval(async () => {
      const list = await api.hostPrunes(fqdn, true).catch(() => []);
      const r = list.find((x) => x.id === act.done.run);
      if (r && r.status !== 'running') { setRun(r); clearInterval(t); }
    }, 2000);
    return () => clearInterval(t);
  }, [act.done]);
  if (plan.error) return html`<${ErrorBox} error=${plan.error} />`;
  if (!plan.data) return html`<${Loading} what="what Prune now would delete (restic is asked)" />`;
  const p = plan.data;
  const months = [...new Set(p.snapshots.map((s) => fmtTime(s.time).slice(0, 7)))];
  const list = p.snapshots.filter((s) => !month || fmtTime(s.time).startsWith(month));
  return html`<h1>Delete ${p.count} snapshots now?</h1>
    <div class="banner error">This skips the 7-day protection. ${p.count} snapshots of ${fqdn}${p.count ? `, taken between ${fmtTime(p.from)} and ${fmtTime(p.to)},` : ''} are forgotten at once, and the repository is pruned.
      ${p.kept} snapshots stay. Holdfast cannot bring them back; S3's old versions of their data stay until about ${fmtDay(p.really_gone_by)}, and only the storage's owners can recover them.</div>
    ${p.not_made_by_holdfast > 0 && html`<p class="muted">${p.not_made_by_holdfast} snapshots that Holdfast did not make are left alone.</p>`}
    <p>${p.snapshots.slice(0, 6).map((s) => html`<${Snap} fqdn=${fqdn} id=${s.id} short=${s.short_id} /> `)}${p.count > 6 ? `… and ${p.count - 6} more` : ''}</p>
    ${p.count > 0 && html`<button class="small" onClick=${() => setShown(!shown)}>${shown ? 'Hide the list' : `Show all ${p.count} snapshots`}</button>`}
    ${shown && html`<div class="chips">${['', ...months].map((m) => html`<span class=${'chip ' + (month === m ? 'on' : '')} onClick=${() => setMonth(m)}>${m || 'All'}</span>`)}</div>
      <${Paged} items=${list} render=${(rows) => html`<table><thead><tr><th>Snapshot</th><th>Taken</th><th>Job</th></tr></thead>
        <tbody>${rows.map((s) => html`<tr><td><${Snap} fqdn=${fqdn} id=${s.id} short=${s.short_id} /></td><td>${fmtTime(s.time)}</td><td>${s.job_id}</td></tr>`)}</tbody></table>`} />
      <p class="muted">Same list from the CLI: <code>holdfast prune-now ${fqdn} --json</code></p>`}
    <h3>The restic commands the server runs, with its maintenance key</h3>
    <pre>${p.commands.join('\n')}</pre>
    <label class="field"><span>Why (audit log)</span><input value=${msg} onInput=${(e) => setMsg(e.target.value)} /></label>
    <label class="field"><span>Type ${p.confirm} to confirm</span><input value=${typed} onInput=${(e) => setTyped(e.target.value)} /></label>
    ${typed && typed !== p.confirm && html`<p class="muted">Type ${p.confirm} exactly to confirm.</p>`}
    <div class="row">
      <button class="danger" disabled=${typed !== p.confirm || act.busy || act.done} onClick=${() => act.run(() => api.pruneNow(fqdn, { confirm: typed, message: msg }))}>Prune ${p.count} snapshots now</button>
      <a class="button" href=${link('agents', fqdn, 'pending')}>Keep the old retention instead</a>
      <button onClick=${() => history.back()}>Cancel</button>
    </div>
    <${ErrorBox} error=${act.error} />
    ${act.done && !run && html`<div class="banner info">Running (run ${act.done.run})…</div>`}
    ${run && html`<div class=${'banner ' + (run.status === 'success' ? 'ok' : 'error')}>Run ${run.id} ${run.status}: ${run.forgotten.length} snapshots forgotten${run.pruned ? `, pruned, ${run.freed} freed` : ''}${run.error ? '; ' + run.error : ''}.</div>`}`;
}

export { go };
