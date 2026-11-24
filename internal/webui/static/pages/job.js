// A job's page (spec §5.2): its state, and one calendar of its restore points, a square a day. With off-site copies a
// square is two halves, site A on top and site B below, each in its own site's state (decision 053). A day lists its
// restore points, each with its snapshot in site A and its copy in site B, to open, restore or delete, in either site
// or both, always with a reason (decision 055). What restic keeps is restic's answer (forget --dry-run), from kept_by.
import { html, useState, useEffect, useRef, usePoll, useAction, link, offsiteLink, fmtTime, fmtDay, dur, bytes, scheduleText, retentionText, windowText, Pill, Snap, can, ErrorBox, Loading, Menu, MONTHS, fullDate, clock, browserZone, localDay, isPG, Kind, repoPath, icons } from '../lib.js';
import { api } from '../api.js';
import { StateBadge } from './host.js';
import { FindFile } from './snapshots.js';
import { DeleteDialog } from './restore.js';
import { points as restorePoints, dayState, cellBg, COMBOS, WORD_A, WORD_B, reasonKey, startOf, step, finestRule } from '../keep.js';

// An off-site job (decision 039): copied from primary, with its own off-site retention.
function VaultJobFacts({ host, job }) {
  const s = job.status || {};
  return html`<dl class="facts">
    <dt>From primary</dt><dd>${job.dump ? html`dump <span class="mono">${job.dump}</span>` : html`<${Kind} job=${job} />`} · ${scheduleText(job.schedule)}</dd>
    <dt>Off-site retention</dt><dd>${retentionText(job.retention)}</dd>
    <dt>Primary retention</dt><dd>${job.primary_retention ? retentionText(job.primary_retention) : '—'} <span class="muted">· set on the server's pages</span></dd>
    <dt>Copies</dt><dd>${s.last_success_at ? 'last copy ' + fmtTime(s.last_success_at) : 'none yet'}${job.behind_s != null ? ` · ${job.behind_s ? dur(job.behind_s + 's') + ' behind primary' : 'up to date'}` : ''}</dd>
    <dt>Newest copy</dt><dd>${s.last_snapshot_id ? html`<${Snap} fqdn=${host.fqdn} id=${s.last_snapshot_id} />` : '—'}</dd>
  </dl>`;
}

