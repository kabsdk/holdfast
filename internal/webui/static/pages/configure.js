import { html, useState, useEffect, usePoll, useAction, link, go, fmtTime, dur, TIERS, Findings, ErrorBox, Loading, can } from '../lib.js';
import { api } from '../api.js';

const UNITS = { within: ['hours', 'days', 'weeks'], hourly: ['hours', 'days'], daily: ['days', 'weeks', 'months'], weekly: ['weeks', 'months'], monthly: ['months', 'years'], yearly: ['years'] };
const FRESH = { within: { n: 2, unit: 'days' }, hourly: { n: 48, unit: 'hours' }, daily: { n: 14, unit: 'days' }, weekly: { n: 8, unit: 'weeks' }, monthly: { n: 12, unit: 'months' }, yearly: { n: 3, unit: 'years' } };
const DAYS = ['mon', 'tue', 'wed', 'thu', 'fri', 'sat', 'sun'];
const clone = (x) => JSON.parse(JSON.stringify(x));
const lines = (s) => s.split('\n').map((x) => x.trim()).filter(Boolean);

function ScheduleEditor({ s, set }) {
  const mode = s.daily ? 'daily' : s.every ? 'every' : s.weekly ? 'weekly' : '';
  const to = (m) => {
    const n = { spread: s.spread, zone: s.zone };
    if (m === 'daily') n.daily = { times: ['01:00'] };
    if (m === 'every') n.every = { n: 4, unit: 'hours', from: '00:00' };
    if (m === 'weekly') n.weekly = { days: ['sun'], at: '02:00' };
    set(n);
  };
  return html`<div>
    <div class="row">${['every', 'daily', 'weekly'].map((m) => html`<label><input type="radio" checked=${mode === m} onChange=${() => to(m)} /> ${m[0].toUpperCase() + m.slice(1)}</label>`)}</div>
    ${s.every && html`<p class="row">Every <input type="number" min="1" style="width:5rem" value=${s.every.n} onInput=${(e) => set({ ...s, every: { ...s.every, n: +e.target.value } })} />
      <select value=${s.every.unit} onChange=${(e) => set({ ...s, every: { ...s.every, unit: e.target.value } })}><option>minutes</option><option>hours</option></select>
      starting each day at <input style="width:5.5rem" value=${s.every.from} onInput=${(e) => set({ ...s, every: { ...s.every, from: e.target.value } })} /></p>`}
    ${s.daily && html`<p class="row">At ${s.daily.times.map((t, i) => html`<span><input style="width:5.5rem" value=${t} onInput=${(e) => { const times = [...s.daily.times]; times[i] = e.target.value; set({ ...s, daily: { times } }); }} />
        ${s.daily.times.length > 1 && html`<button class="small" onClick=${() => set({ ...s, daily: { times: s.daily.times.filter((_, k) => k !== i) } })}>×</button>`}</span>`)}
      <button class="small" onClick=${() => set({ ...s, daily: { times: [...s.daily.times, '13:00'] } })}>+ add time</button></p>`}
    ${s.weekly && html`<p class="row">On ${DAYS.map((d) => html`<label><input type="checkbox" checked=${s.weekly.days.includes(d)}
        onChange=${(e) => set({ ...s, weekly: { ...s.weekly, days: e.target.checked ? DAYS.filter((x) => x === d || s.weekly.days.includes(x)) : s.weekly.days.filter((x) => x !== d) } })} />${d}</label>`)}
      at <input style="width:5.5rem" value=${s.weekly.at} onInput=${(e) => set({ ...s, weekly: { ...s.weekly, at: e.target.value } })} /></p>`}
    <p class="row"><label><input type="checkbox" checked=${!!s.spread?.enabled} onChange=${(e) => set({ ...s, spread: { minutes: s.spread?.minutes || 20, enabled: e.target.checked } })} />
      Spread each start randomly over</label> <input type="number" min="0" style="width:5rem" value=${s.spread?.minutes || 0} onInput=${(e) => set({ ...s, spread: { ...(s.spread || {}), minutes: +e.target.value } })} /> min
      <span class="muted">so hosts don't all start at once</span></p>
    <p class="row"><label>Time zone <input placeholder="the server's default" value=${s.zone || ''} onInput=${(e) => set({ ...s, zone: e.target.value || undefined })} /></label></p>
  </div>`;
}

