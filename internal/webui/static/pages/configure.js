// A job's form: files where the agent runs, or PostgreSQL databases of one of its connections, a job each (decision
// 055). Every check is the server's (POST …/jobs/{job}/check); one job saves with PUT, several as one revision with POST
// …/jobs, the same as holdfast job set with --database repeated.
import { html, useState, useEffect, usePoll, useAction, link, go, fmtTime, dur, bytes, ago, TIERS, Findings, ErrorBox, Loading, can, retentionText, isPG, dbIcon, folderIcon, repoPath } from '../lib.js';
import { api } from '../api.js';

// Each tier's window is in its own unit: one per day for N days, one per week for N weeks. A window saved in another
// unit is shown in this one where that is exact (2 weeks are 14 days), and as it is where it is not.
const UNIT = { within: 'days', hourly: 'hours', daily: 'days', weekly: 'weeks', monthly: 'months', yearly: 'years' };
const PER = { hours: { days: 24 }, days: { weeks: 7 }, months: { years: 12 } };
const inUnit = (k, w) => (w.unit === UNIT[k] ? w : PER[UNIT[k]]?.[w.unit] ? { n: w.n * PER[UNIT[k]][w.unit], unit: UNIT[k] } : w);
const FRESH = { within: { n: 2, unit: 'days' }, hourly: { n: 48, unit: 'hours' }, daily: { n: 14, unit: 'days' }, weekly: { n: 8, unit: 'weeks' }, monthly: { n: 12, unit: 'months' }, yearly: { n: 3, unit: 'years' } };
const DAYS = ['mon', 'tue', 'wed', 'thu', 'fri', 'sat', 'sun'];
const clone = (x) => JSON.parse(JSON.stringify(x));
const lines = (s) => s.split('\n').map((x) => x.trim()).filter(Boolean);
// ROLES stands for a connection's roles and other globals among its databases: the job "roles", pg_dumpall.
const ROLES = ':roles';

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
      <select value=${s.every.unit} onChange=${(e) => set({ ...s, every: { ...s.every, unit: e.target.value } })}><option value="minutes">minutes</option><option value="hours">hours</option></select>
      starting each day at <input style="width:5.5rem" value=${s.every.from} onInput=${(e) => set({ ...s, every: { ...s.every, from: e.target.value } })} /></p>`}
    ${s.daily && html`<p class="row">At ${s.daily.times.map((t, i) => html`<span><input style="width:5.5rem" value=${t} onInput=${(e) => { const times = [...s.daily.times]; times[i] = e.target.value; set({ ...s, daily: { times } }); }} />
        ${s.daily.times.length > 1 && html`<button class="small" onClick=${() => set({ ...s, daily: { times: s.daily.times.filter((_, k) => k !== i) } })}>×</button>`}</span>`)}
      <button class="small" onClick=${() => set({ ...s, daily: { times: [...s.daily.times, '13:00'] } })}>+ add time</button></p>`}
    ${s.weekly && html`<p class="row">On ${DAYS.map((d) => html`<label><input type="checkbox" checked=${s.weekly.days.includes(d)}
        onChange=${(e) => set({ ...s, weekly: { ...s.weekly, days: e.target.checked ? DAYS.filter((x) => x === d || s.weekly.days.includes(x)) : s.weekly.days.filter((x) => x !== d) } })} />${d}</label>`)}
      at <input style="width:5.5rem" value=${s.weekly.at} onInput=${(e) => set({ ...s, weekly: { ...s.weekly, at: e.target.value } })} /></p>`}
    <p class="row"><label><input type="checkbox" checked=${!!s.spread?.enabled} onChange=${(e) => set({ ...s, spread: { minutes: s.spread?.minutes || 20, enabled: e.target.checked } })} />
      Spread each start randomly over</label> <input type="number" min="0" style="width:5rem" value=${s.spread?.minutes || 0} onInput=${(e) => set({ ...s, spread: { ...(s.spread || {}), minutes: +e.target.value } })} /> min
      <span class="muted">so agents don't all start at once</span></p>
    <p class="row"><label>Time zone <input placeholder="the server's default" value=${s.zone || ''} onInput=${(e) => set({ ...s, zone: e.target.value || undefined })} /></label></p>
  </div>`;
}