// SiteB is site B's line: where it keeps the copies, how many, and whether it keeps the same as site A; with the job's
// off-site settings (decision 053).
function SiteB({ host, job, pts, storage, onChange }) {
  const act = useAction();
  const oj = host.offsite.jobs.find((x) => x.id === job.id);
  const save = (fn, body) => act.run(() => fn(host.fqdn, job.id, body || {})).then((r) => { if (r) onChange(); });
  const blocking = act.error?.data?.blocking || [];
  const canSet = can(host, 'config.jobs');
  const there = pts.filter((p) => p.sb === 'copied' || p.sb === 'waits').length;
  const pending = pts.filter((p) => p.sb === 'pending').length;
  return html`<div class="siteline b"><span class="tag">SITE B</span><code>${repoPath(storage, host.fqdn)}</code>
    <span class="muted">${[storage?.endpoint, `${there} restore points${pending ? `, ${pending} copy pending` : ', up to date'}`, oj.excluded && 'new snapshots are not copied',
      oj.own_retention ? `its own retention: ${retentionText(oj.retention)}` : 'the same retention as site A'].filter(Boolean).join(' · ')}</span>
    ${oj.shorter_than_primary && html`<${Pill} kind="warning">keeps less than site A (O101)<//>`}
    ${canSet && oj.own_retention && html`<button class="small" disabled=${act.busy} onClick=${() => save(api.offsiteMatchPrimary)}>Keep the same as site A</button>`}
    ${canSet && html`<a class="button small" href=${offsiteLink('agents', host.fqdn, 'configure', job.id)}>Set its own…</a>`}
    ${canSet && !oj.retired && html`<button class="small" disabled=${act.busy} onClick=${() => save(oj.excluded ? api.offsiteInclude : api.offsiteExclude)}>${oj.excluded ? 'Copy again' : 'Stop copying'}</button>`}
    ${blocking.length > 0 ? html`<div class="banner warning" style="flex-basis:100%">To keep the same as site A, accept ${blocking.map((b) => `${b.code} (${b.message})`).join('; ')}.
      <button class="small" onClick=${() => save(api.offsiteMatchPrimary, { accept: blocking.map((b) => b.code) })}>Accept and save</button></div>` : html`<${ErrorBox} error=${act.error} />`}
  </div>`;
}

const RULE_WORD = { within: 'every backup', hourly: 'one per hour', daily: 'one per day', weekly: 'one per week', monthly: 'one per month', yearly: 'one per year' };
const swatch = (st) => html`<i class="swatch" style=${cellBg(st)}></i>`;

// SiteCell is one site's side of a restore point: its snapshot (or copy), or what happened to it.
function SiteCell({ p, site, fqdn, mayDelete, onCancel, vault }) {
  const st = site === 'a' ? p.sa : p.sb;
  const s = site === 'a' ? p.a : p.b;
  const idTxt = site === 'a' ? p.short : p.copyShort;
  if (st === 'kept' || st === 'copied') {
    return html`<span>${site === 'a' ? html`<a class="mono" href=${link('agents', fqdn, 'snapshots', p.id)}>${idTxt}</a>`
      : html`<a class="mono" href=${offsiteLink('agents', fqdn, 'snapshots', p.copyId)}>${idTxt}</a>`} ${icons.ok()}${s && s.pending_until ? html` <span class="sub">outside the new retention; waits until ${fmtDay(s.pending_until)}</span>` : ''}</span>`;
  }
  if (st === 'waits') {
    return html`<span><span class="pill warning wrap" title=${s.deletion_reason || ''}>deletion asked ${fmtTime(s.deletion_requested_at)}${s.deletion_requested_by ? ` by ${s.deletion_requested_by}` : ''}; it waits 7 days</span>
      ${s.deletion_reason ? html` <span class="sub">${s.deletion_reason}</span>` : ''}
      ${mayDelete && html` <button type="button" class="link small-link" onClick=${() => onCancel(site, p)}>Cancel</button>`}</span>`;
  }
  if (st === 'deleted') {
    const when = site === 'a' ? (p.b?.deleted_in_primary_at || s?.deleted_on_request_at || s?.gone_at) : s?.gone_at;
    const asked = site === 'a' ? !!(p.b?.deleted_in_primary_at || s?.deleted_on_request_at) : false;
    return html`<span class="muted small-text" title=${s?.deletion_reason || ''}>${idTxt ? html`<s class="mono">${idTxt}</s> ` : ''}${asked ? 'deleted on request' : 'deleted'}${when ? ' ' + fmtDay(when) : ''}${s?.deletion_requested_by ? ` by ${s.deletion_requested_by}` : ''}${s?.deletion_reason ? `: ${s.deletion_reason}` : ''}</span>`;
  }
  if (st === 'pending') return html`<span class="warntext small-text">copy pending: the server copies it next</span>`;
  return html`<span class="muted small-text">${p.sa === 'deleted' ? 'never copied: deleted in site A first' : 'not copied: copying is stopped for this job'}</span>`;
}

// RestorePoints is the calendar and the chosen day's restore points.
function RestorePoints({ h, j, pts, off, vault, siteA, siteB, days, events, reload }) {
  const fqdn = h.fqdn;
  const pg = isPG(j);
  const mayDelete = can(h, 'snapshot.delete');
  const now = new Date();
  const today = startOf('day', now).getTime();
  const dayOf = (t) => startOf('day', t).getTime();
  const byDay = new Map();
  pts.forEach((p) => { const d = dayOf(p.time); if (!byDay.has(d)) byDay.set(d, []); byDay.get(d).push(p); });
  // The last 92 days' runs, from the agent page's strip: failed backups, and the days before the job's first backup.
  const ran = new Map();
  ((days || []).find((x) => x.job_id === j.id)?.days || []).forEach((d) => ran.set(localDay(d.date).getTime(), d));
  const failedRuns = new Map();
  (events || []).filter((e) => e.kind === 'backup' && e.job_id === j.id && e.status !== 'success' && e.status !== 'warning' && e.status !== 'skipped')
    .forEach((e) => { const d = dayOf(e.finished); if (!failedRuns.has(d)) failedRuns.set(d, []); failedRuns.get(d).push(e); });
  const [focus, setFocus] = useState(pts[0] ? dayOf(pts[0].time) : today);
  const [sel, setSel] = useState([]);
  const [anchor, setAnchor] = useState(null);
  const [rule, setRule] = useState(null);
  const [dlg, setDlg] = useState(null);
  const [notice, setNotice] = useState(null);
  const act = useAction();
  const scroller = useRef(null);
  useEffect(() => { if (scroller.current) scroller.current.scrollLeft = scroller.current.scrollWidth; }, []);
  // A restore point can be deleted where it still is, and where no deletion waits for it already.
  const deletable = (p) => p.sa === 'kept' || (off && p.sb === 'copied');
  const oldest = pts.length ? dayOf(pts[pts.length - 1].time) : today;
  const start = startOf('week', Math.min(oldest, step('day', today, -12 * 7).getTime()));
  const allDays = [];
  for (let d = start; d.getTime() <= today; d = step('day', d, 1)) allDays.push(d);
  const weeks = Math.ceil(allDays.length / 7);
  const keysIn = (x, y) => { const lo = Math.min(x, y), hi = Math.max(x, y); return pts.filter((p) => { const d = dayOf(p.time); return d >= lo && d <= hi && deletable(p); }).map((p) => p.key); };
  const add = (keys) => setSel([...new Set([...sel, ...keys])]);
  const click = (t, e) => {
    if (e.shiftKey && anchor != null && mayDelete) { add(keysIn(anchor, t)); setFocus(t); return; }
    setFocus(t); setAnchor(t);
  };
  const monthSel = (y, m) => { if (mayDelete) add(pts.filter((p) => { const d = new Date(p.time); return d.getFullYear() === y && d.getMonth() === m && deletable(p); }).map((p) => p.key)); };
  const items = [];
  const example = {};
  // The months' labels over their first weeks. The first column's month is often only a day or two of it: when the
  // next month starts less than three columns later, a label's width, the first one goes, and the year moves on.
  const labels = [];
  allDays.forEach((d, i) => {
    if (d.getDate() === 1 || i === 0) {
      const mo = d.getMonth();
      if (!labels.length || labels[labels.length - 1].mo !== mo) labels.push({ col: Math.floor(i / 7) + 2, mo, yr: d.getFullYear() });
    }
  });
  if (labels.length > 1 && labels[1].col - labels[0].col < 3) labels.shift();
  labels.forEach(({ col, mo, yr }, n) => {
    items.push(html`<button type="button" class="ml" style=${`grid-row:1;grid-column:${col}`} disabled=${!mayDelete}
      data-tip=${mayDelete ? `${MONTHS[mo]} ${yr}\nSelect every restore point of the month` : null} onClick=${() => monthSel(yr, mo)}>${MONTHS[mo]}${mo === 0 || n === 0 ? ' ' + yr : ''}</button>`);
  });
  allDays.forEach((d, i) => {
    const t = d.getTime(), col = Math.floor(i / 7) + 2, row = (i % 7) + 2;
    const pos = `grid-row:${row};grid-column:${col}`;
    const ps = byDay.get(t) || [];
    const r = ran.get(t);
    const fails = r ? r.failed : (failedRuns.get(t) || []).length;
    const before = !ps.length && (r ? r.before : t < oldest);
    let cls = 'cell', style = pos, tip = fullDate(d);
    if (before) { cls += ' c-before'; tip += '\nBefore the oldest restore point'; }
    else if (ps.length) {
      const st = dayState(ps, off);
      const k = st.a + '/' + st.b;
      if (!(k in example)) example[k] = t;
      style += ';' + cellBg(st);
      tip += '\n' + ps.map((p) => `${clock(p.time)}  ${vault ? '' : 'site A: '}${WORD_A[p.sa]}${off ? `  ·  site B: ${WORD_B[p.sb]}` : ''}`).join('\n');
      if (rule && !ps.some((p) => p.kept.some((x) => reasonKey(x) === rule))) cls += ' c-dim';
    } else {
      cls += ' c-none';
      const fr = finestRule(j.retention, d, now);
      tip += t === today ? '\nNo backup yet today' : fr === 'day' || fr === 'hour' ? '\nNo restore point: the backup failed or did not run' : '\nNo restore point kept: retention thinned it';
      if (rule) cls += ' c-dim';
    }
    if (fails) { cls += ' c-fail'; tip += `\n${fails} failed ${fails === 1 ? 'backup' : 'backups'}`; }
    if (ps.some((p) => sel.includes(p.key))) cls += ' c-sel';
    if (t === focus) cls += ' c-focus';
    items.push(html`<button type="button" class=${cls} style=${style} data-tip=${tip} aria-label=${tip.replace(/\n/g, ', ')} aria-pressed=${t === focus ? 'true' : 'false'} disabled=${before} onClick=${(e) => click(t, e)}></button>`);
  });
  [[0, 'Mon'], [2, 'Wed'], [4, 'Fri']].forEach(([rw, l]) => items.push(html`<span class="wl" style=${`grid-row:${rw + 2};grid-column:1`}>${l}</span>`));
  const ret = j.retention || {};
  const chips = [...['within', 'hourly', 'daily', 'weekly', 'monthly', 'yearly'].filter((k) => ret[k]).map((k) => [k, html`${RULE_WORD[k]} for ${windowText(ret[k])}`]),
    ...(ret.keep_last ? [['last', html`always the newest ${ret.keep_last}`]] : [])]
    .map(([k, label]) => [k, label, pts.filter((p) => p.sa !== 'deleted' && p.kept.some((x) => reasonKey(x) === k)).length]);
  const fs = byDay.get(focus) || [];
  const ff = failedRuns.get(focus) || [];
  const fr = ran.get(focus);
  const inA = pts.filter((p) => p.sa !== 'deleted').length;
  const cols = mayDelete ? `26px 60px minmax(150px,1fr) ${off ? 'minmax(150px,1fr) ' : ''}84px minmax(0,1fr) 170px` : `60px minmax(150px,1fr) ${off ? 'minmax(150px,1fr) ' : ''}84px minmax(0,1fr) 100px`;
  const dayKeys = fs.filter(deletable).map((p) => p.key);
  const chosen = pts.filter((p) => sel.includes(p.key));
  const cancel = (site, p) => act.run(() => (site === 'a' ? api.cancelDeletion(fqdn, p.id) : api.offsiteCancelDeletion(fqdn, p.copyId))).then((r) => r && reload());
  // A restore point opens on the agent's page of its snapshot, even when only site B holds it: it restores from there.
  const open = (p) => link('agents', fqdn, 'snapshots', p.id);
  const restorable = (p) => p.sa === 'kept' || p.sa === 'waits' || (off && (p.sb === 'copied' || p.sb === 'waits'));
  return html`<section class="list" aria-labelledby="rp-h">
    <div class="list-head" style="align-items:flex-start"><div style="display:flex;flex-direction:column;gap:.4rem;min-width:0;flex:1 1 40rem">
      <h2 id="rp-h">Restore points <span class="muted" style="font-weight:400;font-size:13px">${pts.length ? `newest ${fmtTime(pts[0].time)} · oldest ${fullDate(pts[pts.length - 1].time)}` : 'none yet'}</span></h2>
      <div class="siteline"><span class="tag">${vault ? 'SITE B' : 'SITE A'}</span><code>${repoPath(siteA, fqdn)}</code><span class="muted">${siteA?.endpoint ? siteA.endpoint + ' · ' : ''}${inA} restore points · ${retentionText(ret)}</span></div>
      ${off && html`<${SiteB} host=${h} job=${j} pts=${pts} storage=${siteB} onChange=${reload} />`}
    </div></div>
    <div class="rules"><span class="muted">Kept by restic:</span>
      ${chips.map(([k, label, n]) => html`<button type="button" class=${'rulechip' + (rule === k ? ' on' : '')} aria-pressed=${rule === k ? 'true' : 'false'} onClick=${() => setRule(rule === k ? null : k)}>${label} <b>${n}</b></button>`)}
      ${rule && html`<span class="muted">its days are highlighted below</span>`}</div>
    <div class="scrollx calwrap" ref=${scroller}><div class="cal" style=${`grid-template-columns:30px repeat(${weeks}, 15px);grid-template-rows:16px repeat(7, 15px)`}>${items}</div></div>
    <div class="legend" style="flex-direction:column;gap:.4rem">
      ${off ? html`<span>Each day: <b>site A on top, site B below</b>, each half in that site’s state. Click an example to go to such a day.</span>
        <span class="row" style="gap:.3rem 1rem">${COMBOS.map(([st, label]) => { const d = example[st.a + '/' + st.b]; return html`<button type="button" class="link legendbtn" disabled=${!d} onClick=${() => setFocus(d)}
          data-tip=${d ? 'For example ' + fullDate(d) : 'No such day in this job'}>${swatch(st)}${label}</button>`; })}</span>`
        : html`<span class="row" style="gap:.3rem 1rem">${['kept', 'waits', 'deleted'].map((k) => html`<span>${swatch({ a: k })}${WORD_A[k]}</span>`)}</span>`}
      <span class="row" style="gap:.3rem 1rem"><span><i class="swatch c-fail" style="background:var(--sq-none)"></i>a backup failed that day</span><span><i class="swatch" style="background:var(--sq-none)"></i>no restore point kept</span>
        <span>Click a day for its restore points.${mayDelete ? ' Shift-click another to select every day between; click a month to select it.' : ''}</span></span>
    </div>
    ${mayDelete && chosen.length > 0 && html`<div class="selbar"><b>${chosen.length} restore ${chosen.length === 1 ? 'point' : 'points'} selected</b>
      <span class="muted">${new Set(chosen.map((p) => dayOf(p.time))).size} days</span>
      <button class="small danger-line" onClick=${() => setDlg(chosen)}>Delete…</button><button type="button" class="link" onClick=${() => setSel([])}>Clear</button></div>`}
    ${notice && html`<div class="banner warning" style="margin:.6rem 1rem">${notice}</div>`}
    <${ErrorBox} error=${act.error} />
    <div class="dayhead"><b>${fullDate(focus)}</b><span class="muted">${fs.length ? `${fs.length} restore ${fs.length === 1 ? 'point' : 'points'}` : 'no restore point'}${fr && fr.failed ? ` · ${fr.failed} failed` : ff.length ? ` · ${ff.length} failed` : ''}</span>
      ${mayDelete && dayKeys.length > 1 && html`<button type="button" class="link" onClick=${() => add(dayKeys)}>Select the day</button>`}</div>
    <div class="scrollx"><div style=${'min-width:' + (off ? 940 : 760) + 'px'}>
      ${fs.length > 0 && html`<div class="gt head" style=${'--cols:' + cols}>${mayDelete && html`<span></span>`}<span>Time</span><span>${vault ? 'This site' : 'Site A'}</span>${off && html`<span>Site B</span>`}<span>Size</span><span>Kept by</span><span></span></div>`}
      ${fs.map((p) => html`<div class="gt" style=${'--cols:' + cols}>
        ${mayDelete && html`<span><input type="checkbox" aria-label=${'Select ' + p.short} disabled=${!deletable(p)} checked=${sel.includes(p.key)}
          onChange=${(e) => setSel(e.target.checked ? [...sel, p.key] : sel.filter((x) => x !== p.key))} /></span>`}
        <span class="num">${clock(p.time)}</span>
        <${SiteCell} p=${p} site="a" fqdn=${fqdn} mayDelete=${mayDelete} onCancel=${cancel} vault=${vault} />
        ${off && html`<${SiteCell} p=${p} site="b" fqdn=${fqdn} mayDelete=${mayDelete} onCancel=${cancel} />`}
        <span class="num muted">${bytes(p.bytes)}</span>
        <span class="muted small-text wrap">${p.kept.length ? p.kept.join(', ') : '—'}</span>
        <span class="row" style="justify-content:flex-end;gap:.35rem">
          ${restorable(p) && html`<a class="button small" href=${open(p) + (pg ? '/restore' : '')}>${pg ? 'Restore…' : 'Open'}</a>`}
          ${mayDelete && deletable(p) && html`<button class="small danger-line" onClick=${() => setDlg([p])}>Delete…</button>`}</span>
      </div>`)}
      ${ff.map((e) => html`<div class="gt bad" style="--cols: 60px minmax(0,1fr)"><span class="num">${clock(e.finished)}</span><span class="errtext">${e.status}, no restore point: ${e.error || 'see the agent’s runs'}</span></div>`)}
      ${!fs.length && !ff.length && html`<p class="muted" style="padding:.6rem 1rem">${fr?.before || focus < oldest ? 'Before the oldest restore point.' : focus === today ? 'No backup yet today.'
        : ['day', 'hour'].includes(finestRule(ret, new Date(focus), now)) ? 'No backup that day: it failed or did not run.' : 'restic kept none of this day’s backups: the retention keeps fewer, older ones.'}</p>`}
    </div></div>
    ${dlg && html`<${DeleteDialog} fqdn=${fqdn} points=${dlg} off=${off} site="a" onClose=${() => setDlg(null)}
      onDone=${(r) => { setDlg(null); setSel([]); setNotice(r.now ? 'Deleting now: the maintenance run forgets them, and Config history lists it.' : `Deletion asked for ${r.done.length}: restic forgets ${r.done.length === 1 ? 'it' : 'them'} once the 7 days have passed. Until then you can cancel.`); reload(); }} />`}
  </section>`;
}

export function Job({ fqdn, job: jid, vault, reloadHosts, settings }) {
  const host = usePoll(() => api.host(fqdn), [fqdn]);
  const withOff = !vault && !!host.data?.offsite && host.data.offsite.jobs.some((x) => x.id === jid);
  const listA = usePoll(() => api.snapshots(fqdn, { job: jid, retention: true, all: true }), [fqdn, jid], 60000);
  const listB = usePoll(() => (withOff ? api.offsiteSnapshots(fqdn, { job: jid, retention: true, all: true }) : Promise.resolve(null)), [fqdn, jid, withOff], 60000);
  const siteB = usePoll(() => (withOff ? api.offsiteSettings() : Promise.resolve(null)), [withOff], 0);
  const days = usePoll(() => api.hostDays(fqdn, 92, browserZone()), [fqdn], 60000);
  const events = usePoll(() => api.hostEvents(fqdn, 200), [fqdn], 60000);
  const cuts = usePoll(() => api.retentionCuts(), [fqdn], 60000);
  const act = useAction();
  const [queued, setQueued] = useState(false);
  if (host.error) return html`<${ErrorBox} error=${host.error} />`;
  if (!host.data) return html`<${Loading} what=${jid} />`;
  const h = host.data;
  const j = h.jobs.find((x) => x.id === jid);
  if (!j) return html`<h1>${jid}</h1><div class="banner error">${fqdn} has no job ${jid}.</div><p><a href=${link('agents', fqdn)}>Back to ${fqdn}</a></p>`;
  const refresh = () => { host.reload(); listA.reload(); listB.reload(); cuts.reload(); reloadHosts(); };
  const probs = (h.problems || []).filter((p) => p.job_id === jid);
  const cut = (cuts.data || []).find((c) => c.fqdn === fqdn && c.job_id === jid);
  const off = withOff && !!listB.data;
  const oj = withOff ? h.offsite.jobs.find((x) => x.id === jid) : null;
  const conn = isPG(j) ? (h.postgresql || []).find((c) => c.name === j.postgresql.connection) : null;
  const ready = listA.data && (!withOff || listB.data || listB.error);
  return html`
    <div class="spread">
      <div style="min-width:0">
        <h1 class="row" style="gap:.6rem"><a class="wrap" href=${link('agents', fqdn)}>${fqdn}</a> <span class="muted" style="font-weight:400">/</span> ${jid} <${StateBadge} host=${h} job=${j} />
          ${j.excluded && html`<${Pill} kind="warning">not copied<//>`}${j.retired && html`<${Pill}>removed in primary<//>`}</h1>
        <p class="muted"><${Kind} job=${j} />${(j.excludes || []).length ? html` without ${j.excludes.map((x) => html`<code>${x}</code> `)}` : ''}</p>
        <p class="muted">${conn ? html`<code>${conn.url}</code> as ${conn.user} · ` : ''}${scheduleText(j.schedule)} · stale after ${dur(j.stale_after)}</p>
      </div>
      <div class="row">
        ${queued && html`<span class="ok">Queued</span>`}
        ${!vault && can(h, 'backup.run') && html`<button class="primary" disabled=${act.busy || queued} onClick=${() => act.run(() => api.runNow(fqdn, jid)).then((r) => r && setQueued(true))}>Back up now</button>`}
        ${can(h, 'config.jobs') && html`<a class="button" href=${link('agents', fqdn, 'configure', jid)}>${vault ? 'Off-site retention' : 'Edit'}</a>`}
        <${Menu} label="More job actions" items=${[
          { label: 'Waiting to be deleted', href: link('agents', fqdn, 'pending') },
          vault && can(h, 'config.jobs') && !j.retired && { label: j.excluded ? 'Include: copy it again' : 'Exclude: stop copying it', run: () => act.run(() => (j.excluded ? api.include : api.exclude)(fqdn, jid, {})).then(refresh) },
          vault && can(h, 'config.jobs') && j.primary_retention && { label: 'Match primary', run: () => act.run(() => api.matchPrimary(fqdn, jid, {})).then(refresh) },
          '-',
          can(h, 'host.remove') && (!vault || j.retired) && { label: 'Remove job…', danger: true, run: () => {
            if (confirm(`Remove job ${jid} from ${fqdn}? Its snapshots stay; the daily retention no longer touches them.`))
              act.run(() => api.removeJob(fqdn, jid, {})).then((r) => { if (r) { reloadHosts(); location.hash = link('agents', fqdn); } });
          } },
        ]} />
      </div>
    </div>
    <${ErrorBox} error=${act.error} />
    ${act.done && act.done.findings && act.done.findings.length > 0 && html`<div class="banner info">Saved as revision ${act.done.revision}: ${act.done.findings.map((f) => f.code + ' ' + f.message).join('; ')}</div>`}
    ${queued && html`<div class="banner ok">Queued: it starts after any running job. Its result appears on ${fqdn}'s page.</div>`}
    ${probs.map((p) => html`<div class=${'banner ' + p.severity}><span>${p.message} <code>${p.code}</code></span></div>`)}
    ${cut && html`<div class="banner warning">Retention cut pending: what the new retention drops stays until ${fmtTime(cut.prunable_from)}, the 7-day protection.
      <div class="row" style="margin-top:.4rem">
        ${can(h, 'config.jobs') && html`<button class="small" onClick=${() => act.run(() => api.keepLonger(fqdn, jid, {})).then(refresh)}>Keep the old retention</button>`}
        <a class="button small" href=${link('agents', fqdn, 'pending')}>What waits</a>
        ${can(h, 'retention.cut') && html`<a class="button small" href=${link('agents', fqdn, 'prune-now')}>Prune now…</a>`}
      </div></div>`}
    ${vault && html`<${VaultJobFacts} host=${h} job=${j} />`}
    ${withOff && listB.error && html`<div class="banner warning">Site B's copies could not be read, so the calendar shows site A only: ${listB.error.message}</div>`}
    ${listA.error ? html`<${ErrorBox} error=${listA.error} />` : !ready ? html`<${Loading} what="the restore points (restic is asked what it keeps)" />`
      : html`<${RestorePoints} key=${off ? 'ab' : 'a'} h=${h} j=${j} pts=${restorePoints(listA.data, off ? listB.data : null, { off, excluded: !!oj?.excluded })} off=${off} vault=${vault}
          siteA=${settings?.storage} siteB=${siteB.data?.storage} days=${days.data?.jobs} events=${events.data} reload=${refresh} />`}
    ${!vault && !isPG(j) && can(h, 'browse') && html`<${FindFile} fqdn=${fqdn} job=${jid} />`}`;
}