function RetentionEditor({ r, set }) {
  return html`<table><tbody>
    ${TIERS.map(([k, label]) => html`<tr>
      <td><label><input type="checkbox" checked=${!!r[k]} onChange=${(e) => { const n = { ...r }; if (e.target.checked) n[k] = { ...FRESH[k] }; else delete n[k]; set(n); }} /> Keep ${label}</label></td>
      <td>${r[k] ? html`for <input type="number" min="1" style="width:5rem" value=${r[k].n} onInput=${(e) => set({ ...r, [k]: { ...r[k], n: +e.target.value } })} />
        <select value=${r[k].unit} onChange=${(e) => set({ ...r, [k]: { ...r[k], unit: e.target.value } })}>${UNITS[k].map((u) => html`<option>${u}</option>`)}</select>` : html`<span class="muted">off</span>`}</td>
      <td class="muted mono">--keep-within${k === 'within' ? '' : '-' + k}</td>
    </tr>`)}
    <tr><td>Always keep the newest</td><td><input type="number" min="0" style="width:5rem" value=${r.keep_last} onInput=${(e) => set({ ...r, keep_last: +e.target.value })} /> snapshots, whatever their age</td><td class="muted mono">--keep-last</td></tr>
  </tbody></table>
  <p class="muted">Windows are times, never counts (spec §4.2). restic decides what each window keeps.</p>`;
}

function Editor({ fqdn, jobID, saved, base, isNew, defaults }) {
  const [id, setID] = useState(isNew ? 'new-job' : jobID);
  const [job, setJob] = useState(() => clone(saved || defaults));
  const [paths, setPaths] = useState((job.paths || []).join('\n'));
  const [excludes, setExcludes] = useState((job.excludes || []).join('\n'));
  const [accept, setAccept] = useState([]);
  const [msg, setMsg] = useState('');
  const [check, setCheck] = useState(null);
  const [checkErr, setCheckErr] = useState(null);
  const save = useAction();
  const current = { ...job, paths: lines(paths), excludes: lines(excludes) };
  const key = JSON.stringify([id, current, accept]);
  useEffect(() => {
    const t = setTimeout(async () => {
      try { setCheck(await api.checkJob(fqdn, id, { job: current, accept })); setCheckErr(null); } catch (err) { setCheckErr(err); }
    }, 300);
    return () => clearTimeout(t);
  }, [key]);
  const onAccept = (code, on) => setAccept(on ? [...accept, code] : accept.filter((c) => c !== code));
  const blocked = check && (check.blocking.length > 0 || (check.problems || []).length > 0);
  const cut = check && check.findings.some((f) => f.code === 'R104') && accept.includes('R104');
  return html`<div class="grid2"><div>
    <div class="panel"><h3>What · job ${isNew ? 'new' : jobID}</h3>
      ${isNew && html`<label class="field"><span>Job ID (lower-case letters, digits, dashes; the snapshot tag job=…)</span><input value=${id} onInput=${(e) => setID(e.target.value)} /></label>`}
      <label class="field"><span>Paths (one per line, absolute)</span><textarea value=${paths} onInput=${(e) => setPaths(e.target.value)} /></label>
      <label class="field"><span>Exclude (one per line, restic --exclude patterns)</span><textarea value=${excludes} onInput=${(e) => setExcludes(e.target.value)} /></label>
    </div>
    <div class="panel"><h3>When it runs</h3><${ScheduleEditor} s=${job.schedule} set=${(schedule) => setJob({ ...job, schedule })} />
      ${check && check.gets.next_runs && html`<p class="muted">Next runs: ${check.gets.next_runs.map(fmtTime).join(', ')}${job.schedule.spread?.enabled ? ` (each + random 0–${job.schedule.spread.minutes} min)` : ''}</p>`}</div>
    <div class="panel"><h3>How long to keep</h3><${RetentionEditor} r=${job.retention} set=${(retention) => setJob({ ...job, retention })} /></div>
    <div class="panel"><h3>Checks</h3>
      <${ErrorBox} error=${checkErr} />
      ${!check ? html`<${Loading} what="the checks" />` : html`
        ${(check.problems || []).map((p) => html`<div class="banner error"><code>${p.path}</code>: ${p.message}</div>`)}
        <p><strong>${check.blocking.filter((f) => f.severity === 'error').length ? 'Errors: this cannot be saved.' : check.blocking.length ? 'Warnings to accept, one by one.' : 'Ready to save.'}</strong></p>
        <${Findings} findings=${check.findings} accepted=${accept} onAccept=${onAccept} />
        <p class="muted">The same from the CLI:</p><pre>${check.cli}</pre>`}
    </div>
  </div><div>
    <div class="panel"><h3>What you get</h3>
      ${check ? html`<dl class="facts">
        <dt>Worst-case data loss</dt><dd>${dur(check.gets.worst_case_loss)}</dd>
        <dt>Restore points</dt><dd>about ${check.gets.restore_points} once it has run long enough</dd>
        <dt>Oldest restore point</dt><dd>about ${check.gets.oldest || '—'}</dd>
        <dt>Stale alert</dt><dd>after ${dur(check.gets.stale_after)} without a success</dd>
      </dl><pre>restic forget --tag job=${id} --group-by host ${check.gets.restic_flags.join(' ')}</pre>` : html`<${Loading} />`}
    </div>
    <div class="panel"><h3>Save as a new revision</h3>
      ${check && (check.changes.length === 0 ? html`<p class="muted">No changes yet.</p>` : html`<pre>${check.changes.map((c) => `${c.path}: ${c.from === undefined ? '' : JSON.stringify(c.from) + ' → '}${c.to === undefined ? '(removed)' : JSON.stringify(c.to)}`).join('\n')}</pre>`)}
      <label class="field"><span>Message</span><input style="width:100%" value=${msg} onInput=${(e) => setMsg(e.target.value)} placeholder=${isNew ? `${fqdn}: add ${id} job` : `${fqdn} ${jobID}: …`} /></label>
      <div class="row">
        <button class=${cut ? 'warn' : 'primary'} disabled=${save.busy || !check || blocked} onClick=${() => save.run(() => api.setJob(fqdn, id, { job: current, accept, message: msg, base_revision: base })).then((r) => r && go(link('hosts', fqdn).slice(1)))}>
          Save${cut ? ' (what the cut drops waits 7 days)' : ''}</button>
        <button onClick=${() => { const j = clone(saved || defaults); setJob(j); setPaths((j.paths || []).join('\n')); setExcludes((j.excludes || []).join('\n')); setAccept([]); }}>Undo changes</button>
      </div>
      <${ErrorBox} error=${save.error} />
      <p class="muted">Every save is a revision. Agents pick it up on their next poll. A save from an old revision is refused, so two people cannot overwrite each other.</p>
    </div>
  </div></div>`;
}

