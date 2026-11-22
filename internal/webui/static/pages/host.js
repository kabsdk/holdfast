import { html, useState, usePoll, useAction, link, go, ago, fmtTime, fmtDay, dur, scheduleText, retentionText, Pill, Snap, can, ErrorBox, Loading, CopyButton } from '../lib.js';
import { api } from '../api.js';
import { jobState } from './fleet.js';

function Banners({ host, onAck }) {
  const errs = (host.problems || []).filter((p) => !p.job_id);
  return html`${errs.map((p) => html`<div class=${'banner ' + p.severity}>
      <code>${p.code}</code> ${p.message}
      ${(p.code === 'disappeared' || p.code === 'foreign_keys') && can(host, 'maintenance') &&
        html` <button class="small" onClick=${onAck}>I have looked into it: clear the alert</button>`}
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
      <pre>${act.done.install_command}</pre><${CopyButton} text=${act.done.install_command} />`}
    <p><button class="small" onClick=${done}>Close</button></p>
  </div>`;
}

function KeyPanel({ host, done }) {
  const [id, setID] = useState('');
  const [secret, setSecret] = useState('');
  const [msg, setMsg] = useState('');
  const act = useAction();
  return html`<div class="panel">
    <h3>Replace the S3 key</h3>
    <p class="muted">Make the key at your S3 server first (docs/storage.md). Holdfast tests it before it keeps it; a key that fails is refused, and the old one stays.</p>
    <label class="field"><span>Access key ID</span><input value=${id} onInput=${(e) => setID(e.target.value)} autocomplete="off" /></label>
    <label class="field"><span>Secret access key</span><input type="password" value=${secret} onInput=${(e) => setSecret(e.target.value)} autocomplete="new-password" /></label>
    <label class="field"><span>Why (audit log)</span><input value=${msg} onInput=${(e) => setMsg(e.target.value)} /></label>
    <div class="row">
      <button class="primary" disabled=${act.busy || !id || !secret} onClick=${() => act.run(() => api.setS3Key(host.fqdn, { s3_key: { access_key_id: id, secret_access_key: secret }, message: msg }))}>Test and replace</button>
      <button onClick=${done}>Close</button>
    </div>
    <${ErrorBox} error=${act.error} />
    ${act.done && html`<div class="banner ok">The new key passed every storage check and is credentials version ${act.done.credentials_version}. The agent gets it on its next poll. Export a new escrow bundle on the server: the last one has the old key.</div>`}
  </div>`;
}

