# The local test lab

Five VMs on this host's KVM and libvirt, on a private NAT network of their own, in the manner of Clusterlane's lab:
the lab is data (`lab/labs/<lab>.yml`), Ansible brings it up (`lab/ansible/`, community.libvirt), and `lab/lab` is the
one command. One lab ships, `m1`.

| VM | Role | OS (image) | vCPU | MiB | Disk GiB | Address |
| --- | --- | --- | --- | --- | --- | --- |
| `s3a` | the storage: MicroCeph (Squid 19.2.3, snap `squid/stable`, held), 3 loop-file OSDs of 4 GiB, RGW on port 80, the buckets `holdfast` and `holdfast-offsite` | Ubuntu 24.04 | 2 | 4096 | 40 | 10.79.20.10 |
| `hfsrv` | Holdfast server: PostgreSQL 16 (AppStream module), database and user `holdfast`; `holdfast-server` and `holdfast` in /usr/bin; Authelia 4.39.28, the OIDC provider for login | AlmaLinux 9.8 | 2 | 2048 | 20 | 10.79.20.20 |
| `ub24` | agent | Ubuntu 24.04 | 1 | 1024 | 12 | 10.79.20.31 |
| `el9` | agent (the RHEL stand-in) | AlmaLinux 9.8 | 1 | 1024 | 12 | 10.79.20.32 |
| `ol9` | agent | Oracle Linux 9.8 (KVM template, UEK) | 1 | 1024 | 37 (the image's own size) | 10.79.20.33 |

hfsrv and the agents have restic 0.19.1 at `/usr/libexec/holdfast/restic`. Each agent has `/usr/bin/holdfast-agent`,
`holdfast-agent.service` (installed; `lab/lab test` enrolls and starts it) and the lab's fidelity tool
`/usr/local/bin/hf-fidelity`. On hfsrv, `holdfast-server` runs, configured as an admin would (below), with the UI and
`/api/v1` for people on port 443 and login through Authelia on port 9091 (below); it is the storage witness too (plan-m10 item 1). Every VM has the others' names in `/etc/hosts`
(`<vm>.m1.holdfast.test` and `<vm>`). SELinux is enforcing on the EL VMs. The network is `hf-m1` (10.79.20.0/24, bridge
`hfbr-m1`), and every libvirt object the lab makes is named `hf-m1-*`.

## The commands

| Command | What it does |
| --- | --- |
| `lab/lab doctor m1` | checks what this host lacks for `up`, and says how to get it; changes nothing |
| `lab/lab up m1` | makes the VMs, or starts them, builds this checkout (`make packages`: the `.deb` and `.rpm` packages of static binaries, and `bin/hf-fidelity`), provisions every VM (`site.yml`: the packages installed with `dpkg -i` or `rpm -U`, the Holdfast server included), then checks the lab; run it again after a failure to resume; on a ready lab it changes nothing |
| `lab/lab status m1 [--json]` | absent, off, ready or broken, with what each VM answered and why it is not ready |
| `lab/lab ssh m1 <vm> [command]` | SSH as root with the lab's key |
| `lab/lab deploy m1` | builds this checkout's packages and installs them on hfsrv and the agents, restarting any Holdfast service that runs |
| `lab/lab test m1 [<vm>...]` | M2's "done when", the fidelity test through Holdfast (below): builds and deploys, provisions the server and Authelia, checks login on the UI through Authelia, then backs up and restores the metadata fixture on each agent VM (or those named) and compares the 13 properties; then M6's rebuild and dump tests, M8's off-site test and M9's server rebuild (below) |
| `lab/lab sizing m1 [<vm>...]` | what one server's gateway carries: every agent backs up new random data at once, timed (below) |
| `lab/lab isolation m1 account` | the RGW-accounts test (below) |
| `lab/lab isolation m1 <user-policy\|bucket-policy> [--maint-policy]` | the same question without accounts |
| `lab/lab stop m1` | shuts the VMs down; `up` starts them again |
| `lab/lab destroy m1 [--yes]` | removes the VMs, volumes, network and state; the image cache stays |

Exit codes: 0 done (status: ready; test: every property kept on every VM and every login check passed), 1 a step failed
(the log says which), a property was lost or a login check failed, 2 refused or misused, 3 the lab is not there, off or broken, 75 busy (another command holds the
lab's lock). `up`, `deploy`, `test`, `isolation`, `stop` and `destroy` write `<state>/runs/<UTC time>-<command>/log`.

First time on a host: `ansible-galaxy collection install -r lab/ansible/requirements.yml`, then `lab/lab doctor m1`.
Its packages are Clusterlane's (libvirt, qemu-img, xorriso, python3-libvirt, python3-lxml, ansible-core, Go). No root
is needed: libvirt owns every disk and seed in the pool `default`.

## Where things are

- **State:** `${XDG_STATE_HOME:-~/.local/state}/holdfast/labs/m1/`: `lab.env` (source it for the lab's names, URLs and
  file paths), `inventory.yml`, the SSH key, `ceph-version`, and `credentials/` (0700, each file 0600):
  the server's RGW keys `holdfast-maint.env`, `holdfast-gateway.env` and `holdfast-offsite.env`, and the experiments'
  `holdfast-provisioner.env` (`AWS_ACCESS_KEY_ID=`, `AWS_SECRET_ACCESS_KEY=`, the format `storage.ReadKeyFile` reads),
  `postgresql-holdfast.pw` and `.dsn`, and the experiments' keys (`test-host.env`, `acct-*.env`, `test-host.restic.pw`).
  No agent has an RGW key (decision 041): each one's identity is its own certificate, in
  `/var/lib/holdfast-agent/agent.pem` on its VM, which no copy leaves. Authelia's, made
  once by `lab/ansible/files/authelia-credentials.sh` with the release's own `authelia`: each user's password
  (`authelia-<user>.pw`) and its argon2id digest (`.digest`), Holdfast's client secret (`authelia-client.secret`) and its
  pbkdf2 digest, the ID-token signing key (`authelia-jwks.pem`), and `authelia-{jwt,session,storage,hmac}.secret`. The
  experiments' transcripts are `isolation-*.txt`, and the fidelity test's results `test-fidelity.txt` and `.json`.
- **The lab's CA:** `ca/` in the state (0700): `ca.key` and the server's key (0600), `ca.crt`, the server's certificate
  and its chain, Authelia's (`authelia.key`, 0600, `authelia.crt`, `authelia.chain.crt`; the same names as the
  server's, with a key of its own), and `ca.sha256`, the pin (sha256 of `ca.crt`'s DER) that is `ca_sha256` in
  `server.yaml` and that the agents check. `lab/ansible/files/lab-ca.sh` makes them once, and a certificate again only
  when it nears expiry.
- **Holdfast's server:** `https://hfsrv.m1.holdfast.test:8443` for the agents, the UI and `/api/v1` with login at
  `https://hfsrv.m1.holdfast.test` (`ui.listen: ":443"`, the same certificate: the lab has no CA browsers trust), and
  the CLI `holdfast` as root on hfsrv (`lab/lab ssh m1 hfsrv holdfast host list`). `/etc/holdfast/` holds `server.yaml`
  (0600, from `lab/ansible/templates/server.yaml.j2`), `tls/` (the certificate with the CA's after it, its key, the
  CA's, which `auth.oidc.ca_file` names), `maintenance.env` (holdfast-maint's key), `gateway.env` (holdfast-gateway's,
  the key the gateway uses for every host's reads and writes), `offsite.env` (holdfast-offsite's, for the off-site
  bucket), `oidc-client-secret`, `credentials.key` (made once there by `holdfast-server keygen`) and `db.dsn`. The
  M9 test keeps the admin's offline copy of all of `/etc/holdfast` in the lab's state, `rebuild/etc-holdfast.tar`
  (0600), as the rebuild runbook says to; a hfsrv rebuilt by `lab/lab up` without it starts with a new database. `server.yaml` has an `offsite:` section,
  so the server copies each host's snapshots to `holdfast-offsite` every 30 seconds; without it the lab would run the
  same, with no off-site copies. `auth.bootstrap_admins` is empty: the default bindings make ada admin.
- **Authelia:** `https://hfsrv.m1.holdfast.test:9091`, the issuer in `server.yaml` (plan-m4 Q9), set up as the e2e
  tests' (`test/e2e/authelia/setup.sh`). Its release tarball `authelia-v4.39.28-linux-amd64.tar.gz` is pinned by URL
  and sha256 in `lab/labs/m1.yml`, downloaded once into `~/.cache/holdfast-lab` and unpacked beside it, and its
  `authelia` and the release's `authelia.service` copied to hfsrv (`/usr/bin`, `/usr/lib/systemd/system`). It runs as
  the user `authelia`, with `/etc/authelia/`: `configuration.yml` (from `templates/authelia.yml.j2`), `users.yml`
  (a file user database: ada in `holdfast-admins`, ed in `holdfast-editors`, vi in `holdfast-viewers`, wes in
  `web-team`), `tls/` and `secrets/` (0600), which the drop-in `authelia.service.d/lab.conf` names
  (`AUTHELIA_*_FILE`). Its database is `/var/lib/authelia/db.sqlite3`. Holdfast is its one client: the code flow with
  PKCE, the device flow, implicit consent, `client_secret_post`, the redirect URI
  `https://hfsrv.m1.holdfast.test/auth/callback`. Its journal: `lab/lab ssh m1 hfsrv journalctl -u authelia`.
- **RGW:** `http://s3a.m1.holdfast.test` from the VMs, `http://10.79.20.10` from this host (`HF_RGW_URL`,
  `HF_RGW_URL_HOST`). The region is `default`, requests are path-style, and the bucket is `holdfast`, owned by
  `holdfast-maint` as `up` makes it, with versioning `Enabled` (GetBucketVersioning is checked on every `up`). Holdfast
  needs it in the documented layout instead (owned by `holdfast-bucket-owner`, below), which `lab/rgw-documented-layout.sh`
  makes. `holdfast-provisioner` has the caps `users=*;user-policy=*`.
- **PostgreSQL:** `hfsrv.m1.holdfast.test:5432`, database and user `holdfast`, with scram from the lab's network.
  `/etc/holdfast/db.dsn` on hfsrv is the localhost DSN.
- **Images:** `~/.cache/holdfast-lab`, each pinned by URL and sha256 in `lab/labs/m1.yml` and checked on download. An
  image another lab's cache already holds with that sha256 is copied from there, read only (`lab_cache_peers`).

## Logging in to the lab's UI

The users are `ada` (admin on every host), `ed` (editor), `vi` (viewer) and `wes` (`web-team`, which has no binding
until you add one with `holdfast access set`, so wes sees no host). Each one's password is in the lab's state, in
`credentials/authelia-<user>.pw`. From this host:

- **A browser:** put hfsrv's line from `lab/lab status m1` in `/etc/hosts`, then open `https://hfsrv.m1.holdfast.test`.
  The UI sends you to Authelia's form on port 9091, and back. Both certificates are from the lab's CA,
  `ca/ca.crt` in the state: import it into the browser, or accept its warning on both ports.
- **The CLI:** `holdfast login --server https://hfsrv.m1.holdfast.test --ca-file ~/.local/state/holdfast/labs/m1/ca/ca.crt`
  starts the device flow. It shows a code and Authelia's page for it, which you confirm in a browser that is logged
  in to Authelia.

## What it costs

9216 MiB of VM memory. Disk on this host, measured: 6.3 GiB added (116,659,856 KiB free before, 110,026,100 after).
That is 1.9 GiB of images in the cache, their copies in the pool, and thin overlays. s3a's overlay is about 2 GiB with
Ceph running. The overlays grow with use, up to their sizes above.

## The RGW experiment: one maintenance key, host keys limited to their prefix

The question: which RGW setup lets one maintenance key read every host's objects and delete under any prefix, while
each host key stays limited to its own prefix (reads and writes under `repos/<fqdn>/`, deletes only under
`repos/<fqdn>/locks/`)? Ceph 19.2.3 Squid.

**Result: an RGW account does it.** `lab/lab isolation m1 account` (27 PASS, 0 FAIL; transcript
`isolation-account.txt`) makes a scratch account, with three users in it: `acct-provisioner`, given the managed policy
`IAMFullAccess` with `radosgw-admin user policy attach`; `acct-maint`; and `acct-host`. It then removes them again.
Through the IAM API (`aws iam put-user-policy`, HTTP 200), the provisioner gives `acct-maint` `s3:*` on the bucket
and `acct-host` the per-prefix host policy. The bucket, made by `acct-maint`, is owned by the account.
- `acct-host`: PUT, GET and list on its own prefix return 200. GET or list of another prefix, a list of the whole
  bucket, and a PUT under another prefix return 403 AccessDenied. DELETE of a non-lock object in its own prefix returns
  403 AccessDenied, and DELETE under `locks/` returns 204. restic `backup` works, and `forget` fails with
  `client.RemoveObject: Access Denied`.
- `acct-maint`: GET of the object `acct-host` wrote returns 200, a list of `repos/` returns 200, and DELETE under
  either prefix returns 204. restic `init`, `check --read-data` ("no errors were found") and
  `forget --keep-last 1 --group-by host --prune` (it deleted the host's pack and indexes) all succeed.

**Without accounts it does not, by policy alone.** In that layout the users are plain users, `holdfast-maint` owns the
bucket, and `test-host` is made through the Admin Ops API. Both a user policy on test-host and a bucket policy naming
`arn:aws:iam:::user/test-host` enforce the host's limits exactly as above. But every object belongs to the user who
uploaded it, so the bucket owner gets 403 AccessDenied reading what test-host wrote. restic `check` as holdfast-maint
fails with `Load(<index/...>) failed: Access Denied`. The setup also refuses
`PutBucketOwnershipControls BucketOwnerEnforced` with 400. Adding a bucket-policy statement that gives
`arn:aws:iam:::user/holdfast-maint` `s3:*` on the bucket and its objects (`--maint-policy`) fixes it, with both
mechanisms (40 and 38 PASS, 0 FAIL).

The lab's own `holdfast-maint`, `holdfast-provisioner` and bucket `holdfast` are still the plain-user layout. As of
the last run (`--maint-policy`), the bucket also carries the grant for `holdfast-maint`.

**Holdfast's own contract test against this RGW** (decision 029, `docs/storage.md`). The test is
`internal/server/storage/probe_live_test.go`, and it is skipped unless `HOLDFAST_PROBE_ENDPOINT` is set:

```
C=~/.local/state/holdfast/labs/m1/credentials
HOLDFAST_PROBE_ENDPOINT=http://10.79.20.10 HOLDFAST_PROBE_BUCKET=holdfast HOLDFAST_PROBE_REGION=default \
HOLDFAST_PROBE_FQDN=test.example.com HOLDFAST_PROBE_MAINT_KEY=$C/holdfast-maint.env HOLDFAST_PROBE_HOST_KEY=$C/test-host.env \
HOLDFAST_PROBE_EXPECT=B003 go test ./internal/server/storage/ -run TestS3ProberLive -v
```

- After `lab/lab isolation m1 user-policy` (plain users, no grant), it reports exactly B003: AccessDenied when the
  maintenance key reads an object the host key wrote.
- After `lab/lab isolation m1 user-policy --maint-policy`, every check B001–B008 passes.
- **Since decision 030** the contract also has B009–B011. The lab's layout fails them:
  - `holdfast-maint` owns the bucket, so it can change the bucket's settings (B010);
  - the `--maint-policy` grant is `s3:*`, so the maintenance key can delete versions for good (B009);
  - the bucket has no lifecycle rule (B011).

- **The documented layout passes everything.** `lab/rgw-documented-layout.sh` rebuilds the bucket in the
  layout `docs/storage.md` describes:
  - an owner of its own, `holdfast-bucket-owner`;
  - versioning, and a lifecycle rule that expires noncurrent versions after 30 days;
  - a bucket policy with the narrowed maintenance grant and its deny on `s3:DeleteObjectVersion`, and
    `docs/storage.md`'s gateway statements for `holdfast-gateway` (decision 041: read and write under `repos/`,
    delete only under `repos/*/locks/`, no version deletes). The experiments' `test-host` keeps the former host
    statements on `repos/test.example.com/` when its key is in `/root/hf-lab/`; no agent has a user.

  Run it with `lab/lab ssh m1 s3a 'bash -s' < lab/rgw-documented-layout.sh`. It deletes the lab
  bucket's data first. With `'bash -s -- --keep-data'` it keeps a bucket already in this layout and its
  data, and only makes the users and puts the lifecycle rule and the policy again; `lab/lab test` runs it
  so. After it, `TestS3ProberLive` with `HOLDFAST_PROBE_EXPECT=` passes every check,
  B001–B011, on Ceph 19.2.3 with plain users. The bucket-policy grant lets the maintenance user read the
  host's objects, although a third identity owns the bucket. Since M8 the host-key checks (B001, B002,
  B005–B007) test the gateway key, as the server's own storage test does at `holdfast host add` and `holdfast check`.
