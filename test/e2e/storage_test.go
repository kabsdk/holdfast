package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestStorageContract: the admin makes each host's S3 key, and Holdfast tests the contract of
// docs/storage.md with plain S3 calls (decision 029).
// - A key that may do too much, or one for the wrong host, is refused at host add, and nothing is saved.
// - A right key is accepted.
// - While the admin's policy is loosened, enrollment is refused.
// - restic ignores the contract test's probe objects.
// - The daily check finds a loosened policy again, and the metric names the check.
// - host set-key replaces the key, and the agent backs up with the new one.
// - A maintenance key that cannot read what host keys wrote (Ceph RGW's ownership case) is caught as B003.
func TestStorageContract(t *testing.T) {
	src := smallSource(t)
	e := newEnv(t)
	const fqdn = "app05.example.com"
	fill := strings.NewReplacer("BUCKET", e.bucket, "FQDN", fqdn).Replace
	add := func(kf string) ([]byte, error) {
		return e.cliErr("host", "add", fqdn, "--s3-key-file", kf, "--job", "data", "--path", src, "--every", "1m")
	}
	refusedWith := func(what string, out []byte, err error, codes ...string) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s was accepted: %s", what, out)
		}
		for _, c := range codes {
			if !strings.Contains(string(out), c) {
				t.Fatalf("%s: refused, but without %s:\n%s", what, c, out)
			}
		}
	}

	broad := fill(`{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["s3:ListBucket","s3:GetBucketLocation"],"Resource":["arn:aws:s3:::BUCKET"]},
 {"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:DeleteObject","s3:DeleteObjectVersion"],"Resource":["arn:aws:s3:::BUCKET/repos/FQDN/*"]}]}`)
	kf, _ := e.makeKey(fqdn, broad)
	out, err := add(kf)
	refusedWith("a key that may delete anything in its prefix and list the bucket", out, err, "B005", "B006", "B007")
	if _, err := e.cliErr("host", "show", fqdn); err == nil {
		t.Fatal("the host was added with a refused key")
	}
	kf, _ = e.makeKey("other.example.com", "")
	out, err = add(kf)
	refusedWith("another host's key", out, err, "B001")

	kf, ak := e.makeKey(fqdn, "")
	out, err = add(kf)
	if err != nil {
		t.Fatalf("the right key was refused: %v\n%s", err, out)
	}
	var res struct{ Enrollment struct{ Code string } }
	must(t, json.Unmarshal(out, &res))
	if h := e.host(fqdn); h.Storage == nil || len(h.Storage.Findings) != 0 {
		t.Fatalf("host view after a good key: %+v", h.Storage)
	}

	// The admin loosens the policy: enrollment is refused, and the audit log says why.
	looseFile := filepath.Join(e.dir, "loose.json")
	must(t, os.WriteFile(looseFile, []byte(fill(`{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["s3:DeleteObject"],"Resource":["arn:aws:s3:::BUCKET/repos/FQDN/*"]}]}`)), 0o600))
	mc(t, "admin", "policy", "create", "hf", "loose-"+e.bucket, looseFile)
	mc(t, "admin", "policy", "attach", "hf", "loose-"+e.bucket, "--user", ak)
	if _, out, err := e.enrollAgentWith(fqdn, res.Enrollment.Code, e.url); err == nil {
		t.Fatalf("enrolled while the host key may delete its snapshots: %s", out)
	}
	audited := false
	for _, a := range e.audit(fqdn) {
		if a.Action == "agent.enroll_refused" && strings.Contains(a.Detail["reason"].(string), "B005") {
			audited = true
		}
	}
	if !audited {
		t.Fatal("the refused enrollment is not audited with B005")
	}
	mc(t, "admin", "policy", "detach", "hf", "loose-"+e.bucket, "--user", ak)
	e.startAgent(fqdn, res.Enrollment.Code) // the code is still valid
	e.waitProgress(fqdn, "first_backup_done", 2*time.Minute)

	// restic ignores a probe object under locks/: its name is not an object ID.
	probe := filepath.Join(e.dir, "probe")
	must(t, os.WriteFile(probe, []byte("holdfast storage probe; safe to delete\n"), 0o600))
	mc(t, "cp", "--quiet", probe, "hf/"+e.bucket+"/repos/"+fqdn+"/locks/holdfast-probe-e2e")
	before := countStatus(e.events(fqdn), "data", "success")
	waitFor(t, "a backup with a probe object in locks/", 2*time.Minute, func() bool { return countStatus(e.events(fqdn), "data", "success") > before })
	if n := countStatus(e.events(fqdn), "data", "failed"); n != 0 {
		t.Fatalf("%d backups failed with a probe object in locks/", n)
	}
	mc(t, "rm", "--quiet", "hf/"+e.bucket+"/repos/"+fqdn+"/locks/holdfast-probe-e2e")

	// Loosened again later: the check finds it, and the metric names the check.
	mc(t, "admin", "policy", "attach", "hf", "loose-"+e.bucket, "--user", ak)
	var checks []struct {
		Status  string
		Command string
		Storage []string `json:"storage_codes"`
	}
	out, _ = e.cliErr("check")
	must(t, json.Unmarshal(out, &checks))
	// MinIO lets s3:DeleteObject delete versions for good too, so B006 fails along with B005.
	if len(checks) != 1 || checks[0].Status != "failed" || !slices.Contains(checks[0].Storage, "B005") {
		t.Fatalf("check while the policy is loose: %+v", checks)
	}
	if m := metrics(t, e.url+"/metrics"); m[`holdfast_storage_contract_failed{host="app05.example.com",code="B005"}`] != 1 {
		t.Fatal("the failed storage check is not exported")
	}
	waitFor(t, "the agent's check that holdfast check asked for", time.Minute, func() bool {
		for _, ev := range e.events(fqdn) {
			if ev.Kind == "check" && ev.CommandID == checks[0].Command {
				return true
			}
		}
		return false
	})

	// A new key replaces it. Once the agent runs with the new key, the old user is removed, so backups
	// must use the new key. (Removing a key while a run still uses it makes that run fail: restic retries,
	// then gives up. docs/storage.md says to wait.)
	kf2, _ := e.makeKey(fqdn, "")
	var set struct {
		CredentialsVersion int `json:"credentials_version"`
	}
	must(t, json.Unmarshal(e.cli("host", "set-key", fqdn, "--s3-key-file", kf2, "-m", "rotate"), &set))
	if set.CredentialsVersion != 2 {
		t.Fatalf("credentials version after set-key: %d", set.CredentialsVersion)
	}
	waitFor(t, "the agent on the new key", time.Minute, func() bool {
		h := e.host(fqdn)
		return h.Agent != nil && h.Agent.CredentialsVersion == 2
	})
	switched := time.Now()
	waitFor(t, "a backup that started on the new key", 2*time.Minute, func() bool {
		for _, ev := range e.events(fqdn) {
			if ev.Kind == "backup" && ev.Status == "success" && ev.Started.After(switched) {
				return true
			}
		}
		return false
	})
	mc(t, "admin", "user", "remove", "hf", ak)
	before = countStatus(e.events(fqdn), "data", "success")
	waitFor(t, "a backup with the new key", 2*time.Minute, func() bool { return countStatus(e.events(fqdn), "data", "success") > before })
	out = e.cli("check")
	checks = nil // json.Unmarshal would keep fields of the old elements that the new answer leaves out
	must(t, json.Unmarshal(out, &checks))
	if checks[0].Status != "requested" || len(checks[0].Storage) != 0 {
		t.Fatalf("check with the new key: %s", out)
	}

	// The maintenance key cannot read what host keys wrote: B003, with the ownership hint.
	e2 := newEnvWith(t, envOpts{MaintPolicy: `{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["s3:ListBucket","s3:GetBucketLocation","s3:GetBucketVersioning","s3:PutObject","s3:DeleteObject"],"Resource":["arn:aws:s3:::*"]}]}`})
	kf3, _ := e2.makeKey(fqdn, "")
	out, err = e2.cliErr("host", "add", fqdn, "--s3-key-file", kf3)
	refusedWith("a set-up where the maintenance key cannot read host objects", out, err, "B003", "docs/storage.md")
}
