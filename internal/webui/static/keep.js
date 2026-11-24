// A job's restore points, as its calendar draws them (spec §5.2). A restore point is a snapshot in site A and, with
// off-site copies, its copy in site B, each in its own site's state. Presentation only: which snapshot restic keeps, and
// why, is restic's answer (forget --dry-run), as the snapshot lists give it in kept_by; what was deleted, and when, is
// the server's.

// The rules, coarsest first, as they appear in a retention and in restic's reasons ("daily within 14d").
export const RULES = [
  { key: 'yearly', unit: 'year', label: 'one per year' },
  { key: 'monthly', unit: 'month', label: 'one per month' },
  { key: 'weekly', unit: 'week', label: 'one per week' },
  { key: 'daily', unit: 'day', label: 'one per day' },
  { key: 'hourly', unit: 'hour', label: 'one per hour' },
];

// reasonKey turns restic's reason into the rule it names: "daily within 14d" is daily, "within 7d" is within,
// "last snapshot" is last. "oldest daily within 14d" is daily too: restic keeps the oldest snapshot to fill a rule
// that has fewer periods than its window.
export function reasonKey(r) {
  r = r.replace(/^oldest /, '');
  if (r.startsWith('last')) return 'last';
  if (r.startsWith('within')) return 'within';
  return r.split(' ')[0];
}

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

// finestRule is the finest rule whose window still covers a day: where the rules keep one per day, a day without a
// snapshot is a missed backup; further back, one per week or month, it was thinned out.
export function finestRule(retention, day, now = new Date()) {
  for (const r of RULES.slice().reverse()) {
    const w = retention[r.key];
    if (w && step('day', day, 1) > windowStart(w, now)) return r.unit;
  }
  return null;
}

/* ---------------- restore points, each site in its own state ---------------- */

// Site A: kept, deletion waits (an admin asked, the 7 days run), or deleted. Site B: copied, copy pending (not copied
// yet), deletion waits, deleted, or never copied (deleted in site A first, or copying is stopped for the job).
export const WORD_A = { kept: 'kept', waits: 'deletion waits', deleted: 'deleted' };
export const WORD_B = { copied: 'copied', pending: 'copy pending', waits: 'deletion waits', deleted: 'deleted', never: 'never copied' };

// point is one restore point: s is the snapshot in site A (null when site A has no record of it), c its copy in site B.
export function point(s, c, off, excluded, original) {
  const sa = !s || s.gone_at ? 'deleted' : s.deletion_requested_at ? 'waits' : 'kept';
  const sb = !off ? null : c ? (c.gone_at ? 'deleted' : c.deletion_requested_at ? 'waits' : 'copied') : sa === 'deleted' || excluded ? 'never' : 'pending';
  const id = s ? s.id : original || c.original;
  return {
    key: id, id, short: s ? s.short_id : id.slice(0, 8), a: s, b: c || null, copyId: c ? c.id : '', copyShort: c ? c.short_id || c.id.slice(0, 8) : '',
    time: (s || c).time, sa, sb, bytes: s?.total_bytes ?? c?.total_bytes,
    kept: (s && !s.gone_at && (s.kept_by || []).length ? s.kept_by : c && !c.gone_at ? c.kept_by : null) || [],
  };
}

// copiesByOriginal finds each snapshot's copy in site B by the original's ID: the copy still there, over one deleted.
export function copiesByOriginal(list) {
  const m = new Map();
  for (const c of list || []) {
    if (!c.original) continue;
    const o = m.get(c.original);
    if (!o || (o.gone_at && !c.gone_at)) m.set(c.original, c);
  }
  return m;
}

// points are a job's restore points, newest first. A snapshot that retention forgot in site A is no restore point,
// unless site B still holds its copy, or an admin deleted it: then its day shows what each site did. The server marks
// an admin's deletion as deleted_on_request_at on site A's snapshot, and as deleted_in_primary_at on site B's copy.
export function points(listA, listB, { off, excluded }) {
  const copies = off ? copiesByOriginal(listB) : new Map();
  const out = [];
  const seen = new Set();
  for (const s of listA || []) {
    const c = copies.get(s.id);
    seen.add(s.id);
    const onRequest = !!(s.deleted_on_request_at || (c && c.deleted_in_primary_at));
    if (s.gone_at && !onRequest && !(c && !c.gone_at)) continue;
    out.push(point(s, c, off, excluded));
  }
  for (const [orig, c] of copies) {
    if (seen.has(orig) || (c.gone_at && !c.deleted_in_primary_at)) continue;
    out.push(point(null, c, off, excluded, orig));
  }
  return out.sort((x, y) => new Date(y.time) - new Date(x.time));
}

// A day's half shows the state that needs a look first.
const PRI_A = ['waits', 'kept', 'deleted'];
const PRI_B = ['waits', 'pending', 'copied', 'deleted', 'never'];
export function dayState(ps, off) {
  return { a: PRI_A.find((x) => ps.some((p) => p.sa === x)), b: off ? PRI_B.find((x) => ps.some((p) => p.sb === x)) : null };
}

// Each state as a half square: solid for there, striped for a deletion that waits, hatched grey for deleted.
const HALF = {
  kept: 'linear-gradient(var(--sq-ok),var(--sq-ok))', copied: 'linear-gradient(var(--sq-ok),var(--sq-ok))',
  waits: 'repeating-linear-gradient(135deg,var(--sq-ok) 0 2px,var(--sq-ok-light) 2px 4px)',
  pending: 'linear-gradient(var(--sq-pending),var(--sq-pending))',
  deleted: 'repeating-linear-gradient(135deg,var(--sq-del) 0 1.2px,var(--panel) 1.2px 3.4px)',
  never: 'linear-gradient(var(--sq-never),var(--sq-never))',
};
export const cellBg = (st) => (st.b ? `background:${HALF[st.a]} top/100% 50% no-repeat, ${HALF[st.b]} bottom/100% 50% no-repeat` : `background:${HALF[st.a]}`);

// The combinations the legend shows, each with what it means.
export const COMBOS = [
  [{ a: 'kept', b: 'copied' }, 'in both sites'],
  [{ a: 'kept', b: 'pending' }, 'in site A; site B’s copy is pending'],
  [{ a: 'kept', b: 'deleted' }, 'only in site A: deleted in site B'],
  [{ a: 'deleted', b: 'copied' }, 'only in site B: deleted in site A'],
  [{ a: 'deleted', b: 'deleted' }, 'deleted in both sites'],
  [{ a: 'waits', b: 'copied' }, 'deletion waits in site A'],
  [{ a: 'kept', b: 'waits' }, 'deletion waits in site B'],
  [{ a: 'waits', b: 'waits' }, 'deletion waits in both'],
  [{ a: 'deleted', b: 'never' }, 'deleted in site A before it was copied'],
];