- The isolation experiments (`lab/lab isolation`) still assume their own layout: `holdfast-maint` owns the
  bucket. Running one replaces this layout, so run `rgw-documented-layout.sh` again afterwards, without
  `--keep-data` (`lab/lab test` refuses a bucket that another identity owns and that holds data; an empty one, as
  `up` makes it, it makes again in this layout). Rebuilding the bucket deletes the
  agents' repositories; the next `lab/lab test` sees that (restic exit 10) and enrolls each agent again, which
  makes Holdfast initialise its repository.

## The fidelity test through Holdfast: M2's "done when"

`lab/lab test m1` (`lab/test-fidelity.sh`) checks that a backup and a restore made through Holdfast keep all 13
metadata properties on the three agent VMs, against the lab's Ceph RGW through the server's gateway (docs/plan-m2.md, Q9). It also checks login on
the UI through Authelia (plan-m4 Q9). It needs a ready lab (`lab/lab up m1`) and no internet on the VMs. It holds the
lab's lock, and takes about two minutes. It:

1. builds this checkout and deploys it (as `deploy`), and provisions Authelia and the server
   (`site.yml --tags binaries,hfserver`);
2. checks login from this host with curl (`lab/test-login.sh`). curl goes to hfsrv's address (`--resolve`) and checks
   both certificates against the lab's CA:
   - Authelia's discovery document has the issuer that `server.yaml` names, and lists the
     `device_authorization_endpoint`;
   - `GET /api/v1/me` on the UI without a session answers 401;
   - `GET /auth/login` answers 302 to Authelia's authorization endpoint, with PKCE S256 and the redirect URI
     `https://hfsrv.m1.holdfast.test/auth/callback`;
   - ada logs in without a browser. Her password from `credentials/authelia-ada.pw` goes on curl's stdin to Authelia's
     first factor, `POST /api/firstfactor`. With that cookie, the same authorization request is answered with a
     redirect to Holdfast's callback. The callback sets Holdfast's session cookie, and with it `GET /api/v1/me` is
     `ada`, `admin_everywhere: true`;
   - `POST /auth/logout` (from the UI's origin) answers 204, and `/api/v1/me` answers 401 again.

   Each check is a line in the summary. The password, the cookies and the authorization code are never in an
   argument, the log or the terminal, and the cookie jar is removed afterwards;
3. puts the bucket policy for the server's keys (`rgw-documented-layout.sh --keep-data`) and lays out the off-site
   bucket (`rgw-offsite-layout.sh`), keeping both buckets' data. No agent has an RGW user;
4. on each agent VM, or each one named (`lab/lab test m1 el9`):
   - makes the fixture anew: `hf-fidelity build /srv/hf-fixture` (`lab/fidelity`, the e2e suite's
     `internal/fidelity`);
   - on hfsrv, `holdfast host add <fqdn> --job fidelity --path /srv/hf-fixture --daily 03:00 --keep-daily 14d --keep-monthly 12mo`;
     on the VM, `holdfast-agent enroll --server https://hfsrv.m1.holdfast.test:8443 --fqdn <fqdn>
     --code <code> --ca-sha256 <pin> --restic /usr/libexec/holdfast/restic`, with the code on stdin, which makes the
     agent's key and certificate (`agent.pem`), then `restore.root: /var/restore` in `agent.yaml` and
     `holdfast-agent.service` enabled and started; the agent backs up at once, through the server's gateway;
   - on a later run, when the host exists and its agent is enrolled with a certificate: `holdfast job run <fqdn> fidelity`
     instead. An agent that lost its enrollment, one enrolled before M8 (a token, not a certificate), or a repository
     that is gone, gets a new code (`holdfast host enroll-code`);
   - waits for the backup's event (`holdfast host events --json`), takes the job's newest snapshot
     (`holdfast snapshot list --refresh --json`, which must be that backup's and made by Holdfast), restores it
     through the agent into a new directory (`holdfast restore <fqdn> <id> --wait --json`, `detail.directory`), and
     runs `hf-fidelity compare /srv/hf-fixture <directory>/srv/hf-fixture` on the VM;
   - on the EL VMs, reads the SELinux denials logged since its run began (`ausearch -m AVC,USER_AVC,SELINUX_ERR`);
     on a failure, the agent's and the server's journal too;
5. prints each VM's OS, kernel and SELinux mode, the 13 properties and the login checks, and exits 1 if a property
   is lost or a login check fails.

The earlier runs' restores in `/var/restore` are removed first; the last one stays for a look. Enrollment codes only
ever travel on SSH's stdin: none is in an argument on this host, in the log or on the terminal.

**Result, 2026-11-21** (Holdfast at `e8eadcd`, whose code is `c6cb429`'s, with this lab; restic 0.19.1, Ceph 19.2.3
Squid; SELinux enforcing on el9 and ol9, no denial logged):

```
VM    OS                              kernel                              SELinux     denials since the run began
ub24  Ubuntu 24.04.5 LTS              6.8.0-147-generic                   none        -
el9   AlmaLinux 9.8 (Olive Jaguar)    5.14.0-687.66.2.el9_8.x86_64        Enforcing   0
ol9   Oracle Linux Server 9.8         6.12.0-204.92.4.2.el9uek.x86_64     Enforcing   0

property                                            ub24    el9     ol9
file set and names (unicode, spaces, newline)       PASS    PASS    PASS
file content (sha256)                               PASS    PASS    PASS
file types incl. FIFO                               PASS    PASS    PASS
owner uid/gid incl. symlink owner                   PASS    PASS    PASS
permission bits incl. setuid, setgid, sticky        PASS    PASS    PASS
mtime (nanoseconds) incl. directories               PASS    PASS    PASS
symlink targets incl. dangling                      PASS    PASS    PASS
user.* and trusted.* xattrs                         PASS    PASS    PASS
POSIX access ACLs                                   PASS    PASS    PASS
POSIX default ACLs on directories                   PASS    PASS    PASS
security.selinux label                              PASS    PASS    PASS
hard links (shared inode)                           PASS    PASS    PASS
sparse file holes                                   PASS    PASS    PASS

result: PASS, 39 of 39 properties kept on 3 VMs
```

It passed in each of the four paths: the first enrollment, a rerun (`job run`), an agent whose state was removed (a new
code), and after the bucket was rebuilt. Two controls show that the comparison can fail: relabelling one restored file
on el9 (`chcon -t var_t`) fails `security.selinux label`, and removing one restored file's ACL on ub24 fails
`POSIX access ACLs`. On el9 the restored `selinux-file.txt` is `system_u:object_r:httpd_sys_content_t:s0`, as its
source is, and the agent and restic run as `unconfined_service_t`. An agent given a wrong `--ca-sha256` is refused
before it sends anything ("the server's certificate chain does not match the pinned fingerprint").

**Result with the login check, 2026-11-22** (Holdfast at `ca4964b`, Authelia 4.39.28): 39 of 39 properties kept, as
above, and

```
login on https://hfsrv.m1.holdfast.test through Authelia 4.39.28 at https://hfsrv.m1.holdfast.test:9091, from this host
Authelia's discovery lists the device_authorization_endpoint          PASS
GET /api/v1/me without a session answers 401                          PASS
GET /auth/login redirects to Authelia's authorization endpoint        PASS
ada logs in through Authelia: /api/v1/me is ada, admin everywhere     PASS
POST /auth/logout ends the session: /api/v1/me 401 again              PASS

result: PASS, 39 of 39 properties kept on 3 VMs, 5 of 5 login checks passed
```

**Result with the packages, 2026-11-22** (M5, plan-m5 item 1): the same 39 of 39 properties and 5 of 5 login
checks, with Holdfast installed from this checkout's packages (`0.5.0~dev43…`): `holdfast-restic` and
`holdfast-agent` on ub24 with `dpkg -i`, and on el9 and ol9 with `rpm -U`; `holdfast-restic`, `holdfast-cli`,
`holdfast-server` and `holdfast-freshness` on hfsrv. The agents run at `Nice=10` and idle I/O priority. Every
host's escrow file holds its current password (`credentials/escrow-age.key` reads it), and the storage test,
B012 included, passes against Ceph RGW. **Then upgraded** to the next build, `0.5.0~dev44` (`63d594c`), package
over package on all four VMs: the agents report the new version, and the test passed again, 39 of 39 and 5 of 5.

The run found one gap: **measuring a repository's size needs `s3:ListBucketVersions`** on Ceph RGW (and AWS), which
the maintenance policy lacked; MinIO lists versions with `s3:ListBucket` alone, so the e2e suite did not see it.
`docs/storage.md` and `rgw-documented-layout.sh` now grant it, and the size error names it.

### M6's lab tests: the rebuild and the dump

After the fidelity test, `lab/lab test m1` runs `lab/test-recovery.sh` (plan-m6 §3, Q8). `site.yml --tags agentdb`
installs each agent's own PostgreSQL first.
- **The bare-metal runbook's first path** (`docs/runbooks/bare-metal.md`), on one VM (`HF_REBUILD_VM`, default the
  first agent): the last fidelity snapshot noted; the fixture moved aside; the agent's packages removed and
  `/etc/holdfast` and `/var/lib/holdfast-agent` deleted; the packages installed again; a new code; the agent enrolled
  with `restore.allow_in_place`; the snapshot restored in place; and `hf-fidelity compare` of the restored fixture with
  the one moved aside.
- **The dump test** on each agent: a database of 1000 rows, a dump in `agent.yaml` (`pg_dump` as `postgres`), a dump job,
  a backup, a restore into a new directory, the file loaded into a fresh database as `postgres`, and its rows counted.

**Result, 2026-11-22** (M6):

```
PASS   ub24 rebuild    wiped, reinstalled, enrolled again, restored in place: 13 of 13 properties kept
PASS   ub24 dump       PostgreSQL 16.15 (Ubuntu): dumped as postgres, restored, loaded: 1000 of 1000 rows
PASS   el9 dump        PostgreSQL 13.23: dumped as postgres, restored, loaded: 1000 of 1000 rows
PASS   ol9 dump        PostgreSQL 13.23: dumped as postgres, restored, loaded: 1000 of 1000 rows
```

The first run found a bug: on el9 and ol9 a dump's restore failed with `xattr.LRemove … security.selinux: permission
denied`. A dump's file has no SELinux label in its snapshot, so restic tried to remove the label the new file got, which
SELinux refuses. A dump's restore, and a restore from another host, now leave that label alone
(`--exclude-xattr security.selinux`); other restores still restore labels exactly, as the fidelity test checks.

### M8's lab test: off-site copies

Then `lab/lab test m1` runs `lab/test-offsite.sh` (plan-m8 §2 part 5). Off-site copies are optional; the lab turns
them on with `offsite:` in hfsrv's `server.yaml`. holdfast-server makes the copies itself (decision 041): there is no
other program, no database of its own (its tables are in the schema `offsite` of `holdfast`) and nothing to register.
The lab has one RGW, so **the off-site bucket is a second bucket on it**, `holdfast-offsite`, with its own user
`holdfast-offsite`, not another realm or cluster as docs/storage.md recommends. The test:
- checks that the server copies off-site (`holdfast settings`);
- waits for each agent's newest fidelity snapshot to be copied, made by Holdfast (`holdfast --offsite snapshot list`),
  and for the host's page to show the copies (`holdfast host show`);
- checks, on one VM (`HF_OFFSITE_VM`, default the first agent), that the agent's certificate cannot read its off-site
  copy through the gateway outside a restore (403);
- restores the copy through that agent (`holdfast restore --from-offsite --wait`), which reads it through the gateway
  while the restore lasts, and compares it with the fixture (`hf-fidelity compare`, 13 properties).

Before M8, M7's test ran holdfast-vault beside the server, with read and restore keys of its own; it passed on
2026-11-22 (Holdfast 0.5.0~dev52), and M8 removed it. On a lab made before M8, the first `lab/lab test` clears what
was left, once: holdfast-vault's package, settings, state and database `holdfast_vault` on hfsrv (`site.yml`); the
agents' users `host-<vm>` (`rgw-documented-layout.sh`); the vault's users and the off-site bucket with M7's copies,
whose passwords were in the vault's database (`rgw-offsite-layout.sh`, which then makes the bucket again, empty); and
those users' keys in the lab's state. Each agent enrolls again, with a certificate; its repository stays.

**Result, 2026-11-22** (M8, Holdfast 0.5.0~dev58 at `5beb9e9`, restic 0.19.1, Ceph 19.2.3; SELinux enforcing on el9 and
ol9, no denial logged). The whole of `lab/lab test m1` passed on a lab made before M8: the fidelity test 39 of 39 and
login 5 of 5, every backup and restore through the gateway; M6's rebuild and dump tests 4 of 4; and

```
PASS   off-site                       holdfast-server copies to holdfast-offsite, every 30s
PASS   ub24 copy                      c0a1e531 copied off-site as 068484b6; the host's page shows 1 copies
PASS   el9 copy                       d88637e7 copied off-site as 8e7e2b5e; the host's page shows 7 copies
PASS   ol9 copy                       3f0cad3a copied off-site as 4095e5d3; the host's page shows 7 copies
PASS   ub24 off-site, no restore      the host's certificate cannot read its off-site copy outside a restore (403)
PASS   ub24 restore from off-site     restored from off-site through the gateway into /var/restore/20261122-165857-root-068484b6: 13 of 13 properties kept
```

No agent VM holds an S3 key: each has its `agent.pem` and the server's CA, and no file in `/etc/holdfast` or
`/var/lib/holdfast-agent` names one. The RGW has no host user. hfsrv runs holdfast-server alone, with the schemas
`public` and `offsite` in the database `holdfast`.

**What the run found in the product.** `restic init` makes a missing bucket itself, owned by the key it runs with, and
on Ceph RGW a plain user may make buckets. The first run removed the off-site bucket (M7's copies) while the server
ran; the off-site copier, which then initialised its repositories without testing the storage first, made the bucket
again, owned by `holdfast-offsite` (B010). Primary already tested the storage before every init. Fixed in `5beb9e9`: the
copier does too, and makes nothing when the test fails. MinIO's policies refuse a bucket to a key without
`s3:CreateBucket`, so the e2e suite could not see it; `internal/restic`'s `TestInitFromTestsTheStorageFirst` covers it.
`rgw-offsite-layout.sh` makes such a bucket again in the layout.

**What the run found in the product.** Both bugs are fixed; see `docs/plan-m2.md`.
- The agent and the server now default `restic_bin` to `/usr/libexec/holdfast/restic`.
- A repository Holdfast initialises, the first time or again, has its keys recorded as known, and the event is
  audited as `repository.initialised`.

As found:
- **The install command lacks restic's path.** The command `holdfast host add` prints is
  `holdfast-agent enroll --server … --fqdn … --code … --ca-sha256 …`. The agent has no default for `restic_bin`
  (`internal/agent/config.go`), so it runs `restic` from `PATH`, and no VM has one there: restic is at
  `/usr/libexec/holdfast/restic` (decision 032). `deploy/examples/agent.yaml`, "written by `holdfast-agent enroll`",
  shows `restic_bin: /usr/libexec/holdfast/restic`, but enroll writes that only when given `--restic`. The lab passes
  `--restic /usr/libexec/holdfast/restic`. The server has the same gap (`restic_bin` has no default), but its
  `server.yaml` is written by hand from the example, which sets it.
- **A repository that Holdfast initialises again is reported as changed outside Holdfast.** After the bucket was
  rebuilt, enrolling again made the server run `restic init` (`ensureStorage`, then `initRepository`). It then
  recorded the new repository's only key through `RecordRepositoryKeys`. Because the host already had known keys,
  that key became "foreign". `holdfast host show` now reports "1 restic passwords were added to the repository
  outside Holdfast" for a key Holdfast made itself. The "snapshots disappeared" alert next to it is correct. The new
  `restic init` is also not in the audit log: the log has `enrollment.code_issued`, `agent.enrolled` and
  `snapshot.disappeared`, but nothing for the initialisation. The lab's three hosts still show both alerts
  (`holdfast host acknowledge <fqdn>` clears them).

### M9's lab tests: the server rebuilt, and what one server carries

Then `lab/lab test m1` runs `lab/test-rebuild.sh`, which walks `docs/runbooks/rebuild-holdfast.md` on hfsrv itself
(plan-m9 Q8). There is one server and no standby (decision 042), so the server is lost and replaced:
- `holdfast export now`, and each agent's snapshots noted;
- the admin's offline copy of `/etc/holdfast` kept in the lab's state (`rebuild/etc-holdfast.tar`);
- the server lost: holdfast-server stopped, the database `holdfast` dropped and made again empty, the packages
  removed, and `/etc/holdfast` and `/var/lib/holdfast-server` deleted;
- the runbook: the same packages installed again, `/etc/holdfast` put back, `holdfast-server restore-export
  -identity <the escrow's age key> -latest` (the key copied to hfsrv for it on SSH's stdin, and deleted after), and
  holdfast-server started;
