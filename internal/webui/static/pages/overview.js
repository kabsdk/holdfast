// Overview, the start page: every agent you may see, worst first, with tiles that count what is wrong and filter the
// list by it. Each problem shows once, under its agent (spec §5.5a); the server works them out (problems.go).
import { html, useState, link, ago, until, useAction, ErrorBox, Problem, isPG, backsUp, dbIcon } from '../lib.js';
import { api } from '../api.js';

// jobState is how a job's pill reads: the server's problem for it, or OK.
export function jobState(host, job) {
  const p = (host.problems || []).find((x) => x.job_id === job.id);
  if (p) return { kind: p.severity, text: { stale: 'stale', failed: job.status?.last_status || 'failed', incomplete: 'incomplete' }[p.code] || p.code };
  if (!job.status) return { kind: '', text: host.state === 'enrolled' ? 'waiting for first backup' : 'not enrolled' };
  return { kind: 'ok', text: 'OK' };
}

// The problems a tile of their own counts; the rest are "Other problems".
const JOB_CODES = ['stale', 'failed', 'incomplete'];
const OFFSITE_CODES = ['offsite_copy', 'offsite_lag'];
const worst = (h) => ((h.problems || []).some((p) => p.severity === 'error') ? 2 : (h.problems || []).length ? 1 : 0);
const other = (h) => (h.problems || []).some((p) => !JOB_CODES.includes(p.code) && !OFFSITE_CODES.includes(p.code));
const offsite = (h) => (h.problems || []).some((p) => OFFSITE_CODES.includes(p.code));
const COLS = 'minmax(0,2fr) minmax(0,3fr) minmax(0,1fr) minmax(0,1fr) minmax(0,1.1fr)';

// agentSub is the line under an agent's name: the address it last connected from, which is all the server knows of
// where it runs (decision 056), and its system.
export function agentSub(h) {
  if (!h.agent) return h.state.replace(/_/g, ' ');
  return [h.agent.remote_addr && `from ${h.agent.remote_addr}`, `${h.agent.os_id} ${h.agent.os_version}`.trim()].filter(Boolean).join(' · ');
}

function AgentRow({ h }) {
  const successes = h.jobs.map((j) => j.status?.last_success_at).filter(Boolean);
  const oldest = successes.length ? successes.reduce((a, b) => (b < a ? b : a)) : null;
  const nexts = h.jobs.map((j) => j.next_runs?.[0]).filter(Boolean);
  const next = nexts.length ? nexts.reduce((a, b) => (b < a ? b : a)) : null;
  const probs = h.problems || [];
  const stale = probs.some((p) => p.code === 'stale');
  const offline = probs.some((p) => p.code === 'offline');
  return html`<div class=${'hostrow' + (worst(h) === 2 ? ' bad' : '')}>
    <div class="gt" style=${'--cols:' + COLS}>
      <div style="min-width:0"><a class="wrap" href=${link('agents', h.fqdn)}><b>${h.fqdn}</b></a>
        <div class="sub">${agentSub(h)}</div></div>
      <div>${h.jobs.length === 0 ? html`<span class="muted">no job yet</span>` : h.jobs.map((j) => {
        const s = jobState(h, j);
        const k = s.kind === 'error' || s.kind === 'warning' ? s.kind : '';
        return html`<a class=${'jobpill ' + k} href=${link('agents', h.fqdn, 'jobs', j.id)} data-tip=${`${j.id}\nBacks up ${backsUp(j)}`}><span class=${'dot ' + (k || s.kind)}></span>${isPG(j) && dbIcon(12)}${j.id}${k ? ' · ' + s.text : ''}</a>`;
      })}</div>
      <span class=${'num' + (stale ? ' error' : '')}>${oldest ? ago(oldest) : '—'}</span>
      <span class="num muted">${next ? until(next) : '—'}</span>
      <span class=${'num' + (offline ? ' error' : ' muted')}>${h.agent ? (offline ? 'offline ' : 'seen ') + ago(h.agent.last_seen_at) : '—'}</span>
    </div>
    ${probs.length > 0 && html`<div class="probs">${probs.map((p) => html`<${Problem} p=${p} />`)}</div>`}
  </div>`;
}

