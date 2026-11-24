// Every /api/v1 call the UI makes, and nothing else talks to the server (decision 028). The parity test
// reads this file: each call is one line of the form `name: (...) => req('METHOD', '/api/v1/...')`.
//
// A browser session is a cookie; the browser adds the Origin header the server checks on every change.
//
// On the off-site pages (#/offsite/), every call goes to the off-site copies' API, /api/v1/offsite/: the same calls,
// on the off-site repositories (decision 041).
import { onOffsite } from './lib.js';

export class APIError extends Error {
  constructor(status, data) {
    super((data && data.error) || `HTTP ${status}`);
    this.status = status;
    this.data = data || {};
  }
}

async function req(method, path, body) {
  if (onOffsite() && path.startsWith('/api/v1/') && !path.startsWith('/api/v1/offsite/')) path = '/api/v1/offsite' + path.slice('/api/v1'.length);
  const opts = { method, credentials: 'same-origin', headers: {} };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const r = await fetch(path, opts);
  if (r.status === 401) {
    location.href = '/auth/login?next=' + encodeURIComponent('/' + location.hash);
    throw new APIError(401, { error: 'not logged in' });
  }
  const text = await r.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = { error: text }; }
  if (!r.ok) throw new APIError(r.status, data);
  return data;
}

const e = encodeURIComponent;
const q = (o) => {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(o)) if (v !== undefined && v !== '' && v !== false) p.set(k, v === true ? '1' : v);
  const s = p.toString();
  return s ? '?' + s : '';
};

