# Runbook: a host rebuilt from nothing

Use this when a host's disks are gone, or the host is replaced. There are two paths: Holdfast is up (the usual
case), or Holdfast is gone too.

A full restore needs a backup of the whole system: a job whose paths cover `/`, with excludes for `/proc`, `/sys`,
`/dev`, `/run`, `/tmp` and the like. Holdfast backs up only what its jobs list. A host whose jobs cover only data
is rebuilt from its installer and configuration management, and then gets its data back (path 1, step 6).

## Path 1: Holdfast is up, and the host comes back as the same agent

This path is walked in the lab (`lab/README.md`, the rebuild test). The agent is the one named `web01.example.com`
in Holdfast. Its name is set when it enrolls (decision 056), not read from the machine, so the new machine's own
hostname does not matter to Holdfast.

1. **Install the OS.**
2. **Install the agent's packages** (`docs/install.md`): `holdfast-restic` and `holdfast-agent`.
3. **Note the last good snapshot first**, before the new agent backs anything up: `holdfast snapshot list
   web01.example.com`. Its first backup after enrolling is of the empty new host, and is only one more snapshot:
   retention keeps the old ones.
4. **Get an enrollment code.** The agent is still in Holdfast: `holdfast agent enroll-code web01.example.com`. If it
   was removed, add it again (`holdfast agent add web01.example.com`): it comes back with its
   repository and its password (spec §3.5), and no job; set its jobs again with `holdfast job set` after step 6, so
   its first backup is not of the empty new host. A decommissioned host starts over, with nothing to restore.
5. **Enroll it**, as root on the host, with the command Holdfast printed, which names the agent (`--name
   web01.example.com`): the new agent makes its own key and certificate, and the server pins them in place of the lost
   host's. Before starting the agent, set what it may do in `/etc/holdfast/agent.yaml`, which is the host's own
   (`deploy/examples/agent.yaml`): `restore.allow_in_place: true` to restore over the fresh system, and
   `postgresql.bin_dir` if it backs up PostgreSQL. Then `systemctl enable --now holdfast-agent`.
6. **Restore** the snapshot from step 3:
   - in place, for configuration and data: `holdfast restore web01.example.com <id> --in-place --only /etc/nginx
     --only /srv/www --wait`. The agent first backs up what it replaces, so this too can be undone;
   - or into a new directory, to copy from: `holdfast restore web01.example.com <id> --wait`, which prints the
     directory under `/var/restore`;
   - a PostgreSQL database backed up by a PostgreSQL job: once the database server runs again, with its roles, an
     admin restores the dump in place (`holdfast restore web01.example.com <id> --in-place --confirm <database>`,
     where `postgresql.allow_in_place_restore` is on), which makes a database that is gone again. Restore the job
     `roles` as a file first, and load it with `psql`, if the roles are gone too: Holdfast never creates roles;
   - another database: restore its export into a new directory, then load it on the host, as root, with the
     database's own tool. The export's systemd timer comes back with the host's configuration management, or with
     `/etc/systemd/system` from the restore.
7. **Check** that the next backup succeeds: `holdfast agent events web01.example.com`.

**Another agent**, such as a spare taking over, is a restore onto another host (spec §5.4): the spare's
`agent.yaml` allows `restore.allow_other_hosts`, and an admin restores with `holdfast restore web01.example.com <id>
--to-host spare01.example.com`. The gateway lets the spare read web01's repository for that restore only.

## Path 2: Holdfast is gone too

Everything needed is outside Holdfast: the recovery file, kept offline, the escrow file in the bucket, and a key from
the storage admin.

1. **Get the repository's password.** Read `escrow/web01.example.com.age` from the bucket, with the maintenance key
   or the storage admin's own, and decrypt it with the recovery file's age key (decisions 036, 047). age refuses the
   recovery file's credentials-key line, so give it the age key alone:

   ```
   grep '^AGE-SECRET-KEY-' holdfast-recovery.txt > age.key
   age -d -i age.key web01.example.com.age > escrow.json
   ```

   It holds `repository` and `repo_password`. Keep both files root-only, and delete them when you are done.
2. **Get a key**: the storage admin makes a read-only key for `repos/web01.example.com/`: `s3:ListBucket` on the
   bucket and `s3:GetObject` on that prefix. Hosts hold no S3 key (decision 041), so this one is made for the
   occasion, and deleted afterwards.
3. **Boot a rescue system** (the distribution's installer in rescue mode), partition and format the new disk, and
   mount it at `/mnt`, with `/mnt/boot` and the other file systems as they were.
4. **Get restic**: from the `holdfast-restic` package (`dpkg-deb -x` or `rpm2cpio … | cpio -id`), or restic's own
   release, checked against its `SHA256SUMS`.
5. **Restore**, without locks, since the key may only read:

   ```
   export RESTIC_REPOSITORY=s3:https://s3.example.internal/holdfast/repos/web01.example.com
   export RESTIC_PASSWORD_FILE=/root/web01.pass       # the repo_password from escrow.json, mode 0600
   export AWS_ACCESS_KEY_ID=… AWS_SECRET_ACCESS_KEY=…  # the read-only key
   restic --no-lock snapshots --host web01.example.com
   restic --no-lock restore <id> --target /mnt
   ```

6. **Make it boot**: fix `/mnt/etc/fstab` if the file systems' UUIDs changed (`blkid`), then `chroot /mnt` and
   reinstall the bootloader and the initramfs (`grub2-install` and `dracut -f` on EL; `grub-install`,
   `update-initramfs -u` and `update-grub` on Ubuntu). On EL, `touch /.autorelabel`, so SELinux labels are set
   again at the first boot.
7. **Reboot**, and once Holdfast is back ([rebuild-holdfast.md](rebuild-holdfast.md)), bring the host back under it
   with path 1, steps 4 to 7.

**Rolling back to a time before a password rotation** works the same way: the escrow file of that time is an old
version of `escrow/web01.example.com.age`, which S3 keeps as long as the lifecycle rule keeps old versions.
