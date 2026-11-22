import { html, useState, usePoll, useAction, link, go, ago, fmtTime, fmtDay, dur, bytes, scheduleText, retentionText, Pill, Snap, can, ErrorBox, Loading, CopyButton } from '../lib.js';
import { api } from '../api.js';
import { jobState } from './fleet.js';

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
      <pre>${act.done.install_command}</pre><${CopyButton} text=${act.done.install_command} />`}
    <p><button class="small" onClick=${done}>Close</button></p>
  </div>`;
}

// In a vault only: the read-only key for restores from it (decision 039). Hosts have no S3 key (decision 041).
function KeyPanel({ host, done }) {
  const [id, setID] = useState('');
  const [secret, setSecret] = useState('');
  const [msg, setMsg] = useState('');
  const act = useAction();
  return html`<div class="panel">
    <h3>The read-only key for restores from this site</h3>
    <p class="muted">A key that may only read <code>repos/${host.fqdn}/</code> in this site's bucket, made at its S3 server (docs/storage.md). The vault gives it, with the repository's password, to the host's agent for each restore from here. Holdfast tests it before it keeps it; a key that fails is refused, and the old one stays.</p>
    <label class="field"><span>Access key ID</span><input value=${id} onInput=${(e) => setID(e.target.value)} autocomplete="off" /></label>
    <label class="field"><span>Secret access key</span><input type="password" value=${secret} onInput=${(e) => setSecret(e.target.value)} autocomplete="new-password" /></label>
    <label class="field"><span>Why (audit log)</span><input value=${msg} onInput=${(e) => setMsg(e.target.value)} /></label>
    <div class="row">
      <button class="primary" disabled=${act.busy || !id || !secret} onClick=${() => act.run(() => api.setS3Key(host.fqdn, { s3_key: { access_key_id: id, secret_access_key: secret }, message: msg }))}>Test and replace</button>
      <button onClick=${done}>Close</button>
    </div>
    <${ErrorBox} error=${act.error} />
    ${act.done && html`<div class="banner ok">The new key passed every storage check and is credentials version ${act.done.credentials_version}. Restores from this site can be granted now.</div>`}
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
      <pre>${r.enrollment.install_command}</pre><${CopyButton} text=${r.enrollment.install_command} />
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

// The repository in the bucket: its size, the escrow file, and a decommission.
function Repository({ host }) {
  const s = host.size, d = host.decommission, esc = host.escrow || {};
  return html`<dl class="facts">
    <dt>Repository</dt><dd><code>repos/${host.fqdn}</code>${s ? ` · ${bytes(s.current_bytes)} in ${s.objects} objects, and ${bytes(s.noncurrent_bytes)} in old versions S3 keeps · measured ${ago(s.measured_at)}` : ' · not measured yet'}</dd>
    <dt>Escrow</dt><dd>${esc.current ? `the bucket's escrow file holds the current password (written ${fmtTime(esc.written_at)})` : esc.error ? `not written: ${esc.error}` : 'not written'}</dd>
    ${host.storage?.object_lock && html`<dt>Object Lock</dt><dd>${host.storage.object_lock} <span class="muted">· reported, not required; Holdfast never extends a lock</span></dd>`}
    ${d && html`<dt>Decommission</dt><dd>${d.done_at
      ? `done ${fmtTime(d.done_at)}: ${d.objects_deleted} objects deleted, as ${d.requested_by} asked. The data is really gone ${d.really_gone_at ? fmtDay(d.really_gone_at) : 'when the lifecycle rule expires the old versions'}.`
      : `asked by ${d.requested_by}; the repository is deleted ${fmtTime(d.due_at)}`}</dd>`}
  </dl>`;
}

// A vault's job (decision 039): copied from the source, with its own off-site retention.
function VaultJobFacts({ host, job }) {
  const s = job.status || {};
  return html`<dl class="facts">
    <dt>From the source</dt><dd>${job.dump ? html`dump <span class="mono">${job.dump}</span>` : html`<span class="mono">${(job.paths || []).join('  ')}</span>`} · ${scheduleText(job.schedule)}</dd>
    <dt>Off-site retention</dt><dd>${retentionText(job.retention)}</dd>
    <dt>Primary retention 🔒</dt><dd>${job.primary_retention ? retentionText(job.primary_retention) : '—'} <span class="muted">· set in the source</span></dd>
    <dt>Copies</dt><dd>${s.last_success_at ? 'last copy ' + fmtTime(s.last_success_at) : 'none yet'}${job.behind_s != null ? ` · ${job.behind_s ? dur(job.behind_s + 's') + ' behind the source' : 'up to date'}` : ''}</dd>
    <dt>Newest copy</dt><dd>${s.last_snapshot_id ? html`<${Snap} fqdn=${host.fqdn} id=${s.last_snapshot_id} />` : '—'}</dd>
  </dl>`;
}

function JobCard({ host, job, cuts, onDone, vault }) {
  const act = useAction();
  const st = jobState(host, job);
  const cut = cuts.find((c) => c.fqdn === host.fqdn && c.job_id === job.id);
  const s = job.status || {};
  return html`<div class="job">
    <div class="head">
      <div><strong>${job.id}</strong> <${Pill} kind=${st.kind}>${st.text}<//>
        ${job.excluded && html` <${Pill} kind="warning">excluded: not copied<//>`}${job.retired && html` <${Pill}>retired in the source<//>`}</div>
      <div class="row">
        ${!vault && can(host, 'backup.run') && html`<button class="small" disabled=${act.busy} onClick=${() => act.run(() => api.runNow(host.fqdn, job.id))}>Back up now</button>`}
        <a class="button small" href=${link('hosts', host.fqdn, 'jobs', job.id, 'snapshots')}>${vault ? 'Copies' : 'Snapshots'}</a>
        ${!vault && can(host, 'restore.new_dir') && s.last_snapshot_id && html`<a class="button small" href=${link('hosts', host.fqdn, 'snapshots', s.last_snapshot_id, 'restore')}>Restore from latest…</a>`}
        ${can(host, 'config.jobs') && html`<a class="button small" href=${link('hosts', host.fqdn, 'configure', job.id)}>${vault ? 'Off-site retention' : 'Edit'}</a>`}
        ${vault && can(host, 'config.jobs') && !job.retired && html`<button class="small" disabled=${act.busy} onClick=${() => act.run(() => (job.excluded ? api.include : api.exclude)(host.fqdn, job.id, {})).then(onDone)}>${job.excluded ? 'Include' : 'Exclude'}</button>`}
        ${vault && can(host, 'config.jobs') && job.primary_retention && html`<button class="small" disabled=${act.busy} onClick=${() => act.run(() => api.matchPrimary(host.fqdn, job.id, {})).then(onDone)}>Match primary</button>`}
        ${can(host, 'host.remove') && (!vault || job.retired) && html`<button class="small" onClick=${() => {
          if (confirm(`Remove job ${job.id} from ${host.fqdn}? Its snapshots stay; the daily retention no longer touches them.`)) act.run(() => api.removeJob(host.fqdn, job.id, {})).then(onDone);
        }}>Remove job</button>`}
      </div>
    </div>
    ${vault ? html`<${VaultJobFacts} host=${host} job=${job} />` : html`<dl class="facts">
      ${job.dump ? html`<dt>Dump</dt><dd><span class="mono">${job.dump}</span> <span class="muted">· defined in the host's agent.yaml</span></dd>`
        : html`<dt>Paths</dt><dd class="mono">${(job.paths || []).join('  ')}${job.excludes?.length ? html` <span class="muted">· excludes ${job.excludes.join(' ')}</span>` : ''}</dd>`}
      <dt>Schedule</dt><dd>${scheduleText(job.schedule)} · longest gap ${dur(job.longest_gap)} · stale after ${dur(job.stale_after)}</dd>
      <dt>Retention</dt><dd>${retentionText(job.retention)}</dd>
      <dt>Agent reported</dt><dd>${s.last_success_at ? 'last success ' + fmtTime(s.last_success_at) : 'no success yet'}${s.last_status && s.last_status !== 'success' ? ` · last run ${s.last_status}${s.last_error ? ': ' + s.last_error : ''}` : ''}</dd>
      <dt>Newest snapshot</dt><dd>${s.last_snapshot_id ? html`<${Snap} fqdn=${host.fqdn} id=${s.last_snapshot_id} />` : '—'}</dd>
      <dt>Next run</dt><dd>${fmtTime(job.next_runs?.[0])}</dd>
    </dl>`}
    ${act.done && act.done.findings && act.done.findings.length > 0 && html`<div class="banner info">Saved as revision ${act.done.revision}: ${act.done.findings.map((f) => f.code + ' ' + f.message).join('; ')}</div>`}
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

// What each vault reports of the host (decision 039): read-only here, set in the vault.
function Offsite({ host, onRestore }) {
  return html`<h2>Off-site copies 🔒</h2>
    <p class="muted">Reported by each vault on its own syncs; its retention is set in the vault. While a vault has not copied a snapshot, the daily run here keeps it for up to 7 days.</p>
    ${host.offsite.map((o) => html`<div class="panel">
      <div class="spread"><strong>${o.vault}</strong>
        ${can(host, 'restore.new_dir') && o.copies > 0 && html`<button class="small" onClick=${() => onRestore(o.vault)}>Restore from ${o.vault}…</button>`}</div>
      <dl class="facts">
        <dt>Copies</dt><dd>${o.copies}${o.pending ? ` · ${o.pending} waiting to be copied` : ''} · last copy ${o.last_copy ? fmtTime(o.last_copy) : 'none yet'} · reported ${o.reported_at ? ago(o.reported_at) : 'never'}</dd>
        ${o.copy_error && html`<dt>Copy error</dt><dd><${Pill} kind="error">${o.copy_error}<//></dd>`}
      </dl>
      <table><thead><tr><th>Job</th><th>Off-site retention</th><th>Copies</th><th>Newest copy</th><th>Behind</th><th></th></tr></thead>
        <tbody>${o.jobs.map((j) => html`<tr><td>${j.id}</td><td>${retentionText(j.retention)}${j.shorter_than_primary ? html` <${Pill} kind="warning">keeps less than primary (O101)<//>` : ''}</td>
          <td>${j.copies}</td><td>${j.newest_copy_at ? fmtTime(j.newest_copy_at) : '—'}</td><td>${j.behind_s ? dur(j.behind_s + 's') : '—'}</td>
          <td>${j.excluded ? 'excluded' : j.retired ? 'retired' : ''}</td></tr>`)}</tbody></table>
    </div>`)}`;
}

// A restore of the host's own snapshot from a vault's copy, in the other site. The vault grants it on its next sync.
function OffsiteRestore({ host, vault, done }) {
  const copies = usePoll(() => api.offsiteCopies(host.fqdn), [host.fqdn], 0);
  const [pick, setPick] = useState('');
  const [target, setTarget] = useState('new');
  const [dir, setDir] = useState('/srv/recovered');
  const [reason, setReason] = useState('');
  const act = useAction();
  const list = ((copies.data || []).find((c) => c.vault === vault) || { copies: [] }).copies;
  return html`<div class="panel">
    <h3>Restore from ${vault}</h3>
    <p class="muted">The copy is restored by this host's agent, from the other site's repository, with a read-only key the vault gives for this one restore. Into a new directory, or another one; never in place.</p>
    <${ErrorBox} error=${copies.error} />
    <label class="field"><span>Copy</span><select value=${pick} onChange=${(e) => setPick(e.target.value)}>
      <option value="">choose…</option>${list.map((c) => html`<option value=${c.id}>${fmtTime(c.time)} · ${c.job_id} · copy ${c.id.slice(0, 8)} of ${c.original.slice(0, 8)}</option>`)}</select></label>
    <label class="field"><input type="radio" checked=${target === 'new'} onChange=${() => setTarget('new')} /> A new restore directory, root-only</label>
    ${can(host, 'restore.in_place') && html`<label class="field"><input type="radio" checked=${target === 'directory'} onChange=${() => setTarget('directory')} /> Another directory:
      <input value=${dir} onInput=${(e) => setDir(e.target.value)} disabled=${target !== 'directory'} /></label>`}
    <label class="field"><span>Reason (audit log)</span><input value=${reason} onInput=${(e) => setReason(e.target.value)} /></label>
    <div class="row">
      <button class="primary" disabled=${act.busy || !pick} onClick=${() => act.run(() => api.restore(host.fqdn, pick, { from_vault: vault, target, directory: target === 'directory' ? dir : undefined, message: reason || undefined }))}>Ask ${vault} for the restore</button>
      <button onClick=${done}>Close</button>
    </div>
    <${ErrorBox} error=${act.error} />
    ${act.done && html`<div class="banner info">Asked as restore ${act.done.command}. ${vault} grants it on its next sync; then the agent runs it. Restores shows its result.</div>`}
  </div>`;
}

export function Host({ fqdn, reloadHosts, vault }) {
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
        <p class="muted">${vault ? `copied from the source · last copy ${h.copied_at ? fmtTime(h.copied_at) : 'none yet'}`
          : h.agent ? `${h.agent.os_id} ${h.agent.os_version} · agent seen ${ago(h.agent.last_seen_at)} · restic ${h.agent.restic_version} · runs revision ${h.agent.applied_revision} (current: ${h.desired_revision})` : `${h.state.replace('_', ' ')} · progress: ${h.progress}`}</p>
      </div>
      <div class="row">
        ${!vault && can(h, 'config.jobs') && h.state !== 'retired' && html`<a class="button" href=${link('hosts', h.fqdn, 'configure')}>Add job</a>`}
        <a class="button" href=${link('hosts', h.fqdn, 'pending')}>Pending</a>
        ${can(h, 'retention.cut') && html`<button onClick=${() => act.run(() => api.retentionRun(h.fqdn))}>Run retention now</button>`}
        ${can(h, 'retention.cut') && html`<a class="button" href=${link('hosts', h.fqdn, 'prune-now')}>Prune now…</a>`}
        ${!vault && can(h, 'config.storage') && html`<button onClick=${() => setPanel('enroll')}>New enrollment code</button>`}
        ${vault && can(h, 'config.storage') && html`<button onClick=${() => setPanel('key')}>Read-only key for restores…</button>`}
        ${!vault && can(h, 'config.storage') && h.state !== 'retired' && html`<button onClick=${() => setPanel('rotate')}>Rotate password…</button>`}
        ${can(h, 'repository.rollback') && !(h.decommission && h.decommission.done_at) && html`<a class="button" href=${link('hosts', h.fqdn, 'rollback')}>Roll back…</a>`}
        ${can(h, 'host.remove') && h.state !== 'retired' && !h.removed_at && (!vault || h.jobs.every((j) => j.retired)) && html`<button onClick=${() => {
          if (confirm(`Remove ${h.fqdn}? Its agent stops backing up; its repository and snapshots stay, and adding it again brings it back.`))
            act.run(() => api.removeHost(h.fqdn, {})).then(refresh);
        }}>Remove host…</button>`}
        ${can(h, 'host.decommission') && h.state === 'retired' && !h.decommission && html`<button class="danger" onClick=${() => setPanel('decommission')}>Decommission…</button>`}
        ${can(h, 'host.decommission') && h.decommission && !h.decommission.done_at && html`<button onClick=${() => act.run(() => api.cancelDecommission(h.fqdn, {})).then(refresh)}>Cancel decommission</button>`}
      </div>
    </div>
    <${ErrorBox} error=${act.error} />
    ${act.done && act.done.run && html`<div class="banner ok">The daily retention run started (run ${act.done.run}). Config history lists past prunes.</div>`}
    ${panel === 'enroll' && html`<${EnrollPanel} host=${h} done=${() => setPanel('')} />`}
    ${panel === 'key' && html`<${KeyPanel} host=${h} done=${() => setPanel('')} />`}
    ${panel === 'rotate' && html`<${RotatePanel} host=${h} done=${() => { setPanel(''); refresh(); }} />`}
    ${panel === 'decommission' && html`<${DecommissionPanel} host=${h} done=${() => { setPanel(''); refresh(); }} />`}
    ${panel.startsWith('offsite:') && html`<${OffsiteRestore} host=${h} vault=${panel.slice(8)} done=${() => setPanel('')} />`}
    <${Banners} host=${h} onAck=${() => act.run(() => api.acknowledge(h.fqdn)).then(refresh)} />
    <${Repository} host=${h} />
    <h2>${h.jobs.length} ${vault ? 'copied jobs' : 'backup jobs'}</h2>
    <p class="muted">${vault ? html`Copies of the source's snapshots, in this site's repository for the host: <code>repos/${h.fqdn}</code>, under the off-site retention.`
      : html`One repository per host, shared by its jobs: <code>repos/${h.fqdn}</code>. They run one at a time.`}</p>
    ${h.jobs.map((j) => html`<${JobCard} key=${j.id} host=${h} job=${j} cuts=${cuts.data || []} onDone=${refresh} vault=${vault} />`)}
    ${!vault && h.offsite && h.offsite.length > 0 && html`<${Offsite} host=${h} onRestore=${(v) => setPanel('offsite:' + v)} />`}
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
        <td>${s.reason === 'deletion' ? `deletion asked by ${s.requested_by}` : s.reason === 'offsite_copy' ? 'a vault has not copied it yet' : `retention cut; revision ${s.kept_by_revision} still keeps it`}</td>
        <td>${fmtTime(s.prunable_at)}</td><td>${fmtDay(s.really_gone_by)}</td>
        <td>${s.reason === 'deletion' && can(host.data, 'snapshot.delete') && html`<button class="small" onClick=${() => act.run(() => api.cancelDeletion(fqdn, s.id)).then(p.reload)}>Cancel deletion</button>`}
          ${s.reason === 'retention_cut' && can(host.data, 'config.jobs') && html`<button class="small" onClick=${() => act.run(() => api.keepLonger(fqdn, s.job_id, { revision: s.kept_by_revision })).then(p.reload)}>Keep longer</button>`}</td>
      </tr>`)}</tbody></table>`}
    ${d.jobs.filter((j) => j.not_made_by_holdfast.length).map((j) => html`<p class="muted">Job ${j.job_id}: ${j.not_made_by_holdfast.length} snapshots that Holdfast did not make are left alone.</p>`)}
    <p><a href=${link('hosts', fqdn)}>Back to ${fqdn}</a></p>`;
}

export { go };
