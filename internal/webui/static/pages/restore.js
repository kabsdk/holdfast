// A snapshot's Restore tab (spec §5.4), one column: from, what, where, then the Run bar. And the dialog that deletes a
// snapshot or its off-site copy (§5.3). Every rule is the server's and the host's; this asks and shows.
import { html, useState, useEffect, useRef, useAction, can, ErrorBox } from '../lib.js';
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

export function Restore({ fqdn, snap, host, hosts, sel, setSel, copy }) {
  const short = fqdn.split('.')[0];
  const [from, setFrom] = useState('primary');
  const [toHost, setToHost] = useState(fqdn);
  const [target, setTarget] = useState('new');
  const [dir, setDir] = useState('/srv/recovered');
  const [overwrite, setOverwrite] = useState('');
  const [reason, setReason] = useState('');
  const act = useAction();
  const [result, setResult] = useState(null);
  const offsite = from === 'offsite';
  const isDump = (snap.tags || []).some((t) => t.startsWith('dump='));
  // Onto another host (decision 041): an admin's, on both hosts, and only a host whose agent.yaml allows it.
  const others = offsite || isDump ? [] : (hosts || []).filter((h) => h.fqdn !== fqdn && h.agent?.restore_bounds?.allow_other_hosts && can(h, 'restore.in_place') && can(host, 'restore.in_place'));
  const th = toHost === fqdn ? host : (hosts || []).find((h) => h.fqdn === toHost) || host;
  const bounds = th.agent?.restore_bounds;
  const thShort = th.fqdn.split('.')[0];
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
  useEffect(() => {
    if (!act.done || !act.done.command) return undefined;
    const t = setInterval(async () => {
      const r = await api.restores().catch(() => null);
      const ev = r && r.done.find((e) => e.command_id === act.done.command);
      if (ev) { setResult(ev); clearInterval(t); }
    }, 2000);
    return () => clearInterval(t);
  }, [act.done]);
  const go = (dry) => { setResult(null); act.run(() => api.restore(fqdn, snap.id, body(dry))); };
  const opt = (value, label, sub, ok, why) => html`<label class=${'opt' + (kind === value ? ' on' : '') + (ok ? '' : ' dis')}>
    <input type="radio" name="target" checked=${kind === value} disabled=${!ok} onChange=${() => setTarget(value)} />
    <span><b>${label}</b><span class="sub" data-tip=${ok ? null : why}>${ok ? sub : why.split('\n')[0]}</span></span></label>`;
  return html`<div class="restore">
    ${copy && html`<section class="list"><div class="list-head"><div class="row"><b>Restore from</b>
      <div class="seg" role="radiogroup" aria-label="Restore from">
        <button type="button" role="radio" aria-checked=${offsite ? 'false' : 'true'} class=${offsite ? '' : 'on'} onClick=${() => setFrom('primary')}>Primary</button>
        <button type="button" role="radio" aria-checked=${offsite ? 'true' : 'false'} class=${offsite ? 'on' : ''} onClick=${() => { setFrom('offsite'); setToHost(fqdn); }}>Off-site copy</button></div>
      <span class="muted">${offsite ? `copy ${copy.id.slice(0, 8)}, read through the server for this restore only` : `snapshot ${snap.short_id}`}</span></div></div></section>`}

    <${What} fqdn=${fqdn} snap=${snap} sel=${sel} setSel=${setSel} />

    <section class="list" aria-labelledby="where-h">
      <div class="list-head"><h2 id="where-h"><span class="step">2</span>Where to</h2>
        ${others.length > 0 && html`<label class="row muted" style="gap:.4rem">On host
          <select value=${toHost} onChange=${(e) => { setToHost(e.target.value); if (target === 'in_place') setTarget('new'); }}>
            <option value=${fqdn}>${fqdn} (its own)</option>${others.map((h) => html`<option value=${h.fqdn}>${h.fqdn}</option>`)}</select></label>`}
        ${offsite && html`<span class="muted">A copy is restored on its own host.</span>`}</div>
      <div class="opts" role="radiogroup" aria-label="Target">
        ${opt('new', 'A new folder', `under ${bounds?.root || '/var/restore'} on ${thShort}, made by the agent, root only; files keep their owner, mode and ACLs`, true, '')}
        ${!isDump && opt('in_place', 'The original location', 'writes into the live paths; the agent first backs up what it replaces', inPlaceOk,
          offsite ? 'never from an off-site copy' : elsewhere ? 'never onto another host' : !can(host, 'restore.in_place') ? 'your role does not allow it' : `not allowed on ${short}\nrestore.allow_in_place in its agent.yaml`)}
        ${!isDump && html`<label class=${'opt' + (kind === 'directory' ? ' on' : '') + (dirOk ? '' : ' dis')}>
          <input type="radio" name="target" checked=${kind === 'directory'} disabled=${!dirOk} onChange=${() => setTarget('directory')} />
          <span style="flex:1"><b>Another folder</b>
            ${dirOk ? html`<input type="text" aria-label="Folder" value=${dir} disabled=${kind !== 'directory'} onInput=${(e) => setDir(e.target.value)} class="mono" style="width:100%;margin-top:.3rem" />`
              : html`<span class="sub" data-tip=${'restore.allow_other_directories in its agent.yaml'}>${can(host, 'restore.in_place') ? `not allowed on ${thShort}` : 'your role does not allow it'}</span>`}</span></label>`}
        ${isDump && html`<p class="muted">A dump snapshot from before 0.1.0 is one file: it is restored into a new folder only, and loaded on the host.</p>`}
      </div>
      ${kind !== 'new' && html`<label class="row muted" style="gap:.4rem;padding:0 1rem .8rem">If a file is already there
        <select value=${overwrite} onChange=${(e) => setOverwrite(e.target.value)}>
          <option value="">restic's default</option><option value="always">always replace</option><option value="if-changed">replace if the content differs</option>
          <option value="if-newer">replace if the snapshot's copy is newer</option><option value="never">never replace: add missing files only</option></select></label>`}
      ${elsewhere && html`<p class="warnline">${thShort} reads ${short}'s repository, read-only, for this restore only, and its root learns ${short}'s repository password.</p>`}
    </section>

    <section class="list runbar" aria-label="Run">
      <p><span class="step">3</span><b>${nothing ? 'Tick what to restore.' : `${what}${copy ? (offsite ? ' from the off-site copy' : ' from primary') : ''} into ${where} on ${thShort}.`}</b></p>
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

// DeleteDialog deletes a snapshot, or its off-site copy, after the 7 days, or now (an admin's). The server checks
// who may; the dialog only asks.
export function DeleteDialog({ fqdn, id, short, offsite, hasCopy, onClose, onDone }) {
  const host = fqdn.split('.')[0];
  const [reason, setReason] = useState('');
  const [typed, setTyped] = useState('');
  const act = useAction();
  const first = useRef(null);
  useEffect(() => {
    if (first.current) first.current.focus();
    const esc = (e) => { if (e.key === 'Escape') onClose(); };
    document.addEventListener('keydown', esc);
    return () => document.removeEventListener('keydown', esc);
  }, []);
  const del = (now) => act.run(() => (offsite ? api.offsiteDeleteSnapshot : api.deleteSnapshot)(fqdn, id, now ? { message: reason, now: true, confirm: typed } : { message: reason }))
    .then((r) => { if (r) onDone(r); });
  return html`<div class="scrim" onClick=${(e) => { if (e.target === e.currentTarget) onClose(); }}>
    <div class="dialog" role="dialog" aria-modal="true" aria-labelledby="del-h">
      <h2 id="del-h">${offsite ? `Delete the off-site copy ${short}?` : `Delete snapshot ${short}?`}</h2>
      <p class="muted">${offsite ? 'The off-site copy is forgotten in 7 days.' : 'restic forgets it in 7 days.'} Until then you can cancel.${!offsite && hasCopy ? ' Its off-site copy stays.' : ''}${offsite ? ' The snapshot in primary stays.' : ''}</p>
      <label class="field"><span>Why (audit log)</span><input ref=${first} value=${reason} onInput=${(e) => setReason(e.target.value)} /></label>
      <div class="row" style="justify-content:flex-end">
        <button onClick=${onClose}>Keep it</button>
        <button class="danger-line" disabled=${act.busy} onClick=${() => del(false)}>Delete in 7 days</button>
      </div>
      <details>
        <summary class="muted">Delete now, without the 7 days (admins)</summary>
        <div class="row" style="margin-top:.6rem">
          <input placeholder=${'type ' + host + ' to confirm'} aria-label=${'Type ' + host + ' to confirm'} value=${typed} onInput=${(e) => setTyped(e.target.value)} autocomplete="off" style="flex:1" />
          <button class="danger" disabled=${act.busy || typed !== host} onClick=${() => del(true)}>Delete now</button>
        </div>
      </details>
      <${ErrorBox} error=${act.error} />
    </div>
  </div>`;
}