function JobCard({ host, job, cuts, onDone }) {
  const act = useAction();
  const st = jobState(host, job);
  const cut = cuts.find((c) => c.fqdn === host.fqdn && c.job_id === job.id);
  const s = job.status || {};
  return html`<div class="job">
    <div class="head">
      <div><strong>${job.id}</strong> <${Pill} kind=${st.kind}>${st.text}<//></div>
      <div class="row">
        ${can(host, 'backup.run') && html`<button class="small" disabled=${act.busy} onClick=${() => act.run(() => api.runNow(host.fqdn, job.id))}>Back up now</button>`}
        <a class="button small" href=${link('hosts', host.fqdn, 'jobs', job.id, 'snapshots')}>Snapshots</a>
        ${can(host, 'restore.new_dir') && s.last_snapshot_id && html`<a class="button small" href=${link('hosts', host.fqdn, 'snapshots', s.last_snapshot_id, 'restore')}>Restore from latest…</a>`}
        ${can(host, 'config.jobs') && html`<a class="button small" href=${link('hosts', host.fqdn, 'configure', job.id)}>Edit</a>`}
        ${can(host, 'host.remove') && html`<button class="small" onClick=${() => {
          if (confirm(`Remove job ${job.id} from ${host.fqdn}? Its snapshots stay; the daily retention no longer touches them.`)) act.run(() => api.removeJob(host.fqdn, job.id, {})).then(onDone);
        }}>Remove job</button>`}
      </div>
    </div>
    <dl class="facts">
      <dt>Paths</dt><dd class="mono">${job.paths.join('  ')}${job.excludes?.length ? html` <span class="muted">· excludes ${job.excludes.join(' ')}</span>` : ''}</dd>
      <dt>Schedule</dt><dd>${scheduleText(job.schedule)} · longest gap ${dur(job.longest_gap)} · stale after ${dur(job.stale_after)}</dd>
      <dt>Retention</dt><dd>${retentionText(job.retention)}</dd>
      <dt>Agent reported</dt><dd>${s.last_success_at ? 'last success ' + fmtTime(s.last_success_at) : 'no success yet'}${s.last_status && s.last_status !== 'success' ? ` · last run ${s.last_status}${s.last_error ? ': ' + s.last_error : ''}` : ''}</dd>
      <dt>Newest snapshot</dt><dd>${s.last_snapshot_id ? html`<${Snap} fqdn=${host.fqdn} id=${s.last_snapshot_id} />` : '—'}</dd>
      <dt>Next run</dt><dd>${fmtTime(job.next_runs?.[0])}</dd>
    </dl>
    ${cut && html`<div class="banner warning">Retention cut pending: what the new retention drops stays until ${fmtTime(cut.prunable_from)}, the 7-day protection.
      <div class="row" style="margin-top:.4rem">
        ${can(host, 'config.jobs') && html`<button class="small" onClick=${() => act.run(() => api.keepLonger(host.fqdn, job.id, {})).then(onDone)}>Keep the old retention</button>`}
        <a class="button small" href=${link('hosts', host.fqdn, 'pending')}>What waits</a>
        ${can(host, 'retention.cut') && html`<a class="button small" href=${link('hosts', host.fqdn, 'prune-now')}>Prune now…</a>`}
      </div></div>`}
    <${ErrorBox} error=${act.error} />
    ${act.done && act.done.command && html`<div class="banner ok">Queued: it starts after any running job. Its result appears in the runs below.</div>`}
  </div>`;
}

