import { html, useState, usePoll, useAction, link, offsiteLink, go, ago, until, fmtTime, fmtDay, dur, bytes, retentionText, scheduleText, Pill, Snap, can, ErrorBox, Loading, EnrollLines, Menu, Problem, icons, fullDate, clock, localDay, browserZone } from '../lib.js';
import { api } from '../api.js';
import { jobState } from './overview.js';

function Banners({ host, onAck }) {
  const errs = (host.problems || []).filter((p) => !p.job_id);
  return html`${errs.map((p) => html`<div class=${'banner ' + p.severity}>
      <code>${p.code}</code> ${p.message}
      ${(p.code === 'disappeared' || p.code === 'foreign_keys') && can(host, 'maintenance') &&
        html` <button class="small" onClick=${onAck}>I have looked into it: clear the alert</button>`}
      ${p.code === 'disappeared' && can(host, 'repository.rollback') &&
        html` <a class="button small" href=${link('hosts', host.fqdn, 'rollback')}>Bring them back from S3's versions…</a>`}
    </div>`)}`;
}

function EnrollPanel({ host, done }) {
  const act = useAction();
  return html`<div class="panel">
    <h3>New enrollment code</h3>
    <p class="muted">Revokes the open one. The code is valid for 15 minutes, once.</p>
    <button disabled=${act.busy} onClick=${() => act.run(() => api.enrollmentCode(host.fqdn))}>Issue a code</button>
    <${ErrorBox} error=${act.error} />
    ${act.done && html`<p>Code <strong class="mono">${act.done.code}</strong>, valid until ${fmtTime(act.done.expires_at)}. Run on ${host.fqdn}, as root:</p>
      <${EnrollLines} e=${act.done} />`}
    <p><button class="small" onClick=${done}>Close</button></p>
  </div>`;
}

// After a compromise (docs/runbooks/compromised-host.md). The server does every step; this only asks.
function RotatePanel({ host, done }) {
  const [msg, setMsg] = useState('');
  const act = useAction();
  const r = act.done;
  return html`<div class="panel">
    <h3>Rotate the repository password</h3>
    <p class="muted">For a host you think is compromised. Holdfast adds a new restic password to the repository, removes the old one, unpins the agent's certificate and issues a new enrollment code. The host backs up again only once it is clean and enrolled again.</p>
    <label class="field"><span>Why (audit log)</span><input value=${msg} onInput=${(e) => setMsg(e.target.value)} /></label>
    <div class="row">
      <button class="danger" disabled=${act.busy || !!r} onClick=${() => act.run(() => api.rotate(host.fqdn, { message: msg }))}>Rotate now</button>
      <button onClick=${done}>Close</button>
    </div>
    <${ErrorBox} error=${act.error} />
    ${r && html`<div class="banner ok">New password: key <code>${r.new_key}</code>, credentials version ${r.credentials_version}; removed key ${r.removed_keys.join(', ') || '—'}. ${r.escrowed ? 'The escrow file holds the new password.' : ''}</div>
      ${r.warning && html`<div class="banner warning">${r.warning}</div>`}
      <p>Once the host is clean or rebuilt, enroll it with code <strong class="mono">${r.enrollment.code}</strong>, valid until ${fmtTime(r.enrollment.expires_at)}. As root on the host:</p>
      <${EnrollLines} e=${r.enrollment} />
      <p class="muted">Enrolling makes the host a new key of its own; the old certificate no longer works.</p>`}
  </div>`;
}

function DecommissionPanel({ host, done }) {
  const [typed, setTyped] = useState('');
  const [msg, setMsg] = useState('');
  const act = useAction();
  return html`<div class="panel">
    <h3>Decommission ${host.fqdn}</h3>
    <p>This deletes the host's repository and its escrow file, <strong>7 days</strong> from now. Until then it can be cancelled. Afterwards, S3 keeps the old versions until the bucket's lifecycle rule expires them${host.storage?.noncurrent_days ? ` (${host.storage.noncurrent_days} days)` : ''}; then the data is really gone.</p>
    <label class="field"><span>Type the FQDN to confirm</span><input value=${typed} onInput=${(e) => setTyped(e.target.value)} autocomplete="off" /></label>
    <label class="field"><span>Why (audit log)</span><input value=${msg} onInput=${(e) => setMsg(e.target.value)} /></label>
    <div class="row">
      <button class="danger" disabled=${act.busy || typed !== host.fqdn} onClick=${() => act.run(() => api.decommission(host.fqdn, { confirm: typed, message: msg })).then(done)}>Decommission after 7 days</button>
      <button onClick=${done}>Close</button>
    </div>
    <${ErrorBox} error=${act.error} />
  </div>`;
}

