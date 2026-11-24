import { html, useState, useEffect, usePoll, useAction, link, go, fmtTime, fmtDay, bytes, Pill, Snap, can, ErrorBox, Loading } from '../lib.js';
import { api } from '../api.js';

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

function Restore({ fqdn, snap, host, only }) {
  const [leaveOut, setLeaveOut] = useState('');
  const [target, setTarget] = useState('new');
  const [dir, setDir] = useState('/srv/recovered');
  const [overwrite, setOverwrite] = useState('');
  const [reason, setReason] = useState('');
  // Onto another host: an admin's. That host reads this one's repository through the gateway, for this
  // restore only (decision 041).
  const [toHost, setToHost] = useState('');
  const act = useAction();
  const [result, setResult] = useState(null);
  const bounds = host.agent?.restore_bounds;
  const isDump = (snap.tags || []).some((t) => t.startsWith('dump='));
  const elsewhere = toHost.trim() !== '' && toHost.trim() !== fqdn;
  const body = (dryRun) => ({
    target, directory: target === 'directory' ? dir : undefined,
    include: only.length ? only : undefined,
    exclude: !only.length && leaveOut.trim() ? leaveOut.split('\n').map((x) => x.trim()).filter(Boolean) : undefined,
    overwrite: target !== 'new' && overwrite ? overwrite : undefined, dry_run: dryRun, message: reason || undefined,
    ...(elsewhere ? { to_host: toHost.trim() } : {}),
  });
  useEffect(() => {
    if (!act.done || !act.done.command) return;
    const t = setInterval(async () => {
      const r = await api.restores().catch(() => null);
      const ev = r && r.done.find((e) => e.command_id === act.done.command);
      if (ev) { setResult(ev); clearInterval(t); }
    }, 2000);
    return () => clearInterval(t);
  }, [act.done]);
  const inPlace = can(host, 'restore.in_place');
  return html`<div class="grid2"><div>
    <h3>1 · What to restore</h3>
    ${only.length ? html`<p>Only <code>${only.join(', ')}</code>. <a href="#" onClick=${(e) => { e.preventDefault(); go(location.hash.slice(1).replace(/\?.*/, '')); location.reload(); }}>Restore everything instead</a></p>`
      : html`<p class="muted">Everything in the snapshot (paths: <code>${(snap.paths || []).join(' ')}</code>). To leave something out, give its path, one per line:</p>
        <textarea placeholder="/etc/ssl/private" value=${leaveOut} onInput=${(e) => setLeaveOut(e.target.value)} />`}
    <h3>2 · Target on ${elsewhere ? toHost : fqdn}</h3>
    ${isDump && html`<p class="muted">This snapshot was made by a dump job, before 0.6.0: one file, restored into a new directory. Loading it is done on the host.</p>`}
    <label class="field"><input type="radio" checked=${target === 'new'} onChange=${() => setTarget('new')} /> A new restore directory under ${elsewhere ? 'that host’s restore root' : bounds?.root || '/var/restore'}, made by the agent, root-only (mode 0700). Files keep their owner, mode and ACLs.</label>
    ${inPlace && !isDump && !elsewhere && html`<label class="field"><input type="radio" checked=${target === 'in_place'} onChange=${() => setTarget('in_place')} disabled=${bounds && !bounds.allow_in_place} />
      The original location: writes into the live paths. The agent first backs up exactly what it will replace. ${bounds && !bounds.allow_in_place ? html`<span class="muted">(this host does not allow it: restore.allow_in_place in agent.yaml)</span>` : ''}</label>`}
    ${inPlace && !isDump && html`<label class="field"><input type="radio" checked=${target === 'directory'} onChange=${() => setTarget('directory')} disabled=${!elsewhere && bounds && !bounds.allow_other_directories} />
      Another directory: <input value=${dir} onInput=${(e) => setDir(e.target.value)} disabled=${target !== 'directory'} />
      ${!elsewhere && bounds && !bounds.allow_other_directories ? html`<span class="muted"> (this host does not allow it)</span>` : ''}</label>`}
    ${inPlace && html`<details><summary>Onto another host</summary>
      <p class="muted">Restore this snapshot on another host, which must allow it (restore.allow_other_hosts in its agent.yaml). That host may read
        ${fqdn}'s repository through this server for this restore only, and never write to it. Its root learns ${fqdn}'s repository password.</p>
      <label class="field"><span>Host</span><input value=${toHost} onInput=${(e) => { setToHost(e.target.value); if (target === 'in_place') setTarget('new'); }} placeholder="the other host's FQDN" /></label>
    </details>`}
    ${target !== 'new' && html`<label class="field"><span>If a file already exists (restic --overwrite)</span>
      <select value=${overwrite} onChange=${(e) => setOverwrite(e.target.value)}>
        <option value="">restic's default</option><option value="always">always replace</option><option value="if-changed">replace only if the content differs</option>
        <option value="if-newer">replace only if the snapshot's copy is newer</option><option value="never">never replace: add missing files only</option>
      </select></label>`}
  </div><div>
    <h3>3 · Run</h3>
    <p class="muted">Runs as root on ${fqdn}, through its agent. The repository and its password stay on the host; you never see them.</p>
    <label class="field"><span>Reason (audit log)</span><input value=${reason} onInput=${(e) => setReason(e.target.value)} placeholder="such as a ticket" /></label>
    <div class="row">
      <button disabled=${act.busy} onClick=${() => { setResult(null); act.run(() => api.restore(fqdn, snap.id, body(true))); }}>Dry run</button>
      <button class=${target === 'in_place' ? 'warn' : 'primary'} disabled=${act.busy} onClick=${() => { setResult(null); act.run(() => api.restore(fqdn, snap.id, body(false))); }}>
        ${target === 'in_place' ? 'Restore into the live paths' : only.length ? `Restore ${only.length} paths` : 'Restore the snapshot'}</button>
    </div>
    <${ErrorBox} error=${act.error} />
    ${act.done && !result && html`<div class="banner info">Queued; the agent picks it up on its next poll, within 24 hours.</div>`}
    ${result && html`<div class=${'banner ' + (result.status === 'success' ? 'ok' : 'error')}>
      ${result.detail?.dry_run === 'true' ? 'Dry run: nothing was written. ' : ''}${result.status}: ${result.detail?.files_restored || 0} of ${result.total_files} files
      ${result.detail?.directory ? html` into <code>${result.detail.directory}</code>` : ''}${result.detail?.pre_restore ? html`; safety backup <code>${result.detail.pre_restore.slice(0, 8)}</code>` : ''}
      ${result.error && html`<div>${result.error}</div>`}</div>`}
  </div></div>`;
}

