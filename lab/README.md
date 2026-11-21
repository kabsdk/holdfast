# The local test lab

Five VMs on this host's KVM and libvirt, on a private NAT network of their own, in the manner of Clusterlane's lab:
the lab is data (`lab/labs/<lab>.yml`), Ansible brings it up (`lab/ansible/`, community.libvirt), and `lab/lab` is the
one command. One lab ships, `m1`.

| VM | Role | OS (image) | vCPU | MiB | Disk GiB | Address |
| --- | --- | --- | --- | --- | --- | --- |
| `s3a` | site A's storage: MicroCeph (Squid 19.2.3, snap `squid/stable`, held), 3 loop-file OSDs of 4 GiB, RGW on port 80 | Ubuntu 24.04 | 2 | 4096 | 40 | 10.79.20.10 |
| `hfsrv` | Holdfast server: PostgreSQL 16 (AppStream module), database and user `holdfast`; `holdfast-server`, `holdfast`, `holdfast-freshness` in /usr/bin | AlmaLinux 9.8 | 2 | 2048 | 20 | 10.79.20.20 |
| `ub24` | agent | Ubuntu 24.04 | 1 | 1024 | 12 | 10.79.20.31 |
| `el9` | agent (the RHEL stand-in) | AlmaLinux 9.8 | 1 | 1024 | 12 | 10.79.20.32 |
| `ol9` | agent | Oracle Linux 9.8 (KVM template, UEK) | 1 | 1024 | 37 (the image's own size) | 10.79.20.33 |

hfsrv and the agents have restic 0.19.1 at `/usr/libexec/holdfast/restic`. Each agent has `/usr/bin/holdfast-agent`,
`holdfast-agent.service` (installed; `lab/lab test` enrolls and starts it) and the lab's fidelity tool
`/usr/local/bin/hf-fidelity`. On hfsrv, `holdfast-server` runs, configured as an admin would (below), and
`holdfast-freshness` is installed but not configured. Every VM has the others' names in `/etc/hosts`
(`<vm>.m1.holdfast.test` and `<vm>`). SELinux is enforcing on the EL VMs. The network is `hf-m1` (10.79.20.0/24, bridge
`hfbr-m1`), and every libvirt object the lab makes is named `hf-m1-*`.

## The commands

| Command | What it does |
| --- | --- |
| `lab/lab doctor m1` | checks what this host lacks for `up`, and says how to get it; changes nothing |
| `lab/lab up m1` | makes the VMs, or starts them, builds this checkout (`CGO_ENABLED=0 make build`, static binaries for every libc, and `bin/hf-fidelity`), provisions every VM (`site.yml`, the Holdfast server included), then checks the lab; run it again after a failure to resume; on a ready lab it changes nothing |
| `lab/lab status m1 [--json]` | absent, off, ready or broken, with what each VM answered and why it is not ready |
| `lab/lab ssh m1 <vm> [command]` | SSH as root with the lab's key |
| `lab/lab deploy m1` | builds this checkout and installs its binaries on hfsrv and the agents, restarting any Holdfast service that runs |
| `lab/lab test m1 [<vm>...]` | M2's "done when", the fidelity test through Holdfast (below): builds and deploys, provisions the server, then backs up and restores the metadata fixture on each agent VM (or those named) and compares the 13 properties |
| `lab/lab isolation m1 account` | the RGW-accounts test (below) |
| `lab/lab isolation m1 <user-policy\|bucket-policy> [--maint-policy]` | the same question without accounts |
| `lab/lab stop m1` | shuts the VMs down; `up` starts them again |
| `lab/lab destroy m1 [--yes]` | removes the VMs, volumes, network and state; the image cache stays |

Exit codes: 0 done (status: ready; test: every property kept on every VM), 1 a step failed (the log says which) or a
property was lost, 2 refused or misused, 3 the lab is not there, off or broken, 75 busy (another command holds the
lab's lock). `up`, `deploy`, `test`, `isolation`, `stop` and `destroy` write `<state>/runs/<UTC time>-<command>/log`.

First time on a host: `ansible-galaxy collection install -r lab/ansible/requirements.yml`, then `lab/lab doctor m1`.
Its packages are Clusterlane's (libvirt, qemu-img, xorriso, python3-libvirt, python3-lxml, ansible-core, Go). No root
is needed: libvirt owns every disk and seed in the pool `default`.

## Where things are

- **State:** `${XDG_STATE_HOME:-~/.local/state}/holdfast/labs/m1/`: `lab.env` (source it for the lab's names, URLs and
  file paths), `inventory.yml`, the SSH key, `ceph-version`, and `credentials/` (0700, each file 0600):
  `holdfast-maint.env` and `holdfast-provisioner.env` (`AWS_ACCESS_KEY_ID=`, `AWS_SECRET_ACCESS_KEY=`, the format
  `storage.ReadKeyFile` reads), `postgresql-holdfast.pw` and `.dsn`, the agents' RGW keys (`host-<vm>.env`, made by
  `lab/lab test`), and the experiments' keys (`test-host.env`, `acct-*.env`, `test-host.restic.pw`). The experiments'
  transcripts are `isolation-*.txt`, and the fidelity test's results `test-fidelity.txt` and `.json`.
- **The lab's CA:** `ca/` in the state (0700): `ca.key` and the server's key (0600), `ca.crt`, the server's certificate
  and its chain, and `ca.sha256`, the pin (sha256 of `ca.crt`'s DER) that is `ca_sha256` in `server.yaml` and that the
  agents check. `lab/ansible/files/lab-ca.sh` makes them once, and the certificate again only when it nears expiry.
- **Holdfast's server:** `https://hfsrv.m1.holdfast.test:8443`, and the CLI `holdfast` as root on hfsrv
  (`lab/lab ssh m1 hfsrv holdfast host list`). `/etc/holdfast/` holds `server.yaml` (0600, from
  `lab/ansible/templates/server.yaml.j2`), `tls/` (the certificate with the CA's after it, its key), `maintenance.env`
  (holdfast-maint's key), `credentials.key` (made once there by `holdfast-server keygen`; no copy is kept elsewhere, so
  a rebuilt hfsrv starts with a new database too) and `db.dsn`. There is no `provisioner_command`: each host's RGW key is
  the lab's, given with `holdfast host add --s3-key-file`.
- **RGW:** `http://s3a.m1.holdfast.test` from the VMs, `http://10.79.20.10` from this host (`HF_RGW_URL`,
  `HF_RGW_URL_HOST`). The region is `default`, requests are path-style, and the bucket is `holdfast`, owned by
  `holdfast-maint` as `up` makes it, with versioning `Enabled` (GetBucketVersioning is checked on every `up`). Holdfast
  needs it in the documented layout instead (owned by `holdfast-bucket-owner`, below), which `lab/rgw-documented-layout.sh`
  makes. `holdfast-provisioner` has the caps `users=*;user-policy=*`.
- **PostgreSQL:** `hfsrv.m1.holdfast.test:5432`, database and user `holdfast`, with scram from the lab's network.
  `/etc/holdfast/db.dsn` on hfsrv is the localhost DSN.
- **Images:** `~/.cache/holdfast-lab`, each pinned by URL and sha256 in `lab/labs/m1.yml` and checked on download. An
  image another lab's cache already holds with that sha256 is copied from there, read only (`lab_cache_peers`).

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
  - a bucket policy with the narrowed maintenance grant and its deny on `s3:DeleteObjectVersion`, plus
    `docs/storage.md`'s four host statements for each host user on its own prefix: `test-host` for
    `repos/test.example.com/`, and the agents' users `host-<vm>` for `repos/<vm>.m1.holdfast.test/`. The agents'
    users are listed in `/root/hf-lab/hosts` on s3a, with their keys beside it; `lab/lab test` writes both from
    the lab's state, and the script makes each user, with its key, when it is missing.

  Run it with `lab/lab ssh m1 s3a 'bash -s' < lab/rgw-documented-layout.sh`. It deletes the lab
  bucket's data first. With `'bash -s -- --keep-data'` it keeps a bucket already in this layout and its
  data, and only makes the users and puts the lifecycle rule and the policy again; `lab/lab test` runs it
  so. After it, `TestS3ProberLive` with `HOLDFAST_PROBE_EXPECT=` passes every check,
  B001–B011, on Ceph 19.2.3 with plain users. The bucket-policy grant lets the maintenance user read the
  host's objects, although a third identity owns the bucket. Each agent's key passes the same checks
  when `holdfast host add` tests it.
- The isolation experiments (`lab/lab isolation`) still assume their own layout: `holdfast-maint` owns the
  bucket. Running one replaces this layout, so run `rgw-documented-layout.sh` again afterwards, without
  `--keep-data` (`lab/lab test` refuses a bucket that another identity owns and that holds data; an empty one, as
  `up` makes it, it makes again in this layout). Rebuilding the bucket deletes the
  agents' repositories; the next `lab/lab test` sees that (restic exit 10) and enrolls each agent again, which
  makes Holdfast initialise its repository.

## The fidelity test through Holdfast: M2's "done when"

`lab/lab test m1` (`lab/test-fidelity.sh`) checks that a backup and a restore made through Holdfast keep all 13
metadata properties on the three agent VMs, against site A's Ceph RGW (docs/plan-m2.md, Q9). It needs a ready lab
(`lab/lab up m1`) and no internet on the VMs. It holds the lab's lock, and takes about two minutes. It:

1. builds this checkout and deploys it (as `deploy`), and provisions the server (`site.yml --tags binaries,hfserver`);
2. makes each agent's RGW user and its key once (`credentials/host-<vm>.env`), and puts them in the bucket policy
   (`rgw-documented-layout.sh --keep-data`);
3. on each agent VM, or each one named (`lab/lab test m1 el9`):
   - makes the fixture anew: `hf-fidelity build /srv/hf-fixture` (`lab/fidelity`, the e2e suite's
     `internal/fidelity`);
   - on hfsrv, `holdfast host add <fqdn> --s3-key-file - --job fidelity --path /srv/hf-fixture --daily 03:00`, with
     the key on stdin; on the VM, `holdfast-agent enroll --server https://hfsrv.m1.holdfast.test:8443 --fqdn <fqdn>
     --code <code> --ca-sha256 <pin> --restic /usr/libexec/holdfast/restic`, with the code on stdin, then
     `restore.root: /var/restore` in `agent.yaml` and `holdfast-agent.service` enabled and started; the agent backs up
     at once;
   - on a later run, when the host exists and its agent is enrolled: `holdfast job run <fqdn> fidelity` instead.
     An agent that lost its enrollment, or a repository that is gone, gets a new code (`holdfast host enroll-code`);
   - waits for the backup's event (`holdfast host events --json`), takes the job's newest snapshot
     (`holdfast snapshot list --refresh --json`, which must be that backup's and made by Holdfast), restores it
     through the agent into a new directory (`holdfast restore <fqdn> <id> --wait --json`, `detail.directory`), and
     runs `hf-fidelity compare /srv/hf-fixture <directory>/srv/hf-fixture` on the VM;
   - on the EL VMs, reads the SELinux denials logged since its run began (`ausearch -m AVC,USER_AVC,SELINUX_ERR`);
     on a failure, the agent's and the server's journal too;
4. prints each VM's OS, kernel and SELinux mode, and the 13 properties, and exits 1 if any is lost.

The earlier runs' restores in `/var/restore` are removed first; the last one stays for a look. S3 keys and enrollment
codes only ever travel on SSH's stdin: none is in an argument on this host, in the log or on the terminal.

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
