# Runbook: a host is compromised

Use this when someone else may have had root on a host that Holdfast backs up. Root on the host can read
the host's restic password and its agent's private key, so assume both are known. The host has no S3 key
(decision 041).

What you can rely on (spec §2.3):
- The host reaches the bucket only through the server's gateway, with its agent's certificate. The gateway lets it
  read and add to its own repository, and delete its own lock files. It cannot delete or overwrite snapshots, and
  it cannot read any other host's repository. Every attempt is refused and audited (`gateway.refused`).
- Bucket versioning keeps every old object version until the lifecycle rule expires it, even against a
  compromised server (B006, B009, B010).

So the attacker can read this host's backups, and can add new snapshots, but cannot destroy the old ones.

## 1. Cut the host off

1. **Rotate the repository password and unpin the agent's certificate:**

   ```
   holdfast host rotate web01.example.com -m "INC-1234: suspected compromise"
   ```

   This adds a new restic password, removes the old one, unpins the agent's certificate and issues a new
   enrollment code (spec §3.5a). The escrow file in the bucket gets the new password. From now on the
   host's certificate gets nothing from the server, not even its own repository: its backups fail.

   If it warns that the old key could not be removed (a backup held the lock for too long, say), run it
   again. The host shows the `rotation` error until the old key is gone.

2. **Look for restic passwords added outside Holdfast.** If `holdfast host show` lists any (the
   `foreign_keys` alert), an attacker may have added one before the rotation, and it is still there. Remove each
   one you did not add, with plain restic, the maintenance key and the new password from the escrow file, then
   acknowledge:

   ```
   restic -r s3:https://s3.example.internal/holdfast/repos/web01.example.com key remove <id>
   holdfast host acknowledge web01.example.com
   ```

There is no S3 key to delete: a host never had one.

## 2. Find the last good snapshot

The attacker may have added snapshots, and may have changed files before the last few backups.

- `holdfast snapshot list web01.example.com --all` lists every snapshot, with any **Not made by Holdfast**
  (snapshots no agent reported) and any that **disappeared**.
- `holdfast snapshot changes web01.example.com <id>` shows what changed between two snapshots: names and
  metadata, never contents. Look for changes to binaries, `/etc`, SSH keys, cron jobs and systemd units
  around the time of the compromise.
- The snapshot just before the first suspicious change is the last good one. Note its ID.

## 3. Rebuild or clean the host, and enroll it again

1. Rebuild the host, or clean it if you are sure you can. Restore what you need from the last good
   snapshot. Once it is enrolled, a restore runs through its agent (spec §5.4).
2. Install the agent, and enroll it with the code `holdfast host rotate` printed. Enrolling makes the agent a new
   key and certificate, and the server pins that one. If the code has expired (after 15 minutes), get a new one:

   ```
   holdfast host enroll-code web01.example.com
   ```

3. Check that the next backup succeeds: `holdfast host events web01.example.com`.

## 4. Keep the record

Every step above is in the host's history and in the audit log, with who did it and when:
`holdfast audit --host web01.example.com`. The rotation is `host.rotated`, the removal of the old key
`repository.key_removed`, the new escrow file `escrow.written`, the new enrollment `agent.enrolled`, and anything
the gateway refused the host `gateway.refused`.

If the escrow bundle from `holdfast escrow export` is stored offline, export a new one: the old one holds
the old password.
