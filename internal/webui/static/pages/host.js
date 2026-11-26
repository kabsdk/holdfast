// An agent's page (decision 056): its jobs with what each backs up and its last backups, its PostgreSQL connections, its
// recent runs and where its repository is. Every rule is the server's; this asks and shows.
import { html, useState, useEffect, usePoll, useAction, link, offsiteLink, go, ago, until, fmtTime, fmtDay, dur, bytes, scheduleText, Pill, Snap, can, ErrorBox, Loading, EnrollLines, Menu, Problem, icons, fullDate, clock, localDay, browserZone, isPG, Kind, dbIcon, repoPath, storedTip, SiteTag } from '../lib.js';
import { api } from '../api.js';
import { jobState, agentSub } from './overview.js';

function Banners({ host, onAck }) {
  const errs = (host.problems || []).filter((p) => !p.job_id);
  return html`${errs.map((p) => html`<div class=${'banner ' + p.severity}>
      <code>${p.code}</code> ${p.message}
      ${(p.code === 'disappeared' || p.code === 'foreign_keys') && can(host, 'maintenance') &&
        html` <button class="small" onClick=${onAck}>I have looked into it: clear the alert</button>`}
      ${p.code === 'disappeared' && can(host, 'repository.rollback') &&
        html` <a class="button small" href=${link('agents', host.fqdn, 'rollback')}>Bring them back from S3's versions…</a>`}
    </div>`)}`;
}

