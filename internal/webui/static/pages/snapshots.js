import { html, useState, useEffect, useRef, useMemo, usePoll, useAction, link, fmtTime, fmtDay, bytes, can, ErrorBox, Loading, Menu, isPG, dbIcon, dumpFile, folderIcon } from '../lib.js';
import { api } from '../api.js';
import { Restore, PGRestore, DeleteDialog, EVERYTHING, onlyThis } from './restore.js';
import { point, copiesByOriginal, WORD_A, WORD_B } from '../keep.js';

// fate is what happens to a snapshot, in a word or two: kept and why, waiting to be deleted, or gone.
export function fate(s) {
  const why = (s.deletion_requested_by ? ` as ${s.deletion_requested_by} asked` : '') + (s.deletion_reason ? ` (“${s.deletion_reason}”)` : '');
  if (s.gone_at && s.deleted_by_holdfast) return { kind: '', text: `deleted by Holdfast ${fmtTime(s.gone_at)}${why}` + (s.really_gone_by ? `; S3's old versions expire by ${fmtDay(s.really_gone_by)}` : '; its data is freed at the next prune') };
  if (s.gone_at) return { kind: 'error', text: `gone from the repository ${fmtTime(s.gone_at)}, without Holdfast deleting it` };
  if (s.deletion_requested_at) return { kind: 'warning', text: `deletion asked ${fmtTime(s.deletion_requested_at)}; it waits 7 days` };
  if (s.deleted_in_primary_at) return { kind: 'warning', text: `deleted in primary ${fmtTime(s.deleted_in_primary_at)}: an admin there deleted the original. This copy stays unless you delete it here too` };
  if (s.pending_until) return { kind: 'warning', text: `outside the new retention; pending until ${fmtTime(s.pending_until)}` };
  if (s.kept_by && s.kept_by.length) return { kind: 'ok', text: 'kept: ' + s.kept_by.join(', ') };
  if (!s.made_by_holdfast) return { kind: 'warning', text: 'not made by Holdfast; left alone' };
  return { kind: '', text: '' };
}

// FindFile is one line at the top of a job's page (spec §5.2), whose results open below it.
export function FindFile({ fqdn, job }) {
  const [text, setText] = useState('');
  const act = useAction();
  const found = (act.done || []).filter((f) => !job || f.job_id === job);
  return html`<div class="panel findfile">
    <form class="row" style="align-items:center" onSubmit=${(e) => { e.preventDefault(); if (text.trim().length >= 2) act.run(() => api.find(fqdn, text.trim())); }}>
      <b style="white-space:nowrap">Find a file in all snapshots</b>
      <input aria-label="Path or name" placeholder="Path or name, e.g. nginx.conf" value=${text} onInput=${(e) => setText(e.target.value)} style="flex:1;min-width:14rem" />
      <button disabled=${act.busy}>Find</button>
    </form>
    <p class="muted small-text" style="margin:.4rem 0 0">Like <code>restic find</code>: names and metadata, never contents.</p>
    <${ErrorBox} error=${act.error} />
    ${act.busy && html`<${Loading} what="every snapshot" />`}
    ${act.done && (found.length === 0 ? html`<p class="muted">No file matching “${text}” in any snapshot${job ? ' of this job' : ''}.</p>` : html`<table>
      <thead><tr><th>Path</th><th>Snapshot</th><th>Size</th><th>Owner · mode</th><th>Modified</th></tr></thead>
      <tbody>${found.flatMap((f) => f.matches.map((m) => html`<tr>
        <td class="mono">${m.path}</td>
        <td><a class="mono" href=${link('agents', fqdn, 'snapshots', f.snapshot) + '/files?path=' + encodeURIComponent(m.path.slice(0, m.path.lastIndexOf('/')) || '/')}>${f.short_id}</a> · ${fmtTime(f.time)}</td>
        <td>${bytes(m.size)}</td><td>${m.user || m.uid} · ${m.permissions}</td><td>${fmtTime(m.mtime)}</td>
      </tr>`))}</tbody></table>`)}
  </div>`;
}

/* ---------------- one snapshot ---------------- */