// The repository in the bucket, on one line: its size, the escrow file, Object Lock and a decommission.
function Repository({ host }) {
  const s = host.size, d = host.decommission, esc = host.escrow || {};
  return html`<section class="factline" aria-label="Repository">
    <span><span class="muted">Repository</span> <code>repos/${host.fqdn}</code>${s ? ` · ${bytes(s.current_bytes)}, and ${bytes(s.noncurrent_bytes)} of old versions S3 keeps` : ' · not measured yet'}</span>
    <span class="row" style="gap:.35rem"><span class="muted">Escrow</span>${esc.current ? html`${icons.ok()} current password` : esc.error ? html`<span class="num error">not written: ${esc.error}</span>` : 'not written'}</span>
    ${host.storage?.object_lock && html`<span><span class="muted">Object Lock</span> ${host.storage.object_lock}</span>`}
    ${host.offsite && html`<span><span class="muted">Off-site</span> ${host.offsite.copies} copies${host.offsite.pending ? `, ${host.offsite.pending} waiting` : ''} · last copy ${host.offsite.last_copy ? ago(host.offsite.last_copy) : 'none yet'}</span>`}
    ${s && html`<span class="muted">measured ${ago(s.measured_at)}</span>`}
    ${d && html`<span><span class="muted">Decommission</span> ${d.done_at
      ? `done ${fmtTime(d.done_at)}: ${d.objects_deleted} objects deleted, as ${d.requested_by} asked; really gone ${d.really_gone_at ? fmtDay(d.really_gone_at) : 'when the lifecycle rule expires the old versions'}`
      : `asked by ${d.requested_by}; the repository is deleted ${fmtTime(d.due_at)}`}</span>`}
  </section>`;
}

// offState is a job's off-site copies in a word or two, for its row: up to date, copying, behind, failing, or not
// copied (decisions 039, 053). Lag and failure are the server's problems; this only words them.
export function offState(host, job) {
  const o = host.offsite;
  const oj = o && o.jobs.find((x) => x.id === job.id);
  if (!oj) return { text: 'not copied yet', kind: 'muted' };
  const sub = oj.own_retention ? 'its own retention' : '';
  if (oj.excluded) return { text: 'not copied', kind: 'muted', sub: oj.copies ? `${oj.copies} copies kept` : '' };
  if (o.copy_error) return { text: 'copy failing', kind: 'error', sub };
  if ((host.problems || []).some((p) => p.code === 'offsite_lag' && p.job_id === job.id)) return { text: `${dur(oj.behind_s + 's')} behind`, kind: 'error', sub };
  return { text: oj.behind_s ? 'copying' : 'up to date', kind: oj.behind_s ? 'muted' : 'ok', sub };
}

// A restore of the host's own snapshot from its off-site copy. The host reads the copy through the gateway, for this
// restore only.
function OffsiteRestore({ host, done }) {
  const copies = usePoll(() => api.offsiteCopies(host.fqdn), [host.fqdn], 0);
  const [pick, setPick] = useState('');
  const [target, setTarget] = useState('new');
  const [dir, setDir] = useState('/srv/recovered');
  const [reason, setReason] = useState('');
  const act = useAction();
  const list = copies.data || [];
  return html`<div class="panel">
    <h3>Restore from off-site</h3>
    <p class="muted">The copy is restored by this host's agent, which reads its off-site repository through this server for this one restore. Into a new directory, or another one; never in place.</p>
    <${ErrorBox} error=${copies.error} />
    <label class="field"><span>Copy</span><select value=${pick} onChange=${(e) => setPick(e.target.value)}>
      <option value="">choose…</option>${list.map((c) => html`<option value=${c.id}>${fmtTime(c.time)} · ${c.job_id} · copy ${c.id.slice(0, 8)} of ${c.original.slice(0, 8)}</option>`)}</select></label>
    <label class="field"><input type="radio" checked=${target === 'new'} onChange=${() => setTarget('new')} /> A new restore directory, root-only</label>
    ${can(host, 'restore.in_place') && html`<label class="field"><input type="radio" checked=${target === 'directory'} onChange=${() => setTarget('directory')} /> Another directory:
      <input value=${dir} onInput=${(e) => setDir(e.target.value)} disabled=${target !== 'directory'} /></label>`}
    <label class="field"><span>Reason (audit log)</span><input value=${reason} onInput=${(e) => setReason(e.target.value)} /></label>
    <div class="row">
      <button class="primary" disabled=${act.busy || !pick} onClick=${() => act.run(() => api.restore(host.fqdn, pick, { from_offsite: true, target, directory: target === 'directory' ? dir : undefined, message: reason || undefined }))}>Restore the copy</button>
      <button onClick=${done}>Close</button>
    </div>
    <${ErrorBox} error=${act.error} />
    ${act.done && html`<div class="banner info">Queued as restore ${act.done.command}; the agent picks it up on its next poll. Restores shows its result.</div>`}
  </div>`;
}