function RetentionEditor({ r, set }) {
  return html`<table><tbody>
    ${TIERS.map(([k, label]) => html`<tr>
      <td><label><input type="checkbox" checked=${!!r[k]} onChange=${(e) => { const n = { ...r }; if (e.target.checked) n[k] = { ...FRESH[k] }; else delete n[k]; set(n); }} /> Keep ${label}</label></td>
      <td>${r[k] ? html`<span class="inline-field">for <input type="number" min="1" style="width:5rem" value=${inUnit(k, r[k]).n} onInput=${(e) => set({ ...r, [k]: { n: +e.target.value, unit: inUnit(k, r[k]).unit } })} /> ${inUnit(k, r[k]).unit}</span>` : html`<span class="muted">off</span>`}</td>
      <td class="muted mono">--keep-within${k === 'within' ? '' : '-' + k}</td>
    </tr>`)}
    <tr><td>Always keep the newest</td><td><span class="inline-field"><input type="number" min="0" style="width:5rem" value=${r.keep_last} onInput=${(e) => set({ ...r, keep_last: +e.target.value })} /> snapshots, whatever their age</span></td><td class="muted mono">--keep-last</td></tr>
  </tbody></table>
  <p class="muted">Windows are times, never counts (spec §4.2). restic decides what each window keeps.</p>`;
}

// The job a database (or ROLES) of a connection makes.
const pgTarget = (conn, db) => (db === ROLES ? { connection: conn, globals: true } : { connection: conn, database: db });
// A database's job ID, as the CLI's job set makes it (cmd/holdfast pgJobID): lower case, with each character a job ID
// cannot hold as a hyphen (My_DB: my-db).
const idOf = (db) => (db === ROLES ? 'roles' : db.toLowerCase().replace(/[^a-z0-9-]/g, '-').replace(/^-+|-+$/g, '').slice(0, 63).replace(/-+$/, ''));

// What the agent runs for a PostgreSQL job, as internal/agent/postgresql.go makes it: shown, never sent.
function pgCommand(id, p) {
  if (p.globals) return `restic backup --stdin-from-command --stdin-filename globals.sql --tag job=${id} -- \\\n  pg_dumpall --globals-only --no-role-passwords --no-password --username=<user> --dbname=<url>`;
  return `restic backup --stdin-from-command --stdin-filename ${p.database}.dump --tag job=${id} -- \\\n  pg_dump --format=custom --compress=0 --lock-wait-timeout=10min --no-password --username=<user> --dbname=<url>`;
}

// Databases is the PostgreSQL part of the form: the connection, and its databases as its last test found them.
function Databases({ host, conns, conn, setConn, picked, setPicked, editing, typed, setTyped }) {
  const c = conns.find((x) => x.name === conn);
  const jobOf = (db) => host.jobs.find((j) => isPG(j) && j.id !== editing?.id && j.postgresql.connection === conn && (db === ROLES ? j.postgresql.globals : j.postgresql.database === db));
  const dbs = c?.last_test?.ok ? c.last_test.result?.databases || [] : null;
  const pick = (d, on) => setPicked(on ? [...picked.filter((x) => x !== d), d] : picked.filter((x) => x !== d));
  return html`<div class="panel"><h3 class="row" style="gap:.4rem">${dbIcon(16)} ${editing ? 'Database' : 'Databases'}</h3>
    <label class="row">Connection <select value=${conn} disabled=${!!editing} onChange=${(e) => { setConn(e.target.value); setPicked([]); }}>
      ${conns.map((x) => html`<option value=${x.name}>${x.name}</option>`)}</select></label>
    ${c && html`<p class="sub">Signs in to <code>${c.url}</code> as <code>${c.user}</code>, with the connection's password. Every job of ${c.name} uses the same credentials: change them once, on the agent's page.</p>`}
    ${editing ? html`<p><b>${editing.postgresql.globals ? 'Roles and other globals' : editing.postgresql.database}</b> <span class="muted">· a job keeps its database; make a new job for another</span></p>`
      : dbs ? html`<p class="sub">On ${conn} at its last test, ${ago(c.last_test.at)}. Tick one for a job, or several for a job each, with the settings below.</p>
        <div class="opts" style="padding:0">${[...dbs, { name: ROLES }].map((d) => {
          const has = jobOf(d.name);
          const on = picked.includes(d.name);
          return html`<label class=${'opt compact' + (on ? ' on' : '') + (has ? ' dis' : '')}>
            <input type="checkbox" checked=${on} disabled=${!!has} onChange=${(e) => pick(d.name, e.target.checked)} />
            <span class="t"><b>${d.name === ROLES ? 'Roles and other globals' : d.name}</b>
              <span class="sub">${d.name === ROLES ? 'pg_dumpall, roles without their passwords; restored as a file only' : `${bytes(d.bytes)} · owner ${d.owner}`}${has ? ` · job ${has.id}` : ''}</span></span></label>`;
        })}</div>`
      : html`<p class="muted">${c?.last_test ? `The last test of ${conn} failed, so its databases are unknown.` : `${conn} has not been tested yet, so its databases are unknown.`} Name one, or test the connection on the agent's page:</p>
        <p class="row"><input class="mono" aria-label="Database" value=${typed} onInput=${(e) => setTyped(e.target.value)} placeholder="database" />
          <label class="row" style="gap:.3rem"><input type="checkbox" checked=${picked.includes(ROLES)} onChange=${(e) => pick(ROLES, e.target.checked)} /> and the roles</label></p>`}
  </div>`;
}

