// A snapshot's Restore tab (spec §5.4), one column: from, what, where, then the Run bar; a PostgreSQL dump's, into a
// new database beside the original, in place, or as its file (decision 055). And the dialog that deletes restore points
// in site A, site B or both (§5.3). Every rule is the server's and the agent's; this asks and shows.
import { html, useState, useEffect, useRef, useAction, can, ErrorBox, shortName, dumpFile, onOffsite, SiteTag } from '../lib.js';
import { api } from '../api.js';

// The selection: everything ticked and you untick (Left out, restic's --exclude), or nothing ticked and you tick
// (Only, --include). restic takes one or the other, never both, so a tick that would need both is greyed out.
export const EVERYTHING = { mode: 'except', paths: [] };
export const onlyThis = (path) => ({ mode: 'only', paths: [path] });
const inside = (p, q) => p === q || p.startsWith(q.endsWith('/') ? q : q + '/');

function stateOf(sel, path) {
  const hit = sel.paths.some((q) => inside(path, q));
  const ancestor = sel.paths.some((q) => q !== path && inside(path, q));
  const below = sel.paths.some((q) => q !== path && inside(q, path));
  const on = sel.mode === 'except' ? !hit : hit;
  return { on, dis: ancestor, mixed: !hit && below };
}

function toggle(sel, path) {
  const rest = sel.paths.filter((q) => !inside(q, path));
  return { ...sel, paths: sel.paths.includes(path) ? rest : [...rest, path] };
}

// A folder's entries, a page at a time, as Files shows them (restic ls).
function Folder({ fqdn, id, path, depth, sel, setSel }) {
  const [pages, setPages] = useState([]);
  const [more, setMore] = useState(null);
  const [loaded, setLoaded] = useState(false);
  const [err, setErr] = useState(null);
  const load = async (page) => {
    try {
      const r = await api.files(fqdn, id, path, page);
      setPages((p) => [...p, ...r.entries]);
      setMore(r.page < r.pages ? { next: r.page + 1 } : null);
      setLoaded(true);
    } catch (e) { setErr(e); }
  };
  useEffect(() => { load(1); }, [fqdn, id, path]);
  const pad = 'padding-left:' + (depth * 1.6 + 2.6) + 'rem';
  if (err) return html`<${ErrorBox} error=${err} />`;
  if (!loaded) return html`<div class="tnode muted" style=${pad}>Loading…</div>`;
  if (!pages.length) return html`<div class="tnode muted" style=${pad}>empty</div>`;
  return html`${pages.map((n) => html`<${Node} key=${n.path} fqdn=${fqdn} id=${id} path=${n.path} name=${n.name + (n.type === 'dir' ? '/' : '')}
      dir=${n.type === 'dir'} depth=${depth} sel=${sel} setSel=${setSel} />`)}
    ${more && html`<div class="tnode" style=${pad}><button type="button" class="link" onClick=${() => { const n = more.next; setMore(null); load(n); }}>Show more</button></div>`}`;
}

function Node({ fqdn, id, path, name, dir, depth, sel, setSel }) {
  const [open, setOpen] = useState(false);
  const st = stateOf(sel, path);
  return html`<div role="treeitem" aria-expanded=${dir ? (open ? 'true' : 'false') : null}>
    <div class="tnode" style=${'padding-left:' + depth * 1.6 + 'rem'}>
      ${dir ? html`<button type="button" class="fold" aria-label=${(open ? 'Close ' : 'Open ') + name} onClick=${() => setOpen(!open)}>${open ? '▾' : '▸'}</button>` : html`<span class="fold"></span>`}
      <label class=${st.dis ? 'dis' : ''}>
        <input type="checkbox" checked=${st.on} disabled=${st.dis} ref=${(el) => { if (el) el.indeterminate = st.mixed; }} onChange=${() => setSel(toggle(sel, path))} />
        <span class="mono">${name}</span>
      </label>
    </div>
    ${dir && open && html`<${Folder} fqdn=${fqdn} id=${id} path=${path} depth=${depth + 1} sel=${sel} setSel=${setSel} />`}
  </div>`;
}

