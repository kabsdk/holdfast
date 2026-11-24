// The UI's shell: navigation, the top bar, and the routes. Each page is in pages/.
import { html, render, useState, useEffect, useRoute, usePoll, go, link, primaryLink, offsiteLink, onOffsite, ErrorBox, Tip } from './lib.js';
import { api } from './api.js';
import { Overview } from './pages/overview.js';
import { Host, Pending } from './pages/host.js';
import { Snapshot } from './pages/snapshots.js';
import { Job } from './pages/job.js';
import { Configure } from './pages/configure.js';
import { Restores } from './pages/restores.js';
import { History, PruneNow } from './pages/history.js';
import { AddAgent } from './pages/addhost.js';
import { Audit, Access, Settings, Exports } from './pages/admin.js';
import { Rollback } from './pages/rollback.js';

function Search() {
  const [text, setText] = useState('');
  const [res, setRes] = useState(null);
  useEffect(() => {
    if (text.trim().length < 2) { setRes(null); return; }
    const t = setTimeout(async () => {
      try { setRes(await api.search(text.trim())); } catch (err) { setRes({ error: err.message }); }
    }, 250);
    return () => clearTimeout(t);
  }, [text]);
  const pick = (href) => { setText(''); setRes(null); location.hash = href.slice(1); };
  const n = res && !res.error ? res.hosts.length + res.jobs.length + res.snapshots.length : 0;
  return html`<div class="searchbox">
    <input type="search" placeholder="Search agent, job or snapshot ID" value=${text} onInput=${(e) => setText(e.target.value)} />
    ${res && html`<div class="results">
      ${res.error && html`<p class="muted">${res.error}</p>`}
      ${!res.error && n === 0 && html`<p class="muted">No agent, job or snapshot matches “${text}”. Snapshot IDs need at least 4 hex characters.</p>`}
      ${!res.error && res.hosts.map((h) => html`<a href="#" onClick=${(e) => { e.preventDefault(); pick(link('agents', h)); }}>Agent · ${h}</a>`)}
      ${!res.error && res.jobs.map((j) => html`<a href="#" onClick=${(e) => { e.preventDefault(); pick(link('agents', j.fqdn, 'jobs', j.job_id)); }}>Job · ${j.fqdn} · ${j.job_id}</a>`)}
      ${!res.error && res.snapshots.map((s) => html`<a href="#" onClick=${(e) => { e.preventDefault(); pick(link('agents', s.fqdn, 'snapshots', s.id)); }}>
        ${s.state === 'present' ? 'Snapshot' : s.state === 'deleted' ? 'Deleted' : 'Disappeared'} · <span class="mono">${s.short_id}</span> · ${s.fqdn} · ${s.job_id}</a>`)}
      ${!res.error && n > 0 && html`<p class="muted">Same search: <code>holdfast search ${text.trim()}</code></p>`}
    </div>`}
  </div>`;
}

