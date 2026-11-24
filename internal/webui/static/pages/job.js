// A job's page: its state, what restic keeps of it as rows of squares, its snapshots, and a calendar of them all
// (spec §5.2). The squares are restic's answer: forget --dry-run's reasons, from the snapshot list.
import { html, useState, usePoll, useAction, link, offsiteLink, fmtTime, dur, bytes, scheduleText, retentionText, Pill, Snap, can, ErrorBox, Loading, Menu, Paged, MONTHS, fullDate, clock } from '../lib.js';
import { api } from '../api.js';
import { StateBadge } from './host.js';
import { fate, FindFile } from './snapshots.js';
import { rows as keepRows, CAP, slotTip, finestRule, startOf, step } from '../keep.js';

// An off-site job (decision 039): copied from primary, with its own off-site retention.
function VaultJobFacts({ host, job }) {
  const s = job.status || {};
  return html`<dl class="facts">
    <dt>From primary</dt><dd>${job.dump ? html`dump <span class="mono">${job.dump}</span>` : html`<span class="mono">${(job.paths || []).join('  ')}</span>`} · ${scheduleText(job.schedule)}</dd>
    <dt>Off-site retention</dt><dd>${retentionText(job.retention)}</dd>
    <dt>Primary retention</dt><dd>${job.primary_retention ? retentionText(job.primary_retention) : '—'} <span class="muted">· set on the server's pages</span></dd>
    <dt>Copies</dt><dd>${s.last_success_at ? 'last copy ' + fmtTime(s.last_success_at) : 'none yet'}${job.behind_s != null ? ` · ${job.behind_s ? dur(job.behind_s + 's') + ' behind primary' : 'up to date'}` : ''}</dd>
    <dt>Newest copy</dt><dd>${s.last_snapshot_id ? html`<${Snap} fqdn=${host.fqdn} id=${s.last_snapshot_id} />` : '—'}</dd>
  </dl>`;
}

// A square's period, shorter, for a row's range: "11 Nov 2026", "Oct 2026", "2024".
const short = (label) => label.replace(/^Week of /, '').replace(/^[A-Z][a-z]{2} (?=\d)/, '');

// Keep draws what restic keeps of a list of snapshots under a retention: primary's, or the off-site copies' under the
// off-site retention. href is where a square leads; only marks a copy whose snapshot primary no longer has.
function Keep({ retention, snaps, href, only, cal, title, extra }) {
  const rows = keepRows(retention, snaps);
  const kept = snaps.filter((s) => !s.gone_at && (s.kept_by || []).length).length;
  return html`<section class="list" aria-labelledby="keep-h">
    <div class="list-head"><div class="row" style="gap:.8rem"><h2 id="keep-h">What restic keeps</h2>${title}</div>
      <div class="row"><span class="muted">${kept} ${only ? 'copies' : 'snapshots'} · from <code>restic forget --dry-run</code></span><a class="button small" href=${cal}>Calendar</a></div></div>
    ${extra}
    <div class="scrollx"><div class="ladder">
      ${rows.map((r) => html`<div class="tier">
        <div class="lab"><b>${r.label}</b><span class="sub">${r.sub}</span></div>
        <div class="slots" aria-label=${r.label}>
          ${r.slots.length > CAP && html`<a class="more" href=${cal} data-tip=${`${r.slots.length - CAP} earlier\nThe calendar has them all`}>+${r.slots.length - CAP} earlier</a>`}
          ${r.slots.slice(-CAP).map((x) => {
            if (x.before) return html`<span class="slot before" data-tip=${x.label + '\nBefore the job’s first snapshot'}></span>`;
            if (!x.s) return html`<span class="slot empty" data-tip=${x.label + '\nNo snapshot in this period'}></span>`;
            const gone = only && only(x.s);
            const tip = slotTip(x.label, x.s) + (gone ? '\nOnly off-site now: primary no longer has it' : '');
            return html`<a class=${'slot' + (x.s.deletion_requested_at ? ' asked' : '') + (gone ? ' only' : '')} href=${href(x.s)} data-tip=${tip} aria-label=${tip.replace(/\n/g, ', ')}></a>`;
          })}
        </div>
        <span class="range">${r.slots.length ? `${short(r.slots[0].label)} → ${short(r.slots[r.slots.length - 1].label)}` : 'none yet'}</span>
      </div>`)}
    </div></div>
    <div class="legend"><span><i class="d-success"></i>kept</span><span><i class="d-asked"></i>deletion asked</span>${only && html`<span><i class="slot only"></i>only off-site now</span>`}<span><i class="d-none"></i>no snapshot in that period</span><span><i class="d-before"></i>before the first snapshot</span>
      <span>A row per rule, a square per period, oldest on the left; a row shows its newest ${CAP}. Hover a square for its date.</span></div>
  </section>`;
}

