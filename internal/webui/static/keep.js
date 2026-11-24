// What restic keeps, drawn: the job page's rows of squares and the calendar. Presentation only: which snapshot is kept,
// and why, is restic's answer (forget --dry-run, spec §5.2), as the snapshot list gives it in kept_by.
import { fullDate, clock, MONTHS } from './lib.js';

// The rules, coarsest first, as they appear in a retention and in restic's reasons ("daily within 14d").
export const RULES = [
  { key: 'yearly', unit: 'year', label: 'One per year' },
  { key: 'monthly', unit: 'month', label: 'One per month' },
  { key: 'weekly', unit: 'week', label: 'One per week' },
  { key: 'daily', unit: 'day', label: 'One per day' },
  { key: 'hourly', unit: 'hour', label: 'One per hour' },
];

// A row shows at most this many squares; the calendar has the rest.
export const CAP = 30;

// reasonKey turns restic's reason into the rule it names: "daily within 14d" is daily, "within 7d" is within,
// "last snapshot" is last. "oldest daily within 14d" is daily too: restic keeps the oldest snapshot to fill a rule
// that has fewer periods than its window.
export function reasonKey(r) {
  r = r.replace(/^oldest /, '');
  if (r.startsWith('last')) return 'last';
  if (r.startsWith('within')) return 'within';
  return r.split(' ')[0];
}

const keyed = (s) => (s.kept_by || []).map(reasonKey);

// Periods, in the browser's time.
export function startOf(unit, t) {
  const d = new Date(t);
  switch (unit) {
    case 'hour': d.setMinutes(0, 0, 0); return d;
    case 'day': d.setHours(0, 0, 0, 0); return d;
    case 'week': d.setHours(0, 0, 0, 0); d.setDate(d.getDate() - ((d.getDay() + 6) % 7)); return d;
    case 'month': return new Date(d.getFullYear(), d.getMonth(), 1);
    default: return new Date(d.getFullYear(), 0, 1);
  }
}

export function step(unit, d, n) {
  const x = new Date(d);
  switch (unit) {
    case 'hour': x.setHours(x.getHours() + n); return x;
    case 'day': x.setDate(x.getDate() + n); return x;
    case 'week': x.setDate(x.getDate() + 7 * n); return x;
    case 'month': return new Date(x.getFullYear(), x.getMonth() + n, 1);
    default: return new Date(x.getFullYear() + n, 0, 1);
  }
}

// windowStart is when a retention window such as {n: 14, unit: "days"} began, counted back from now.
export function windowStart(w, now) {
  const unit = { hours: 'hour', days: 'day', weeks: 'week', months: 'month', years: 'year' }[w.unit] || 'day';
  if (unit === 'week') return step('day', now, -7 * w.n);
  if (unit === 'month' || unit === 'year') {
    const x = new Date(now);
    if (unit === 'month') x.setMonth(x.getMonth() - w.n); else x.setFullYear(x.getFullYear() - w.n);
    return x;
  }
  return step(unit, now, -w.n);
}

function periodLabel(unit, d) {
  switch (unit) {
    case 'hour': return `${fullDate(d)}, ${clock(d)}`;
    case 'day': return fullDate(d);
    case 'week': return 'Week of ' + fullDate(d);
    case 'month': return `${MONTHS[d.getMonth()]} ${d.getFullYear()}`;
    default: return String(d.getFullYear());
  }
}

// slotTip is what a square says on hover: the period, the snapshot, and restic's reasons for keeping it.
export function slotTip(label, s) {
  const lines = [label];
  lines.push(label === fullDate(s.time) ? `Snapshot ${s.short_id} at ${clock(s.time)}` : `Snapshot ${s.short_id}, ${fullDate(s.time)} ${clock(s.time)}`);
  if ((s.kept_by || []).length) lines.push('Kept: ' + s.kept_by.join(', '));
  if (s.deletion_requested_at) lines.push('Deletion asked: it waits 7 days');
  return lines.join('\n');
}

// rows are the job page's rows: one per rule of the retention, one square per period of its window, oldest first;
// then "every backup" and the newest N, whose squares are snapshots. A square holds the snapshot restic keeps by that
// rule in that period, or nothing.
// A period that ended before the job's first snapshot has before set: it could not have one.
export function rows(retention, snaps, now = new Date()) {
  const kept = snaps.filter((s) => !s.gone_at && (s.kept_by || []).length).sort((a, b) => new Date(b.time) - new Date(a.time));
  const first = snaps.reduce((m, s) => Math.min(m, new Date(s.time).getTime()), Infinity);
  const out = [];
  for (const r of RULES) {
    const w = retention[r.key];
    if (!w) continue;
    const from = windowStart(w, now);
    const mine = kept.filter((s) => keyed(s).includes(r.key));
    const slots = [];
    for (let p = startOf(r.unit, now), i = 0; step(r.unit, p, 1) > from && i < 5000; p = step(r.unit, p, -1), i++) {
      const end = step(r.unit, p, 1);
      const label = periodLabel(r.unit, p);
      const s = mine.find((x) => new Date(x.time) >= p && new Date(x.time) < end);
      slots.unshift({ label, s, before: !s && end.getTime() <= first });
    }
    out.push({ key: r.key, label: r.label, sub: `for ${w.n} ${w.unit} · ${slots.filter((x) => x.s).length} kept`, unit: r.unit, slots });
  }
  const within = kept.filter((s) => keyed(s).includes('within'));
  if (retention.within) {
    out.push({ key: 'within', label: 'Every backup', sub: `for ${retention.within.n} ${retention.within.unit} · ${within.length} kept`, unit: 'snapshot',
      slots: within.slice().reverse().map((s) => ({ label: fullDate(s.time), s })) });
  }
  const last = kept.filter((s) => keyed(s).includes('last'));
  if (retention.keep_last) {
    out.push({ key: 'last', label: `The newest ${retention.keep_last}`, sub: 'always kept', unit: 'snapshot',
      slots: last.slice().reverse().map((s) => ({ label: fullDate(s.time), s })) });
  }
  return out;
}

// finestRule is the finest rule whose window still covers a day: where the rules keep one per day, a day without a
// snapshot is a missed backup; further back, one per week or month, it was thinned out.
export function finestRule(retention, day, now = new Date()) {
  for (const r of RULES.slice().reverse()) {
    const w = retention[r.key];
    if (w && step('day', day, 1) > windowStart(w, now)) return r.unit;
  }
  return null;
}