// StateBadge is a job's state beside its name: a tick when it is fine, else what is wrong.
export function StateBadge({ host, job }) {
  const s = jobState(host, job);
  if (s.kind === 'ok') return icons.ok();
  return html`<${Pill} kind=${s.kind}>${s.text}<//>`;
}

const DAY_TEXT = { success: 'backed up', warning: 'incomplete: some files could not be read', failed: 'failed' };

// Strip is a job's last 14 days, a square a day, coloured by the worst run of the day (spec §5.1a).
function Strip({ job, days }) {
  if (!days) return html`<div class="strip" aria-hidden="true">${Array.from({ length: 14 }, () => html`<span class="d-wait"></span>`)}</div>`;
  const today = new Date().toDateString();
  const tip = (d) => {
    const day = localDay(d.date);
    if (d.before) return `${fullDate(day)}\nBefore the job’s first backup`;
    if (d.runs === 0) return `${fullDate(day)}\n${day.toDateString() === today ? 'No backup yet today' : 'No backup ran'}`;
    const n = d.runs === 1 ? '1 backup' : `${d.runs} backups`;
    const parts = [d.success && `${d.success} backed up`, d.warning && `${d.warning} incomplete`, d.failed && `${d.failed} failed`].filter(Boolean);
    return `${fullDate(day)}\n${n}: ${d.runs === 1 ? DAY_TEXT[d.worst] : parts.join(', ')}`;
  };
  const cls = (d) => (d.before ? 'd-before' : d.runs === 0 ? (localDay(d.date).toDateString() === today ? 'd-later' : 'd-none') : 'd-' + d.worst);
  return html`<div class="strip" role="img" aria-label=${job + ', the last 14 days: ' + days.days.map((d) => tip(d).replace('\n', ': ')).join('; ')}>
    ${days.days.map((d) => html`<span class=${cls(d)} data-tip=${tip(d)}></span>`)}</div>`;
}