export function Overview({ me, hosts, vault, settings }) {
  const [filter, setFilter] = useState('all');
  const [q, setQ] = useState('');
  const check = useAction();
  const states = hosts.flatMap((h) => h.jobs.map((j) => jobState(h, j)));
  const count = (code) => hosts.flatMap((h) => h.problems || []).filter((p) => p.code === code).length;
  const tiles = [
    ['all', vault ? 'Copied agents' : 'Agents', hosts.length, `${states.filter((s) => s.kind === 'ok').length} of ${states.length} jobs OK`, ''],
    ['stale', 'Stale', count('stale'), 'no backup in time', 'error'],
    ['failed', 'Failed', count('failed'), 'last run failed', 'error'],
    ['incomplete', 'Incomplete', count('incomplete'), 'files not read', 'warning'],
    !vault && settings.offsite && ['offsite', 'Off-site', hosts.filter(offsite).length, 'copies failing or behind', 'error'],
    ['other', 'Other problems', hosts.filter(other).length, 'offline, drift, PostgreSQL…', 'warning'],
  ].filter(Boolean);
  const keep = (h) => filter === 'all' || (filter === 'other' ? other(h) : filter === 'offsite' ? offsite(h) : (h.problems || []).some((p) => p.code === filter));
  const text = q.trim().toLowerCase();
  const shown = hosts.filter((h) => keep(h) && (!text || h.fqdn.includes(text))).sort((a, b) => worst(b) - worst(a) || a.fqdn.localeCompare(b.fqdn));
  const canCheck = hosts.some((h) => (h.actions || []).includes('maintenance'));
  const title = tiles.find((t) => t[0] === filter)[1];
  return html`
    <div class="spread">
      <h1>Overview</h1>
      <div class="row">
        ${canCheck && html`<button disabled=${check.busy} onClick=${() => check.run(() => api.checkAll())}>Check all repositories</button>`}
        ${me.add_hosts && !vault && html`<a class="button primary" href="#/add-agent">Add agent</a>`}
      </div>
    </div>
    <${ErrorBox} error=${check.error} />
    ${check.done && html`<div class="banner ok">Checks asked for on ${check.done.length} agents. Each agent runs restic check on its next poll; its result is in the agent's runs.</div>`}
    <section class="tiles" aria-label="Status: choose one to filter the agents">
      ${tiles.map(([id, label, n, sub, kind]) => html`<button type="button" class=${'tile' + (filter === id ? ' on' : '')} aria-pressed=${filter === id ? 'true' : 'false'}
          onClick=${() => setFilter(filter === id ? 'all' : id)}>
        <span class="l">${label}</span><span class=${'n ' + (id === 'all' ? '' : n === 0 ? 'zero' : kind)}>${n}</span><span class="s">${sub}</span></button>`)}
    </section>
    <section class="list" aria-label="Agents">
      <div class="list-head">
        <div class="row" style="align-items:baseline">
          <h2>${filter === 'all' ? (vault ? 'All copied agents' : 'All agents') : title}</h2>
          <span class="muted">${shown.length} ${shown.length === 1 ? 'agent' : 'agents'}${filter === 'all' ? ' · problems first' : ''}</span>
          ${filter !== 'all' && html`<button type="button" class="link" onClick=${() => setFilter('all')}>Show all agents</button>`}
        </div>
        <input id="host-filter" type="search" aria-label="Filter agents by name" placeholder="Filter by name" value=${q} onInput=${(e) => setQ(e.target.value)} />
      </div>
      <div class="scrollx"><div style="min-width:860px">
        <div class="gt head" style=${'--cols:' + COLS}><span>Agent</span><span>Jobs</span><span>Last backup</span><span>Next backup</span><span>Agent seen</span></div>
        ${shown.map((h) => html`<${AgentRow} key=${h.fqdn} h=${h} />`)}
        ${shown.length === 0 && html`<p class="muted" style="padding:1rem">${hosts.length === 0 ? 'No agents yet.' : 'No agent matches.'}</p>`}
      </div></div>
    </section>`;
}
