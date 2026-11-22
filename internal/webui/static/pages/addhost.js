import { html, useState, usePoll, useAction, link, fmtTime, ErrorBox, CopyButton, Findings } from '../lib.js';
import { api } from '../api.js';

function Progress({ fqdn }) {
  const h = usePoll(() => api.host(fqdn), [fqdn], 3000);
  const p = h.data?.progress;
  const step = (done, text) => html`<li class=${done ? 'done' : ''}>${text}</li>`;
  return html`<ol class="steps">
    ${step(true, 'Enrollment code issued · single use · valid 15 minutes')}
    ${step(p === 'connected' || p === 'first_backup_done', p === 'waiting' || !p ? 'Waiting for the agent to connect…' : `Agent connected${h.data?.agent ? ` · ${h.data.agent.os_id} ${h.data.agent.os_version} · restic ${h.data.agent.restic_version}` : ''}`)}
    ${step(p === 'first_backup_done', p === 'first_backup_done' ? 'First backup finished' : 'First backup')}
  </ol>
  ${p === 'first_backup_done' && html`<a class="button primary" href=${link('hosts', fqdn)}>Open ${fqdn}</a>`}`;
}

export function AddHost({ me, settings }) {
  const [fqdn, setFqdn] = useState('');
  const [withJob, setWithJob] = useState(true);
  const [jobID, setJobID] = useState('system');
  const [paths, setPaths] = useState('/etc\n/root\n/home');
  const [accept, setAccept] = useState([]);
  const act = useAction();
  if (!me.add_hosts) return html`<div class="banner warning">Adding hosts needs config.storage (an admin).</div>`;
  const submit = () => {
    const body = { fqdn: fqdn.trim(), accept };
    if (withJob) {
      body.job_id = jobID.trim();
      body.job = { ...settings.default_job, paths: paths.split('\n').map((x) => x.trim()).filter(Boolean) };
    }
    act.run(() => api.addHost(body));
  };
  const done = act.done;
  const findings = act.error?.data?.findings;
  return html`<h1>Add host</h1>
    <p class="muted">The FQDN is the host's identity. Adding it saves a config revision and issues a one-time enrollment code for the agent. The host needs no S3 key: it backs up through this server, with a certificate it makes when it enrolls.</p>
    ${!done && html`<div class="grid2"><div class="panel">
      <label class="field"><span>FQDN</span><input style="width:100%" placeholder="web03.dc1.example.com" value=${fqdn} onInput=${(e) => setFqdn(e.target.value)} /></label>
      <h3><label><input type="checkbox" checked=${withJob} onChange=${(e) => setWithJob(e.target.checked)} /> And a first job</label></h3>
      ${withJob && html`<label class="field"><span>Job ID</span><input value=${jobID} onInput=${(e) => setJobID(e.target.value)} /></label>
        <label class="field"><span>Paths (one per line)</span><textarea value=${paths} onInput=${(e) => setPaths(e.target.value)} /></label>
        <p class="muted">It starts from the default: daily at 02:00 with a 20-minute spread; one per day for 14 days, per week for 8 weeks, per month for 12 months, per year for 3 years; the newest 3. Change it later under Configure.</p>`}
      <button class="primary" disabled=${act.busy || !fqdn.trim()} onClick=${submit}>Add host and issue enrollment code</button>
      <${ErrorBox} error=${act.error} />
      ${findings && html`<${Findings} findings=${findings} accepted=${accept} onAccept=${(c, on) => setAccept(on ? [...accept, c] : accept.filter((x) => x !== c))} />`}
    </div>
    ${me.admin_everywhere && html`<${ImportHost} />`}</div>`}
    ${done && html`<${Enroll} fqdn=${fqdn.trim()} done=${done} />`}`;
}

function Enroll({ fqdn, done }) {
  return html`<div class="panel">
    <h2>Run this on ${fqdn} as root</h2>
    <pre>${done.enrollment.install_command}</pre><${CopyButton} text=${done.enrollment.install_command} />
    <p class="muted">The code ${done.enrollment.code} is valid until ${fmtTime(done.enrollment.expires_at)}, once. The agent never downloads anything; install its package first.</p>
    ${done.warning && html`<div class="banner warning">${done.warning}</div>`}
    <${Progress} fqdn=${fqdn} />
  </div>`;
}

// A host back from its escrow file, when the server was rebuilt without an export that has it (plan-m9 §2.4, Q7).
// The file's contents go to the server once, and are never shown back.
function ImportHost() {
  const [fqdn, setFqdn] = useState('');
  const [file, setFile] = useState('');
  const act = useAction();
  if (act.done) return html`<div>
    <div class="banner ok">Imported ${fqdn.trim()} in r${act.done.revision}: its repository opens with the escrowed password. It has no job yet: add them under Configure.</div>
    <${Enroll} fqdn=${fqdn.trim()} done=${act.done} /></div>`;
  return html`<div class="panel">
    <h3>Or import a host from its escrow file</h3>
    <p class="muted">For a host the server no longer knows, after a rebuild without an export that has it. Decrypt its escrow file with the offline key (<code>${'age -d -i key.txt escrow/<fqdn>.age'}</code>) and paste it here. Its repository stays; the password must open it. Same: <code>${'holdfast host import <fqdn> --escrow-file -'}</code></p>
    <label class="field"><span>FQDN</span><input style="width:100%" placeholder="web03.dc1.example.com" value=${fqdn} onInput=${(e) => setFqdn(e.target.value)} /></label>
    <label class="field"><span>Escrow file, decrypted</span><textarea class="mono" autocomplete="off" spellcheck="false" value=${file} onInput=${(e) => setFile(e.target.value)} placeholder='{"format": "holdfast-escrow-1", …}' /></label>
    <button class="primary" disabled=${act.busy || !fqdn.trim() || !file.trim()} onClick=${() => act.run(() => api.importHost({ fqdn: fqdn.trim(), escrow_file: file })).then((r) => r && setFile(''))}>Import host and issue enrollment code</button>
    <${ErrorBox} error=${act.error} />
  </div>`;
}
