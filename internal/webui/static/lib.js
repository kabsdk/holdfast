// Shared pieces of the UI: presentation only. Every rule, check and permission is the server's
// (decision 028); this file formats what the API says.
import { html, useState, useEffect, useCallback, useRef } from './vendor/htm-preact-3.1.1.module.js';
export { html, render, useState, useEffect, useCallback, useRef, useMemo } from './vendor/htm-preact-3.1.1.module.js';

/* ---------------- routing: #/path ---------------- */

// The off-site copies' pages (decision 041) are the same pages under #/offsite/, on the off-site API, in red.
export function onOffsite() {
  return /^#\/offsite(\/|$)/.test(location.hash);
}

export function currentPath() {
  const p = location.hash.replace(/^#/, '') || '/';
  return p.replace(/^\/offsite(?=\/|$)/, '') || '/';
}

export function go(path) {
  location.hash = '#' + (onOffsite() ? '/offsite' : '') + path;
}

export function useRoute() {
  const [path, setPath] = useState(currentPath());
  useEffect(() => {
    const on = () => setPath(currentPath());
    addEventListener('hashchange', on);
    return () => removeEventListener('hashchange', on);
  }, []);
  return path;
}

export const enc = encodeURIComponent;
// link stays on the pages it is on: the server's, or the off-site ones. primaryLink and offsiteLink cross over.
export const link = (...parts) => (onOffsite() ? '#/offsite/' : '#/') + parts.map((p) => enc(p)).join('/');
export const primaryLink = (...parts) => '#/' + parts.map((p) => enc(p)).join('/');
export const offsiteLink = (...parts) => '#/offsite/' + parts.map((p) => enc(p)).join('/');

/* ---------------- data ---------------- */

// usePoll loads data, and again every interval while the page is open (no live push).
export function usePoll(load, deps, interval = 10000) {
  const [state, setState] = useState({ data: null, error: null, loading: true });
  const alive = useRef(true);
  const run = useCallback(async () => {
    try {
      const data = await load();
      if (alive.current) setState({ data, error: null, loading: false });
    } catch (err) {
      if (alive.current) setState((s) => ({ data: s.data, error: err, loading: false }));
    }
  }, deps);
  useEffect(() => {
    alive.current = true;
    setState({ data: null, error: null, loading: true });
    run();
    const t = interval ? setInterval(run, interval) : null;
    return () => {
      alive.current = false;
      if (t) clearInterval(t);
    };
  }, [run]);
  return { ...state, reload: run };
}

// useAction runs one API call at a time and keeps its outcome for the page to show.
export function useAction() {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);
  const [done, setDone] = useState(null);
  const run = async (fn) => {
    setBusy(true);
    setError(null);
    setDone(null);
    try {
      const r = await fn();
      setDone(r ?? true);
      return r;
    } catch (err) {
      setError(err);
      return undefined;
    } finally {
      setBusy(false);
    }
  };
  const reset = () => { setDone(null); setError(null); };
  return { busy, error, done, run, setError, setDone, reset };
}

/* ---------------- formatting ---------------- */

const pad = (n) => String(n).padStart(2, '0');