function What({ fqdn, snap, sel, setSel }) {
  const everything = sel.mode === 'except' && sel.paths.length === 0;
  const nothing = sel.mode === 'only' && sel.paths.length === 0;
  const what = everything ? 'Everything' : nothing ? 'Nothing ticked' : sel.mode === 'except' ? `Everything except ${sel.paths.length}` : `Only ${sel.paths.length}`;
  return html`<section class="list" aria-labelledby="what-h">
    <div class="list-head"><h2 id="what-h"><span class="step">1</span>What to restore</h2><span class="muted">${what}</span></div>
    <div class="tree" role="tree" aria-label="The snapshot's files">
      <div class="tnode">
        <span class="fold"></span>
        <label><input type="checkbox" checked=${everything} ref=${(el) => { if (el) el.indeterminate = !everything && !nothing; }}
          onChange=${() => setSel(sel.mode === 'except' ? { mode: 'only', paths: [] } : EVERYTHING)} /> <b>Everything in the snapshot</b></label>
      </div>
      ${(snap.paths || []).map((p) => html`<${Node} key=${p} fqdn=${fqdn} id=${snap.id} path=${p} name=${p} dir=${true} depth=${1} sel=${sel} setSel=${setSel} />`)}
    </div>
    ${sel.paths.length > 0 && html`<div class="chips restore-chips"><span class="muted">${sel.mode === 'except' ? 'Left out' : 'Only'}</span>
      ${sel.paths.map((p) => html`<span class="chip-x"><code>${p}</code><button type="button" aria-label=${(sel.mode === 'except' ? 'Put back ' : 'Remove ') + p}
        onClick=${() => setSel(toggle(sel, p))}>×</button></span>`)}
      <span class="muted" style="flex-basis:100%">${sel.mode === 'except' ? 'restic restores everything else (--exclude).' : 'restic restores these alone (--include).'}</span></div>`}
  </section>`;
}

// useResult follows a queued restore until the agent reports it, in Restores' list.
function useResult(done) {
  const [result, setResult] = useState(null);
  useEffect(() => {
    setResult(null);
    if (!done || !done.command) return undefined;
    const t = setInterval(async () => {
      const r = await api.restores().catch(() => null);
      const ev = r && r.done.find((e) => e.command_id === done.command);
      if (ev) { setResult(ev); clearInterval(t); }
    }, 2000);
    return () => clearInterval(t);
  }, [done]);
  return [result, setResult];
}

