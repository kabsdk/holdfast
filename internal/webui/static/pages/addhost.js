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
  const [keyID, setKeyID] = useState('');
  const [secret, setSecret] = useState('');
  const [withJob, setWithJob] = useState(true);
  const [jobID, setJobID] = useState('system');
  const [paths, setPaths] = useState('/etc\n/root\n/home');
  const [accept, setAccept] = useState([]);
  const act = useAction();
  if (!me.add_hosts) return html`<div class="banner warning">Adding hosts needs config.storage (an admin).</div>`;
  const submit = () => {
    const body = { fqdn: fqdn.trim(), accept };
    if (keyID || secret) body.s3_key = { access_key_id: keyID.trim(), secret_access_key: secret.trim() };
    if (withJob) {
      body.job_id = jobID.trim();
      body.job = { ...settings.default_job, paths: paths.split('\n').map((x) => x.trim()).filter(Boolean) };
    }
    act.run(() => api.addHost(body));
  };
  const done = act.done;
  const findings = act.error?.data?.findings;
  return html`<h1>Add host</h1>
    <p class="muted">The FQDN is the host's identity. Adding it saves a config revision and issues a one-time enrollment code for the agent.</p>
    ${!done && html`<div class="grid2"><div class="panel">
      <label class="field"><span>FQDN</span><input style="width:100%" placeholder="web03.dc1.example.com" value=${fqdn} onInput=${(e) => setFqdn(e.target.value)} /></label>
      <h3>The host's S3 key</h3>
      <p class="muted">Make it at your S3 server first: it may use only <code>repos/${fqdn || '<fqdn>'}/</code>, and delete only under its <code>locks/</code> (docs/storage.md). Holdfast tests it before it keeps it.</p>
      <label class="field"><span>Access key ID</span><input value=${keyID} onInput=${(e) => setKeyID(e.target.value)} autocomplete="off" /></label>
      <label class="field"><span>Secret access key</span><input type="password" value=${secret} onInput=${(e) => setSecret(e.target.value)} autocomplete="new-password" /></label>
      <h3><label><input type="checkbox" checked=${withJob} onChange=${(e) => setWithJob(e.target.checked)} /> And a first job</label></h3>
      ${withJob && html`<label class="field"><span>Job ID</span><input value=${jobID} onInput=${(e) => setJobID(e.target.value)} /></label>
        <label class="field"><span>Paths (one per line)</span><textarea value=${paths} onInput=${(e) => setPaths(e.target.value)} /></label>
        <p class="muted">It starts from the default: daily at 02:00 with a 20-minute spread; one per day for 14 days, per week for 8 weeks, per month for 12 months, per year for 3 years; the newest 3. Change it later under Configure.</p>`}
      <button class="primary" disabled=${act.busy || !fqdn.trim()} onClick=${submit}>Add host and issue enrollment code</button>
      <${ErrorBox} error=${act.error} />
      ${findings && html`<${Findings} findings=${findings} accepted=${accept} onAccept=${(c, on) => setAccept(on ? [...accept, c] : accept.filter((x) => x !== c))} />`}
    </div></div>`}
    ${done && html`<div class="panel">
      <h2>Run this on ${fqdn} as root</h2>
      <pre>${done.enrollment.install_command}</pre><${CopyButton} text=${done.enrollment.install_command} />
      <p class="muted">The code ${done.enrollment.code} is valid until ${fmtTime(done.enrollment.expires_at)}, once. The agent never downloads anything; install its package first.</p>
      ${done.warning && html`<div class="banner warning">${done.warning}</div>`}
      <${Progress} fqdn=${fqdn.trim()} />
    </div>`}`;
}