export function Host({ fqdn, reloadHosts }) {
  const host = usePoll(() => api.host(fqdn), [fqdn]);
  const events = usePoll(() => api.hostEvents(fqdn, 50), [fqdn]);
  const cuts = usePoll(() => api.retentionCuts(), [fqdn], 60000);
  const [panel, setPanel] = useState('');
  const act = useAction();
  if (host.error) return html`<${ErrorBox} error=${host.error} />`;
  if (!host.data) return html`<${Loading} what=${fqdn} />`;
  const h = host.data;
  const refresh = () => { host.reload(); events.reload(); reloadHosts(); };
  const errors = (h.problems || []).filter((p) => p.severity === 'error').length;
  return html`
    <div class="spread">
      <div>
        <h1>${h.fqdn} <${Pill} kind=${errors ? 'error' : (h.problems || []).length ? 'warning' : 'ok'}>${errors ? `${errors} problems` : (h.problems || []).length ? 'to check' : `all ${h.jobs.length} jobs OK`}<//></h1>
        <p class="muted">${h.agent ? `${h.agent.os_id} ${h.agent.os_version} · agent seen ${ago(h.agent.last_seen_at)} · restic ${h.agent.restic_version} · runs revision ${h.agent.applied_revision} (current: ${h.desired_revision})` : `${h.state.replace('_', ' ')} · progress: ${h.progress}`}</p>
      </div>
      <div class="row">
        ${can(h, 'config.jobs') && h.state !== 'retired' && html`<a class="button" href=${link('hosts', h.fqdn, 'configure')}>Add job</a>`}
        <a class="button" href=${link('hosts', h.fqdn, 'pending')}>Pending</a>
        ${can(h, 'retention.cut') && html`<button onClick=${() => act.run(() => api.retentionRun(h.fqdn))}>Run retention now</button>`}
        ${can(h, 'retention.cut') && html`<a class="button" href=${link('hosts', h.fqdn, 'prune-now')}>Prune now…</a>`}
        ${can(h, 'config.storage') && html`<button onClick=${() => setPanel('enroll')}>New enrollment code</button>`}
        ${can(h, 'config.storage') && html`<button onClick=${() => setPanel('key')}>Replace S3 key</button>`}
        ${can(h, 'host.remove') && h.state !== 'retired' && !h.removed_at && html`<button onClick=${() => {
          if (confirm(`Remove ${h.fqdn}? Its agent stops backing up; its repository and snapshots stay, and adding it again brings it back.`))
            act.run(() => api.removeHost(h.fqdn, {})).then(refresh);
        }}>Remove host…</button>`}
      </div>
    </div>
    <${ErrorBox} error=${act.error} />
    ${act.done && act.done.run && html`<div class="banner ok">The daily retention run started (run ${act.done.run}). Config history lists past prunes.</div>`}
    ${panel === 'enroll' && html`<${EnrollPanel} host=${h} done=${() => setPanel('')} />`}
    ${panel === 'key' && html`<${KeyPanel} host=${h} done=${() => setPanel('')} />`}
    <${Banners} host=${h} onAck=${() => act.run(() => api.acknowledge(h.fqdn)).then(refresh)} />
    <h2>${h.jobs.length} backup jobs</h2>
    <p class="muted">One repository per host, shared by its jobs: <code>repos/${h.fqdn}</code>. They run one at a time.</p>
    ${h.jobs.map((j) => html`<${JobCard} key=${j.id} host=${h} job=${j} cuts=${cuts.data || []} onDone=${refresh} />`)}
    <h2>Recent runs · all jobs</h2>
    ${events.data ? html`<table>
      <thead><tr><th>Finished</th><th>Job</th><th>Kind</th><th>Status</th><th>Duration</th><th>Snapshot</th><th>Details</th><th>Started by</th></tr></thead>
      <tbody>${events.data.map((e) => html`<tr>
        <td>${fmtTime(e.finished)}</td><td>${e.job_id}</td><td>${e.kind.replace('_', ' ')}${e.command_id && e.kind === 'backup' ? ' (manual)' : ''}</td>
        <td><${Pill} kind=${e.status === 'success' ? 'ok' : e.status === 'warning' ? 'warning' : e.status === 'skipped' ? '' : 'error'}>${e.status}<//></td>
        <td>${Math.max(0, Math.round((new Date(e.finished) - new Date(e.started)) / 1000))} s</td>
        <td>${e.snapshot_id && html`<${Snap} fqdn=${h.fqdn} id=${e.snapshot_id} />`}</td>
        <td class="muted">${e.error || (e.detail && e.detail.directory ? 'into ' + e.detail.directory : '')}</td>
        <td>${e.initiated_by || 'schedule'}</td>
      </tr>`)}</tbody></table>` : html`<${Loading} what="runs" />`}`;
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
        <td>${s.reason === 'deletion' ? `deletion asked by ${s.requested_by}` : `retention cut; revision ${s.kept_by_revision} still keeps it`}</td>
        <td>${fmtTime(s.prunable_at)}</td><td>${fmtDay(s.really_gone_by)}</td>
        <td>${s.reason === 'deletion' && can(host.data, 'snapshot.delete') && html`<button class="small" onClick=${() => act.run(() => api.cancelDeletion(fqdn, s.id)).then(p.reload)}>Cancel deletion</button>`}
          ${s.reason !== 'deletion' && can(host.data, 'config.jobs') && html`<button class="small" onClick=${() => act.run(() => api.keepLonger(fqdn, s.job_id, { revision: s.kept_by_revision })).then(p.reload)}>Keep longer</button>`}</td>
      </tr>`)}</tbody></table>`}
    ${d.jobs.filter((j) => j.not_made_by_holdfast.length).map((j) => html`<p class="muted">Job ${j.job_id}: ${j.not_made_by_holdfast.length} snapshots that Holdfast did not make are left alone.</p>`)}
    <p><a href=${link('hosts', fqdn)}>Back to ${fqdn}</a></p>`;
}

export { go };