// A files snapshot is one tree (round 10): its folders open in place, each entry with what changed against an older
// snapshot of the job, as restic diff marks it, and its mode, owner, size and time. A folder is read when it is opened
// (restic ls of that folder, 1 000 entries at a time); the marks come from one restic diff. Names and metadata only:
// Holdfast never shows a file's contents.
const CHANGE = {
  '+': ['add', '+ new', 'in this snapshot, not in the older one'],
  '-': ['del', '− removed', 'in the older snapshot, not in this one'],
  M: ['mod', 'M changed', 'its content changed'],
  U: ['meta', 'U metadata', 'only its metadata changed: mode, owner or times'],
  T: ['type', 'T type', 'its type changed, such as a file that is a symlink now'],
};
const MAX_CHANGE_PAGES = 10; // the first 10 000 changes mark the tree
const trimSlash = (p) => (p.length > 1 && p.endsWith('/') ? p.slice(0, -1) : p);
const parentOf = (p) => p.slice(0, p.lastIndexOf('/')) || '/';
const childOf = (dir, name) => (dir === '/' ? '/' + name : dir + '/' + name);

// loadChanges reads restic diff's marks against an older snapshot, a page of 1 000 at a time.
async function loadChanges(fqdn, id, against) {
  const first = await api.changes(fqdn, id, against, 1);
  let all = first.changes;
  for (let p = 2; p <= Math.min(first.pages, MAX_CHANGE_PAGES); p++) all = all.concat((await api.changes(fqdn, id, against, p)).changes);
  return { ...first, changes: all, capped: first.pages > MAX_CHANGE_PAGES };
}