export const api = {
  me: () => req('GET', '/api/v1/me'),
  status: () => req('GET', '/api/v1/status'),
  settings: () => req('GET', '/api/v1/settings'),
  hosts: () => req('GET', '/api/v1/agents'),
  host: (f) => req('GET', `/api/v1/agents/${e(f)}`),
  addHost: (body) => req('POST', '/api/v1/agents', body),
  notifyTest: () => req('POST', '/api/v1/notify/test', {}),
  importHost: (body) => req('POST', '/api/v1/agents/import', body),
  removeHost: (f, body) => req('DELETE', `/api/v1/agents/${e(f)}`, body),
  hostEvents: (f, limit) => req('GET', `/api/v1/agents/${e(f)}/events${q({ limit })}`),
  hostDays: (f, days, tz) => req('GET', `/api/v1/agents/${e(f)}/days${q({ days, tz })}`),
  enrollmentCode: (f) => req('POST', `/api/v1/agents/${e(f)}/enrollment-code`),
  rotate: (f, body) => req('POST', `/api/v1/agents/${e(f)}/rotate`, body),
  decommission: (f, body) => req('POST', `/api/v1/agents/${e(f)}/decommission`, body),
  cancelDecommission: (f, body) => req('DELETE', `/api/v1/agents/${e(f)}/decommission`, body),
  rollbackPreview: (f, to) => req('GET', `/api/v1/agents/${e(f)}/rollback${q({ to })}`),
  rollback: (f, body) => req('POST', `/api/v1/agents/${e(f)}/rollback`, body),
  rollbacks: (f) => req('GET', `/api/v1/agents/${e(f)}/rollbacks`),
  acknowledge: (f) => req('POST', `/api/v1/agents/${e(f)}/acknowledge`),
  snapshots: (f, opts) => req('GET', `/api/v1/agents/${e(f)}/snapshots${q(opts || {})}`),
  files: (f, id, path, page) => req('GET', `/api/v1/agents/${e(f)}/snapshots/${e(id)}/files${q({ path, page })}`),
  changes: (f, id, against, page) => req('GET', `/api/v1/agents/${e(f)}/snapshots/${e(id)}/changes${q({ against, page })}`),
  find: (f, pattern, snapshot) => req('GET', `/api/v1/agents/${e(f)}/find${q({ pattern, snapshot })}`),
  restore: (f, id, body) => req('POST', `/api/v1/agents/${e(f)}/snapshots/${e(id)}/restore`, body),
  restorePair: (f, id, body) => req('POST', `/api/v1/agents/${e(f)}/snapshots/${e(id)}/restore-pair`, body),
  restorePairStatus: (f, a, b, path) => req('GET', `/api/v1/agents/${e(f)}/restore-pair${q({ a, b, path })}`),
  restores: () => req('GET', '/api/v1/restores'),
  search: (text) => req('GET', `/api/v1/search${q({ q: text })}`),
  setJob: (f, job, body) => req('PUT', `/api/v1/agents/${e(f)}/jobs/${e(job)}`, body),
  setJobs: (f, body) => req('POST', `/api/v1/agents/${e(f)}/jobs`, body),
  // An agent's PostgreSQL connections: the passwords are write-only, and a test lists what the server holds.
  connections: (f) => req('GET', `/api/v1/agents/${e(f)}/postgresql`),
  setConnection: (f, name, body) => req('PUT', `/api/v1/agents/${e(f)}/postgresql/${e(name)}`, body),
  removeConnection: (f, name, message) => req('DELETE', `/api/v1/agents/${e(f)}/postgresql/${e(name)}${q({ message })}`),
  testConnection: (f, name) => req('POST', `/api/v1/agents/${e(f)}/postgresql/${e(name)}/test`),
  removeJob: (f, job, body) => req('DELETE', `/api/v1/agents/${e(f)}/jobs/${e(job)}`, body),
  checkJob: (f, job, body) => req('POST', `/api/v1/agents/${e(f)}/jobs/${e(job)}/check`, body),
  keepLonger: (f, job, body) => req('POST', `/api/v1/agents/${e(f)}/jobs/${e(job)}/keep-longer`, body),
  runNow: (f, job) => req('POST', `/api/v1/agents/${e(f)}/jobs/${e(job)}/run`),
  pending: (f) => req('GET', `/api/v1/agents/${e(f)}/pending`),
  retentionRun: (f) => req('POST', `/api/v1/agents/${e(f)}/retention/run`),
  pruneNowPlan: (f) => req('GET', `/api/v1/agents/${e(f)}/prune-now`),
  pruneNow: (f, body) => req('POST', `/api/v1/agents/${e(f)}/prune-now`, body),
  deleteSnapshot: (f, id, body) => req('POST', `/api/v1/agents/${e(f)}/snapshots/${e(id)}/delete`, body),
  cancelDeletion: (f, id) => req('DELETE', `/api/v1/agents/${e(f)}/snapshots/${e(id)}/delete`),
  hostPrunes: (f, all) => req('GET', `/api/v1/agents/${e(f)}/prunes${q({ all })}`),
  prunes: (all) => req('GET', `/api/v1/prunes${q({ all })}`),
  retentionCuts: () => req('GET', '/api/v1/retention-cuts'),
  revisions: (limit) => req('GET', `/api/v1/revisions${q({ limit })}`),
  revision: (n) => req('GET', `/api/v1/revisions/${e(n)}`),
  diff: (a, b) => req('GET', `/api/v1/revisions/${e(a)}/diff/${e(b)}`),
  audit: (opts) => req('GET', `/api/v1/audit${q(opts || {})}`),
  checkAll: () => req('POST', '/api/v1/maintenance/check'),
  access: () => req('GET', '/api/v1/access'),
  setAccess: (body) => req('PUT', '/api/v1/access', body),
  sessions: () => req('GET', '/api/v1/sessions'),
  endSessions: (user) => req('DELETE', `/api/v1/sessions/${e(user)}`),
  offsiteCopies: (f) => req('GET', `/api/v1/agents/${e(f)}/offsite`),
  exclude: (f, job, body) => req('POST', `/api/v1/agents/${e(f)}/jobs/${e(job)}/exclude`, body),
  include: (f, job, body) => req('POST', `/api/v1/agents/${e(f)}/jobs/${e(job)}/include`, body),
  matchPrimary: (f, job, body) => req('POST', `/api/v1/agents/${e(f)}/jobs/${e(job)}/match-primary`, body),
  // The off-site copies' API from the server's own pages: a job's Off-site view and its off-site settings.
  offsiteSnapshots: (f, opts) => req('GET', `/api/v1/offsite/agents/${e(f)}/snapshots${q(opts || {})}`),
  offsiteExclude: (f, job, body) => req('POST', `/api/v1/offsite/agents/${e(f)}/jobs/${e(job)}/exclude`, body),
  offsiteInclude: (f, job, body) => req('POST', `/api/v1/offsite/agents/${e(f)}/jobs/${e(job)}/include`, body),
  offsiteMatchPrimary: (f, job, body) => req('POST', `/api/v1/offsite/agents/${e(f)}/jobs/${e(job)}/match-primary`, body),
  offsiteDeleteSnapshot: (f, id, body) => req('POST', `/api/v1/offsite/agents/${e(f)}/snapshots/${e(id)}/delete`, body),
  offsiteCancelDeletion: (f, id) => req('DELETE', `/api/v1/offsite/agents/${e(f)}/snapshots/${e(id)}/delete`),
  offsiteSettings: () => req('GET', '/api/v1/offsite/settings'),
  exports: () => req('GET', '/api/v1/exports'),
  exportNow: () => req('POST', '/api/v1/exports'),
  logout: () => req('POST', '/auth/logout'),
};