export function Configure({ fqdn, job, settings }) {
  const host = usePoll(() => api.host(fqdn), [fqdn], 0);
  const status = usePoll(() => api.status(), [fqdn], 0);
  if (host.error) return html`<${ErrorBox} error=${host.error} />`;
  if (!host.data || !status.data) return html`<${Loading} what=${fqdn} />`;
  const h = host.data;
  if (!can(h, 'config.jobs')) return html`<div class="banner warning">Changing jobs on ${fqdn} needs config.jobs (an editor or an admin).</div>`;
  const saved = h.jobs.find((j) => j.id === job);
  const asJob = (j) => j && { engine: 'restic', paths: j.paths, excludes: j.excludes || [], schedule: j.schedule, retention: j.retention };
  return html`<h1>Configure · ${fqdn}</h1>
    <p class="muted">Every save is a revision in PostgreSQL. Schedule and retention are checked together: errors block the save, warnings must be accepted one by one.</p>
    <div class="tabs">${h.jobs.map((j) => html`<a class=${j.id === job ? 'on' : ''} href=${link('hosts', fqdn, 'configure', j.id)}>${j.id}</a>`)}
      <a class=${!saved ? 'on' : ''} href=${link('hosts', fqdn, 'configure')}>+ Add job</a></div>
    <${Editor} key=${job || 'new'} fqdn=${fqdn} jobID=${job} saved=${asJob(saved)} isNew=${!saved} base=${status.data.head_revision} defaults=${settings.default_job} />`;
}