function Shell({ me, settings }) {
  const path = useRoute();
  const hosts = usePoll(() => api.hosts(), []);
  const status = usePoll(() => api.status(), [], 60000); // the server's own problems, such as its export
  // A query (?path=… on Files, ?connection=… on Add job) belongs to the page, not to the route.
  const seg = path.split('?')[0].split('/').filter(Boolean).map(decodeURIComponent);
  // Hosts became agents (decision 056): the old addresses still open the same pages.
  if (seg[0] === 'hosts') seg[0] = 'agents';
  if (seg[0] === 'add-host') seg[0] = 'add-agent';
  const errors = (hosts.data || []).filter((h) => (h.problems || []).some((p) => p.severity === 'error'));
  // The off-site pages are this UI in red, on the off-site API, with the off-site actions only (decisions 039, 041).
  const vault = settings.role === 'vault';
  const ctx = { me, settings, vault, hosts: hosts.data || [], reloadHosts: hosts.reload };

  let page;
  // Overview is the start page; the Problems page it replaced still opens it (spec §5.5a).
  if (seg.length === 0 || seg[0] === 'problems') page = html`<${Overview} ...${ctx} />`;
  else if (seg[0] === 'restores' && !vault) page = html`<${Restores} ...${ctx} />`;
  else if (seg[0] === 'history') page = html`<${History} ...${ctx} />`;
  else if (seg[0] === 'add-agent' && !vault) page = html`<${AddAgent} ...${ctx} />`;
  else if (seg[0] === 'audit') page = html`<${Audit} ...${ctx} />`;
  else if (seg[0] === 'access' && !vault) page = html`<${Access} ...${ctx} />`;
  else if (seg[0] === 'settings') page = html`<${Settings} ...${ctx} />`;
  else if (seg[0] === 'exports' && !vault) page = html`<${Exports} ...${ctx} />`;
  else if (seg[0] === 'agents' && seg[1]) {
    const fqdn = seg[1];
    if (seg.length === 2) page = html`<${Host} fqdn=${fqdn} ...${ctx} />`;
    else if (seg[2] === 'pending') page = html`<${Pending} fqdn=${fqdn} ...${ctx} />`;
    else if (seg[2] === 'prune-now') page = html`<${PruneNow} fqdn=${fqdn} ...${ctx} />`;
    else if (seg[2] === 'rollback') page = html`<${Rollback} fqdn=${fqdn} ...${ctx} />`;
    else if (seg[2] === 'configure') page = html`<${Configure} fqdn=${fqdn} job=${seg[3] || ''} ...${ctx} />`;
    // A job's page, with its calendar of restore points; …/snapshots and …/calendar, its old pages, open it too.
    else if (seg[2] === 'jobs' && seg[3] && (!seg[4] || seg[4] === 'snapshots' || seg[4] === 'calendar')) page = html`<${Job} key=${fqdn + '/' + seg[3]} fqdn=${fqdn} job=${seg[3]} ...${ctx} />`;
    else if (seg[2] === 'snapshots' && seg[3]) page = html`<${Snapshot} fqdn=${fqdn} id=${seg[3]} tab=${seg[4] || 'files'} ...${ctx} />`;
  }
  if (!page) page = html`<h1>Not found</h1><p><a href=${link()}>Back to the overview</a></p>`;

  const here = location.hash || '#/';
  // Overview stays marked on the pages under it: an agent, its jobs and snapshots.
  const under = here === link() || here.startsWith(link('agents')) || here.startsWith(link('hosts')) || here.startsWith(link('problems'));
  const nav = (href, label, extra) => html`<a href=${href} class=${(href === link() ? under : here.startsWith(href)) ? 'on' : ''}>${label}${extra}</a>`;
  return html`<div class=${'layout' + (vault ? ' vault' : '')}>
    <nav class="side">
      <a class="brand" href=${link()}>Holdfast${vault ? ' off-site' : ''}</a>
      ${vault && html`<div class="badge" title=${'The off-site copies, in ' + settings.storage.bucket}>OFF-SITE · ${settings.storage.bucket}</div>`}
      ${nav(link(), vault ? 'Copied agents' : 'Overview', errors.length > 0 && html`<span class="count" aria-label=${errors.length + ' agents with errors'}>${errors.length}</span>`)}
      ${!vault && nav(link('restores'), 'Restores')}
      ${nav(link('history'), vault ? 'Off-site history' : 'Config history')}
      ${nav(link('audit'), vault ? 'Off-site audit log' : 'Audit log')}
      ${me.admin_everywhere && !vault && nav(link('access'), 'Access')}
      ${!vault && nav(link('exports'), 'Exports')}
      ${nav(link('settings'), 'Settings')}
      ${!vault && settings.offsite && html`<div class="section">Off-site</div>${nav(offsiteLink(), 'Off-site copies')}`}
      ${vault && html`<div class="section">The server</div>${nav(primaryLink(), 'Back to the server')}`}
    </nav>
    <main>
      <header class="top">
        <div class="crumbs"><a href=${link()}>Holdfast${vault ? ' off-site' : ''}</a> / ${seg.length && seg[0] !== 'problems' ? seg.slice(0, 2).join(' / ') : 'Overview'}</div>
        <${Search} />
        <span>${me.name} · ${me.admin_everywhere ? 'admin' : 'by agent'} · via ${me.name === 'local:root' ? 'holdfast ui-login' : settings.login.issuer ? new URL(settings.login.issuer).host : 'the root socket'}</span>
        <button class="small" onClick=${async () => { try { await api.logout(); } finally { location.href = '/'; } }}>Log out</button>
      </header>
      ${hosts.error && html`<${ErrorBox} error=${hosts.error} />`}
      ${!vault && (status.data?.problems || []).map((p) => {
        // Where to look next: the export's problems on Exports, a failing alert channel on Settings.
        const where = p.code === 'notify' ? [link('settings'), 'Settings'] : p.code.startsWith('export') ? [link('exports'), 'Exports'] : null;
        return html`<div class=${'banner ' + (p.severity === 'error' ? 'error' : 'warning')}>${p.message}${where && html` · <a href=${where[0]}>${where[1]}</a>`}</div>`;
      })}
      ${page}
    </main>
    <${Tip} />
  </div>`;
}

function App() {
  useRoute(); // a change between the server's pages and the off-site ones loads both again
  const site = onOffsite() ? 'offsite' : 'primary';
  const [state, setState] = useState({});
  useEffect(() => {
    setState({});
    Promise.all([api.me(), api.settings()]).then(([me, settings]) => setState({ me, settings, site }), (error) => setState({ error, site }));
  }, [site]);
  if (state.error) return html`<div style="padding:2rem"><${ErrorBox} error=${state.error} /></div>`;
  if (!state.me || state.site !== site) return html`<p class="muted" style="padding:2rem">Loading Holdfast…</p>`;
  return html`<${Shell} key=${site} me=${state.me} settings=${state.settings} />`;
}

render(html`<${App} />`, document.getElementById('app'));
export { go };