export function Restore({ fqdn, snap, host, hosts, sel, setSel, copy, aGone }) {
  const short = shortName(fqdn);
  const [from, setFrom] = useState(aGone ? 'offsite' : 'primary');
  const [toHost, setToHost] = useState(fqdn);
  const [target, setTarget] = useState('new');
  const [dir, setDir] = useState('/srv/recovered');
  const [overwrite, setOverwrite] = useState('');
  const [reason, setReason] = useState('');
  const act = useAction();
  const [result, setResult] = useResult(act.done);
  const offsite = from === 'offsite';
  const isDump = (snap.tags || []).some((t) => t.startsWith('dump='));
  // Onto another host (decision 041): an admin's, on both hosts, and only a host whose agent.yaml allows it.
  const others = offsite || isDump ? [] : (hosts || []).filter((h) => h.fqdn !== fqdn && h.agent?.restore_bounds?.allow_other_hosts && can(h, 'restore.in_place') && can(host, 'restore.in_place'));
  const th = toHost === fqdn ? host : (hosts || []).find((h) => h.fqdn === toHost) || host;
  const bounds = th.agent?.restore_bounds;
  const thShort = shortName(th.fqdn);
  const elsewhere = th.fqdn !== fqdn;
  const inPlaceOk = !elsewhere && !offsite && !isDump && can(host, 'restore.in_place') && !(bounds && !bounds.allow_in_place);
  const dirOk = !isDump && can(host, 'restore.in_place') && !(bounds && !bounds.allow_other_directories);
  const kind = (target === 'in_place' && !inPlaceOk) || (target === 'directory' && !dirOk) ? 'new' : target;
  const nothing = sel.mode === 'only' && sel.paths.length === 0;
  const what = sel.mode === 'except' ? (sel.paths.length ? `Everything except ${sel.paths.length} ${sel.paths.length === 1 ? 'item' : 'items'}` : 'Everything')
    : `Only ${sel.paths.length} ${sel.paths.length === 1 ? 'item' : 'items'}`;
  const where = kind === 'new' ? `a new folder under ${bounds?.root || '/var/restore'}` : kind === 'directory' ? dir : 'the original location';
  const body = (dryRun) => ({
    target: kind, directory: kind === 'directory' ? dir : undefined,
    include: sel.mode === 'only' ? sel.paths : undefined,
    exclude: sel.mode === 'except' && sel.paths.length ? sel.paths : undefined,
    overwrite: kind !== 'new' && overwrite ? overwrite : undefined, dry_run: dryRun, message: reason || undefined,
    ...(elsewhere ? { to_host: th.fqdn } : {}), ...(offsite ? { from_offsite: true } : {}),
  });
  const go = (dry) => { setResult(null); act.run(() => api.restore(fqdn, offsite && copy ? copy.id : snap.id, body(dry))); };
  const opt = (value, label, sub, ok, why) => html`<label class=${'opt' + (kind === value ? ' on' : '') + (ok ? '' : ' dis')}>
    <input type="radio" name="target" checked=${kind === value} disabled=${!ok} onChange=${() => setTarget(value)} />
    <span><b>${label}</b><span class="sub" data-tip=${ok ? null : why}>${ok ? sub : why.split('\n')[0]}</span></span></label>`;
  return html`<div class="restore">
    ${copy && html`<section class="list"><div class="list-head"><div class="row"><b>Restore from</b>
      <div class="seg" role="radiogroup" aria-label="Restore from">
        <button type="button" role="radio" aria-checked=${offsite ? 'false' : 'true'} class=${offsite ? '' : 'on'} disabled=${aGone} onClick=${() => setFrom('primary')}>Site A${aGone ? ' (deleted)' : ''}</button>
        <button type="button" role="radio" aria-checked=${offsite ? 'true' : 'false'} class=${offsite ? 'on' : ''} onClick=${() => { setFrom('offsite'); setToHost(fqdn); }}>Site B copy</button></div>
      <span class="muted">${offsite ? `copy ${copy.id.slice(0, 8)}, read through the server for this restore only` : `snapshot ${snap.short_id}`}</span></div></div></section>`}

    ${aGone ? html`<p class="muted">Site A no longer holds the snapshot, so its files cannot be listed: the copy is restored whole.</p>`
      : html`<${What} fqdn=${fqdn} snap=${snap} sel=${sel} setSel=${setSel} />`}

    <section class="list" aria-labelledby="where-h">
      <div class="list-head"><h2 id="where-h"><span class="step">2</span>Where to</h2>
        ${others.length > 0 && html`<label class="row muted" style="gap:.4rem">On agent
          <select value=${toHost} onChange=${(e) => { setToHost(e.target.value); if (target === 'in_place') setTarget('new'); }}>
            <option value=${fqdn}>${fqdn} (its own)</option>${others.map((h) => html`<option value=${h.fqdn}>${h.fqdn}</option>`)}</select></label>`}
        ${offsite && html`<span class="muted">A copy is restored by its own agent.</span>`}</div>
      <div class="opts" role="radiogroup" aria-label="Target">
        ${opt('new', 'A new folder', `under ${bounds?.root || '/var/restore'} on ${thShort}, made by the agent, root only; files keep their owner, mode and ACLs`, true, '')}
        ${!isDump && opt('in_place', 'The original location', 'writes into the live paths; the agent first backs up what it replaces', inPlaceOk,
          offsite ? 'never from an off-site copy' : elsewhere ? 'never onto another agent' : !can(host, 'restore.in_place') ? 'your role does not allow it' : `not allowed on ${short}\nrestore.allow_in_place in its agent.yaml`)}
        ${!isDump && html`<label class=${'opt' + (kind === 'directory' ? ' on' : '') + (dirOk ? '' : ' dis')}>
          <input type="radio" name="target" checked=${kind === 'directory'} disabled=${!dirOk} onChange=${() => setTarget('directory')} />
          <span style="flex:1"><b>Another folder</b>
            ${dirOk ? html`<input type="text" aria-label="Folder" value=${dir} disabled=${kind !== 'directory'} onInput=${(e) => setDir(e.target.value)} class="mono" style="width:100%;margin-top:.3rem" />`
              : html`<span class="sub" data-tip=${'restore.allow_other_directories in its agent.yaml'}>${can(host, 'restore.in_place') ? `not allowed on ${thShort}` : 'your role does not allow it'}</span>`}</span></label>`}
        ${isDump && html`<p class="muted">A dump snapshot from before 0.1.0 is one file: it is restored into a new folder only, and loaded by hand where the agent runs.</p>`}
      </div>
      ${kind !== 'new' && html`<label class="row muted" style="gap:.4rem;padding:0 1rem .8rem">If a file is already there
        <select value=${overwrite} onChange=${(e) => setOverwrite(e.target.value)}>
          <option value="">restic's default</option><option value="always">always replace</option><option value="if-changed">replace if the content differs</option>
          <option value="if-newer">replace if the snapshot's copy is newer</option><option value="never">never replace: add missing files only</option></select></label>`}
      ${elsewhere && html`<p class="warnline">${thShort} reads ${short}'s repository, read-only, for this restore only, and its root learns ${short}'s repository password.</p>`}
    </section>

    <section class="list runbar" aria-label="Run">
      <p><span class="step">3</span><b>${nothing ? 'Tick what to restore.' : `${what}${copy ? (offsite ? ' from site B’s copy' : ' from site A') : ''} into ${where} on ${thShort}.`}</b></p>
      <div class="row">
        <input type="text" aria-label="Reason, for the audit log" placeholder="Reason, for the audit log (such as a ticket)" value=${reason} onInput=${(e) => setReason(e.target.value)} style="flex:1 1 16rem" />
        <button disabled=${act.busy || nothing} onClick=${() => go(true)}>Dry run</button>
        <button class=${kind === 'in_place' ? 'warn' : 'primary'} disabled=${act.busy || nothing} onClick=${() => go(false)}>${kind === 'in_place' ? 'Restore into the live paths' : 'Restore'}</button>
      </div>
      <${ErrorBox} error=${act.error} />
      ${act.done && !result && html`<div class="banner info">Queued on ${thShort}: its agent picks it up on its next poll, within 24 hours.</div>`}
      ${result && html`<div class=${'banner ' + (result.status === 'success' ? 'ok' : 'error')}>
        ${result.detail?.dry_run === 'true' ? 'Dry run: nothing was written. ' : ''}${result.status}: ${result.detail?.files_restored || 0} of ${result.total_files} files
        ${result.detail?.directory ? html` into <code>${result.detail.directory}</code>` : ''}${result.detail?.pre_restore ? html`; safety backup <code>${result.detail.pre_restore.slice(0, 8)}</code>` : ''}
        ${result.error && html`<div>${result.error}</div>`}</div>`}
    </section>
  </div>`;
}

// DeleteDialog deletes restore points: in site A, in site B (their off-site copies, decision 041) or both, after the 7
// days, or now (an admin's). It always asks why: the reason goes into the audit log (decision 055). A point is
// { id, short, copyId, copyShort, sa, sb }: its snapshot in site A and its copy in site B, each in its site's state.
// The server checks who may, and each deletion is its own request, which can be cancelled apart.
export function DeleteDialog({ fqdn, points, off, site, onClose, onDone }) {
  const short = shortName(fqdn);
  const inA = points.filter((p) => p.sa === 'kept');
  const inB = off ? points.filter((p) => p.sb === 'copied') : [];
  const [where, setWhere] = useState(site === 'b' || !inA.length ? 'b' : 'a');
  const [reason, setReason] = useState('');
  const [typed, setTyped] = useState('');
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState([]);
  const first = useRef(null);
  useEffect(() => {
    if (first.current) first.current.focus();
    const esc = (e) => { if (e.key === 'Escape') onClose(); };
    document.addEventListener('keydown', esc);
    return () => document.removeEventListener('keydown', esc);
  }, []);
  const w = off ? where : 'a';
  const goA = w !== 'b' ? inA : [], goB = w !== 'a' ? inB : [];
  const n = points.length;
  const del = async (now) => {
    setBusy(true);
    const body = now ? { message: reason, now: true, confirm: typed } : { message: reason };
    const errs = [], done = [];
    for (const p of goA) {
      try { done.push(await api.deleteSnapshot(fqdn, p.id, body)); } catch (err) { errs.push([`${p.short} in site A`, err]); }
    }
    for (const p of goB) {
      try { done.push(await api.offsiteDeleteSnapshot(fqdn, p.copyId, body)); } catch (err) { errs.push([`copy ${p.copyShort} in site B`, err]); }
    }
    setBusy(false);
    setFailed(errs);
    if (!errs.length) onDone({ done, now });
  };
  const ids = (list, k) => list.slice(0, 3).map((p) => p[k]).join(' ') + (list.length > 3 ? ' …' : '');
  const q = `'${(reason.trim() || '…').replace(/'/g, `'\\''`)}'`;
  const now = typed === short ? ` --now --confirm ${short}` : '';
  // On the off-site pages, site A is the off-site repository itself: the CLI reaches it with --offsite.
  const cli = [goA.length && `holdfast${onOffsite() ? ' --offsite' : ''} snapshot delete ${fqdn} ${ids(goA, 'short')} --reason ${q}${now}`,
    goB.length && `holdfast --offsite snapshot delete ${fqdn} ${ids(goB, 'copyShort')} --reason ${q}${now}`].filter(Boolean).join('\n');
  const of = (count) => `${count} of ${n}${count < n ? ' (the others are not there, or their deletion is asked already)' : ''}`;
  const opt = (v, label, ok, sub) => html`<label class=${'opt compact' + (where === v ? ' on' : '') + (ok ? '' : ' dis')}>
    <input type="radio" name="where" checked=${where === v} disabled=${!ok} onChange=${() => setWhere(v)} />
    <span class="t"><b>${label}</b><span class="sub">${sub}</span></span></label>`;
  const text = !off ? '' : w === 'a' ? 'in site A; the copies in site B stay' : w === 'b' ? 'in site B only; site A keeps its snapshots' : 'in both sites, as deletions you can cancel apart';
  return html`<div class="scrim" onClick=${(e) => { if (e.target === e.currentTarget) onClose(); }}>
    <div class="dialog" role="dialog" aria-modal="true" aria-labelledby="del-h">
      <h2 id="del-h">${n === 1 ? `Delete snapshot ${points[0].short || points[0].copyShort}?` : `Delete ${n} snapshots?`}</h2>
      <p class="muted">${n > 1 && html`<span class="mono">${points.slice(0, 6).map((p) => p.short || p.copyShort).join(' ')}</span>${n > 6 ? ` and ${n - 6} more` : ''}. `}restic forgets ${n === 1 ? 'it' : 'them'} in 7 days${text ? ', ' + text : ''}. Until then you can cancel.</p>
      ${off && html`<div class="opts" style="padding:0" role="radiogroup" aria-label="Where">
        ${opt('a', html`<${SiteTag} />`, inA.length > 0, of(inA.length))}${opt('b', html`<${SiteTag} b /> the off-site copies`, inB.length > 0, of(inB.length))}
        ${opt('both', 'Both sites', inA.length > 0 && inB.length > 0, `${inA.length} in site A and ${inB.length} in site B`)}</div>`}
      <label class="field"><span>Why <b class="errtext">(required)</b>: it goes into the audit log</span>
        <textarea class="reason" ref=${first} aria-label="Why" value=${reason} onInput=${(e) => setReason(e.target.value)} placeholder="Such as: dump taken during the failed migration, ticket OPS-512"></textarea></label>
      <div class="row" style="justify-content:flex-end">
        <button onClick=${onClose}>Keep ${n === 1 ? 'it' : 'them'}</button>
        <button class="danger-line" disabled=${busy || !reason.trim() || !(goA.length + goB.length)} onClick=${() => del(false)}>Delete in 7 days</button>
      </div>
      <details>
        <summary class="muted">Delete now, without the 7 days (admins)</summary>
        <div class="row" style="margin-top:.6rem">
          <input placeholder=${'type ' + short + ' to confirm'} aria-label=${'Type ' + short + ' to confirm'} value=${typed} onInput=${(e) => setTyped(e.target.value)} autocomplete="off" style="flex:1" />
          <button class="danger" disabled=${busy || !reason.trim() || typed !== short || !(goA.length + goB.length)} onClick=${() => del(true)}>Delete now</button>
        </div>
      </details>
      <pre>${cli || '—'}</pre>
      ${failed.map(([what, err]) => html`<div class="banner error">${what}: ${err.message}</div>`)}
    </div>
  </div>`;
}

