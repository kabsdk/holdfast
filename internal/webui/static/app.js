// The UI's shell: navigation, the top bar, and the routes. Each page is in pages/.
import { html, render, useState, useEffect, useRoute, usePoll, go, link, primaryLink, offsiteLink, onOffsite, ErrorBox } from './lib.js';
import { api } from './api.js';
import { Fleet, Problems } from './pages/fleet.js';
import { Host, Pending } from './pages/host.js';
import { JobSnapshots, Snapshot } from './pages/snapshots.js';
import { Configure } from './pages/configure.js';
import { Restores } from './pages/restores.js';
import { History, PruneNow } from './pages/history.js';
import { AddHost } from './pages/addhost.js';
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
    <input type="search" placeholder="Search FQDN, job or snapshot ID" value=${text} onInput=${(e) => setText(e.target.value)} />
    ${res && html`<div class="results">
      ${res.error && html`<p class="muted">${res.error}</p>`}
      ${!res.error && n === 0 && html`<p class="muted">No host, job or snapshot matches “${text}”. Snapshot IDs need at least 4 hex characters.</p>`}
      ${!res.error && res.hosts.map((h) => html`<a href="#" onClick=${(e) => { e.preventDefault(); pick(link('hosts', h)); }}>Host · ${h}</a>`)}
      ${!res.error && res.jobs.map((j) => html`<a href="#" onClick=${(e) => { e.preventDefault(); pick(link('hosts', j.fqdn, 'jobs', j.job_id, 'snapshots')); }}>Job · ${j.fqdn} · ${j.job_id}</a>`)}
      ${!res.error && res.snapshots.map((s) => html`<a href="#" onClick=${(e) => { e.preventDefault(); pick(link('hosts', s.fqdn, 'snapshots', s.id)); }}>
        ${s.state === 'present' ? 'Snapshot' : s.state === 'deleted' ? 'Deleted' : 'Disappeared'} · <span class="mono">${s.short_id}</span> · ${s.fqdn} · ${s.job_id}</a>`)}
      ${!res.error && n > 0 && html`<p class="muted">Same search: <code>holdfast search ${text.trim()}</code></p>`}
    </div>`}
  </div>`;
}

function Shell({ me, settings }) {
  const path = useRoute();
  const hosts = usePoll(() => api.hosts(), []);
  const status = usePoll(() => api.status(), [], 60000); // the server's own problems, such as its export
  const seg = path.split('/').filter(Boolean).map(decodeURIComponent);
  const attention = (hosts.data || []).filter((h) => (h.problems || []).length > 0);
  const errors = attention.filter((h) => h.problems.some((p) => p.severity === 'error'));
  // The off-site pages are this UI in red, on the off-site API, with the off-site actions only (decisions 039, 041).
  const vault = settings.role === 'vault';
  const ctx = { me, settings, vault, hosts: hosts.data || [], reloadHosts: hosts.reload };

  let page;
  if (seg.length === 0) page = html`<${Fleet} ...${ctx} />`;
  else if (seg[0] === 'problems') page = html`<${Problems} ...${ctx} />`;
  else if (seg[0] === 'restores' && !vault) page = html`<${Restores} ...${ctx} />`;
  else if (seg[0] === 'history') page = html`<${History} ...${ctx} />`;
  else if (seg[0] === 'add-host' && !vault) page = html`<${AddHost} ...${ctx} />`;
  else if (seg[0] === 'audit') page = html`<${Audit} ...${ctx} />`;
  else if (seg[0] === 'access' && !vault) page = html`<${Access} ...${ctx} />`;
  else if (seg[0] === 'settings') page = html`<${Settings} ...${ctx} />`;
  else if (seg[0] === 'exports' && !vault) page = html`<${Exports} ...${ctx} />`;
  else if (seg[0] === 'hosts' && seg[1]) {
    const fqdn = seg[1];
    if (seg.length === 2) page = html`<${Host} fqdn=${fqdn} ...${ctx} />`;
    else if (seg[2] === 'pending') page = html`<${Pending} fqdn=${fqdn} ...${ctx} />`;
    else if (seg[2] === 'prune-now') page = html`<${PruneNow} fqdn=${fqdn} ...${ctx} />`;
    else if (seg[2] === 'rollback') page = html`<${Rollback} fqdn=${fqdn} ...${ctx} />`;
    else if (seg[2] === 'configure') page = html`<${Configure} fqdn=${fqdn} job=${seg[3] || ''} ...${ctx} />`;
    else if (seg[2] === 'jobs' && seg[4] === 'snapshots') page = html`<${JobSnapshots} fqdn=${fqdn} job=${seg[3]} ...${ctx} />`;
    else if (seg[2] === 'snapshots' && seg[3]) page = html`<${Snapshot} fqdn=${fqdn} id=${seg[3]} tab=${seg[4] || 'files'} ...${ctx} />`;
  }
  if (!page) page = html`<h1>Not found</h1><p><a href=${link()}>Back to the fleet</a></p>`;

  const here = location.hash || '#/';
  const nav = (href, label, extra) => html`<a href=${href} class=${here === href || (href !== link() && here.startsWith(href)) ? 'on' : ''}>${label}${extra}</a>`;
  return html`<div class=${'layout' + (vault ? ' vault' : '')}>
    <nav class="side">
      <a class="brand" href=${link()}>Holdfast${vault ? ' off-site' : ''}</a>
      ${vault && html`<div class="badge" title=${'The off-site copies, in ' + settings.storage.bucket}>OFF-SITE · ${settings.storage.bucket}</div>`}
      ${nav(link(), vault ? 'Copied hosts' : 'Fleet')}
      ${nav(link('problems'), 'Problems', errors.length > 0 && html`<span class="count">${errors.length}</span>`)}
      ${!vault && nav(link('restores'), 'Restores')}
      ${nav(link('history'), vault ? 'Off-site history' : 'Config history')}
      ${nav(link('audit'), vault ? 'Off-site audit log' : 'Audit log')}
      ${me.admin_everywhere && !vault && nav(link('access'), 'Access')}
      ${!vault && nav(link('exports'), 'Exports')}
      ${nav(link('settings'), 'Settings')}
      ${!vault && settings.offsite && html`<div class="section">Off-site</div>${nav(offsiteLink(), 'Off-site copies')}`}
      ${vault && html`<div class="section">The server</div>${nav(primaryLink(), 'Back to the fleet')}`}
      ${attention.length > 0 && html`<div class="section">Needs attention</div>
        <div class="attn">${attention.slice(0, 12).map((h) => html`<a href=${link('hosts', h.fqdn)}>${h.fqdn.split('.')[0]}<span class="muted">${h.fqdn.slice(h.fqdn.indexOf('.'))}</span></a>`)}</div>`}
    </nav>
    <main>
      <header class="top">
        <div class="crumbs"><a href=${link()}>Holdfast${vault ? ' off-site' : ''}</a> / ${seg.length ? seg.slice(0, 2).join(' / ') : 'Fleet'}</div>
        <${Search} />
        <span>${me.name} · ${me.admin_everywhere ? 'admin' : 'by host'} · via ${me.name === 'local:root' ? 'holdfast ui-login' : settings.login.issuer ? new URL(settings.login.issuer).host : 'the root socket'}</span>
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