function EnrollPanel({ host, done }) {
  const act = useAction();
  return html`<div class="panel">
    <h3>New enrollment code</h3>
    <p class="muted">Revokes the open one. The code is valid for 15 minutes, once.</p>
    <button disabled=${act.busy} onClick=${() => act.run(() => api.enrollmentCode(host.fqdn))}>Issue a code</button>
    <${ErrorBox} error=${act.error} />
    ${act.done && html`<p>Code <strong class="mono">${act.done.code}</strong>, valid until ${fmtTime(act.done.expires_at)}. Run it where ${host.fqdn} runs, as root:</p>
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
    <p class="muted">For an agent you think is compromised. Holdfast adds a new restic password to the repository, removes the old one, unpins the agent's certificate and issues a new enrollment code. The agent backs up again only once it is clean and enrolled again.</p>
    <label class="field"><span>Why (audit log)</span><input value=${msg} onInput=${(e) => setMsg(e.target.value)} /></label>
    <div class="row">
      <button class="danger" disabled=${act.busy || !!r} onClick=${() => act.run(() => api.rotate(host.fqdn, { message: msg }))}>Rotate now</button>
      <button onClick=${done}>Close</button>
    </div>
    <${ErrorBox} error=${act.error} />
    ${r && html`<div class="banner ok">New password: key <code>${r.new_key}</code>, credentials version ${r.credentials_version}; removed key ${r.removed_keys.join(', ') || '—'}. ${r.escrowed ? 'The escrow file holds the new password.' : ''}</div>
      ${r.warning && html`<div class="banner warning">${r.warning}</div>`}
      <p>Once the agent's machine is clean or rebuilt, enroll it with code <strong class="mono">${r.enrollment.code}</strong>, valid until ${fmtTime(r.enrollment.expires_at)}. As root there:</p>
      <${EnrollLines} e=${r.enrollment} />
      <p class="muted">Enrolling makes the agent a new key of its own; the old certificate no longer works.</p>`}
  </div>`;
}

function DecommissionPanel({ host, done }) {
  const [typed, setTyped] = useState('');
  const [msg, setMsg] = useState('');
  const act = useAction();
  return html`<div class="panel">
    <h3>Decommission ${host.fqdn}</h3>
    <p>This deletes the agent's repository and its escrow file, <strong>7 days</strong> from now. Until then it can be cancelled. Afterwards, S3 keeps the old versions until the bucket's lifecycle rule expires them${host.storage?.noncurrent_days ? ` (${host.storage.noncurrent_days} days)` : ''}; then the data is really gone.</p>
    <label class="field"><span>Type the agent's name to confirm</span><input value=${typed} onInput=${(e) => setTyped(e.target.value)} autocomplete="off" /></label>
    <label class="field"><span>Why (audit log)</span><input value=${msg} onInput=${(e) => setMsg(e.target.value)} /></label>
    <div class="row">
      <button class="danger" disabled=${act.busy || typed !== host.fqdn} onClick=${() => act.run(() => api.decommission(host.fqdn, { confirm: typed, message: msg })).then(done)}>Decommission after 7 days</button>
      <button onClick=${done}>Close</button>
    </div>
    <${ErrorBox} error=${act.error} />
  </div>`;
}

// sizeText is a repository's size, and what S3 still keeps of what was deleted from it: what a rollback can bring back.
function sizeText(s, days) {
  if (!s) return html`<span class="muted">not measured yet</span>`;
  return html`<span data-tip=${'Measured ' + fmtTime(s.measured_at)}>${bytes(s.current_bytes)}, and ${bytes(s.noncurrent_bytes)} of deleted data S3 keeps${days ? ` for ${days} days` : ''}</span>`;
}

// The repository, a row per subject: where it is and its size, once for all the agent's jobs, in each site when there
// are two; the escrow file; Object Lock; a decommission.
function Repository({ host, settings, siteB, hostB }) {
  const d = host.decommission, esc = host.escrow || {}, o = host.offsite;
  return html`<dl class="facts box" aria-label="Repository">
    <dt>${o ? html`<${SiteTag} />` : 'Repository'}</dt><dd><code>${repoPath(settings?.storage, host.fqdn)}</code> · ${sizeText(host.size, host.storage?.noncurrent_days)}</dd>
    ${o && html`<dt><${SiteTag} b /></dt><dd><code>${repoPath(siteB?.storage, host.fqdn)}</code>${hostB?.size ? html` · ${sizeText(hostB.size, hostB.storage?.noncurrent_days)}` : ''}${` · ${o.copies} copies${o.pending ? `, ${o.pending} waiting` : ''}, the last ${o.last_copy ? ago(o.last_copy) : 'none yet'}`}</dd>`}
    <dt>Escrow</dt><dd>${esc.current ? html`${icons.ok()} current password` : esc.error ? html`<span class="num error">not written: ${esc.error}</span>` : 'not written'}</dd>
    ${host.storage?.object_lock && html`<dt>Object Lock</dt><dd>${host.storage.object_lock}</dd>`}
    ${d && html`<dt>Decommission</dt><dd>${d.done_at
      ? `done ${fmtTime(d.done_at)}: ${d.objects_deleted} objects deleted, as ${d.requested_by} asked; really gone ${d.really_gone_at ? fmtDay(d.really_gone_at) : 'when the lifecycle rule expires the old versions'}`
      : `asked by ${d.requested_by}; the repository is deleted ${fmtTime(d.due_at)}`}</dd>`}
  </dl>`;
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

// A restore of the agent's own snapshot from its off-site copy. The agent reads the copy through the gateway, for this
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
    <p class="muted">The copy is restored by this agent, which reads its off-site repository through this server for this one restore. Into a new directory, or another one; never in place.</p>
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

// What became of a slot, as the server says (spec §5.1a), and its square's colour: a backup's own, or none.
const SLOT_TEXT = { success: 'backed up', warning: 'incomplete: some files could not be read', failed: 'failed', missed: 'missed: no backup ran for it',
  skipped: 'skipped: the job’s previous run was still going', due: 'not run yet', before: 'before the job’s first backup' };
const SLOT_CLASS = { success: 'd-success', warning: 'd-warning', failed: 'd-failed', missed: 'd-none', skipped: 'd-skipped', due: 'd-later', before: 'd-before' };
const isoDay = (t) => { const d = new Date(t); return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`; };

// Slots is a job's last backups: a square per backup its schedule called for, oldest first, so a missed one shows and
// a weekly job's seven cover seven weeks. A square opens the job's page on its day.
function Slots({ fqdn, job, slots }) {
  if (!slots) return html`<div class="strip slots" aria-hidden="true">${Array.from({ length: 7 }, () => html`<span class="d-wait"></span>`)}</div>`;
  const tip = (s) => {
    const how = s.finished && s.state !== 'missed' ? `${SLOT_TEXT[s.state]} at ${clock(s.finished)}${s.unscheduled ? ', outside the schedule' : s.runs > 1 ? `, after ${s.runs} tries` : ''}` : SLOT_TEXT[s.state];
    return `${fullDate(s.at)} ${clock(s.at)}\n${how}${s.error ? ': ' + s.error : ''}`;
  };
  return html`<div class="strip slots" role="list" aria-label=${job + ', the last backups'}>${slots.map((s) => (s.state === 'before'
    ? html`<span role="listitem" class=${SLOT_CLASS[s.state]} data-tip=${tip(s)} aria-label=${tip(s).replace('\n', ': ')}></span>`
    : html`<a role="listitem" class=${SLOT_CLASS[s.state]} href=${link('agents', fqdn, 'jobs', job) + '?day=' + isoDay(s.finished || s.at)} data-tip=${tip(s)} aria-label=${tip(s).replace('\n', ': ')}></a>`))}</div>`;
}

