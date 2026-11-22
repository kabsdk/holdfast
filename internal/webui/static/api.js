// Every /api/v1 call the UI makes, and nothing else talks to the server (decision 028). The parity test
// reads this file: each call is one line of the form `name: (...) => req('METHOD', '/api/v1/...')`.
//
// A browser session is a cookie; the browser adds the Origin header the server checks on every change.

export class APIError extends Error {
  constructor(status, data) {
    super((data && data.error) || `HTTP ${status}`);
    this.status = status;
    this.data = data || {};
  }
}

async function req(method, path, body) {
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
  hosts: () => req('GET', '/api/v1/hosts'),
  host: (f) => req('GET', `/api/v1/hosts/${e(f)}`),
  addHost: (body) => req('POST', '/api/v1/hosts', body),
  removeHost: (f, body) => req('DELETE', `/api/v1/hosts/${e(f)}`, body),
  hostEvents: (f, limit) => req('GET', `/api/v1/hosts/${e(f)}/events${q({ limit })}`),
  enrollmentCode: (f) => req('POST', `/api/v1/hosts/${e(f)}/enrollment-code`),
  setS3Key: (f, body) => req('PUT', `/api/v1/hosts/${e(f)}/s3-key`, body),
  rotate: (f, body) => req('POST', `/api/v1/hosts/${e(f)}/rotate`, body),
  decommission: (f, body) => req('POST', `/api/v1/hosts/${e(f)}/decommission`, body),
  cancelDecommission: (f, body) => req('DELETE', `/api/v1/hosts/${e(f)}/decommission`, body),
  rollbackPreview: (f, to) => req('GET', `/api/v1/hosts/${e(f)}/rollback${q({ to })}`),
  rollback: (f, body) => req('POST', `/api/v1/hosts/${e(f)}/rollback`, body),
  rollbacks: (f) => req('GET', `/api/v1/hosts/${e(f)}/rollbacks`),
  acknowledge: (f) => req('POST', `/api/v1/hosts/${e(f)}/acknowledge`),
  snapshots: (f, opts) => req('GET', `/api/v1/hosts/${e(f)}/snapshots${q(opts || {})}`),
  files: (f, id, path, page) => req('GET', `/api/v1/hosts/${e(f)}/snapshots/${e(id)}/files${q({ path, page })}`),
  changes: (f, id, against, page) => req('GET', `/api/v1/hosts/${e(f)}/snapshots/${e(id)}/changes${q({ against, page })}`),
  find: (f, pattern) => req('GET', `/api/v1/hosts/${e(f)}/find${q({ pattern })}`),
  restore: (f, id, body) => req('POST', `/api/v1/hosts/${e(f)}/snapshots/${e(id)}/restore`, body),
  restorePair: (f, id, body) => req('POST', `/api/v1/hosts/${e(f)}/snapshots/${e(id)}/restore-pair`, body),
  restorePairStatus: (f, a, b, path) => req('GET', `/api/v1/hosts/${e(f)}/restore-pair${q({ a, b, path })}`),
  restores: () => req('GET', '/api/v1/restores'),
  search: (text) => req('GET', `/api/v1/search${q({ q: text })}`),
  setJob: (f, job, body) => req('PUT', `/api/v1/hosts/${e(f)}/jobs/${e(job)}`, body),
  removeJob: (f, job, body) => req('DELETE', `/api/v1/hosts/${e(f)}/jobs/${e(job)}`, body),
  checkJob: (f, job, body) => req('POST', `/api/v1/hosts/${e(f)}/jobs/${e(job)}/check`, body),
  keepLonger: (f, job, body) => req('POST', `/api/v1/hosts/${e(f)}/jobs/${e(job)}/keep-longer`, body),
  runNow: (f, job) => req('POST', `/api/v1/hosts/${e(f)}/jobs/${e(job)}/run`),
  pending: (f) => req('GET', `/api/v1/hosts/${e(f)}/pending`),
  retentionRun: (f) => req('POST', `/api/v1/hosts/${e(f)}/retention/run`),
  pruneNowPlan: (f) => req('GET', `/api/v1/hosts/${e(f)}/prune-now`),
  pruneNow: (f, body) => req('POST', `/api/v1/hosts/${e(f)}/prune-now`, body),
  deleteSnapshot: (f, id, body) => req('POST', `/api/v1/hosts/${e(f)}/snapshots/${e(id)}/delete`, body),
  cancelDeletion: (f, id) => req('DELETE', `/api/v1/hosts/${e(f)}/snapshots/${e(id)}/delete`),
  hostPrunes: (f, all) => req('GET', `/api/v1/hosts/${e(f)}/prunes${q({ all })}`),
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
  logout: () => req('POST', '/auth/logout'),
};
