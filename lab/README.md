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

hfsrv and the agents have restic 0.19.1 at `/usr/libexec/holdfast/restic`. Each agent has `/usr/bin/holdfast-agent` and
`holdfast-agent.service` installed, not enabled and not enrolled. hfsrv's units are installed and disabled too. Every
VM has the others' names in `/etc/hosts` (`<vm>.m1.holdfast.test` and `<vm>`). SELinux is enforcing on the EL VMs.
The network is `hf-m1` (10.79.20.0/24, bridge `hfbr-m1`), and every libvirt object the lab makes is named `hf-m1-*`.

## The commands

| Command | What it does |
| --- | --- |
| `lab/lab doctor m1` | checks what this host lacks for `up`, and says how to get it; changes nothing |
| `lab/lab up m1` | makes the VMs, or starts them, builds this checkout (`CGO_ENABLED=0 make build`, static binaries for every libc), provisions every VM (`site.yml`), then checks the lab; run it again after a failure to resume; on a ready lab it changes nothing |
| `lab/lab status m1 [--json]` | absent, off, ready or broken, with what each VM answered and why it is not ready |
| `lab/lab ssh m1 <vm> [command]` | SSH as root with the lab's key |
| `lab/lab deploy m1` | builds this checkout and installs its binaries on hfsrv and the agents, restarting any Holdfast service that runs |
| `lab/lab isolation m1 account` | the RGW-accounts test (below) |
| `lab/lab isolation m1 <user-policy\|bucket-policy> [--maint-policy]` | the same question without accounts |
| `lab/lab stop m1` | shuts the VMs down; `up` starts them again |
| `lab/lab destroy m1 [--yes]` | removes the VMs, volumes, network and state; the image cache stays |

Exit codes: 0 done (status: ready), 1 a step failed (the log says which), 2 refused or misused, 3 the lab is not there,
off or broken, 75 busy (another command holds the lab's lock). `up`, `deploy`, `isolation`, `stop` and `destroy`
write `<state>/runs/<UTC time>-<command>/log`.

First time on a host: `ansible-galaxy collection install -r lab/ansible/requirements.yml`, then `lab/lab doctor m1`.
Its packages are Clusterlane's (libvirt, qemu-img, xorriso, python3-libvirt, python3-lxml, ansible-core, Go). No root
is needed: libvirt owns every disk and seed in the pool `default`.

## Where things are

- **State:** `${XDG_STATE_HOME:-~/.local/state}/holdfast/labs/m1/`: `lab.env` (source it for the lab's names, URLs and
  file paths), `inventory.yml`, the SSH key, `ceph-version`, and `credentials/` (0700, each file 0600):
  `holdfast-maint.env` and `holdfast-provisioner.env` (`AWS_ACCESS_KEY_ID=`, `AWS_SECRET_ACCESS_KEY=`, the format
  `storage.ReadKeyFile` reads), `postgresql-holdfast.pw` and `.dsn`, and the experiments' keys (`test-host.env`,
  `acct-*.env`, `test-host.restic.pw`). The experiments' transcripts are `isolation-*.txt`.
- **RGW:** `http://s3a.m1.holdfast.test` from the VMs, `http://10.79.20.10` from this host (`HF_RGW_URL`,
  `HF_RGW_URL_HOST`). The region is `default`, requests are path-style, and the bucket is `holdfast`, owned by
  `holdfast-maint`, with versioning `Enabled` (GetBucketVersioning is checked on every `up`). `holdfast-provisioner` has
  the caps `users=*;user-policy=*`.
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
    `test-host`'s per-prefix statements.

  Run it with `lab/lab ssh m1 s3a 'bash -s' < lab/rgw-documented-layout.sh`. It deletes the lab
  bucket's data first. After it, `TestS3ProberLive` with `HOLDFAST_PROBE_EXPECT=` passes every check,
  B001–B011, on Ceph 19.2.3 with plain users. The bucket-policy grant lets the maintenance user read the
  host's objects, although a third identity owns the bucket.
- The isolation experiments (`lab/lab isolation`) still assume their own layout: `holdfast-maint` owns the
  bucket. Running one replaces this layout, so run `rgw-documented-layout.sh` again afterwards.

## Troubleshooting

- **No internet from the VMs:** Docker's FORWARD policy, as in Clusterlane's lab (`lab/lab doctor m1` checks it).
- **A VM never answers SSH:** `virsh -c qemu:///system console hf-m1-<vm>` shows its boot and cloud-init.
- **A checksum mismatch:** remove the file from `~/.cache/holdfast-lab/` and run `up` again.
- **An image gone (404):** AlmaLinux moves old minor releases to `vault.almalinux.org` (already the pinned URL here),
  Ubuntu keeps dated `release-*` directories, and Oracle keeps `OL9/u8/`. To move on, pin a new URL and sha256.
- **Files edited from Windows** through `\\wsl.localhost\...` may lose their execute bit: `chmod 755 lab/lab lab/*.sh`.
