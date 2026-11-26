// Holdfast's own date field (spec §5.2): a day typed or picked as YYYY-MM-DD, empty for any date, with a calendar in
// the UI's style under it: English, Monday first, weekends like any other day, Today and Any date. Days that cannot be
// chosen are greyed out: after today, and on the wrong side of the other end of a range. The browser's own picker
// cannot do this: its language, format and look are the browser's.
import { html, useState, useEffect, useRef } from './lib.js';

const MONTH_NAMES = ['January', 'February', 'March', 'April', 'May', 'June', 'July', 'August', 'September', 'October', 'November', 'December'];
const WEEKDAYS = ['Mo', 'Tu', 'We', 'Th', 'Fr', 'Sa', 'Su'];

// isoDay is a date as YYYY-MM-DD, in the browser's zone.
export const isoDay = (d) => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;

// parseDay is a YYYY-MM-DD that is a real day, or null.
export function parseDay(s) {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec((s || '').trim());
  if (!m) return null;
  const d = new Date(+m[1], +m[2] - 1, +m[3]);
  return d.getMonth() === +m[2] - 1 ? d : null;
}

// dayProblem is what is wrong with a typed day, in the server's words: not a day, or after today.
export function dayProblem(v) {
  const s = (v || '').trim();
  if (!s) return '';
  if (!parseDay(s)) return `“${s}” isn't a date: type it as YYYY-MM-DD, such as 2026-11-01.`;
  if (s > isoDay(new Date())) return `${s} is after today: no file has been modified then yet.`;
  return '';
}

// rangeProblem is what is wrong with a range of days, From to To, either one empty: the first wrong one, or From after
// To. bad says which ends to mark.
export function rangeProblem(from, to) {
  const f = dayProblem(from), t = dayProblem(to);
  if (f) return { msg: f, bad: { from: true } };
  if (t) return { msg: t, bad: { to: true } };
  if (from && to && from.trim() > to.trim()) return { msg: `From (${from.trim()}) is after To (${to.trim()}).`, bad: { from: true, to: true } };
  return { msg: '', bad: {} };
}

// DateField is one date: value is YYYY-MM-DD or empty, for any date. min and max limit the calendar's days, each
// with why (such as "after To"), besides today, which is always the last day.
export function DateField({ value, onChange, label, min, max, minWhy, maxWhy, bad }) {
  const [open, setOpen] = useState(null); // the month shown, while the calendar is open
  const [pos, setPos] = useState({ top: 0, left: 0 });
  const input = useRef(null);
  const pop = useRef(null);
  const today = isoDay(new Date());
  const why = (v) => (v > today ? 'after today' : max && v > max ? maxWhy || 'too late' : min && v < min ? minWhy || 'too early' : '');
  const place = () => { const r = input.current.getBoundingClientRect(); setPos({ top: r.bottom + 6, left: Math.max(8, Math.min(r.left, innerWidth - 264)) }); };
  const show = () => { const d = parseDay(value) || new Date(); setOpen(new Date(d.getFullYear(), d.getMonth(), 1)); place(); };
  const close = () => setOpen(null);
  useEffect(() => {
    if (!open) return undefined;
    const outside = (e) => { if (!pop.current?.contains(e.target) && e.target !== input.current) close(); };
    const move = () => place();
    document.addEventListener('mousedown', outside);
    addEventListener('scroll', move, true);
    addEventListener('resize', move);
    return () => { document.removeEventListener('mousedown', outside); removeEventListener('scroll', move, true); removeEventListener('resize', move); };
  }, [!!open]);
  const pick = (v) => { onChange(v); close(); };
  let cal = null;
  if (open) {
    const sel = parseDay(value);
    const lead = (open.getDay() + 6) % 7;
    const days = new Date(open.getFullYear(), open.getMonth() + 1, 0).getDate();
    const cells = [];
    for (let d = 1; d <= days; d++) {
      const v = isoDay(new Date(open.getFullYear(), open.getMonth(), d));
      const w = why(v);
      cells.push(html`<button type="button" class=${[sel && isoDay(sel) === v ? 'on' : '', v === today ? 'today' : ''].join(' ')} disabled=${!!w}
        aria-label=${v + (w ? ', ' + w : '')} data-tip=${w ? `${v}: ${w}` : null} onClick=${() => pick(v)}>${d}</button>`);
    }
    const now = new Date();
    const last = open.getFullYear() * 12 + open.getMonth() >= now.getFullYear() * 12 + now.getMonth();
    const step = (n) => setOpen(new Date(open.getFullYear(), open.getMonth() + n, 1));
    cal = html`<div class="datepop" ref=${pop} role="dialog" aria-label=${'Choose ' + label} style=${`top:${pos.top}px;left:${pos.left}px`} onMouseDown=${(e) => e.preventDefault()}>
      <div class="dphead"><button type="button" class="small" aria-label="Previous month" onClick=${() => step(-1)}>‹</button><b>${MONTH_NAMES[open.getMonth()]} ${open.getFullYear()}</b>
        <button type="button" class="small" aria-label="Next month" disabled=${last} data-tip=${last ? 'No later months: nothing is modified in the future' : null} onClick=${() => step(1)}>›</button></div>
      <div class="dpgrid">${WEEKDAYS.map((w) => html`<span class="wd">${w}</span>`)}${Array.from({ length: lead }, () => html`<span></span>`)}${cells}</div>
      <div class="dpfoot"><button type="button" class="small" disabled=${!!why(today)} onClick=${() => pick(today)}>Today</button>
        <button type="button" class="link" onClick=${() => pick('')}>Any date</button></div></div>`;
  }
  return html`<span class="datefield"><input ref=${input} type="text" inputmode="numeric" autocomplete="off" placeholder="any date" aria-label=${label + ', YYYY-MM-DD'}
      class=${bad ? 'bad' : ''} aria-invalid=${bad ? 'true' : 'false'} value=${value}
      onFocus=${show} onClick=${() => { if (!open) show(); }}
      onInput=${(e) => { onChange(e.target.value); const d = parseDay(e.target.value); if (open && d) setOpen(new Date(d.getFullYear(), d.getMonth(), 1)); }}
      onKeyDown=${(e) => { if (e.key === 'Escape' || e.key === 'Enter') { if (open) e.preventDefault(); close(); } }} />${cal}</span>`;
}

