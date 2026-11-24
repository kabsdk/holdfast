import { html, useState, useEffect, usePoll, useAction, link, fmtTime, fmtDay, bytes, can, ErrorBox, Loading, Menu, isPG, dbIcon, dumpFile } from '../lib.js';
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

export function FindFile({ fqdn, job }) {
  const [text, setText] = useState('');
  const act = useAction();
  const found = (act.done || []).filter((f) => !job || f.job_id === job);
  return html`<div class="panel">
    <h3>Find a file in all snapshots</h3>
    <p class="muted">Like <code>restic find</code>: names and metadata, never contents.</p>
    <form class="row" onSubmit=${(e) => { e.preventDefault(); if (text.trim().length >= 2) act.run(() => api.find(fqdn, text.trim())); }}>
      <input placeholder="Path or name, e.g. nginx.conf" value=${text} onInput=${(e) => setText(e.target.value)} style="flex:1" />
      <button disabled=${act.busy}>Find</button>
    </form>
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

function Files({ fqdn, id, host, onRestoreOnly }) {
  const start = new URLSearchParams(location.hash.split('?')[1] || '').get('path') || '/';
  const [dir, setDir] = useState(start);
  const [page, setPage] = useState(1);
  const files = usePoll(() => api.files(fqdn, id, dir, page), [fqdn, id, dir, page], 0);
  const parts = dir.split('/').filter(Boolean);
  return html`<p class="muted">Names and metadata, like <code>restic ls --long</code>. Holdfast never shows file contents: to read a file, restore it to a root-only directory where the agent runs.</p>
    <div class="row"><a href="#" onClick=${(e) => { e.preventDefault(); setDir('/'); setPage(1); }}>/</a>
      ${parts.map((p, i) => html`<span>/</span><a href="#" onClick=${(e) => { e.preventDefault(); setDir('/' + parts.slice(0, i + 1).join('/')); setPage(1); }}>${p}</a>`)}</div>
    <${ErrorBox} error=${files.error} />
    ${!files.data ? html`<${Loading} />` : html`<table>
      <thead><tr><th>Name</th><th>Size</th><th>Owner</th><th>Mode</th><th>Modified</th><th></th></tr></thead>
      <tbody>${files.data.entries.map((n) => html`<tr>
        <td>${n.type === 'dir' ? html`<a href="#" onClick=${(e) => { e.preventDefault(); setDir(n.path); setPage(1); }}>${n.name}/</a>` : n.name}</td>
        <td>${n.type === 'dir' ? '' : bytes(n.size)}</td><td>${n.user || n.uid}:${n.group || n.gid}</td><td class="mono">${n.permissions}</td><td>${fmtTime(n.mtime)}</td>
        <td>${can(host, 'restore.new_dir') && html`<button class="small" onClick=${() => onRestoreOnly(n.path)}>Restore only this</button>`}</td>
      </tr>`)}</tbody></table>
      ${files.data.pages > 1 && html`<div class="pager"><button class="small" disabled=${page <= 1} onClick=${() => setPage(page - 1)}>Previous</button>
        <span>page ${files.data.page} of ${files.data.pages} · ${files.data.total} entries</span>
        <button class="small" disabled=${page >= files.data.pages} onClick=${() => setPage(page + 1)}>Next</button></div>`}`}`;
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

function Changes({ fqdn, id, host, older }) {
  const [against, setAgainst] = useState(older[0]?.id || '');
  const [page, setPage] = useState(1);
  const ch = usePoll(() => (against ? api.changes(fqdn, id, against, page) : Promise.resolve(null)), [fqdn, id, against, page], 0);
  if (older.length === 0) return html`<p class="muted">This is the oldest snapshot of the job: there is nothing to compare it with.</p>`;
  const marks = { '+': 'added', '-': 'removed', M: 'content changed', U: 'metadata only', T: 'type changed' };
  return html`<div class="row"><label>Compare with (older snapshot)
      <select value=${against} onChange=${(e) => { setAgainst(e.target.value); setPage(1); }}>
        ${older.map((s, i) => html`<option value=${s.id}>${s.short_id} · ${fmtTime(s.time)}${i === 0 ? ' (previous)' : ''}</option>`)}
      </select></label> → this snapshot</div>
    <p class="muted">What <code>restic diff --metadata</code> shows: the paths that changed, never their contents (spec §5.3).</p>
    <${ErrorBox} error=${ch.error} />
    ${!ch.data ? html`<${Loading} />` : html`
      <div class="chips"><span class="pill ok">+${ch.data.added} added</span><span class="pill error">−${ch.data.removed} removed</span>
        <span class="pill warning">${ch.data.content_changed} content changed</span><span class="pill">${ch.data.metadata_changed} metadata only</span></div>
      ${ch.data.changes.length === 0 ? html`<p class="muted">No differences.</p>` : html`<table>
        <thead><tr><th></th><th>Path</th><th></th></tr></thead>
        <tbody>${ch.data.changes.map((c) => html`<tr><td class="mono" title=${marks[c.modifier] || c.modifier}>${c.modifier}</td><td class="mono">${c.path}</td>
          <td>${c.modifier === 'M' && !c.path.endsWith('/') && can(host, 'restore.new_dir') && html`<${DiffOnHost} fqdn=${fqdn} id=${id} against=${against} path=${c.path} />`}</td></tr>`)}</tbody></table>`}
      ${ch.data.pages > 1 && html`<div class="pager"><button class="small" disabled=${page <= 1} onClick=${() => setPage(page - 1)}>Previous</button>
        <span>page ${ch.data.page} of ${ch.data.pages}</span><button class="small" disabled=${page >= ch.data.pages} onClick=${() => setPage(page + 1)}>Next</button></div>`}`}`;
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
  const t = tab;
  const mayDelete = can(h, 'snapshot.delete');
  const canDelete = mayDelete && (p.sa === 'kept' || p.sb === 'copied');
  const live = (b) => b && b.id && !b.gone_at ? b : null;
  // Files and Changes read the snapshot in site A; Restore reads it there, or its copy in site B.
  const tabs = (pg ? [['restore', 'Restore', 'restore.new_dir']] : aGone ? (bHolds ? [['restore', 'Restore', 'restore.new_dir']] : [])
    : [['files', 'Files', 'browse'], ['changes', 'Changes', 'browse'], ['restore', 'Restore', 'restore.new_dir']]).filter(([, , perm]) => can(h, perm));
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
      ${shown === 'files' && html`<${Files} fqdn=${fqdn} id=${snap.id} host=${h} onRestoreOnly=${(x) => { setSel(onlyThis(x)); location.hash = base + '/restore'; }} />`}
      ${shown === 'changes' && html`<${Changes} fqdn=${fqdn} id=${snap.id} host=${h} older=${older} />`}
      ${shown === 'restore' && (pg ? html`<${PGRestore} fqdn=${fqdn} snap=${snap} host=${h} job=${job} copy=${live(copy)} aGone=${aGone} settings=${settings} />`
        : html`<${Restore} fqdn=${fqdn} snap=${snap} host=${h} hosts=${hosts} sel=${sel} setSel=${setSel} copy=${live(copy)} aGone=${aGone} />`)}`}
    ${!pg && !can(h, 'browse') && html`<p class="muted">File names are for editors and admins (decision 033).</p>`}
    ${dlg && html`<${DeleteDialog} fqdn=${fqdn} points=${[p]} off=${off} site=${p.sa === 'kept' ? 'a' : 'b'}
      onClose=${() => setDlg(false)} onDone=${(r) => { setDlg(false); setDeleted(r); reload(); }} />`}`;
}

