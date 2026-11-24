import { html, useState, useEffect, usePoll, useAction, link, fmtTime, fmtDay, bytes, Pill, can, ErrorBox, Loading, Menu } from '../lib.js';
import { api } from '../api.js';
import { Restore, DeleteDialog, EVERYTHING, onlyThis } from './restore.js';

// fate is what happens to a snapshot, in a word or two: kept and why, waiting to be deleted, or gone.
export function fate(s) {
  if (s.gone_at && s.deleted_by_holdfast) return { kind: '', text: `deleted by Holdfast ${fmtTime(s.gone_at)}` + (s.really_gone_by ? `; S3's old versions expire by ${fmtDay(s.really_gone_by)}` : '; its data is freed at the next prune') };
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
        <td><a class="mono" href=${link('hosts', fqdn, 'snapshots', f.snapshot) + '/files?path=' + encodeURIComponent(m.path.slice(0, m.path.lastIndexOf('/')) || '/')}>${f.short_id}</a> · ${fmtTime(f.time)}</td>
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
  return html`<p class="muted">Names and metadata, like <code>restic ls --long</code>. Holdfast never shows file contents: to read a file, restore it to a root-only directory on the host.</p>
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
    <button class="small" disabled=${act.busy || act.done} onClick=${() => act.run(() => api.restorePair(fqdn, id, { path, against }))}>Restore both versions to diff on the host</button>
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

export function Snapshot({ fqdn, id, tab, vault, hosts }) {
  const all = usePoll(() => api.snapshots(fqdn, { all: true }), [fqdn], 30000);
  const hostPoll = usePoll(() => api.host(fqdn), [fqdn], 0);
  // With an off-site bucket: the snapshot's copy, for its line here, Restore from it, and deleting it (decision 053).
  const withOff = !vault && !!hostPoll.data?.offsite;
  const copies = usePoll(() => (withOff ? api.offsiteCopies(fqdn) : Promise.resolve([])), [fqdn, withOff], 60000);
  const [sel, setSel] = useState(EVERYTHING);
  const [dlg, setDlg] = useState(null);
  const [deleted, setDeleted] = useState(null);
  const act = useAction();
  if (all.error || hostPoll.error) return html`<${ErrorBox} error=${all.error || hostPoll.error} />`;
  if (!all.data || !hostPoll.data) return html`<${Loading} what="the snapshot" />`;
  // The off-site pages restore nothing themselves: a restore from off-site is asked on the host's own page (decision 039).
  const host = { data: vault ? { ...hostPoll.data, actions: (hostPoll.data.actions || []).filter((a) => !a.startsWith('restore.')) } : hostPoll.data };
  const snap = all.data.find((s) => s.id.startsWith(id));
  if (!snap) return html`<h1>Snapshot ${id}</h1><div class="banner error">No snapshot with this ID exists in this repository.</div>`;
  const older = all.data.filter((s) => s.job_id === snap.job_id && !s.gone_at && s.time < snap.time);
  const f = fate(snap);
  const base = link('hosts', fqdn, 'snapshots', snap.id);
  const tabs = [['files', 'Files', 'browse'], ['changes', 'Changes', 'browse'], ['restore', 'Restore', 'restore.new_dir']].filter(([, , p]) => can(host.data, p));
  const t = tab.split('?')[0];
  const copy = withOff ? (copies.data || []).find((c) => c.original === snap.id) : null;
  const mayDelete = can(host.data, 'snapshot.delete');
  return html`
    <div class="spread">
      <div style="min-width:0">
        <h1>Snapshot <span class="mono">${snap.short_id}</span></h1>
        <p class="muted"><a href=${link('hosts', fqdn)}>${fqdn}</a> · ${snap.job_id ? html`<a href=${link('hosts', fqdn, 'jobs', snap.job_id)}>${snap.job_id}</a>` : 'not made by Holdfast'} · ${fmtTime(snap.time)} · ${snap.total_files ?? '—'} files · ${bytes(snap.total_bytes)} · ${(snap.paths || []).map((p) => html`<code>${p}</code> `)}</p>
        ${snap.original && html`<p class="muted">A copy of <span class="mono">${snap.original.slice(0, 8)}</span> in primary.</p>`}
        ${copy && html`<p class="muted">Off-site copy <span class="mono">${copy.id.slice(0, 8)}</span>.</p>`}
      </div>
      <${Menu} label="More snapshot actions" items=${[
        { label: 'Copy the full ID', run: () => { try { navigator.clipboard.writeText(snap.id).catch(() => {}); } catch { /* not allowed here */ } } },
        !snap.gone_at && mayDelete && !snap.deletion_requested_at && { label: snap.deleted_in_primary_at ? 'Delete here too…' : 'Delete snapshot…', danger: true, run: () => setDlg('p') },
        copy && mayDelete && { label: 'Delete the off-site copy…', danger: true, run: () => setDlg('o') },
      ]} />
    </div>
    ${f.text && html`<div class=${'banner ' + (f.kind || 'info')}><span>${f.text}</span>
      ${snap.deletion_requested_at && !snap.gone_at && mayDelete && html` <button class="small" onClick=${() => act.run(() => api.cancelDeletion(fqdn, snap.id)).then(all.reload)}>Cancel the deletion</button>`}</div>`}
    <${ErrorBox} error=${act.error} />
    ${deleted && deleted.status === 'pending' && html`<div class="banner warning">Deletion asked: the daily run forgets it from ${fmtTime(deleted.prunable_at)}.</div>`}
    ${deleted && deleted.status === 'deleting' && html`<div class="banner warning">Deleting now (run ${deleted.run}). Config history lists it.</div>`}
    ${!snap.gone_at && tabs.length > 0 && html`<div class="tabs">${tabs.map(([k, label]) => html`<a class=${t === k ? 'on' : ''} href=${base + '/' + k}>${label}</a>`)}</div>
      ${t === 'files' && can(host.data, 'browse') && html`<${Files} fqdn=${fqdn} id=${snap.id} host=${host.data} onRestoreOnly=${(p) => { setSel(onlyThis(p)); location.hash = base + '/restore'; }} />`}
      ${t === 'changes' && can(host.data, 'browse') && html`<${Changes} fqdn=${fqdn} id=${snap.id} host=${host.data} older=${older} />`}
      ${t === 'restore' && can(host.data, 'restore.new_dir') && html`<${Restore} fqdn=${fqdn} snap=${snap} host=${host.data} hosts=${hosts} sel=${sel} setSel=${setSel} copy=${copy} />`}`}
    ${!can(host.data, 'browse') && html`<p class="muted">File names are for editors and admins (decision 033).</p>`}
    ${dlg && html`<${DeleteDialog} fqdn=${fqdn} id=${dlg === 'o' ? copy.id : snap.id} short=${dlg === 'o' ? copy.id.slice(0, 8) : snap.short_id} offsite=${dlg === 'o'} hasCopy=${!!copy}
      onClose=${() => setDlg(null)} onDone=${(r) => { setDlg(null); setDeleted(r); all.reload(); copies.reload(); }} />`}`;
}