function JobRow({ host, job, days, slots, cut, onRun, queued, vault, cols, storedB }) {
  const probs = (host.problems || []).filter((p) => p.job_id === job.id);
  const s = job.status || {};
  const off = !vault && host.offsite ? offState(host, job) : null;
  return html`<div class=${'gt' + (probs.some((p) => p.severity === 'error') ? ' bad' : '')} style=${'--cols:' + cols}>
    <div style="min-width:0">
      <div class="row" style="gap:.5rem"><a href=${link('agents', host.fqdn, 'jobs', job.id)}><b>${job.id}</b></a><${StateBadge} host=${host} job=${job} />
        ${job.excluded && html`<${Pill} kind="warning">not copied<//>`}${job.retired && html`<${Pill}>removed in primary<//>`}
        ${cut && html`<a class="pill warning" href=${link('agents', host.fqdn, 'jobs', job.id)}>retention cut pending</a>`}</div>
      <div class="sub">${scheduleText(job.schedule)}</div>
      ${probs.map((p) => html`<${Problem} p=${{ ...p, job_id: '' }} />`)}
    </div>
    <div style="min-width:0"><${Kind} job=${job} more=${link('agents', host.fqdn, 'jobs', job.id)} /></div>
    ${vault ? html`<${Strip} job=${job.id} days=${days} />` : html`<${Slots} fqdn=${host.fqdn} job=${job.id} slots=${slots} />`}
    <div><div class="num">${s.last_success_at ? ago(s.last_success_at) : '—'}</div>${s.last_success_at && html`<div class="sub">${fmtTime(s.last_success_at)}</div>`}</div>
    <span class="num muted">${job.next_runs?.[0] ? until(job.next_runs[0]) : '—'}</span>
    ${off && html`<div><div class=${'offstate ' + off.kind}>${off.text}</div>${off.sub && html`<div class="sub">${off.sub}</div>`}</div>`}
    <div class="stored"><div class="num nowrap" data-tip=${storedTip(job.stored, off ? 'site A' : '')}>${off ? html`<${SiteTag} /> ` : ''}${job.stored ? bytes(job.stored.bytes) : '—'}</div>
      ${off && html`<div class="num nowrap" data-tip=${storedTip(storedB, 'site B')}><${SiteTag} b /> ${storedB ? bytes(storedB.bytes) : '—'}</div>`}</div>
    <div class="row" style="justify-content:flex-end;gap:.4rem;flex-wrap:nowrap;white-space:nowrap">
      ${queued && html`<span class="ok" style="font-size:12.5px">Queued</span>`}
      ${!vault && can(host, 'backup.run') && html`<button class="small" disabled=${queued} onClick=${onRun}>Back up now</button>`}
      ${!vault && can(host, 'restore.new_dir') && s.last_snapshot_id && html`<a class="button small" href=${link('agents', host.fqdn, 'snapshots', s.last_snapshot_id, 'restore')}>Restore…</a>`}
    </div>
  </div>`;
}
// The squares' column: seven last backups, or on the off-site pages fourteen days of copies.
const jobCols = (off, vault) => (off ? `minmax(0,1.4fr) minmax(0,1.4fr) 130px minmax(0,1fr) minmax(0,.7fr) minmax(0,.9fr) minmax(0,.9fr) 210px`
  : `minmax(0,1.5fr) minmax(0,1.5fr) ${vault ? '225px' : '130px'} minmax(0,1fr) minmax(0,.8fr) minmax(0,.7fr) 210px`);

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

// checked says what a check read: the repository's structure, and the part of its data its read_data_subset names, a
// different one each day (spec §3.4a). An agent that does not say which read one part of the cycle's.
const ordinal = (n) => n + ([11, 12, 13].includes(n % 100) ? 'th' : ({ 1: 'st', 2: 'nd', 3: 'rd' }[n % 10] || 'th'));
function checked(e, cycle) {
  const m = /^(\d+)\/(\d+)$/.exec(e.detail?.read_data_subset || '');
  if (m) return `its structure, and data part ${m[1]} of ${m[2]} (all of it is read every ${m[2]} days)`;
  return cycle > 1 ? `its structure, and a ${ordinal(cycle)} of the data` : 'its structure, and all of the data';
}

function Runs({ host, events, cycle }) {
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
        const detail = e.error || (e.kind === 'check' ? 'sound: ' + checked(e, cycle) : '') || (e.detail && e.detail.directory ? 'into ' + e.detail.directory : '') || (e.detail && e.detail.database ? 'into database ' + e.detail.database : '') || (g.n > 1 && e.status === 'success' ? 'all OK' : '');
        return html`<div class="gt" style=${'--cols:' + cols + ';align-items:baseline'}>
          <span class="num muted">${g.n > 1 ? `${fmtDay(e.finished)} ${clock(g.from)}–${clock(e.finished)}` : fmtTime(e.finished)}</span>
          <span>${e.kind === 'check' ? html`<span class="muted">the repository</span>` : e.job_id}</span>
          <span style="min-width:0"><${Pill} kind=${pill(e.status)}>${e.status === 'success' ? what : `${what} ${e.status}${g.n > 1 ? ' ×' + g.n : ''}`}<//> <span class="muted wrap">${detail}</span></span>
          <span>${e.snapshot_id ? html`<${Snap} fqdn=${host.fqdn} id=${e.snapshot_id} />` : html`<span class="muted">—</span>`}</span>
          <span class="muted">${e.initiated_by || 'schedule'}</span>
        </div>`;
      })}</div></div>`}
  </section>`;
}

// Limits is what the agent's own agent.yaml allows, which only root where it runs can change.
function Limits({ host }) {
  const b = host.agent?.restore_bounds, pg = host.agent?.postgresql;
  if (!b && !pg) return null;
  const yes = (x) => (x ? 'allowed' : 'never');
  const parts = [];
  if (b) {
    parts.push(html`restores into <code>${b.root || '/var/restore'}</code>`);
    parts.push(`in place for files ${yes(b.allow_in_place)}`);
    parts.push(`into other folders ${yes(b.allow_other_directories)}`);
    parts.push(`restores of other agents' snapshots ${yes(b.allow_other_hosts)}`);
  }
  if (pg && pg.client) parts.push(`in place for PostgreSQL ${yes(pg.allow_in_place_restore)}`);
  return html`<p class="muted">Set in its agent.yaml, which only root where it runs can change: ${parts.map((p, i) => html`${i ? '; ' : ''}${p}`)}.</p>`;
}