- each agent back online by itself, with no new code, and its snapshots listed as before; a backup through the
  gateway on one VM (`HF_REBUILD_VM`, default the first agent); `holdfast export list` says the server was restored.

`lab/lab sizing m1` (`lab/test-sizing.sh`) measures what one server's gateway carries, for install.md: on every agent
at once, a backup of new random data (`HF_SIZING_MIB`, default 256 MiB each, which restic can neither deduplicate nor
compress), timed by the server's events, with hfsrv's CPU and what its network received. The job is removed after
(its snapshots stay) and the fixture deleted. The lab is one host's KVM, so the figure is the lab's. A new job's first
backup runs at once, so the job is made and backed up first on a tiny fixture; only the run after the random data is
written is measured.

**Result, 2026-11-22** (M9, Holdfast 0.5.0~dev62, restic 0.19.1, Ceph 19.2.3; SELinux enforcing on el9 and ol9, no
denial logged). The whole of `lab/lab test m1` passed: fidelity 39 of 39 and login 5 of 5, M6 4 of 4, M8 6 of 6, and

```
PASS   export                   meta/2026-11-22.age, 69 KiB, off-site too
PASS   server lost              database dropped and made again empty; packages, /etc/holdfast and /var/lib/holdfast-server removed
PASS   reinstalled              holdfast-restic, -cli, -server and -freshness 0.5.0~dev62.20261122182842-1 installed again; /etc/holdfast put back
PASS   restored                 from meta/2026-11-22.age: head revision 24, 3 hosts
PASS   ub24 back                online by itself, with no new code; 33 of 33 snapshots listed
PASS   el9 back                 online by itself, with no new code; 14 of 14 snapshots listed
PASS   ol9 back                 online by itself, with no new code; 14 of 14 snapshots listed
PASS   ub24 backup              backed up through the gateway on the rebuilt server
PASS   export list              says the server was restored from 2026-11-22.age
```

