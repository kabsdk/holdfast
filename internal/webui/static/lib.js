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
  return { busy, error, done, run, setError, setDone };
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

export function Pill({ kind, children }) {
  return html`<span class=${'pill ' + (kind || '')}>${children}</span>`;
}

export function Snap({ fqdn, id, short }) {
  return html`<a class="mono" href=${link('hosts', fqdn, 'snapshots', id)}>${short || (id || '').slice(0, 8)}</a>`;
}

// can says whether the host's answer lists the action: the server worked it out.
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

// What to run on a host to enroll it: the one-line install (decision 044), or the enrollment alone where the agent's
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