/* ---------------- PostgreSQL connections (decision 055) ---------------- */

// useTest follows a connection test the server queued: until the agent reports it, the card says it is testing.
function useTest(fqdn, reload) {
  const [waiting, setWaiting] = useState({});
  useEffect(() => {
    if (!Object.keys(waiting).length) return undefined;
    const t = setInterval(reload, 2000);
    const stop = setTimeout(() => setWaiting({}), 120000);
    return () => { clearInterval(t); clearTimeout(stop); };
  }, [waiting, fqdn]);
  return [waiting, setWaiting];
}

function TestLine({ c, client }) {
  const t = c.last_test;
  if (!t) return html`<span class="sub">Not tested yet.</span>`;
  if (!t.ok) return html`<span class="prob error">${icons.error()}<span>Last test ${ago(t.at)}: ${t.error || t.result?.error || 'it failed'}</span></span>`;
  const r = t.result || {};
  const ru = r.restore_user;
  return html`<span class="sub">Last test ${ago(t.at)}: connected · PostgreSQL ${r.server_version || '?'} · ${r.tls ? r.tls : 'no TLS'} · ${(r.databases || []).length} databases${r.client || client ? ` · pg_dump ${r.client || client} on the agent` : ''}${ru ? (ru.error ? ` · restore user: ${ru.error}` : ` · restore user ${ru.createdb ? 'may create databases' : 'may not create databases'}${(ru.acts_as || []).length ? ' and act as ' + ru.acts_as.join(', ') : ''}`) : ''}</span>`;
}