function Editor({ host, fqdn, jobID, saved, base, isNew, defaults, vault, primary, settings, pre }) {
  const conns = host.postgresql || [];
  const [kind, setKind] = useState(saved ? (isPG(saved) ? 'pg' : 'files') : pre.connection ? 'pg' : 'files');
  const [conn, setConn] = useState(saved?.postgresql?.connection || pre.connection || conns[0]?.name || '');
  const [picked, setPicked] = useState(pre.databases || []);
  const [typed, setTyped] = useState('');
  const [id, setID] = useState(isNew ? '' : jobID);
  const [job, setJob] = useState(() => clone(saved || defaults));
  const [paths, setPaths] = useState((job.paths || []).join('\n'));
  const [excludes, setExcludes] = useState((job.excludes || []).join('\n'));
  const [accept, setAccept] = useState([]);
  const [msg, setMsg] = useState('');
  const [checks, setChecks] = useState(null); // [{id, check}] or null
  const [checkErr, setCheckErr] = useState(null);
  const save = useAction();
  const pg = kind === 'pg';
  // The databases a new PostgreSQL form makes jobs of: the ticked ones, or the one typed when the last test found none.
  const chosen = !pg ? [] : saved ? [saved.postgresql.globals ? ROLES : saved.postgresql.database] : [...(typed.trim() ? [typed.trim()] : []), ...picked];
  const several = !saved && pg && chosen.length > 1;
  const singleID = saved ? jobID : pg ? (id || (chosen[0] ? idOf(chosen[0]) : '')) : id;
  const common = { schedule: job.schedule, retention: job.retention, limit_upload_kib: job.limit_upload_kib };
  // A job backs up paths, or a database. Saving a dump job from before 0.6.0 makes it a paths job (decision 052).
  const jobs = !pg ? [[singleID, { ...common, engine: 'restic', paths: lines(paths), excludes: lines(excludes), dump: undefined }]]
    : several ? chosen.map((d) => [idOf(d), { ...common, engine: 'postgresql', paths: [], postgresql: pgTarget(conn, d) }])
      : [[singleID, { ...common, engine: 'postgresql', paths: [], postgresql: chosen[0] ? pgTarget(conn, chosen[0]) : { connection: conn } }]];
  const key = JSON.stringify([jobs, accept]);
  useEffect(() => {
    if (jobs.some(([j]) => !j)) { setChecks(null); return undefined; }
    const t = setTimeout(async () => {
      try {
        setChecks(await Promise.all(jobs.map(async ([j, body]) => ({ id: j, check: await api.checkJob(fqdn, j, { job: body, accept }) }))));
        setCheckErr(null);
      } catch (err) { setCheckErr(err); }
    }, 300);
    return () => clearTimeout(t);
  }, [key]);
  const onAccept = (code, on) => setAccept(on ? [...accept, code] : accept.filter((c) => c !== code));
  const all = checks || [];
  const first = all[0]?.check;
  const blocked = !checks || all.some(({ check }) => check.blocking.length > 0 || (check.problems || []).length > 0);
  const errors = all.some(({ check }) => check.blocking.some((f) => f.severity === 'error') || (check.problems || []).length > 0);
  const cut = all.some(({ check }) => check.findings.some((f) => f.code === 'R104')) && accept.includes('R104');
  const missing = pg && !chosen.length ? 'Tick a database, or the roles.' : jobs.some(([j]) => !j) ? 'Give the job an ID.' : '';
  const onSave = () => save.run(() => (several
    ? api.setJobs(fqdn, { jobs: Object.fromEntries(jobs), accept, message: msg, base_revision: base })
    : api.setJob(fqdn, singleID, { job: jobs[0][1], accept, message: msg, base_revision: base }))).then((r) => r && go(link('agents', fqdn).slice(1)));
  const reset = () => { const j = clone(saved || defaults); setJob(j); setPaths((j.paths || []).join('\n')); setExcludes((j.excludes || []).join('\n')); setAccept([]); };
  const n = jobs.length;
  return html`<div class="grid2"><div>
    ${vault && html`<div class="panel"><h3>Off-site retention · job ${jobID}</h3>
      <p class="muted">The rest of the job is set on the server's pages. Primary retention 🔒: ${primary ? retentionText(primary) : '—'}. Keeping less off-site is warning O101, which you accept.</p></div>`}
    ${!vault && html`<div class="panel"><h3>What it backs up${saved ? ` · job ${jobID}` : ''}</h3>
      <div class="opts" style="padding:0" role="radiogroup" aria-label="What it backs up">
        <label class=${'opt' + (pg ? ' on' : '') + ((saved && !isPG(saved)) || !conns.length ? ' dis' : '')}>
          <input type="radio" name="what" checked=${pg} disabled=${(saved && !isPG(saved)) || !conns.length} onChange=${() => setKind('pg')} />
          <span class="t"><b class="row" style="gap:.35rem">${dbIcon()} PostgreSQL databases</b>
            <span class="sub">${conns.length ? 'Databases of a connection of this agent, or its roles. Each one is a job of its own.' : host.agent?.postgresql?.client ? 'Add a PostgreSQL connection on the agent’s page first.' : `${fqdn} has no PostgreSQL client: postgresql.bin_dir is not set in its agent.yaml.`}</span></span></label>
        <label class=${'opt' + (!pg ? ' on' : '') + (saved && isPG(saved) ? ' dis' : '')}>
          <input type="radio" name="what" checked=${!pg} disabled=${saved && isPG(saved)} onChange=${() => setKind('files')} />
          <span class="t"><b class="row" style="gap:.35rem">${folderIcon()} Files where the agent runs</b><span class="sub">Paths, as restic backs them up.</span></span></label>
      </div>
      ${saved?.dump && html`<div class="banner warning">This job backed up the dump <code>${saved.dump}</code>, which Holdfast 0.6 no longer runs: Holdfast runs no command where an agent runs (decision 052). Give it the paths the command writes to, and save.</div>`}
      ${!pg && html`<label class="field"><span>Paths (one per line, absolute)</span><textarea aria-label="Paths" value=${paths} onInput=${(e) => setPaths(e.target.value)} /></label>
        <label class="field"><span>Exclude (one per line, restic --exclude patterns)</span><textarea value=${excludes} onInput=${(e) => setExcludes(e.target.value)} /></label>
        <p class="muted">A directory server, or a database without a PostgreSQL connection here, is backed up from the export it writes: a timer where the agent runs makes the export, and the job backs up its directory.</p>`}
    </div>
    ${pg && conns.length > 0 && html`<${Databases} host=${host} conns=${conns} conn=${conn} setConn=${setConn} picked=${picked} setPicked=${setPicked} editing=${saved} typed=${typed} setTyped=${setTyped} />`}
    <div class="panel">${several
      ? html`<p class="row"><b>Job IDs</b> <span class="muted">each database's own name: ${jobs.map(([j]) => j).join(', ')}</span></p>`
      : html`<label class="field"><span>Job ID (lower-case letters, digits, dashes; the snapshot tag job=…)${pg && !saved ? ': the database’s name unless you change it' : ''}</span>
          <input class="mono" value=${singleID} disabled=${!!saved} onInput=${(e) => setID(e.target.value)} placeholder=${pg ? 'the database’s name' : 'system'} /></label>`}</div>
    <div class="panel"><h3>When ${n > 1 ? 'they run' : 'it runs'}</h3><${ScheduleEditor} s=${job.schedule} set=${(schedule) => setJob({ ...job, schedule })} />
      ${first && first.gets.next_runs && html`<p class="muted">Next runs: ${first.gets.next_runs.map(fmtTime).join(', ')}${job.schedule.spread?.enabled ? ` (each + random 0–${job.schedule.spread.minutes} min)` : ''}</p>`}</div>`}
    <div class="panel"><h3>How long to keep${vault ? ' here' : ''}</h3><${RetentionEditor} r=${job.retention} set=${(retention) => setJob({ ...job, retention })} /></div>
    ${!vault && html`<div class="panel"><h3>Run controls</h3>
      <p class="row"><label>Cap the upload at <input type="number" min="0" style="width:7rem" value=${job.limit_upload_kib || 0}
        onInput=${(e) => setJob({ ...job, limit_upload_kib: +e.target.value || undefined })} /> KiB/s</label> <span class="muted">0 for no limit (restic --limit-upload)</span></p>
      <p class="muted">A failed backup is retried after 5, 15 and 45 minutes, while that is before its next run. The agent runs restic${pg ? ' and pg_dump' : ''} at low CPU and disk priority.</p></div>`}
    <div class="panel"><h3>Checks</h3>
      <${ErrorBox} error=${checkErr} />
      ${missing ? html`<p><strong>${missing}</strong></p>` : !checks ? html`<${Loading} what="the checks" />` : html`
        <p><strong>${errors ? 'Errors: this cannot be saved.' : blocked ? 'Warnings to accept, one by one.' : 'Ready to save.'}</strong></p>
        ${all.map(({ id: j, check }) => html`
          ${n > 1 && html`<h4 class="muted" style="margin:.6rem 0 .2rem">Job ${j}</h4>`}
          ${(check.problems || []).map((p) => html`<div class="banner error"><code>${p.path}</code>: ${p.message}</div>`)}
          <${Findings} findings=${check.findings} accepted=${accept} onAccept=${onAccept} />`)}
        <p class="muted">The same from the CLI:</p><pre>${all.map(({ check }) => check.cli).join('\n')}</pre>`}
    </div>
  </div><div>
    <div class="panel"><h3>What you get</h3>
      ${first ? html`<dl class="facts">
        <dt>Each run</dt><dd>${vault ? 'The off-site copies follow primary’s; this retention decides what stays here.'
          : pg ? (n > 1 ? 'pg_dump writes each database as one file; restic encrypts it, stores only what changed since the last one, and keeps it as a snapshot of its job.'
            : `${chosen[0] === ROLES ? 'pg_dumpall writes the roles and other globals' : `pg_dump writes ${chosen[0] || 'the database'}`} as one file; restic encrypts it, stores only what changed since the last one, and keeps it as a snapshot.`)
          : 'restic backs up the paths, encrypted, storing only what changed.'}</dd>
        <dt>Worst-case data loss</dt><dd>${dur(first.gets.worst_case_loss)}</dd>
        <dt>Restore points</dt><dd>about ${first.gets.restore_points} once it has run long enough</dd>
        <dt>Oldest restore point</dt><dd>about ${first.gets.oldest || '—'}</dd>
        <dt>Stale alert</dt><dd>after ${dur(first.gets.stale_after)} without a success</dd>
        ${!vault && html`<dt>Stored in</dt><dd>site A, <code>${repoPath(settings?.storage, fqdn)}</code>${settings?.offsite ? '; copied to site B' : ''}</dd>`}
      </dl>
      <details class="exact"><summary>Exact commands</summary>
        ${pg && !vault && html`<p class="sub">The agent runs pg_dump through restic. The connection reaches pg_dump in a clean environment, the password in a file only the agent reads; none of it is on a command line.</p>
          <pre>${jobs.filter(([, b]) => b.postgresql.database || b.postgresql.globals).map(([j, b]) => pgCommand(j, b.postgresql)).join('\n\n') || '—'}</pre>`}
        <p class="sub">What restic keeps, decided by restic:</p>
        <pre>${jobs.map(([j]) => `restic forget --tag job=${j} --group-by host ${first.gets.restic_flags.join(' ')}`).join('\n')}</pre>
      </details>` : html`<p class="muted">${missing || 'Loading…'}</p>`}
    </div>
    <div class="panel"><h3>Save as a new revision</h3>
      ${n === 1 && first && (first.changes.length === 0 ? html`<p class="muted">No changes yet.</p>` : html`<pre>${first.changes.map((c) => `${c.path}: ${c.from === undefined ? '' : JSON.stringify(c.from) + ' → '}${c.to === undefined ? '(removed)' : JSON.stringify(c.to)}`).join('\n')}</pre>`)}
      <label class="field"><span>Message</span><input style="width:100%" value=${msg} onInput=${(e) => setMsg(e.target.value)} placeholder=${isNew ? `${fqdn}: add ${n > 1 ? n + ' jobs' : singleID || 'a'} job` : `${fqdn} ${jobID}: …`} /></label>
      <div class="row">
        <button class=${cut ? 'warn' : 'primary'} disabled=${save.busy || blocked || !!missing} onClick=${onSave}>
          ${n > 1 ? `Save ${n} jobs` : 'Save'}${cut ? ' (what the cut drops waits 7 days)' : ''}</button>
        <button onClick=${reset}>Undo changes</button>
      </div>
      <${ErrorBox} error=${save.error} />
      <p class="muted">Every save is a revision. Agents pick it up on their next poll. A save from an old revision is refused, so two people cannot overwrite each other.</p>
    </div>
  </div></div>`;
}