function DeleteSnapshot({ fqdn, snap, host, onDone }) {
  const [confirmText, setConfirm] = useState('');
  const [reason, setReason] = useState('');
  const act = useAction();
  const short = fqdn.split('.')[0];
  if (!can(host, 'snapshot.delete') || snap.gone_at) return null;
  return html`<div class="panel"><h3>${snap.deleted_in_primary_at ? 'Delete here too' : 'Delete this snapshot'}</h3>
    <p class="muted">restic forgets it after 7 days, unless the deletion is cancelled; its space is freed at the next prune, and S3 keeps the old versions until the bucket's lifecycle rule expires them. Delete now skips the 7 days.</p>
    <label class="field"><span>Why (audit log)</span><input value=${reason} onInput=${(e) => setReason(e.target.value)} /></label>
    <div class="row">
      ${snap.deletion_requested_at ? html`<button onClick=${() => act.run(() => api.cancelDeletion(fqdn, snap.id)).then(onDone)}>Cancel the deletion</button>`
        : html`<button onClick=${() => act.run(() => api.deleteSnapshot(fqdn, snap.id, { message: reason })).then(onDone)}>Delete after 7 days</button>`}
      <input placeholder=${'type ' + short + ' to Delete now'} value=${confirmText} onInput=${(e) => setConfirm(e.target.value)} />
      <button class="danger" disabled=${confirmText !== short || act.busy} onClick=${() => act.run(() => api.deleteSnapshot(fqdn, snap.id, { message: reason, now: true, confirm: confirmText })).then(onDone)}>Delete now</button>
    </div>
    <${ErrorBox} error=${act.error} />
    ${act.done && act.done.status === 'pending' && html`<div class="banner warning">Pending: the daily run forgets it from ${fmtTime(act.done.prunable_at)}.</div>`}
    ${act.done && act.done.status === 'deleting' && html`<div class="banner warning">Deleting now (run ${act.done.run}). Config history lists it.</div>`}
  </div>`;
}

export function Snapshot({ fqdn, id, tab, vault }) {
  const all = usePoll(() => api.snapshots(fqdn, { all: true }), [fqdn], 30000);
  const hostPoll = usePoll(() => api.host(fqdn), [fqdn], 0);
  const [only, setOnly] = useState([]);
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
  return html`<h1>Snapshot <span class="mono">${snap.short_id}</span></h1>
    ${f.text && html`<div class=${'banner ' + (f.kind || 'info')}>${f.text}</div>`}
    <dl class="facts"><dt>Host</dt><dd><a href=${link('hosts', fqdn)}>${fqdn}</a></dd><dt>Job</dt><dd>${snap.job_id ? html`<a href=${link('hosts', fqdn, 'jobs', snap.job_id)}>${snap.job_id}</a>` : html`<span class="muted">none: not made by Holdfast</span>`}</dd>
      <dt>Paths</dt><dd class="mono">${(snap.paths || []).join(' ')}</dd><dt>Taken</dt><dd>${fmtTime(snap.time)}</dd>
      <dt>Files · size</dt><dd>${snap.total_files ?? '—'} · ${bytes(snap.total_bytes)}</dd><dt>ID</dt><dd class="mono">${snap.id}</dd>
      ${snap.original && html`<dt>Copied from</dt><dd class="mono">${snap.original} <span class="muted">· in primary</span></dd>`}</dl>
    ${!snap.gone_at && tabs.length > 0 && html`<div class="tabs">${tabs.map(([k, label]) => html`<a class=${t === k ? 'on' : ''} href=${base + '/' + k}>${label}</a>`)}</div>
      ${t === 'files' && can(host.data, 'browse') && html`<${Files} fqdn=${fqdn} id=${snap.id} host=${host.data} onRestoreOnly=${(p) => { setOnly([p]); go(base.slice(1) + '/restore'); }} />`}
      ${t === 'changes' && can(host.data, 'browse') && html`<${Changes} fqdn=${fqdn} id=${snap.id} host=${host.data} older=${older} />`}
      ${t === 'restore' && can(host.data, 'restore.new_dir') && html`<${Restore} fqdn=${fqdn} snap=${snap} host=${host.data} only=${only} />`}`}
    ${!can(host.data, 'browse') && html`<p class="muted">File names are for editors and admins (decision 033).</p>`}
    <${DeleteSnapshot} fqdn=${fqdn} snap=${snap} host=${host.data} onDone=${all.reload} />`;
}