The agents had waited out the outage and backed up what they missed as soon as the new server answered. The first
run's backup check counted the newest events only, which those catch-up backups had pushed out; the test now waits for
its own command's event.

`lab/lab sizing m1`, the same day (hfsrv with 2 vCPU and 2 GiB):

```
ub24      256 MiB added in  11.6 s
el9       256 MiB added in  11.6 s
ol9       256 MiB added in  11.6 s

3 agents at once: 768 MiB added in 11.6 s through the gateway, 66.0 MiB/s; the server: CPU 21% busy, received 771 MiB
```

The CPU figure is over a window a few seconds longer than the backups, so it reads low; install.md says "about a
quarter".

## Troubleshooting

- **No internet from the VMs:** Docker's FORWARD policy, as in Clusterlane's lab (`lab/lab doctor m1` checks it).
- **A VM never answers SSH:** `virsh -c qemu:///system console hf-m1-<vm>` shows its boot and cloud-init.
- **A checksum mismatch:** remove the file from `~/.cache/holdfast-lab/` and run `up` again.
- **An image gone (404):** AlmaLinux moves old minor releases to `vault.almalinux.org` (already the pinned URL here),
  Ubuntu keeps dated `release-*` directories, and Oracle keeps `OL9/u8/`. To move on, pin a new URL and sha256.
- **Files edited from Windows** through `\\wsl.localhost\...` may lose their execute bit: `chmod 755 lab/lab lab/*.sh`.
- **`lab/lab test` fails:** the terminal says at which step and on which VM, and the run's log has each command's
  answer, the agent's and the server's journal since the run began, and on EL the SELinux denials. The server's
  own journal: `lab/lab ssh m1 hfsrv journalctl -u holdfast-server`.