function Snapshots({ fqdn, job, snaps, href, offsite }) {
  const [m, setM] = useState('');
  const month = (s) => { const d = new Date(s.time); return `${MONTHS[d.getMonth()]} ${d.getFullYear()}`; };
  const months = [...new Set(snaps.map(month))];
  const shown = snaps.filter((s) => !m || month(s) === m);
  return html`<section class="list" aria-labelledby="snaps-h">
    <div class="list-head">
      <div class="row"><h2 id="snaps-h">${offsite ? 'Off-site copies' : 'Snapshots'}</h2>
        <label class="row muted" style="gap:.4rem">Taken in <select value=${m} onChange=${(e) => setM(e.target.value)}>
          <option value="">any month</option>${months.map((x) => html`<option value=${x}>${x}</option>`)}</select></label></div>
    </div>
    <${Paged} items=${shown} render=${(rows) => html`<table>
      <thead><tr><th>Taken</th><th>Snapshot</th><th>Files · size</th><th>Retention</th></tr></thead>
      <tbody>${rows.map((s) => { const f = fate(s); return html`<tr>
        <td class="num">${fmtTime(s.time)}</td><td><a class="mono" href=${href(s)}>${s.short_id}</a></td>
        <td class="num">${s.total_files ?? '—'} · ${bytes(s.total_bytes)}</td><td>${f.text && html`<${Pill} kind=${f.kind + ' wrap'}>${f.text}<//>`}</td></tr>`; })}</tbody>
    </table>`} />
    ${!offsite && html`<div style="padding:0 1rem 1rem"><${FindFile} fqdn=${fqdn} job=${job.id} /></div>`}
  </section>`;
}

// The job's off-site copies, on its own page (decision 053): the same squares and list, under the off-site retention,
// with whether that is primary's or the job's own, and the off-site settings of the job.
function OffsiteView({ host, job, primary, seg, onChange }) {
  const list = usePoll(() => api.offsiteSnapshots(host.fqdn, { job: job.id, retention: true }), [host.fqdn, job.id], 60000);
  const act = useAction();
  const oj = host.offsite.jobs.find((x) => x.id === job.id);
  const inPrimary = new Set((primary || []).filter((s) => !s.gone_at).map((s) => s.id));
  const isOnly = (s) => !(s.original && inPrimary.has(s.original));
  const href = (s) => (isOnly(s) ? offsiteLink('hosts', host.fqdn, 'snapshots', s.id) : link('hosts', host.fqdn, 'snapshots', s.original));
  const save = (fn, body) => act.run(() => fn(host.fqdn, job.id, body || {})).then((r) => { if (r) { onChange(); list.reload(); } });
  const blocking = act.error?.data?.blocking || [];
  const canSet = can(host, 'config.jobs');
  const info = html`<div class="siteinfo">
    <span>Off-site keeps <b>${oj.own_retention ? 'its own retention' : 'the same as primary'}</b>${oj.own_retention ? `: ${retentionText(oj.retention)}` : ', and follows every change there'}${oj.excluded ? html` · <b>new snapshots are not copied</b>` : ''}
      ${oj.shorter_than_primary && html` <${Pill} kind="warning">keeps less than primary (O101)<//>`}</span>
    ${canSet && oj.own_retention && html`<button class="small" disabled=${act.busy} onClick=${() => save(api.offsiteMatchPrimary)}>Keep the same as primary</button>`}
    ${canSet && html`<a class="button small" href=${offsiteLink('hosts', host.fqdn, 'configure', job.id)}>Set its own…</a>`}
    ${canSet && !oj.retired && html`<button class="small" disabled=${act.busy} onClick=${() => save(oj.excluded ? api.offsiteInclude : api.offsiteExclude)}>${oj.excluded ? 'Copy again' : 'Stop copying'}</button>`}
  </div>`;
  return html`
    ${blocking.length > 0 ? html`<div class="banner warning">To keep the same as primary, accept ${blocking.map((b) => `${b.code} (${b.message})`).join('; ')}.
      <button class="small" onClick=${() => save(api.offsiteMatchPrimary, { accept: blocking.map((b) => b.code) })}>Accept and save</button></div>` : html`<${ErrorBox} error=${act.error} />`}
    ${list.error ? html`<${ErrorBox} error=${list.error} />` : !list.data ? html`<${Loading} what="the off-site copies (restic is asked)" />` : html`
      <${Keep} retention=${oj.retention} snaps=${list.data} href=${href} only=${isOnly} cal=${offsiteLink('hosts', host.fqdn, 'jobs', job.id, 'calendar')} title=${seg} extra=${info} />
      <${Snapshots} fqdn=${host.fqdn} job=${job} snaps=${list.data} href=${href} offsite=${true} />`}`;
}

export function Job({ fqdn, job: jid, vault, reloadHosts }) {
  const host = usePoll(() => api.host(fqdn), [fqdn]);
  const list = usePoll(() => api.snapshots(fqdn, { job: jid, retention: true }), [fqdn, jid], 60000);
  const cuts = usePoll(() => api.retentionCuts(), [fqdn], 60000);
  const act = useAction();
  const [queued, setQueued] = useState(false);
  const [site, setSite] = useState('p');
  if (host.error) return html`<${ErrorBox} error=${host.error} />`;
  if (!host.data) return html`<${Loading} what=${jid} />`;
  const h = host.data;
  const j = h.jobs.find((x) => x.id === jid);
  if (!j) return html`<h1>${jid}</h1><div class="banner error">${fqdn} has no job ${jid}.</div><p><a href=${link('hosts', fqdn)}>Back to ${fqdn}</a></p>`;
  const refresh = () => { host.reload(); list.reload(); cuts.reload(); reloadHosts(); };
  const probs = (h.problems || []).filter((p) => p.job_id === jid);
  const cut = (cuts.data || []).find((c) => c.fqdn === fqdn && c.job_id === jid);
  // With an off-site bucket, Primary | Off-site switches the squares and the list between the two repositories.
  const withOff = !vault && !!h.offsite && h.offsite.jobs.some((x) => x.id === jid);
  const seg = withOff && html`<div class="seg" role="tablist" aria-label="Which repository">
    <button type="button" role="tab" aria-selected=${site === 'p' ? 'true' : 'false'} class=${site === 'p' ? 'on' : ''} onClick=${() => setSite('p')}>Primary</button>
    <button type="button" role="tab" aria-selected=${site === 'o' ? 'true' : 'false'} class=${site === 'o' ? 'on' : ''} onClick=${() => setSite('o')}>Off-site</button></div>`;
  const primaryHref = (s) => link('hosts', fqdn, 'snapshots', s.id);
  return html`
    <div class="spread">
      <div style="min-width:0">
        <h1 class="row" style="gap:.6rem">${jid} <${StateBadge} host=${h} job=${j} />
          ${j.excluded && html`<${Pill} kind="warning">not copied<//>`}${j.retired && html`<${Pill}>removed in primary<//>`}</h1>
        <p class="muted">${(j.paths || []).map((p) => html`<code>${p}</code> `)}${(j.excludes || []).length ? html`without ${j.excludes.map((x) => html`<code>${x}</code> `)}` : ''}· ${scheduleText(j.schedule)} · stale after ${dur(j.stale_after)}</p>
      </div>
      <div class="row">
        ${queued && html`<span class="ok">Queued</span>`}
        ${!vault && can(h, 'backup.run') && html`<button class="primary" disabled=${act.busy || queued} onClick=${() => act.run(() => api.runNow(fqdn, jid)).then((r) => r && setQueued(true))}>Back up now</button>`}
        ${can(h, 'config.jobs') && html`<a class="button" href=${link('hosts', fqdn, 'configure', jid)}>${vault ? 'Off-site retention' : 'Edit'}</a>`}
        <${Menu} label="More job actions" items=${[
          { label: 'Waiting to be deleted', href: link('hosts', fqdn, 'pending') },
          vault && can(h, 'config.jobs') && !j.retired && { label: j.excluded ? 'Include: copy it again' : 'Exclude: stop copying it', run: () => act.run(() => (j.excluded ? api.include : api.exclude)(fqdn, jid, {})).then(refresh) },
          vault && can(h, 'config.jobs') && j.primary_retention && { label: 'Match primary', run: () => act.run(() => api.matchPrimary(fqdn, jid, {})).then(refresh) },
          '-',
          can(h, 'host.remove') && (!vault || j.retired) && { label: 'Remove job…', danger: true, run: () => {
            if (confirm(`Remove job ${jid} from ${fqdn}? Its snapshots stay; the daily retention no longer touches them.`))
              act.run(() => api.removeJob(fqdn, jid, {})).then((r) => { if (r) { reloadHosts(); location.hash = link('hosts', fqdn); } });
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
        <a class="button small" href=${link('hosts', fqdn, 'pending')}>What waits</a>
        ${can(h, 'retention.cut') && html`<a class="button small" href=${link('hosts', fqdn, 'prune-now')}>Prune now…</a>`}
      </div></div>`}
    ${vault && html`<${VaultJobFacts} host=${h} job=${j} />`}
    ${withOff && site === 'o' ? html`<${OffsiteView} host=${h} job=${j} primary=${list.data} seg=${seg} onChange=${refresh} />`
      : list.error ? html`<${ErrorBox} error=${list.error} />` : !list.data ? html`<${Loading} what="what restic keeps (restic is asked)" />` : html`
      <${Keep} retention=${j.retention} snaps=${list.data} href=${primaryHref} cal=${link('hosts', fqdn, 'jobs', jid, 'calendar')} title=${seg} />
      <${Snapshots} fqdn=${fqdn} job=${j} snaps=${list.data} href=${primaryHref} />`}`;
}

/* ---------------- the calendar: every kept snapshot, a square a day, a block a year ---------------- */

function yearGrid(y, cell) {
  const jan1 = new Date(y, 0, 1);
  const start = step('day', jan1, -((jan1.getDay() + 6) % 7));
  const items = [];
  for (let m = 0; m < 12; m++) {
    const c = Math.floor(Math.round((new Date(y, m, 1) - start) / 864e5) / 7);
    items.push(html`<span class="ml" style=${`grid-row:1;grid-column:${c + 2}`}>${MONTHS[m]}</span>`);
  }
  [[0, 'Mon'], [2, 'Wed'], [4, 'Fri']].forEach(([r, l]) => items.push(html`<span class="wl" style=${`grid-row:${r + 2};grid-column:1`}>${l}</span>`));
  let kept = 0, cols = 0;
  for (let d = start, i = 0; d.getFullYear() <= y; d = step('day', d, 1), i++) {
    const c = Math.floor(i / 7), r = i % 7;
    cols = c + 1;
    if (d.getFullYear() !== y) continue;
    const x = cell(d);
    const pos = `grid-row:${r + 2};grid-column:${c + 2}`;
    if (x.s) {
      kept++;
      items.push(html`<a class=${'cell ' + x.cls} style=${pos} href=${x.href} data-tip=${x.tip} aria-label=${x.tip.replace(/\n/g, ', ')}></a>`);
    } else {
      items.push(html`<span class=${'cell ' + x.cls} style=${pos} data-tip=${x.tip || null}></span>`);
    }
  }
  return { kept, grid: html`<div class="cal" style=${`grid-template-columns:30px repeat(${cols}, 13px);grid-template-rows:14px repeat(7, 13px)`}>${items}</div>` };
}

export function Calendar({ fqdn, job: jid }) {
  const host = usePoll(() => api.host(fqdn), [fqdn], 0);
  const list = usePoll(() => api.snapshots(fqdn, { job: jid, retention: true }), [fqdn, jid], 0);
  if (host.error || list.error) return html`<${ErrorBox} error=${host.error || list.error} />`;
  if (!host.data || !list.data) return html`<${Loading} what="the calendar (restic is asked what it keeps)" />`;
  const j = host.data.jobs.find((x) => x.id === jid);
  if (!j) return html`<div class="banner error">${fqdn} has no job ${jid}.</div>`;
  const kept = list.data.filter((s) => !s.gone_at && (s.kept_by || []).length).sort((a, b) => new Date(b.time) - new Date(a.time));
  const byDay = new Map();
  kept.forEach((s) => { const k = startOf('day', s.time).getTime(); if (!byDay.has(k)) byDay.set(k, []); byDay.get(k).push(s); });
  const now = new Date();
  const today = startOf('day', now).getTime();
  const oldest = kept.length ? startOf('day', kept[kept.length - 1].time).getTime() : today;
  const cell = (d) => {
    const t = d.getTime();
    if (t > today || t < oldest) return { cls: 'c-out' };
    const date = fullDate(d);
    const ss = byDay.get(t);
    if (ss) {
      const s = ss[0];
      const lines = [date, ss.length > 1 ? `${ss.length} snapshots kept; the newest, ${s.short_id}, at ${clock(s.time)}` : `Snapshot ${s.short_id} at ${clock(s.time)}`,
        'Kept: ' + [...new Set(ss.flatMap((x) => x.kept_by))].join(', ')];
      if (ss.some((x) => x.deletion_requested_at)) lines.push('Deletion asked: it waits 7 days');
      return { s, cls: ss.some((x) => x.deletion_requested_at) ? 'c-asked' : 'c-ok', href: link('hosts', fqdn, 'snapshots', s.id), tip: lines.join('\n') };
    }
    if (t === today) return { cls: 'c-thin', tip: date + '\nNo backup yet today' };
    const rule = finestRule(j.retention, d, now);
    if (rule === 'day' || rule === 'hour') return { cls: 'c-gap', tip: date + '\nNo snapshot: the backup failed or did not run' };
    return { cls: 'c-thin', tip: date + '\n' + (rule ? `Not kept: here the rules keep one per ${rule}` : 'Not kept: older than every rule') };
  };
  const years = [];
  for (let y = now.getFullYear(); y >= new Date(oldest).getFullYear(); y--) years.push(y);
  return html`
    <div class="spread">
      <div style="min-width:0">
        <h1>${jid} · every kept snapshot</h1>
        <p class="muted">${kept.length} snapshots${kept.length ? ` · newest ${fmtTime(kept[0].time)} · oldest ${fmtTime(kept[kept.length - 1].time)}` : ''} · keeps ${retentionText(j.retention)}</p>
      </div>
      <a class="button" href=${link('hosts', fqdn, 'jobs', jid)}>Back to ${jid}</a>
    </div>
    <div class="legend"><span><i class="c-ok"></i>kept</span><span><i class="c-asked"></i>deletion asked</span>
      <span><i class="c-gap"></i>missed: the daily rule should have one</span><span><i class="c-thin"></i>not kept: retention thinned it</span>
      <span>Hover a day for its snapshot; click to open it.</span></div>
    ${kept.length === 0 ? html`<p class="muted">No snapshot is kept yet.</p>` : years.map((y) => {
      const g = yearGrid(y, cell);
      return html`<section class="list year" aria-label=${String(y)}>
        <div class="list-head"><h2>${y}</h2><span class="muted">${g.kept} ${g.kept === 1 ? 'day' : 'days'} with a kept snapshot</span></div>
        <div class="scrollx" style="padding:0 1rem 1rem">${g.grid}</div>
      </section>`;
    })}`;
}