export function fmtTime(t) {
  if (!t) return '—';
  const d = new Date(t);
  if (isNaN(d) || d.getFullYear() < 2000) return '—';
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export function fmtDay(t) {
  return t ? fmtTime(t).slice(0, 10) : '—';
}

export function ago(t) {
  if (!t) return '—';
  const s = (Date.now() - new Date(t).getTime()) / 1000;
  if (s < 0) return 'just now';
  if (s < 90) return `${Math.round(s)} s ago`;
  if (s < 5400) return `${Math.round(s / 60)} min ago`;
  if (s < 172800) return `${Math.round(s / 3600)} h ago`;
  return `${Math.round(s / 86400)} d ago`;
}

const WEEKDAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
export const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

// fullDate names a day the way a tooltip does: "Mon 23 Nov 2026".
export function fullDate(d) {
  d = new Date(d);
  return `${WEEKDAYS[d.getDay()]} ${d.getDate()} ${MONTHS[d.getMonth()]} ${d.getFullYear()}`;
}

// clock is the time of day: "02:04".
export function clock(t) {
  const d = new Date(t);
  return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// localDay turns the API's "2026-11-23" into that day at local midnight.
export function localDay(s) {
  const [y, m, d] = s.split('-').map(Number);
  return new Date(y, m - 1, d);
}

// browserZone is the browser's time zone by name, for answers given in days, such as the agent page's squares.
export function browserZone() {
  try { return Intl.DateTimeFormat().resolvedOptions().timeZone || ''; } catch { return ''; }
}

// until says how soon a time comes: "in 45 min", "in 13 h", "in 2 d".
export function until(t) {
  if (!t) return '—';
  const s = (new Date(t).getTime() - Date.now()) / 1000;
  if (s <= 0) return 'due now';
  if (s < 5400) return `in ${Math.max(1, Math.round(s / 60))} min`;
  if (s < 129600) return `in ${Math.round(s / 3600)} h`;
  return `in ${Math.round(s / 86400)} d`;
}

export function bytes(n) {
  if (n == null) return '—';
  const u = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i ? n.toFixed(1) : n) + ' ' + u[i];
}

// Durations come from Go as "24h0m0s"; this makes them read like "1 d".
export function dur(s) {
  if (!s) return '—';
  const m = /^(?:(\d+)h)?(?:(\d+)m)?(?:([\d.]+)s)?$/.exec(s);
  if (!m) return s;
  const h = +(m[1] || 0), min = +(m[2] || 0);
  if (h >= 48 && h % 24 === 0) return `${h / 24} d`;
  if (h >= 24) return `${Math.floor(h / 24)} d ${h % 24 ? (h % 24) + ' h' : ''}`.trim();
  if (h) return `${h} h${min ? ' ' + min + ' min' : ''}`;
  return `${min} min`;
}

export function windowText(d) {
  return d ? `${d.n} ${d.n === 1 ? d.unit.replace(/s$/, '') : d.unit}` : '';
}

export function scheduleText(s) {
  if (!s) return '—';
  let t = '—';
  if (s.daily) t = 'daily at ' + s.daily.times.join(', ');
  else if (s.every) t = `every ${s.every.n} ${s.every.unit} from ${s.every.from || '00:00'}`;
  else if (s.weekly) t = `weekly on ${s.weekly.days.join(', ')} at ${s.weekly.at}`;
  if (s.spread && s.spread.enabled && s.spread.minutes) t += `, spread ${s.spread.minutes} min`;
  if (s.zone) t += ` (${s.zone})`;
  return t;
}

const TIERS = [['within', 'every backup'], ['hourly', 'one per hour'], ['daily', 'one per day'], ['weekly', 'one per week'], ['monthly', 'one per month'], ['yearly', 'one per year']];

export function retentionText(r) {
  if (!r) return '—';
  const parts = TIERS.filter(([k]) => r[k]).map(([k, label]) => `${label} for ${windowText(r[k])}`);
  parts.push(`the newest ${r.keep_last}`);
  return parts.join('; ');
}

export { TIERS };

/* ---------------- components ---------------- */

export function ErrorBox({ error }) {
  if (!error) return null;
  const d = error.data || {};
  return html`<div class="banner error" role="alert">
    <div>${error.message}</div>
    ${(d.problems || []).length > 0 && html`<ul>${d.problems.map((p) => html`<li><code>${p.path}</code>: ${p.message}</li>`)}</ul>`}
    ${d.storage && (d.storage.findings || []).length > 0 && html`<ul>${d.storage.findings.map((f) => html`<li><code>${f.code}</code> ${f.message}</li>`)}</ul>`}
  </div>`;
}

export function Loading({ what }) {
  return html`<p class="muted">Loading ${what || ''}…</p>`;
}

// Icons, drawn inline. Each carries its meaning as a label, so colour is never the only sign.
export const icons = {
  error: () => html`<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" role="img" aria-label="Error"><circle cx="12" cy="12" r="9"></circle><path d="M15 9l-6 6M9 9l6 6"></path></svg>`,
  warning: () => html`<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" role="img" aria-label="Warning"><path d="M12 9v4"></path><path d="M12 17h.01"></path><path d="M10.3 3.9L1.8 18a2 2 0 001.7 3h17a2 2 0 001.7-3L13.7 3.9a2 2 0 00-3.4 0z"></path></svg>`,
  ok: () => html`<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.6" stroke-linecap="round" stroke-linejoin="round" role="img" aria-label="OK" class="ok"><path d="M5 12l5 5L20 7"></path></svg>`,
};

// Problem is one line of what the server knows is wrong: its severity, the job it is about, its message and code.
export function Problem({ p }) {
  return html`<div class=${'prob ' + (p.severity === 'error' ? 'error' : 'warning')}>
    ${p.severity === 'error' ? icons.error() : icons.warning()}
    <span>${p.job_id ? p.job_id + ': ' : ''}${p.message} <code>${p.code}</code></span>
  </div>`;
}

// Menu is the ⋯ button for a page's rarer actions. Each item is { label, run } or { label, href }, with danger for
// one that deletes or removes, and count for a number beside it; '-' is a separator. It closes on a click outside
// and on Escape.
export function Menu({ label, items }) {
  const [open, setOpen] = useState(false);
  const ref = useRef(null);
  const pop = useRef(null);
  useEffect(() => {
    if (!open) return undefined;
    // A menu that would start off the left edge, on a narrow window, opens rightwards instead.
    if (pop.current && pop.current.getBoundingClientRect().left < 8) { pop.current.style.left = '0'; pop.current.style.right = 'auto'; }
    const outside = (e) => { if (ref.current && !ref.current.contains(e.target)) setOpen(false); };
    const esc = (e) => { if (e.key === 'Escape') setOpen(false); };
    document.addEventListener('click', outside);
    document.addEventListener('keydown', esc);
    return () => { document.removeEventListener('click', outside); document.removeEventListener('keydown', esc); };
  }, [open]);
  const list = items.filter(Boolean).filter((it, i, a) => it !== '-' || (i > 0 && i < a.length - 1 && a[i - 1] !== '-'));
  if (list.length === 0) return null;
  return html`<div class="menu" ref=${ref}>
    <button type="button" class="icon" aria-label=${label} aria-haspopup="menu" aria-expanded=${open ? 'true' : 'false'} onClick=${() => setOpen(!open)}>
      <svg width="18" height="18" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><circle cx="5" cy="12" r="1.8"></circle><circle cx="12" cy="12" r="1.8"></circle><circle cx="19" cy="12" r="1.8"></circle></svg>
    </button>
    ${open && html`<div class="menu-pop" role="menu" ref=${pop}>${list.map((it, i) => it === '-'
      ? html`<div key=${i} class="menu-sep" role="separator"></div>`
      : it.href
        ? html`<a key=${i} role="menuitem" class=${'menu-item' + (it.danger ? ' danger' : '')} href=${it.href} onClick=${() => setOpen(false)}>
            <span>${it.label}</span>${it.count != null ? html`<span class="muted">${it.count}</span>` : null}</a>`
        : html`<button key=${i} type="button" role="menuitem" class=${'menu-item' + (it.danger ? ' danger' : '')} onClick=${() => { setOpen(false); it.run(); }}>
            <span>${it.label}</span>${it.count != null ? html`<span class="muted">${it.count}</span>` : null}</button>`)}</div>`}
  </div>`;
}

// Tip shows the text of any element with data-tip at once, when the pointer is on it or the keyboard reaches it: the
// first line bold, the rest under it. One Tip serves the whole UI.
export function Tip() {
  const [tip, setTip] = useState(null);
  const ref = useRef(null);
  useEffect(() => {
    const target = (e) => (e.target && e.target.closest ? e.target.closest('[data-tip]') : null);
    const show = (e) => { const el = target(e); if (el) setTip({ text: el.getAttribute('data-tip'), r: el.getBoundingClientRect() }); };
    const hide = (e) => { if (target(e)) setTip(null); };
    const clear = () => setTip(null);
    document.addEventListener('mouseover', show);
    document.addEventListener('mouseout', hide);
    document.addEventListener('focusin', show);
    document.addEventListener('focusout', hide);
    addEventListener('scroll', clear, true);
    addEventListener('hashchange', clear);
    return () => {
      document.removeEventListener('mouseover', show);
      document.removeEventListener('mouseout', hide);
      document.removeEventListener('focusin', show);
      document.removeEventListener('focusout', hide);
      removeEventListener('scroll', clear, true);
      removeEventListener('hashchange', clear);
    };
  }, []);
  useEffect(() => {
    const el = ref.current;
    if (!el || !tip) return;
    const w = el.offsetWidth, h = el.offsetHeight, r = tip.r;
    el.style.left = Math.max(8, Math.min(r.left + r.width / 2 - w / 2, innerWidth - w - 8)) + 'px';
    el.style.top = (r.top - h - 8 >= 8 ? r.top - h - 8 : r.bottom + 8) + 'px';
    el.style.visibility = 'visible';
  }, [tip]);
  if (!tip || !tip.text) return null;
  const [first, ...rest] = tip.text.split('\n');
  return html`<div key=${tip.text + tip.r.left + tip.r.top} class="tip" ref=${ref} role="tooltip" style="visibility:hidden;left:0;top:0">
    <b>${first}</b>${rest.map((l) => html`<span>${l}</span>`)}</div>`;
}

export function Pill({ kind, children }) {
  return html`<span class=${'pill ' + (kind || '')}>${children}</span>`;
}

export function Snap({ fqdn, id, short }) {
  return html`<a class="mono" href=${link('agents', fqdn, 'snapshots', id)}>${short || (id || '').slice(0, 8)}</a>`;
}

// shortName is what Delete now and Prune now ask you to type: an agent's name up to its first dot.
export const shortName = (name) => (name || '').split('.')[0];

// isPG says whether a job backs up a PostgreSQL database or its roles (decision 055).
export const isPG = (job) => !!job && job.engine === 'postgresql' && !!job.postgresql;

// backsUp says in words what a job backs up: its paths, or its database on a connection.
export function backsUp(job) {
  if (isPG(job)) return job.postgresql.globals ? `PostgreSQL ${job.postgresql.connection}: roles and other globals` : `PostgreSQL ${job.postgresql.connection}: database ${job.postgresql.database}`;
  return (job.paths || []).join('  ') || '—';
}

// The dump file a PostgreSQL job's snapshot holds.
export const dumpFile = (job) => (job.postgresql.globals ? 'globals.sql' : job.postgresql.database + '.dump');

// Icons for what a job backs up: a database, or files.
export const dbIcon = (size = 14) => html`<svg class="kind" width=${size} height=${size} viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" role="img" aria-label="PostgreSQL"><ellipse cx="12" cy="5" rx="8" ry="3"></ellipse><path d="M4 5v14c0 1.7 3.6 3 8 3s8-1.3 8-3V5"></path><path d="M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3"></path></svg>`;
export const folderIcon = (size = 14) => html`<svg class="kind" width=${size} height=${size} viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" role="img" aria-label="Files"><path d="M3 7a2 2 0 012-2h4l2 2h8a2 2 0 012 2v8a2 2 0 01-2 2H5a2 2 0 01-2-2z"></path></svg>`;

// Kind is a job's "Backs up" cell: a database with its connection, or the paths.
export function Kind({ job }) {
  if (isPG(job)) return html`<span class="kindline">${dbIcon()}<span>${job.postgresql.globals ? 'roles and other globals' : html`<b>${job.postgresql.database}</b>`} <span class="muted">on ${job.postgresql.connection}</span></span></span>`;
  return html`<span class="kindline">${folderIcon()}<span class="mono wrap">${(job.paths || []).join('  ') || '—'}</span></span>`;
}

// repoPath is where a site keeps an agent's repository: its bucket and prefix, as the settings give the bucket.
export const repoPath = (storage, agent) => `${storage && storage.bucket ? storage.bucket + '/' : ''}repos/${agent}`;

// can says whether the agent's answer lists the action: the server worked it out.
export const can = (host, perm) => !!host && (host.actions || []).includes(perm);

export function Findings({ findings, accepted, onAccept, blocking }) {
  if (!findings || findings.length === 0) return html`<p class="ok">No problems found.</p>`;
  const blocked = new Set((blocking || []).map((b) => b.code));
  return html`<ul class="findings">
    ${findings.map((f) => html`<li class=${f.severity}>
      <code>${f.code}</code> <strong>${f.severity === 'error' ? 'Error.' : f.severity === 'warning' ? 'Warning.' : 'Info.'}</strong> ${f.message}
      ${f.severity === 'warning' && onAccept && html`<label class="accept"><input type="checkbox" checked=${(accepted || []).includes(f.code)}
        onChange=${(ev) => onAccept(f.code, ev.target.checked)} /> I accept</label>`}
      ${f.severity === 'warning' && !onAccept && !blocked.has(f.code) && html` <em>(accepted)</em>`}
    </li>`)}
  </ul>`;
}

export function CopyButton({ text }) {
  const [copied, setCopied] = useState(false);
  return html`<button type="button" class="small" onClick=${async () => {
    try { await navigator.clipboard.writeText(text); setCopied(true); setTimeout(() => setCopied(false), 1500); } catch { /* not allowed here */ }
  }}>${copied ? 'Copied' : 'Copy'}</button>`;
}

// What to run where an agent runs to enroll it: the one-line install (decision 044), or the enrollment alone where the agent's
// packages are installed already, such as by configuration management.
export function EnrollLines({ e }) {
  return html`<pre>${e.install_script}</pre><${CopyButton} text=${e.install_script} />
    <details><summary class="muted">The agent's packages are installed already?</summary>
      <pre>${e.install_command}</pre><${CopyButton} text=${e.install_command} />
    </details>`;
}

export function Paged({ items, size = 25, render }) {
  const [page, setPage] = useState(0);
  const pages = Math.max(1, Math.ceil(items.length / size));
  const p = Math.min(page, pages - 1);
  const slice = items.slice(p * size, p * size + size);
  return html`${render(slice)}
    ${pages > 1 && html`<div class="pager">
      <button class="small" disabled=${p === 0} onClick=${() => setPage(p - 1)}>Previous</button>
      <span>${p * size + 1}–${Math.min(items.length, p * size + size)} of ${items.length} · page ${p + 1} of ${pages}</span>
      <button class="small" disabled=${p >= pages - 1} onClick=${() => setPage(p + 1)}>Next</button>
    </div>`}`;
}