// One connection: what it reaches, its last test, and its databases with the job that backs each up.
function ConnCard({ host, c, testing, onTest, onEdit }) {
  const jobs = host.jobs.filter((j) => isPG(j) && j.postgresql.connection === c.name);
  const jobFor = (db) => jobs.find((j) => (db === null ? j.postgresql.globals : j.postgresql.database === db));
  const dbs = c.last_test?.ok ? c.last_test.result?.databases || [] : null;
  const rows = dbs ? [...dbs.map((d) => ({ name: d.name, bytes: d.bytes, owner: d.owner })), { name: null }] : [];
  const nojob = rows.filter((r) => !jobFor(r.name));
  const add = (names) => link('agents', host.fqdn, 'configure') + '?' + new URLSearchParams({ connection: c.name, databases: names.join(',') }).toString();
  const mayAdd = can(host, 'config.jobs');
  const cols = 'minmax(170px,1fr) 90px minmax(110px,.8fr) minmax(150px,1fr) 110px';
  return html`<div class="conn">
    <div class="conn-head">
      <div style="min-width:0;flex:1 1 420px;display:flex;flex-direction:column;gap:.25rem">
        <b>${c.name} <span class="muted" style="font-weight:400;font-size:13px">· used by ${c.used_by.length} ${c.used_by.length === 1 ? 'job' : 'jobs'}</span></b>
        <code class="wrap">${c.url}</code>
        <span class="sub">Signs in as <code>${c.user}</code> · ${c.password_set ? 'password set, sealed' : 'no password: peer authentication on the socket'}${c.restore_user ? html` · restores as <code>${c.restore_user}</code>${c.restore_password_set ? ', password set, sealed' : ', no password'}` : ' · no restore user: dumps restore as files only'}</span>
        ${testing ? html`<span class="sub">Testing on the agent: it connects on its next poll…</span>` : html`<${TestLine} c=${c} client=${host.agent?.postgresql?.client} />`}
      </div>
      <div class="row">
        ${can(host, 'config.jobs') && html`<button class="small" disabled=${testing} onClick=${onTest}>${testing ? 'Testing…' : 'Test connection'}</button>`}
        ${can(host, 'config.storage') && html`<button class="small" onClick=${onEdit}>Edit…</button>`}
      </div>
    </div>
    ${dbs && html`<div class="scrollx"><div style="min-width:640px">
      <div class="gt head" style=${'--cols:' + cols}><span>Database, at the last test</span><span>Size</span><span>Owner</span><span>Job</span><span></span></div>
      ${rows.map((r) => {
        const j = jobFor(r.name);
        return html`<div class="gt" style=${'--cols:' + cols}>
          <span>${r.name === null ? html`<span class="muted">roles and other globals</span>` : html`<b>${r.name}</b>`}</span>
          <span class="num muted">${r.name === null ? '' : bytes(r.bytes)}</span><span class="muted">${r.owner || ''}</span>
          <span>${j ? html`<a href=${link('agents', host.fqdn, 'jobs', j.id)}>${j.id}</a> <${StateBadge} host=${host} job=${j} />` : html`<span class="warntext" data-tip="No job backs this up.">in no job</span>`}</span>
          <span>${!j && mayAdd && html`<a class="button small" href=${add([r.name === null ? ':roles' : r.name])}>Add job…</a>`}</span>
        </div>`;
      })}
      ${nojob.length > 1 && mayAdd && html`<div class="selbar"><span>${nojob.length} in no job.</span><a class="button small" href=${add(nojob.map((r) => (r.name === null ? ':roles' : r.name)))}>Add a job for each…</a>
        <span class="muted">one job per database, the same schedule and retention, saved as one revision</span></div>`}
    </div></div>`}
  </div>`;
}

