import { html, useState, usePoll, useAction, link, fmtTime, ErrorBox, EnrollLines } from '../lib.js';
import { api } from '../api.js';

function Progress({ fqdn }) {
  const h = usePoll(() => api.host(fqdn), [fqdn], 3000);
  const p = h.data?.progress;
  const a = h.data?.agent;
  const step = (done, text) => html`<li class=${done ? 'done' : ''}>${text}</li>`;
  return html`<ol class="steps">
    ${step(true, 'Enrollment code issued · single use · valid 15 minutes')}
    ${step(p === 'connected' || p === 'first_backup_done', p === 'waiting' || !p ? 'Waiting for the agent to connect…' : `Agent connected${a ? `${a.remote_addr ? ` from ${a.remote_addr}` : ''} · ${a.os_id} ${a.os_version} · restic ${a.restic_version}` : ''}`)}
    ${step(p === 'first_backup_done', p === 'first_backup_done' ? 'First backup finished' : 'First backup')}
  </ol>
  ${p === 'first_backup_done' && html`<a class="button primary" href=${link('agents', fqdn)}>Open ${fqdn}</a>`}`;
}

// Adding an agent is one API call, and its first job another (PUT …/jobs/{job}), the same two as holdfast agent add and
// holdfast job set. The job takes the default schedule and retention; Configure changes them. An agent is named by its
// name (decision 056): a machine's FQDN, or a name such as k3s-postgres-main for an agent in a pod.
export function AddAgent({ me }) {
  const [fqdn, setFqdn] = useState('');
  const [withJob, setWithJob] = useState(true);
  const [jobID, setJobID] = useState('system');
  const [paths, setPaths] = useState('/etc\n/root\n/home');
  const act = useAction();
  if (!me.add_hosts) return html`<div class="banner warning">Adding agents needs config.storage (an admin).</div>`;
  const name = fqdn.trim();
  const submit = () => act.run(async () => {
    const host = await api.addHost({ fqdn: name });
    if (!host || !withJob) return { host };
    try {
      const job = await api.setJob(name, jobID.trim(), { job: { paths: paths.split('\n').map((x) => x.trim()).filter(Boolean) } });
      return { host, job };
    } catch (err) {
      return { host, jobError: err };
    }
  });
  const done = act.done;
  return html`<h1>Add agent</h1>
    <p class="muted">The name is the agent's identity and its repository's prefix: a machine's FQDN, such as web03.dc1.example.com, or a name of its own, such as k3s-postgres-main for an agent in a pod. Adding it saves a config revision and issues a one-time enrollment code. The agent needs no S3 key: it backs up through this server, with a certificate it makes when it enrolls.</p>
    ${!done && html`<div class="grid2"><div class="panel">
      <label class="field"><span>Name</span><input style="width:100%" placeholder="web03.dc1.example.com or k3s-postgres-main" value=${fqdn} onInput=${(e) => setFqdn(e.target.value)} /></label>
      <h3><label><input type="checkbox" checked=${withJob} onChange=${(e) => setWithJob(e.target.checked)} /> And a first job, of files</label></h3>
      ${withJob && html`<label class="field"><span>Job ID</span><input value=${jobID} onInput=${(e) => setJobID(e.target.value)} /></label>
        <label class="field"><span>Paths (one per line)</span><textarea value=${paths} onInput=${(e) => setPaths(e.target.value)} /></label>
        <p class="muted">It takes the default schedule and retention: daily at 02:00 with a 20-minute spread; one per day for 14 days, per week for 8 weeks, per month for 12 months, per year for 3 years; the newest 3. Change them later under Configure. An agent that backs up PostgreSQL gets its connections and their jobs on its page, once it is enrolled.</p>`}
      <button class="primary" disabled=${act.busy || !name} onClick=${submit}>Add agent and issue enrollment code</button>
      <${ErrorBox} error=${act.error} />
    </div>
    ${me.admin_everywhere && html`<${ImportAgent} />`}</div>`}
    ${done && html`<${Enroll} fqdn=${name} done=${done.host} />
      ${done.job && html`<div class="banner ok">Job ${jobID.trim()} saved in r${done.job.revision}${done.job.findings?.length ? ` (${done.job.findings.map((f) => f.code).join(', ')})` : ''}.</div>`}
      ${done.jobError && html`<div class="banner warning">The agent was added, but its first job was not saved. Add it under Configure on <a href=${link('agents', name)}>${name}</a>.</div><${ErrorBox} error=${done.jobError} />`}`}`;
}

function Enroll({ fqdn, done }) {
  return html`<div class="panel">
    <h2>Run this where ${fqdn} runs, as root</h2>
    <${EnrollLines} e=${done.enrollment} />
    <p class="muted">The code ${done.enrollment.code} is valid until ${fmtTime(done.enrollment.expires_at)}, once. The line installs the agent's packages of this server's release, checked against their SHA256SUMS, and enrolls the agent.</p>
    ${done.warning && html`<div class="banner warning">${done.warning}</div>`}
    <${Progress} fqdn=${fqdn} />
  </div>`;
}

// An agent back from its escrow file, when the server was rebuilt without an export that has it.
// The file's contents go to the server once, and are never shown back.
function ImportAgent() {
  const [fqdn, setFqdn] = useState('');
  const [file, setFile] = useState('');
  const act = useAction();
  if (act.done) return html`<div>
    <div class="banner ok">Imported ${fqdn.trim()} in r${act.done.revision}: its repository opens with the escrowed password. It has no job yet: add them under Configure.</div>
    <${Enroll} fqdn=${fqdn.trim()} done=${act.done} /></div>`;
  return html`<div class="panel">
    <h3>Or import an agent from its escrow file</h3>
    <p class="muted">For an agent the server does not know, whose repository is in the bucket. A rebuild brings back the agents in <code>escrow/</code> by itself; this is for the rest. Decrypt its escrow file with the age key of the recovery file (<code>${'grep AGE-SECRET-KEY holdfast-recovery.txt > key.txt; age -d -i key.txt <agent>.age'}</code>) and paste it here. Its repository stays; the password must open it. Same: <code>${'holdfast agent import <agent> --escrow-file -'}</code></p>
    <label class="field"><span>Name</span><input style="width:100%" placeholder="web03.dc1.example.com" value=${fqdn} onInput=${(e) => setFqdn(e.target.value)} /></label>
    <label class="field"><span>Escrow file, decrypted</span><textarea class="mono" autocomplete="off" spellcheck="false" value=${file} onInput=${(e) => setFile(e.target.value)} placeholder='{"format": "holdfast-escrow-1", …}' /></label>
    <button class="primary" disabled=${act.busy || !fqdn.trim() || !file.trim()} onClick=${() => act.run(() => api.importHost({ fqdn: fqdn.trim(), escrow_file: file })).then((r) => r && setFile(''))}>Import agent and issue enrollment code</button>
    <${ErrorBox} error=${act.error} />
  </div>`;
}