function FileTree({ fqdn, snap, host, older, onRestoreOnly }) {
  const start = new URLSearchParams(location.hash.split('?')[1] || '').get('path');
  const [open, setOpen] = useState(() => {
    const s = new Set(['/']);
    [...(snap.paths || []), ...(start ? [start] : [])].forEach((p) => { for (let x = trimSlash(p); x !== '/'; x = parentOf(x)) s.add(x); });
    return s;
  });
  const [dirs, setDirs] = useState({}); // folder → its entries as far as read
  const [before, setBefore] = useState({}); // folder → its entries in the older snapshot, for what was removed
  const asked = useRef(new Set());
  const [only, setOnly] = useState(false);
  const [against, setAgainst] = useState(older[0]?.id || '');
  const ch = usePoll(() => (against ? loadChanges(fqdn, snap.id, against) : Promise.resolve(null)), [fqdn, snap.id, against], 0);
  const [q, setQ] = useState('');
  const find = useAction();
  const [diffAt, setDiffAt] = useState(null);
  const mayRestore = can(host, 'restore.new_dir');
  // The marks by path, how many changed below each folder, and what each folder lost.
  const idx = useMemo(() => {
    const marks = new Map(), inside = new Map(), removed = new Map();
    (ch.data?.changes || []).forEach((c) => {
      const p = trimSlash(c.path);
      marks.set(p, c.modifier);
      for (let d = parentOf(p); ; d = parentOf(d)) { inside.set(d, (inside.get(d) || 0) + 1); if (d === '/') break; }
      if (c.modifier === '-') { const d = parentOf(p); removed.set(d, [...(removed.get(d) || []), p.slice(p.lastIndexOf('/') + 1)]); }
    });
    return { marks, inside, removed };
  }, [ch.data]);
  const read = (p, page = 1) => {
    asked.current.add(p + '#' + page);
    api.files(fqdn, snap.id, p, page).then(
      (d) => setDirs((m) => ({ ...m, [p]: { ...d, entries: page > 1 && m[p] ? m[p].entries.concat(d.entries) : d.entries } })),
      (err) => setDirs((m) => ({ ...m, [p]: { error: err, entries: [] } })));
  };
  // Open folders are read once; a folder that lost entries is read in the older snapshot too, for their metadata.
  useEffect(() => {
    open.forEach((p) => { if (!asked.current.has(p + '#1')) read(p); });
    if (against) idx.removed.forEach((_, d) => {
      if (open.has(d) && !asked.current.has('before:' + d)) {
        asked.current.add('before:' + d);
        api.files(fqdn, against, d, 1).then((x) => setBefore((m) => ({ ...m, [d]: x.entries })), () => {});
      }
    });
  });
  const toggle = (p) => { const s = new Set(open); if (s.has(p)) s.delete(p); else s.add(p); setOpen(s); };
  const showOnly = () => {
    // The folders with changes open, the shallowest first, at most 30 at once.
    const add = [...idx.inside.keys()].sort((x, y) => x.split('/').length - y.split('/').length).slice(0, 30);
    setOpen(new Set([...open, ...add]));
    setOnly(true);
  };
  const expandAll = () => setOpen(new Set([...open, ...Object.values(dirs).flatMap((d) => (d.entries || []).filter((n) => n.type === 'dir').map((n) => n.path))].slice(0, 400)));
  const rows = [];
  const walk = (dir, depth) => {
    const d = dirs[dir];
    if (!d) { rows.push({ note: 'Reading the folder…', depth }); return; }
    if (d.error) { rows.push({ error: d.error, depth }); return; }
    const here = new Set(d.entries.map((n) => n.name));
    const gone = (idx.removed.get(dir) || []).filter((name) => !here.has(name)).map((name) => {
      const old = (before[dir] || []).find((x) => x.name === name);
      return { ...(old || { name, type: '' }), name, path: childOf(dir, name), gone: true };
    });
    const list = [...d.entries, ...gone].sort((x, y) => (x.name < y.name ? -1 : x.name > y.name ? 1 : 0));
    list.forEach((n) => {
      const mark = idx.marks.get(n.path);
      const isDir = n.type === 'dir' && !n.gone;
      if (only && !mark && !(isDir && idx.inside.get(n.path))) return;
      const isOpen = isDir && open.has(n.path);
      rows.push({ n, mark, depth, isDir, isOpen });
      if (n.path === diffAt) rows.push({ diff: n.path, depth });
      if (isOpen) walk(n.path, depth + 1);
    });
    if (!only && d.page < d.pages) rows.push({ more: d.total - d.entries.length, dir, page: d.page, depth });
  };
  walk('/', 0);
  const pattern = (s) => (/[*?[]/.test(s) ? s : `*${s}*`);
  const found = (find.done || []).flatMap((f) => f.matches || []);
  const cd = ch.data;
  const chip = (k, n) => n > 0 && html`<span class=${'chg ' + CHANGE[k][0]}>${CHANGE[k][1].split(' ')[0]} ${n}</span>`;
  const row = ({ n, mark, depth, isDir, isOpen, path }) => html`<div class=${'trow' + (n.gone ? ' gone' : '')} key=${n.path}>
    <span class="tname" style=${`padding-left:${depth * 18}px`}>
      ${isDir ? html`<button type="button" class="twist" aria-expanded=${isOpen ? 'true' : 'false'} aria-label=${(isOpen ? 'Close ' : 'Open ') + n.path} onClick=${() => toggle(n.path)}>${isOpen ? '▾' : '▸'}</button>${folderIcon(13)}` : html`<span class="tpad"></span>`}
      <span class=${path ? 'mono' : ''}>${path ? n.path : n.name}${isDir && !path ? '/' : ''}</span>
      ${isDir && !isOpen && idx.inside.get(n.path) > 0 && html`<span class="inside">${idx.inside.get(n.path)} changed inside</span>`}</span>
    <span>${mark && CHANGE[mark] && html`<span class=${'chg ' + CHANGE[mark][0]} data-tip=${CHANGE[mark][2]}>${CHANGE[mark][1]}</span>`}</span>
    <span class="tmeta">${n.permissions || ''}</span><span class="tmeta">${n.type ? `${n.user || n.uid}:${n.group || n.gid}` : ''}</span>
    <span class="tmeta" style="text-align:right">${n.type && n.type !== 'dir' ? bytes(n.size) : ''}</span><span class="tmeta">${n.mtime ? fmtTime(n.mtime) : ''}</span>
    <span class="row" style="justify-content:flex-end;gap:.5rem">${mark === 'M' && !isDir && mayRestore && against && html`<button type="button" class="link small-link" onClick=${() => setDiffAt(diffAt === n.path ? null : n.path)}>Diff…</button>`}
      ${!n.gone && mayRestore && html`<button type="button" class="link small-link" data-tip=${'Restore only ' + n.path + (isDir ? ', and what is in it' : '')} onClick=${() => onRestoreOnly(n.path)}>Restore…</button>`}</span>
  </div>`;
  return html`
    <div class="treebar">
      <span class="seg"><button type="button" class=${only ? '' : 'on'} onClick=${() => setOnly(false)}>All files</button><button type="button" class=${only ? 'on' : ''} disabled=${!cd} onClick=${showOnly}>Only changes${cd ? html` <b>${cd.total}</b>` : ''}</button></span>
      <form class="row" style="gap:.4rem;flex:0 1 22rem;min-width:0" onSubmit=${(e) => { e.preventDefault(); if (q.trim().length >= 2) find.run(() => api.find(fqdn, pattern(q.trim()), snap.id)); }}>
        <input type="search" placeholder="Find a name, such as *.conf" aria-label="Find a name in this snapshot" value=${q} onInput=${(e) => { setQ(e.target.value); if (!e.target.value) find.reset(); }} style="flex:1;min-width:0" />
        <button class="small" disabled=${find.busy}>Find</button></form>
      <button type="button" class="link" onClick=${expandAll}>Expand all</button><button type="button" class="link" onClick=${() => setOpen(new Set(['/']))}>Collapse all</button>
      <span class="muted row" style="margin-left:auto;gap:.35rem">${older.length ? html`Changes against
        <select value=${against} onChange=${(e) => setAgainst(e.target.value)} aria-label="The older snapshot to compare with">
          ${older.map((s, i) => html`<option value=${s.id}>${s.short_id} · ${fmtTime(s.time)}${i === 0 ? ' (the one before)' : ''}</option>`)}</select>
        ${cd && html`${chip('+', cd.added)}${chip('-', cd.removed)}${chip('M', cd.content_changed)}${chip('U', cd.metadata_changed)}${chip('T', cd.type_changed)}${cd.total === 0 ? 'nothing changed' : ''}`}`
        : 'The job’s first snapshot: nothing to compare it with.'}</span>
    </div>
    <${ErrorBox} error=${ch.error || find.error} />
    ${cd?.capped && html`<div class="banner info">The first ${(MAX_CHANGE_PAGES * 1000).toLocaleString('en')} of ${cd.total.toLocaleString('en')} changes are marked; <code>holdfast snapshot changes</code> lists them all.</div>`}
    ${find.busy && html`<${Loading} what="this snapshot" />`}
    <div class="scrollx"><div style="min-width:840px">
      <div class="trow head"><span>Name</span><span>Change</span><span>Mode</span><span>Owner</span><span style="text-align:right">Size</span><span>Modified</span><span></span></div>
      ${find.done ? html`
        <div class="trow note"><span class="muted">${found.length ? `${found.length} ${found.length === 1 ? 'match' : 'matches'} for “${q}” in this snapshot` : `Nothing in this snapshot matches “${q}”.`}
          <button type="button" class="link small-link" onClick=${() => { setQ(''); find.reset(); }}>Show the tree</button></span></div>
        ${found.map((n) => row({ n, mark: idx.marks.get(n.path), depth: 0, isDir: false, isOpen: false, path: true }))}`
      : rows.map((r) => r.n ? row(r)
        : r.diff ? html`<div class="trow note"><span style=${`padding-left:${r.depth * 18 + 34}px`}><${DiffOnHost} fqdn=${fqdn} id=${snap.id} against=${against} path=${r.diff} /></span></div>`
        : r.more ? html`<div class="trow note"><span class="muted" style=${`padding-left:${r.depth * 18 + 34}px`}>${r.more.toLocaleString('en')} more · <button type="button" class="link small-link" onClick=${() => read(r.dir, r.page + 1)}>show the next 1 000</button></span></div>`
        : r.error ? html`<div class="trow note"><span style=${`padding-left:${r.depth * 18 + 34}px`}><${ErrorBox} error=${r.error} /></span></div>`
        : html`<div class="trow note"><span class="muted" style=${`padding-left:${r.depth * 18 + 34}px`}>${r.note}</span></div>`)}
      ${!find.done && only && cd && cd.total === 0 && html`<div class="trow note"><span class="muted">Nothing changed against the older snapshot.</span></div>`}
    </div></div>
    <div class="legend">${Object.entries(CHANGE).map(([k, [cls, label]]) => html`<span><span class=${'chg ' + cls}>${label.split(' ')[0]}</span> ${label.split(' ').slice(1).join(' ')}</span>`)}
      <span>restic diff’s marks against the older snapshot; hover one for what it means. Names and metadata only: to read a file, restore it where the agent runs.</span></div>`;
}

function DiffOnHost({ fqdn, id, against, path }) {
  const act = useAction();
  const [status, setStatus] = useState(null);
  useEffect(() => {
    if (!act.done) return;
    const t = setInterval(async () => {
      try {
        const st = await api.restorePairStatus(fqdn, act.done.commands[0], act.done.commands[1], act.done.path);
        setStatus(st);
        if (st.status !== 'queued') clearInterval(t);
      } catch (err) { setStatus({ status: 'failed', error: err.message }); clearInterval(t); }
    }, 2000);
    return () => clearInterval(t);
  }, [act.done]);
  return html`<span>
    <button class="small" disabled=${act.busy || act.done} onClick=${() => act.run(() => api.restorePair(fqdn, id, { path, against }))}>Restore both versions to diff where the agent runs</button>
    <${ErrorBox} error=${act.error} />
    ${act.done && !status && html`<span class="muted"> queued…</span>`}
    ${status && status.status === 'queued' && html`<span class="muted"> waiting for the agent…</span>`}
    ${status && status.status === 'failed' && html`<div class="banner error">${status.error}</div>`}
    ${status && status.status === 'done' && html`<div class="banner ok">Both versions are on ${fqdn}, readable by root only. Compare them there:<pre>${status.diff}</pre></div>`}
  </span>`;
}

// SiteBanner is one site's word on a snapshot that is not simply kept: its deletion waits, with Cancel, or it is gone.
function SiteBanner({ where, s, mayDelete, onCancel }) {
  if (!s) return null;
  if (s.gone_at) {
    const f = fate(s);
    return html`<div class=${'banner ' + (f.kind === 'error' ? 'error' : 'info')}><span>${where}: ${f.text}</span></div>`;
  }
  if (s.deletion_requested_at) {
    return html`<div class="banner warning"><span>${where}: deletion asked ${fmtTime(s.deletion_requested_at)}${s.deletion_requested_by ? ` by ${s.deletion_requested_by}` : ''}${s.deletion_reason ? ` (“${s.deletion_reason}”)` : ''}; restic forgets it once its 7 days have passed.</span>
      ${mayDelete && html` <button class="small" onClick=${onCancel}>Cancel the deletion</button>`}</div>`;
  }
  return null;
}

export function Snapshot({ fqdn, id, tab, vault, hosts, settings }) {
  const all = usePoll(() => api.snapshots(fqdn, { all: true }), [fqdn], 30000);
  const hostPoll = usePoll(() => api.host(fqdn), [fqdn], 0);
  // With an off-site bucket: the snapshot's copy in site B, its state there, Restore from it, and deleting it (decision 053).
  const withOff = !vault && !!hostPoll.data?.offsite;
  const listB = usePoll(() => (withOff ? api.offsiteSnapshots(fqdn, { all: true }) : Promise.resolve([])), [fqdn, withOff], 60000);
  const [sel, setSel] = useState(EVERYTHING);
  const [dlg, setDlg] = useState(false);
  const [deleted, setDeleted] = useState(null);
  const act = useAction();
  if (all.error || hostPoll.error) return html`<${ErrorBox} error=${all.error || hostPoll.error} />`;
  if (!all.data || !hostPoll.data || (withOff && !listB.data && !listB.error)) return html`<${Loading} what="the snapshot" />`;
  // The off-site pages restore nothing themselves: a restore from off-site is asked on the agent's own page (decision 039).
  const host = { data: vault ? { ...hostPoll.data, actions: (hostPoll.data.actions || []).filter((a) => !a.startsWith('restore.')) } : hostPoll.data };
  const h = host.data;
  const off = withOff && !!listB.data;
  const copies = off ? copiesByOriginal(listB.data) : new Map();
  let snap = all.data.find((s) => s.id.startsWith(id));
  // A snapshot site A has no record of any more, whose copy site B still holds: its page restores from the copy.
  const bOnly = !snap && off ? [...copies.entries()].find(([orig]) => orig.startsWith(id)) : null;
  if (bOnly) snap = { id: bOnly[0], short_id: bOnly[0].slice(0, 8), time: bOnly[1].time, job_id: bOnly[1].job_id, paths: bOnly[1].paths, tags: bOnly[1].tags, gone_at: bOnly[1].time, deleted_by_holdfast: true, unknown: true };
  if (!snap) return html`<h1>Snapshot ${id}</h1><div class="banner error">No snapshot with this ID exists in this repository.</div>`;
  const copy = off ? copies.get(snap.id) || null : null;
  const oj = off ? h.offsite.jobs.find((x) => x.id === snap.job_id) : null;
  const p = point(snap.unknown ? null : snap, copy, off, !!oj?.excluded, snap.id);
  const aGone = p.sa === 'deleted';
  const bHolds = off && (p.sb === 'copied' || p.sb === 'waits');
  // A removed PostgreSQL job's dumps still restore into a database: the snapshot list carries what the job was.
  const job = h.jobs.find((j) => j.id === snap.job_id) || (snap.postgresql ? { id: snap.job_id, engine: 'postgresql', postgresql: snap.postgresql, removed: true } : undefined);
  const pg = isPG(job);
  const older = all.data.filter((s) => s.job_id === snap.job_id && !s.gone_at && s.time < snap.time);
  const base = link('agents', fqdn, 'snapshots', snap.id);
  const t = tab === 'changes' ? 'files' : tab; // Changes is part of Files now; its old address opens Files
  const mayDelete = can(h, 'snapshot.delete');
  const canDelete = mayDelete && (p.sa === 'kept' || p.sb === 'copied');
  const live = (b) => b && b.id && !b.gone_at ? b : null;
  // Files, with what changed, reads the snapshot in site A; Restore reads it there, or its copy in site B.
  const tabs = (pg ? [['restore', 'Restore', 'restore.new_dir']] : aGone ? (bHolds ? [['restore', 'Restore', 'restore.new_dir']] : [])
    : [['files', 'Files', 'browse'], ['restore', 'Restore', 'restore.new_dir']]).filter(([, , perm]) => can(h, perm));
  const shown = tabs.some(([k]) => k === t) ? t : tabs[0]?.[0];
  const f = fate(snap);
  const reload = () => { all.reload(); listB.reload(); };
  return html`
    <div class="spread">
      <div style="min-width:0">
        ${pg ? html`<h1 class="row" style="gap:.6rem">${job.postgresql.globals ? 'Roles and other globals' : html`Dump of ${job.postgresql.database}`} <span class="pill">${dbIcon(12)} ${job.postgresql.connection}</span></h1>
            <p class="muted">Snapshot <span class="mono">${snap.short_id}</span> · <a href=${link('agents', fqdn)}>${fqdn}</a> · <a href=${link('agents', fqdn, 'jobs', snap.job_id)}>${snap.job_id}</a> · ${fmtTime(snap.time)} · <code>${dumpFile(job)}</code> ${bytes(snap.total_bytes ?? copy?.total_bytes)}</p>`
          : html`<h1>Snapshot <span class="mono">${snap.short_id}</span></h1>
            <p class="muted"><a href=${link('agents', fqdn)}>${fqdn}</a> · ${snap.job_id ? html`<a href=${link('agents', fqdn, 'jobs', snap.job_id)}>${snap.job_id}</a>` : 'not made by Holdfast'} · ${fmtTime(snap.time)} · ${snap.total_files ?? '—'} files · ${bytes(snap.total_bytes)} · ${(snap.paths || []).map((x) => html`<code>${x}</code> `)}</p>`}
        ${snap.original && html`<p class="muted">A copy of <span class="mono">${snap.original.slice(0, 8)}</span> in site A.</p>`}
        ${off && html`<p class="muted">Site A: ${WORD_A[p.sa]} · Site B: ${WORD_B[p.sb]}${copy ? html` (copy <span class="mono">${p.copyShort}</span>)` : ''}</p>`}
        ${!aGone && (snap.kept_by || []).length > 0 && html`<p class="muted">Kept: ${snap.kept_by.join(', ')}</p>`}
      </div>
      <div class="row">
        ${canDelete && html`<button class="danger-line" onClick=${() => setDlg(true)}>Delete…</button>`}
        <${Menu} label="More snapshot actions" items=${[
          { label: 'Copy the full ID', run: () => { try { navigator.clipboard.writeText(snap.id).catch(() => {}); } catch { /* not allowed here */ } } },
          snap.job_id && { label: 'Back to the job’s calendar', href: link('agents', fqdn, 'jobs', snap.job_id) },
        ]} />
      </div>
    </div>
    ${!snap.unknown && (snap.gone_at || snap.deletion_requested_at) ? html`<${SiteBanner} where=${off ? 'Site A' : 'This snapshot'} s=${snap} mayDelete=${mayDelete}
        onCancel=${() => act.run(() => api.cancelDeletion(fqdn, snap.id)).then(reload)} />`
      : !aGone && f.text && f.kind !== 'ok' && html`<div class=${'banner ' + (f.kind || 'info')}><span>${f.text}</span></div>`}
    ${copy && html`<${SiteBanner} where=${`Site B, copy ${p.copyShort}`} s=${copy} mayDelete=${mayDelete} onCancel=${() => act.run(() => api.offsiteCancelDeletion(fqdn, copy.id)).then(reload)} />`}
    ${aGone && !bHolds && html`<div class="banner info"><span>Nothing is left to restore: ${off ? 'neither site holds it any more' : 'it is deleted'}.</span></div>`}
    ${aGone && bHolds && html`<div class="banner info"><span>Site B still holds it: restore from site B’s copy below.</span></div>`}
    ${withOff && listB.error && html`<div class="banner warning">Site B's copies could not be read: ${listB.error.message}</div>`}
    <${ErrorBox} error=${act.error} />
    ${deleted && html`<div class="banner warning">${deleted.now ? 'Deleting now: the maintenance run forgets it, and Config history lists it.' : 'Deletion asked: restic forgets it once its 7 days have passed. Until then you can cancel.'}</div>`}
    ${tabs.length > 0 && html`<div class="tabs">${tabs.map(([k, label]) => html`<a class=${shown === k ? 'on' : ''} href=${base + '/' + k}>${label}</a>`)}</div>
      ${shown === 'files' && html`<section class="list tabpanel"><${FileTree} fqdn=${fqdn} snap=${snap} host=${h} older=${older} onRestoreOnly=${(x) => { setSel(onlyThis(x)); location.hash = base + '/restore'; }} /></section>`}
      ${shown === 'restore' && (pg ? html`<${PGRestore} fqdn=${fqdn} snap=${snap} host=${h} job=${job} copy=${live(copy)} aGone=${aGone} settings=${settings} />`
        : html`<${Restore} fqdn=${fqdn} snap=${snap} host=${h} hosts=${hosts} sel=${sel} setSel=${setSel} copy=${live(copy)} aGone=${aGone} />`)}`}
    ${!pg && !can(h, 'browse') && html`<p class="muted">File names are for editors and admins (decision 033).</p>`}
    ${dlg && html`<${DeleteDialog} fqdn=${fqdn} points=${[p]} off=${off} site=${p.sa === 'kept' ? 'a' : 'b'}
      onClose=${() => setDlg(false)} onDone=${(r) => { setDlg(false); setDeleted(r); reload(); }} />`}`;
}