// Add or change a connection. The passwords are write-only: left empty, the stored one stays.
function ConnDialog({ host, c, onClose, onSaved }) {
  const editing = !!c.name;
  const [f, setF] = useState({ name: c.name || '', url: c.url || '', user: c.user || '', pw: '', peer: editing && !c.password_set, ruser: c.restore_user || '', rpw: '', msg: '' });
  const act = useAction();
  const rm = useAction();
  const set = (k) => (e) => setF({ ...f, [k]: e.target.type === 'checkbox' ? e.target.checked : e.target.value });
  useEffect(() => {
    const esc = (e) => { if (e.key === 'Escape') onClose(); };
    document.addEventListener('keydown', esc);
    return () => document.removeEventListener('keydown', esc);
  }, []);
  const name = f.name.trim();
  const body = () => {
    const b = { url: f.url.trim(), user: f.user.trim(), restore_user: f.ruser.trim() || undefined, message: f.msg || undefined };
    if (f.peer) b.password = '';
    else if (f.pw) b.password = f.pw;
    if (f.rpw && f.ruser.trim()) b.restore_password = f.rpw;
    return b;
  };
  const save = () => act.run(() => api.setConnection(host.fqdn, name, body())).then((r) => r && onSaved(name, r.test_command));
  const remove = () => rm.run(() => api.removeConnection(host.fqdn, c.name, f.msg || undefined)).then((r) => r && onSaved(null, null));
  const q = (s) => (/^[A-Za-z0-9_./:=@,-]+$/.test(s) ? s : `'${s.replace(/'/g, `'\\''`)}'`);
  const cli = `holdfast agent connection set ${host.fqdn} ${name || '<name>'} --url ${q(f.url.trim() || '<url>')} --user ${f.user.trim() || '<user>'}`
    + (f.peer ? ' --no-password' : f.pw || !editing ? ' --password-file backup.pass' : '')
    + (f.ruser.trim() ? ` --restore-user ${f.ruser.trim()}${f.rpw || !editing || !c.restore_password_set ? ' --restore-password-file restore.pass' : ''}` : '');
  return html`<div class="scrim" onClick=${(e) => { if (e.target === e.currentTarget) onClose(); }}>
    <div class="dialog wide" role="dialog" aria-modal="true" aria-labelledby="cn-h">
      <h2 id="cn-h">${editing ? `Edit connection ${c.name}` : `Add a PostgreSQL connection to ${host.fqdn}`}</h2>
      <div class="formgrid">
        <label for="cn-n">Name</label><input id="cn-n" class="mono" value=${f.name} disabled=${editing} onInput=${set('name')} placeholder="postgres-main" />
        <label for="cn-u">Connection URL</label><input id="cn-u" class="mono" value=${f.url} onInput=${set('url')} placeholder="postgresql://db.example.internal:5432/?sslmode=verify-full&sslrootcert=/etc/holdfast/pg/ca.crt" />
        <span></span><span class="sub">libpq’s form, without a user, password or database: they have fields of their own. A socket is <code>postgresql:///?host=/var/run/postgresql</code>. Paths, such as sslrootcert, are where the agent runs.</span>
        <label for="cn-us">User</label><input id="cn-us" class="mono" value=${f.user} onInput=${set('user')} placeholder="holdfast_backup" />
        <label for="cn-p">Password</label>
        <span class="row"><input id="cn-p" type="password" value=${f.pw} disabled=${f.peer} onInput=${set('pw')} autocomplete="new-password"
            placeholder=${editing && c.password_set ? 'set · leave empty to keep it' : ''} style="flex:1 1 12rem" />
          <label class="row" style="gap:.3rem"><input type="checkbox" checked=${f.peer} onChange=${set('peer')} /> no password: peer authentication</label></span>
        <label for="cn-ru">Restore user</label><input id="cn-ru" class="mono" value=${f.ruser} onInput=${set('ruser')} placeholder="optional: without it, dumps restore as files only" />
        <label for="cn-rp">Restore password</label><input id="cn-rp" type="password" value=${f.rpw} disabled=${!f.ruser.trim()} onInput=${set('rpw')} autocomplete="new-password"
          placeholder=${editing && c.restore_password_set ? 'set · leave empty to keep it' : ''} />
        <label for="cn-m">Message</label><input id="cn-m" value=${f.msg} onInput=${set('msg')} placeholder=${`${host.fqdn}: ${editing ? 'change' : 'add'} connection ${name || '…'}`} />
      </div>
      <p class="sub">The passwords are sealed on the server, sent only to this agent, and never shown again. Saving makes a new revision, and the agent tests the connection at once: it connects as it will for a backup, and reports what it finds.</p>
      <pre>${cli}</pre>
      <${ErrorBox} error=${act.error || rm.error} />
      <div class="row" style="justify-content:flex-end">
        ${editing && html`<button class="danger-line" style="margin-right:auto" disabled=${rm.busy || c.used_by.length > 0} title=${c.used_by.length ? 'Jobs use it: remove them first' : ''} onClick=${remove}>Remove connection</button>`}
        <button onClick=${onClose}>Cancel</button>
        <button class="primary" disabled=${act.busy || !name} onClick=${save}>Save and test</button>
      </div>
    </div>
  </div>`;
}

function Connections({ host, reload }) {
  const conns = usePoll(() => api.connections(host.fqdn), [host.fqdn], 30000);
  const [dlg, setDlg] = useState(null);
  const refresh = () => { conns.reload(); reload(); };
  const [waiting, setWaiting] = useTest(host.fqdn, refresh);
  const act = useAction();
  const list = conns.data || host.postgresql || [];
  // A test is done once the connection's last test is the one queued.
  const testing = (c) => !!waiting[c.name] && c.last_test?.command !== waiting[c.name];
  useEffect(() => {
    const left = Object.fromEntries(Object.entries(waiting).filter(([n, id]) => !list.some((c) => c.name === n && c.last_test?.command === id)));
    if (Object.keys(left).length !== Object.keys(waiting).length) setWaiting(left);
  }, [conns.data]);
  return html`<section class="list" aria-labelledby="pg-h">
    <div class="list-head">
      <div style="display:flex;flex-direction:column;gap:.15rem;min-width:0;flex:1 1 30rem">
        <h2 id="pg-h" class="row" style="gap:.4rem">${dbIcon(16)} PostgreSQL connections</h2>
        <span class="sub">A connection holds the address and the credentials, once; every job of its databases uses them.${host.agent?.postgresql?.client ? ` pg_dump ${host.agent.postgresql.client} where the agent runs.` : ' The agent has no PostgreSQL client: root there sets postgresql.bin_dir in its agent.yaml.'}</span>
      </div>
      ${can(host, 'config.storage') && html`<button onClick=${() => setDlg({})}>Add connection…</button>`}
    </div>
    <${ErrorBox} error=${conns.error || act.error} />
    ${list.length === 0 && html`<p class="muted" style="padding:0 1rem 1rem">No connection yet.</p>`}
    ${list.map((c) => html`<${ConnCard} key=${c.name} host=${host} c=${c} testing=${testing(c)}
      onTest=${() => act.run(() => api.testConnection(host.fqdn, c.name)).then((r) => r && setWaiting({ ...waiting, [c.name]: r.command }))}
      onEdit=${() => setDlg(c)} />`)}
    ${dlg && html`<${ConnDialog} host=${host} c=${dlg} onClose=${() => setDlg(null)}
      onSaved=${(name, test) => { setDlg(null); if (name && test) setWaiting({ ...waiting, [name]: test }); refresh(); }} />`}
  </section>`;
}

export function Host({ fqdn, reloadHosts, vault, settings }) {
  const host = usePoll(() => api.host(fqdn), [fqdn]);
  const events = usePoll(() => api.hostEvents(fqdn, 200), [fqdn]);
  // The off-site pages draw their copies a day at a time; the server's pages each job's last backups (spec §5.1a).
  const days = usePoll(() => (vault ? api.hostDays(fqdn, 14, browserZone()) : Promise.resolve(null)), [fqdn, vault], 60000);
  const slots = usePoll(() => (vault ? Promise.resolve(null) : api.hostSlots(fqdn, 7)), [fqdn, vault], 60000);
  const cuts = usePoll(() => api.retentionCuts(), [fqdn], 60000);
  const withOff = !vault && !!host.data?.offsite;
  const siteB = usePoll(() => (withOff ? api.offsiteSettings() : Promise.resolve(null)), [withOff], 0);
  const hostB = usePoll(() => (withOff ? api.offsiteHost(fqdn) : Promise.resolve(null)), [fqdn, withOff], 60000); // site B's Stored
  const [panel, setPanel] = useState('');
  const [queued, setQueued] = useState({});
  const act = useAction();
  if (host.error) return html`<${ErrorBox} error=${host.error} />`;
  if (!host.data) return html`<${Loading} what=${fqdn} />`;
  const h = host.data;
  const refresh = () => { host.reload(); events.reload(); days.reload(); slots.reload(); reloadHosts(); };
  const probs = h.problems || [];
  const errors = probs.filter((p) => p.severity === 'error').length;
  const needy = new Set(probs.filter((p) => p.job_id).map((p) => p.job_id)).size;
  const run = (j) => act.run(() => api.runNow(h.fqdn, j.id)).then((r) => { if (r) setQueued({ ...queued, [j.id]: true }); });
  const cols = jobCols(withOff, vault);
  const a = h.agent;
  const withPG = !vault && (!!a?.postgresql?.client || (h.postgresql || []).length > 0);
  return html`
    <div class="spread">
      <div style="min-width:0;flex:1 1 32rem">
        <h1 class="row" style="gap:.6rem"><span class="wrap">${h.fqdn}</span>
          ${probs.length === 0 ? html`<${Pill} kind="ok">${h.jobs.length === 1 ? 'its job is OK' : `all ${h.jobs.length} jobs OK`}<//>`
            : html`<${Pill} kind=${errors ? 'error' : 'warning'}>${needy ? `${needy} ${needy === 1 ? 'job needs' : 'jobs need'} attention` : 'needs attention'}<//>`}</h1>
        ${vault ? html`<p class="muted">copied from primary · last copy ${h.copied_at ? fmtTime(h.copied_at) : 'none yet'}</p>`
          : a ? html`<dl class="facts head">
              <dt>Agent</dt><dd>seen ${ago(a.last_seen_at)}${a.remote_addr ? `, connects from ${a.remote_addr}` : ''}${h.in_sync ? '' : html` <${Pill} kind="warning">runs revision ${a.applied_revision}, not ${h.desired_revision}<//>`}</dd>
              <dt>Software</dt><dd>Holdfast agent ${a.agent_version} · restic ${a.restic_version} · ${a.os_id} ${a.os_version}${a.postgresql?.client ? ` · PostgreSQL client ${a.postgresql.client}` : ''}</dd></dl>`
          : html`<p class="muted">${agentSub(h)} · progress: ${h.progress}</p>`}
        ${!vault && html`<${Limits} host=${h} />`}
      </div>
      <div class="row">
        ${!vault && can(h, 'config.jobs') && h.state !== 'retired' && html`<a class="button" href=${link('agents', h.fqdn, 'configure')}>Add job</a>`}
        <${Menu} label="More agent actions" items=${[
          { label: 'Waiting to be deleted', href: link('agents', h.fqdn, 'pending') },
          can(h, 'retention.cut') && { label: 'Run retention now', run: () => act.run(() => api.retentionRun(h.fqdn)) },
          can(h, 'retention.cut') && { label: 'Prune now…', href: link('agents', h.fqdn, 'prune-now') },
          withOff && can(h, 'restore.new_dir') && h.offsite.copies > 0 && { label: 'Restore from off-site…', run: () => setPanel('offsite') },
          withOff && { label: 'Off-site pages', href: offsiteLink('agents', h.fqdn) },
          '-',
          !vault && can(h, 'config.storage') && { label: 'New enrollment code', run: () => setPanel('enroll') },
          !vault && can(h, 'config.storage') && h.state !== 'retired' && { label: 'Rotate password…', run: () => setPanel('rotate') },
          can(h, 'repository.rollback') && !(h.decommission && h.decommission.done_at) && { label: 'Roll back…', href: link('agents', h.fqdn, 'rollback') },
          '-',
          can(h, 'host.remove') && h.state !== 'retired' && !h.removed_at && (!vault || h.jobs.every((j) => j.retired)) && { label: 'Remove agent…', danger: true, run: () => {
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
        ${(days.error || slots.error) && html`<span class="muted">The ${vault ? 'last 14 days' : 'last backups'} could not be read: ${(days.error || slots.error).message}</span>`}</div>
      ${h.jobs.length === 0 ? html`<p class="muted" style="padding:0 1rem 1rem">No job yet.${!vault && can(h, 'config.jobs') ? html` <a href=${link('agents', h.fqdn, 'configure')}>Add one</a>.` : ''}</p>` : html`
      <div class="scrollx"><div style=${'min-width:' + (withOff ? 1060 : 880) + 'px'}>
        <div class="gt head" style=${'--cols:' + cols}><span>Job</span><span>Backs up</span><span>${vault ? 'Last 14 days' : 'Last 7 backups'}</span><span>Last backup</span><span>Next</span>${withOff && html`<span>Off-site</span>`}<span>Stored</span><span></span></div>
        ${h.jobs.map((j) => html`<${JobRow} key=${j.id} host=${h} job=${j} vault=${vault} cols=${cols} queued=${!!queued[j.id]} onRun=${() => run(j)}
          storedB=${(hostB.data?.jobs || []).find((x) => x.id === j.id)?.stored}
          days=${days.data && days.data.jobs.find((x) => x.job_id === j.id)} slots=${slots.data && slots.data.jobs.find((x) => x.job_id === j.id)?.slots} cut=${(cuts.data || []).some((c) => c.fqdn === h.fqdn && c.job_id === j.id)} />`)}
      </div></div>
      ${vault ? html`<div class="legend"><span><i class="d-success"></i>backed up</span><span><i class="d-warning"></i>incomplete</span><span><i class="d-failed"></i>failed</span>
        <span><i class="d-none"></i>no run</span><span><i class="d-later"></i>not yet today</span><span><i class="d-before"></i>before the first backup</span><span>One square a day; the worst run of the day sets its colour.</span></div>`
      : html`<div class="legend"><span><i class="d-success"></i>backed up</span><span><i class="d-warning"></i>incomplete</span><span><i class="d-failed"></i>failed</span>
        <span><i class="d-none"></i>missed</span><span><i class="d-skipped"></i>skipped</span><span><i class="d-later"></i>not run yet</span><span><i class="d-before"></i>before the first backup</span>
        <span>A square per backup the schedule called for, the newest on the right; click one for its day on the job's page.</span></div>`}`}
    </section>
    ${withPG && html`<${Connections} host=${h} reload=${refresh} />`}
    ${events.data ? html`<${Runs} host=${h} events=${events.data} cycle=${settings?.maintenance?.check_cycle_days || 30} />` : html`<${Loading} what="runs" />`}
    <${Repository} host=${h} settings=${settings} siteB=${siteB.data} hostB=${hostB.data} />`;
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
    <p><a href=${link('agents', fqdn)}>Back to ${fqdn}</a></p>`;
}

export { go };