/* ---------------- a PostgreSQL dump's restore (decision 055) ---------------- */

export function PGRestore({ fqdn, snap, host, job, copy, aGone, settings }) {
  const p = job.postgresql;
  const db = p.database;
  const conn = (host.postgresql || []).find((c) => c.name === p.connection);
  const test = conn?.last_test?.ok ? conn.last_test.result : null;
  const row = (test?.databases || []).find((d) => d.name === db);
  const owner = row ? row.owner : null;
  const roles = !!p.globals;
  const canDB = !roles && !!conn && !!conn.restore_user;
  const agentAllows = !!host.agent?.postgresql?.allow_in_place_restore;
  const admin = can(host, 'restore.in_place');
  const [from, setFrom] = useState(aGone ? 'b' : 'a');
  const inPlaceOk = canDB && admin && agentAllows && from === 'a';
  const [target, setTarget] = useState(canDB ? 'database' : 'new');
  const kind = (target === 'in_place' && !inPlaceOk) || (target === 'database' && !canDB) ? (canDB ? 'database' : 'new') : target;
  const [typed, setTyped] = useState('');
  const [reason, setReason] = useState('');
  const act = useAction();
  const [result, setResult] = useResult(act.done);
  // The new database's name, as the server makes it (the snapshot list's restore_database).
  const auto = roles ? '' : snap.restore_database || '';
  const file = dumpFile(job);
  const blocked = kind === 'in_place' && typed !== db;
  const id = from === 'b' && copy ? copy.id : snap.id;
  const go = (dry) => {
    setResult(null);
    act.run(() => api.restore(fqdn, id, { target: kind, dry_run: dry, message: reason || undefined, ...(kind === 'in_place' ? { confirm: typed } : {}), ...(from === 'b' ? { from_offsite: true } : {}) }));
  };
  const src = from === 'b' && copy ? `site B’s copy ${copy.id.slice(0, 8)}` : `snapshot ${snap.short_id}`;
  const what = kind === 'in_place' ? `${db} on ${p.connection}, replaced by this dump` : kind === 'database' ? `${db} as a new database ${auto} on ${p.connection}` : `${file} into a new folder on ${fqdn}`;
  const opt = (v, ok, label, sub, extra) => html`<label class=${'opt' + (kind === v ? ' on' : '') + (ok ? '' : ' dis')}>
    <input type="radio" name="target" checked=${kind === v} disabled=${!ok} onChange=${() => setTarget(v)} />
    <span class="t"><b>${label}</b><span class="sub">${sub}</span>${extra}</span></label>`;
  const d = result?.detail || {};
  return html`<div class="restore">
    ${copy && html`<section class="list"><div class="list-head"><div class="row"><b>Restore from</b>
      <div class="seg" role="radiogroup" aria-label="Restore from">
        <button type="button" role="radio" aria-checked=${from === 'a' ? 'true' : 'false'} class=${from === 'a' ? 'on' : ''} disabled=${aGone} onClick=${() => setFrom('a')}>Site A${aGone ? ' (deleted)' : ''}</button>
        <button type="button" role="radio" aria-checked=${from === 'b' ? 'true' : 'false'} class=${from === 'b' ? 'on' : ''} onClick=${() => setFrom('b')}>Site B copy</button></div>
      <span class="muted">${from === 'b' ? `copy ${copy.id.slice(0, 8)}, read through the server for this restore only` : `snapshot ${snap.short_id}`}</span></div></div></section>`}
    <section class="list" aria-labelledby="where-h">
      <div class="list-head"><h2 id="where-h"><span class="step">1</span>Where to</h2><span class="muted">Onto ${p.connection} itself, or as a file. Never into another database you name.</span></div>
      <div class="opts" role="radiogroup" aria-label="Target">
        ${opt('database', canDB, 'A new database beside it',
          roles ? 'Roles are kept as a file only: Holdfast never creates roles on a server.' : !conn ? `The connection ${p.connection} is gone from ${fqdn}.` : !conn.restore_user ? `The connection ${p.connection} has no restore user.`
            : `On ${p.connection}, owned by ${owner || 'the original’s owner'} like the original, and named for you. Nothing that exists is touched.`,
          canDB && html`<span class="autoname" aria-label="The new database's name">${auto}</span>`)}
        ${opt('in_place', inPlaceOk, 'In place',
          roles ? 'Never for roles.' : !canDB ? 'The connection has no restore user.' : from === 'b' ? 'Never from site B’s copy.' : !admin ? 'Your role does not allow it: in place needs restore.in_place, an admin’s.'
            : !agentAllows ? `${fqdn}’s agent.yaml does not allow it (postgresql.allow_in_place_restore).`
              : row ? `Replaces ${db} with this dump. First the agent dumps ${db} as it is now, as a safety snapshot; then it restores in one transaction: all of it, or nothing.`
                : `${db} was not on ${p.connection} at the last test: the agent creates it and restores into it.`)}
        ${opt('new', true, 'The dump file', `${file} into a new folder under ${host.agent?.restore_bounds?.root || '/var/restore'} on ${fqdn}, readable by root only. For loading it by hand.`)}
      </div>
      ${kind !== 'new' && html`<details class="exact" style="padding:0 1rem .9rem"><summary>Exact commands, run by the agent as ${conn.restore_user}</summary>
        <pre>${kind === 'database' ? `CREATE DATABASE "${auto}" OWNER ${owner || '<the original’s owner>'}\nrestic dump ${id.slice(0, 8)} /${file} | pg_restore --dbname=${auto} --single-transaction --exit-on-error`
          : `restic backup --stdin-from-command --tag pre-restore … -- pg_dump --format=custom --compress=0 --dbname=${db}\nrestic dump ${id.slice(0, 8)} /${file} | pg_restore --dbname=${db} --clean --if-exists --single-transaction --exit-on-error`}</pre></details>`}
    </section>
    <section class="list runbar" aria-label="Run">
      <p><span class="step">2</span><b>${what}, from ${src}.</b></p>
      ${kind === 'in_place' && html`<label class="row">Type <b>${db}</b> to confirm <input class="mono" style="width:220px" aria-label=${'Type ' + db + ' to confirm'} value=${typed} onInput=${(e) => setTyped(e.target.value)} autocomplete="off" /></label>`}
      <div class="row">
        <input type="text" aria-label="Reason, for the audit log" placeholder="Reason, for the audit log (such as a ticket)" value=${reason} onInput=${(e) => setReason(e.target.value)} style="flex:1 1 16rem" />
        <button disabled=${act.busy || blocked} onClick=${() => go(true)}>Dry run</button>
        <button class=${kind === 'in_place' ? 'warn' : 'primary'} disabled=${act.busy || blocked} onClick=${() => go(false)}>${kind === 'in_place' ? `Restore over ${db}` : 'Restore'}</button>
      </div>
      <${ErrorBox} error=${act.error} />
      ${act.done && !result && html`<div class="banner info">Queued on ${fqdn}: its agent picks it up on its next poll.</div>`}
      ${result && html`<div class=${'banner ' + (result.status === 'success' ? 'ok' : 'error')}>
        ${d.dry_run === 'true' ? 'Dry run: nothing was written. ' : ''}${result.status}${d.database ? html`: ${d.in_place === 'true' ? `${d.database} on ${d.connection}, in place` : `new database ${d.database} on ${d.connection}`}${d.owner ? `, owned by ${d.owner}` : ''}` : ''}
        ${d.tables ? `; ${d.tables} tables` : ''}${d.directory ? html` into <code>${d.directory}</code>` : ''}${d.pre_restore ? html`; safety snapshot <code>${d.pre_restore.slice(0, 8)}</code> holds it as it was just before` : ''}
        ${result.status === 'success' && !d.database && d.dry_run !== 'true' && html`<pre>${roles ? `psql --file=${d.directory || '<folder>'}/globals.sql` : `pg_restore --dbname=<database> ${d.directory || '<folder>'}/${file}`}</pre>`}
        ${result.error && html`<div>${result.error}</div>`}</div>`}
    </section>
  </div>`;
}