function JobRow({ host, job, days, cut, onRun, queued, vault, cols }) {
  const probs = (host.problems || []).filter((p) => p.job_id === job.id);
  const s = job.status || {};
  const off = !vault && host.offsite ? offState(host, job) : null;
  return html`<div class=${'gt' + (probs.some((p) => p.severity === 'error') ? ' bad' : '')} style=${'--cols:' + cols}>
    <div style="min-width:0">
      <div class="row" style="gap:.5rem"><a href=${link('hosts', host.fqdn, 'jobs', job.id)}><b>${job.id}</b></a><${StateBadge} host=${host} job=${job} />
        ${job.excluded && html`<${Pill} kind="warning">not copied<//>`}${job.retired && html`<${Pill}>removed in primary<//>`}
        ${cut && html`<a class="pill warning" href=${link('hosts', host.fqdn, 'jobs', job.id)}>retention cut pending</a>`}
        <span class="sub">${scheduleText(job.schedule)}</span></div>
      ${probs.map((p) => html`<${Problem} p=${{ ...p, job_id: '' }} />`)}
    </div>
    <${Strip} job=${job.id} days=${days} />
    <div><div class="num">${s.last_success_at ? ago(s.last_success_at) : '—'}</div>${s.last_success_at && html`<div class="sub">${fmtTime(s.last_success_at)}</div>`}</div>
    <span class="num muted">${job.next_runs?.[0] ? until(job.next_runs[0]) : '—'}</span>
    ${off && html`<div><div class=${'offstate ' + off.kind}>${off.text}</div>${off.sub && html`<div class="sub">${off.sub}</div>`}</div>`}
    <div class="row" style="justify-content:flex-end;gap:.4rem">
      ${queued && html`<span class="ok" style="font-size:12.5px">Queued</span>`}
      ${!vault && can(host, 'backup.run') && html`<button class="small" disabled=${queued} onClick=${onRun}>Back up now</button>`}
      ${!vault && can(host, 'restore.new_dir') && s.last_snapshot_id && html`<a class="button small" href=${link('hosts', host.fqdn, 'snapshots', s.last_snapshot_id, 'restore')}>Restore…</a>`}
    </div>
  </div>`;
}
const JOB_COLS = 'minmax(0,2.4fr) 225px minmax(0,1fr) minmax(0,.9fr) 210px';
const JOB_COLS_OFFSITE = 'minmax(0,2.2fr) 225px minmax(0,1fr) minmax(0,.8fr) minmax(0,1fr) 210px';

// Recent runs, folded: a job's backups on one day that ended the same way are one line, and so are a run and its
// retries. Restores, checks and the rest are a line each.
function fold(events) {
  const out = [];
  for (const e of events) {
    const day = new Date(e.finished).toDateString();
    const g = e.kind === 'backup' && !e.command_id && out.find((x) => x.fold && x.job === e.job_id && x.day === day && x.status === e.status);
    if (g) {
      g.n++;
      g.from = e.started < g.from ? e.started : g.from;
      continue;
    }
    out.push({ fold: e.kind === 'backup' && !e.command_id, job: e.job_id, day, status: e.status, n: 1, e, from: e.started });
  }
  return out;
}

function Runs({ host, events }) {
  const groups = fold(events).slice(0, 8);
  const cols = '170px 110px minmax(0,3fr) 100px 110px';
  const pill = (st) => (st === 'success' ? 'ok' : st === 'warning' ? 'warning' : st === 'skipped' ? '' : 'error');
  return html`<section class="list" aria-labelledby="runs-h">
    <div class="list-head"><h2 id="runs-h">Recent runs</h2></div>
    ${groups.length === 0 ? html`<p class="muted" style="padding:0 1rem 1rem">No run yet.</p>` : html`<div class="scrollx"><div style="min-width:760px">
      <div class="gt head" style=${'--cols:' + cols}><span>When</span><span>Job</span><span>Result</span><span>Snapshot</span><span>By</span></div>
      ${groups.map((g) => {
        const e = g.e;
        const what = e.kind === 'backup' ? (g.n > 1 ? `${g.n} backups` : 'backup') : e.kind.replace(/_/g, ' ');
        const detail = e.error || (e.detail && e.detail.directory ? 'into ' + e.detail.directory : '') || (g.n > 1 && e.status === 'success' ? 'all OK' : '');
        return html`<div class="gt" style=${'--cols:' + cols + ';align-items:baseline'}>
          <span class="num muted">${g.n > 1 ? `${fmtDay(e.finished)} ${clock(g.from)}–${clock(e.finished)}` : fmtTime(e.finished)}</span>
          <span>${e.job_id}</span>
          <span style="min-width:0"><${Pill} kind=${pill(e.status)}>${e.status === 'success' ? what : `${what} ${e.status}${g.n > 1 ? ' ×' + g.n : ''}`}<//> <span class="muted wrap">${detail}</span></span>
          <span>${e.snapshot_id ? html`<${Snap} fqdn=${host.fqdn} id=${e.snapshot_id} />` : html`<span class="muted">—</span>`}</span>
          <span class="muted">${e.initiated_by || 'schedule'}</span>
        </div>`;
      })}</div></div>`}
  </section>`;
}

export function Host({ fqdn, reloadHosts, vault }) {
  const host = usePoll(() => api.host(fqdn), [fqdn]);
  const events = usePoll(() => api.hostEvents(fqdn, 200), [fqdn]);
  const days = usePoll(() => api.hostDays(fqdn, 14, browserZone()), [fqdn], 60000);
  const cuts = usePoll(() => api.retentionCuts(), [fqdn], 60000);
  const [panel, setPanel] = useState('');
  const [queued, setQueued] = useState({});
  const act = useAction();
  if (host.error) return html`<${ErrorBox} error=${host.error} />`;
  if (!host.data) return html`<${Loading} what=${fqdn} />`;
  const h = host.data;
  const refresh = () => { host.reload(); events.reload(); days.reload(); reloadHosts(); };
  const probs = h.problems || [];
  const errors = probs.filter((p) => p.severity === 'error').length;
  const needy = new Set(probs.filter((p) => p.job_id).map((p) => p.job_id)).size;
  const run = (j) => act.run(() => api.runNow(h.fqdn, j.id)).then((r) => { if (r) setQueued({ ...queued, [j.id]: true }); });
  const withOff = !vault && !!h.offsite;
  const cols = withOff ? JOB_COLS_OFFSITE : JOB_COLS;
  return html`
    <div class="spread">
      <div>
        <h1 class="row" style="gap:.6rem">${h.fqdn}
          ${probs.length === 0 ? html`<${Pill} kind="ok">all ${h.jobs.length} jobs OK<//>`
            : html`<${Pill} kind=${errors ? 'error' : 'warning'}>${needy ? `${needy} ${needy === 1 ? 'job needs' : 'jobs need'} attention` : 'needs attention'}<//>`}</h1>
        <p class="muted">${vault ? `copied from primary · last copy ${h.copied_at ? fmtTime(h.copied_at) : 'none yet'}`
          : h.agent ? html`${h.agent.os_id} ${h.agent.os_version} · agent seen ${ago(h.agent.last_seen_at)} · restic ${h.agent.restic_version}${h.in_sync ? '' : html` · <${Pill} kind="warning">runs revision ${h.agent.applied_revision}, not ${h.desired_revision}<//>`}`
          : `${h.state.replace(/_/g, ' ')} · progress: ${h.progress}`}</p>
      </div>
      <div class="row">
        ${!vault && can(h, 'config.jobs') && h.state !== 'retired' && html`<a class="button" href=${link('hosts', h.fqdn, 'configure')}>Add job</a>`}
        <${Menu} label="More host actions" items=${[
          { label: 'Waiting to be deleted', href: link('hosts', h.fqdn, 'pending') },
          can(h, 'retention.cut') && { label: 'Run retention now', run: () => act.run(() => api.retentionRun(h.fqdn)) },
          can(h, 'retention.cut') && { label: 'Prune now…', href: link('hosts', h.fqdn, 'prune-now') },
          withOff && can(h, 'restore.new_dir') && h.offsite.copies > 0 && { label: 'Restore from off-site…', run: () => setPanel('offsite') },
          withOff && { label: 'Off-site pages', href: offsiteLink('hosts', h.fqdn) },
          '-',
          !vault && can(h, 'config.storage') && { label: 'New enrollment code', run: () => setPanel('enroll') },
          !vault && can(h, 'config.storage') && h.state !== 'retired' && { label: 'Rotate password…', run: () => setPanel('rotate') },
          can(h, 'repository.rollback') && !(h.decommission && h.decommission.done_at) && { label: 'Roll back…', href: link('hosts', h.fqdn, 'rollback') },
          '-',
          can(h, 'host.remove') && h.state !== 'retired' && !h.removed_at && (!vault || h.jobs.every((j) => j.retired)) && { label: 'Remove host…', danger: true, run: () => {
            if (confirm(`Remove ${h.fqdn}? Its agent stops backing up; its repository and snapshots stay, and adding it again brings it back.`))
              act.run(() => api.removeHost(h.fqdn, {})).then(refresh);
          } },
          can(h, 'host.decommission') && h.state === 'retired' && !h.decommission && { label: 'Decommission…', danger: true, run: () => setPanel('decommission') },
          can(h, 'host.decommission') && h.decommission && !h.decommission.done_at && { label: 'Cancel the decommission', run: () => act.run(() => api.cancelDecommission(h.fqdn, {})).then(refresh) },
        ]} />
      </div>
    </div>
    <${ErrorBox} error=${act.error} />
    ${act.done && act.done.run && html`<div class="banner ok">The daily retention run started (run ${act.done.run}). Config history lists past prunes.</div>`}
    ${panel === 'enroll' && html`<${EnrollPanel} host=${h} done=${() => setPanel('')} />`}
    ${panel === 'rotate' && html`<${RotatePanel} host=${h} done=${() => { setPanel(''); refresh(); }} />`}
    ${panel === 'decommission' && html`<${DecommissionPanel} host=${h} done=${() => { setPanel(''); refresh(); }} />`}
    ${panel === 'offsite' && html`<${OffsiteRestore} host=${h} done=${() => setPanel('')} />`}
    <${Banners} host=${h} onAck=${() => act.run(() => api.acknowledge(h.fqdn)).then(refresh)} />

    <section class="list" aria-labelledby="jobs-h">
      <div class="list-head"><h2 id="jobs-h">${vault ? 'Copied jobs' : 'Jobs'}</h2>
        ${days.error && html`<span class="muted">The last 14 days could not be read: ${days.error.message}</span>`}</div>
      ${h.jobs.length === 0 ? html`<p class="muted" style="padding:0 1rem 1rem">No job yet.${!vault && can(h, 'config.jobs') ? html` <a href=${link('hosts', h.fqdn, 'configure')}>Add one</a>.` : ''}</p>` : html`
      <div class="scrollx"><div style=${'min-width:' + (withOff ? 1020 : 900) + 'px'}>
        <div class="gt head" style=${'--cols:' + cols}><span>Job</span><span>Last 14 days</span><span>Last backup</span><span>Next</span>${withOff && html`<span>Off-site</span>`}<span></span></div>
        ${h.jobs.map((j) => html`<${JobRow} key=${j.id} host=${h} job=${j} vault=${vault} cols=${cols} queued=${!!queued[j.id]} onRun=${() => run(j)}
          days=${days.data && days.data.jobs.find((x) => x.job_id === j.id)} cut=${(cuts.data || []).some((c) => c.fqdn === h.fqdn && c.job_id === j.id)} />`)}
      </div></div>
      <div class="legend"><span><i class="d-success"></i>backed up</span><span><i class="d-warning"></i>incomplete</span><span><i class="d-failed"></i>failed</span>
        <span><i class="d-none"></i>no run</span><span><i class="d-later"></i>not yet today</span><span><i class="d-before"></i>before the first backup</span><span>One square a day; the worst run of the day sets its colour.</span></div>`}
    </section>
    ${events.data ? html`<${Runs} host=${h} events=${events.data} />` : html`<${Loading} what="runs" />`}
    <${Repository} host=${h} />`;
}

export function Pending({ fqdn }) {
  const p = usePoll(() => api.pending(fqdn), [fqdn], 60000);
  const host = usePoll(() => api.host(fqdn), [fqdn], 0);
  const act = useAction();
  if (p.error) return html`<${ErrorBox} error=${p.error} />`;
  if (!p.data) return html`<${Loading} what="what waits (restic is asked)" />`;
  const d = p.data;
  return html`<h1>Pending on ${fqdn}</h1>
    <p class="muted">Nothing a retention in force during the last 7 days keeps is forgotten (spec §3.4). These wait; the daily run forgets them from the date shown. S3 keeps their data's old versions for ${d.noncurrent_days || '?'} more days.</p>
    <${ErrorBox} error=${act.error} />
    ${d.forget.length > 0 && html`<div class="banner info">${d.forget.length} snapshots are past the 7 days and go at the next daily run.</div>`}
    ${d.pending.length === 0 ? html`<div class="banner ok">Nothing is pending.</div>` : html`<table>
      <thead><tr><th>Snapshot</th><th>Taken</th><th>Job</th><th>Why it waits</th><th>Forgotten from</th><th>Really gone by</th><th></th></tr></thead>
      <tbody>${d.pending.map((s) => html`<tr>
        <td><${Snap} fqdn=${fqdn} id=${s.id} short=${s.short_id} /></td><td>${fmtTime(s.time)}</td><td>${s.job_id}</td>
        <td>${s.reason === 'deletion' ? `deletion asked by ${s.requested_by}` : s.reason === 'offsite_copy' ? 'not copied off-site yet' : `retention cut; revision ${s.kept_by_revision} still keeps it`}</td>
        <td>${fmtTime(s.prunable_at)}</td><td>${fmtDay(s.really_gone_by)}</td>
        <td>${s.reason === 'deletion' && can(host.data, 'snapshot.delete') && html`<button class="small" onClick=${() => act.run(() => api.cancelDeletion(fqdn, s.id)).then(p.reload)}>Cancel deletion</button>`}
          ${s.reason === 'retention_cut' && can(host.data, 'config.jobs') && html`<button class="small" onClick=${() => act.run(() => api.keepLonger(fqdn, s.job_id, { revision: s.kept_by_revision })).then(p.reload)}>Keep longer</button>`}</td>
      </tr>`)}</tbody></table>`}
    ${d.jobs.filter((j) => j.not_made_by_holdfast.length).map((j) => html`<p class="muted">Job ${j.job_id}: ${j.not_made_by_holdfast.length} snapshots that Holdfast did not make are left alone.</p>`)}
    <p><a href=${link('hosts', fqdn)}>Back to ${fqdn}</a></p>`;
}

export { go };