// pre is what the agent page's Add job… asks for: #/agents/<agent>/configure?connection=<c>&databases=<a>,<b>,:roles
function preset() {
  const q = new URLSearchParams(location.hash.split('?')[1] || '');
  return { connection: q.get('connection') || '', databases: (q.get('databases') || '').split(',').filter(Boolean) };
}

export function Configure({ fqdn, job, settings, vault }) {
  const host = usePoll(() => api.host(fqdn), [fqdn], 0);
  const status = usePoll(() => api.status(), [fqdn], 0);
  if (host.error) return html`<${ErrorBox} error=${host.error} />`;
  if (!host.data || !status.data) return html`<${Loading} what=${fqdn} />`;
  const h = host.data;
  if (!can(h, 'config.jobs')) return html`<div class="banner warning">Changing jobs on ${fqdn} needs config.jobs (an editor or an admin).</div>`;
  const saved = h.jobs.find((j) => j.id === job);
  const asJob = (j) => j && { engine: j.engine || 'restic', paths: j.paths || [], excludes: j.excludes || [], schedule: j.schedule, retention: j.retention,
    limit_upload_kib: j.limit_upload_kib, dump: j.dump, postgresql: j.postgresql };
  const pre = saved ? { connection: '', databases: [] } : preset();
  return html`<h1>${saved ? 'Configure' : 'Add a job'} · <a href=${link('agents', fqdn)}>${fqdn}</a></h1>
    <p class="muted">Every save is a revision in PostgreSQL. Schedule and retention are checked together: errors block the save, warnings must be accepted one by one.</p>
    <div class="tabs">${h.jobs.map((j) => html`<a class=${j.id === job ? 'on' : ''} href=${link('agents', fqdn, 'configure', j.id)}>${isPG(j) && dbIcon(12)} ${j.id}</a>`)}
      ${!vault && html`<a class=${!saved ? 'on' : ''} href=${link('agents', fqdn, 'configure')}>+ Add job</a>`}</div>
    ${vault && !saved ? html`<div class="banner info">The off-site jobs follow the server's: choose one above to set its off-site retention.</div>`
      : html`<${Editor} key=${(job || 'new') + JSON.stringify(pre)} host=${h} fqdn=${fqdn} jobID=${job} saved=${asJob(saved)} isNew=${!saved} base=${status.data.head_revision} defaults=${settings.default_job}
      vault=${vault} primary=${saved?.primary_retention} settings=${settings} pre=${pre} />`}`;
}
