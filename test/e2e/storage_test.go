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

// TestStorageContract: hosts hold no S3 key (decision 041). The admin makes two keys for the bucket, and Holdfast
// tests the gateway key's part of the contract of docs/storage.md on each host's prefix, with plain S3 calls.
// - A gateway key that may do too much refuses host add, and nothing is saved.
// - With docs/storage.md's policy, the host is added.
// - While the admin's policy is loosened, enrollment is refused, and the audit log says why.
// - restic ignores the contract test's probe objects.
// - The daily check finds a loosened policy again, and the metric names the check.
// - A maintenance key that cannot read what the gateway key wrote (Ceph RGW's ownership case) is caught as B003.
func TestStorageContract(t *testing.T) {
	src := smallSource(t)
	broad := func(b string) string {
		return `{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["s3:ListBucket","s3:GetBucketLocation"],"Resource":["arn:aws:s3:::` + b + `"]},
 {"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:DeleteObject","s3:DeleteObjectVersion"],"Resource":["arn:aws:s3:::` + b + `/*"]}]}`
	}
	e := newEnv(t)
	e.setGatewayPolicy(broad(e.bucket))
	const fqdn = "app05.example.com"
	add := func() ([]byte, error) {
		return e.cliErr("host", "add", fqdn, "--job", "data", "--path", src, "--every", "1m", testAccept)
	}
	out, err := add()
	if err == nil {
		t.Fatalf("a gateway key that may delete anything and use the whole bucket was accepted: %s", out)
	}
	for _, c := range []string{"B005", "B006", "B007"} {
		if !strings.Contains(string(out), c) {
			t.Fatalf("refused, but without %s:\n%s", c, out)
		}
	}
	if _, err := e.cliErr("host", "show", fqdn); err == nil {
		t.Fatal("the host was added while the storage failed")
	}

	e.setGatewayPolicy(e.gatewayPolicy())
	out, err = add()
	if err != nil {
		t.Fatalf("docs/storage.md's gateway policy was refused: %v\n%s", err, out)
	}
	var res struct{ Enrollment struct{ Code string } }
	must(t, json.Unmarshal(out, &res))
	if h := e.host(fqdn); h.Storage == nil || len(h.Storage.Findings) != 0 {
		t.Fatalf("host view with the right policy: %+v", h.Storage)
	}

	// The admin loosens the policy: enrollment is refused, and the audit log says why.
	loose := filepath.Join(e.dir, "loose.json")
	must(t, os.WriteFile(loose, []byte(`{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["s3:DeleteObject"],"Resource":["arn:aws:s3:::`+e.bucket+`/repos/*"]}]}`), 0o600))
	mc(t, "admin", "policy", "create", "hf", "loose-"+e.bucket, loose)
	mc(t, "admin", "policy", "attach", "hf", "loose-"+e.bucket, "--user", e.gwKey)
	if _, out, err := e.enrollAgentWith(fqdn, res.Enrollment.Code, e.url); err == nil {
		t.Fatalf("enrolled while the gateway key may delete snapshots: %s", out)
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
	mc(t, "admin", "policy", "detach", "hf", "loose-"+e.bucket, "--user", e.gwKey)
	e.startAgent(fqdn, res.Enrollment.Code) // the code is still valid
	e.waitProgress(fqdn, "first_backup_done", 2*time.Minute)

	// restic ignores a probe object under locks/: its name is not an object ID, and the gateway lists only IDs.
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
	mc(t, "admin", "policy", "attach", "hf", "loose-"+e.bucket, "--user", e.gwKey)
	var checks []struct {
		Status  string
		Command string
		Storage []string `json:"storage_codes"`
	}
	out, _ = e.cliErr("check")
	must(t, json.Unmarshal(out, &checks))
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
	mc(t, "admin", "policy", "detach", "hf", "loose-"+e.bucket, "--user", e.gwKey)
	out = e.cli("check")
	checks = nil // json.Unmarshal would keep fields of the old elements that the new answer leaves out
	must(t, json.Unmarshal(out, &checks))
	if checks[0].Status != "requested" || len(checks[0].Storage) != 0 {
		t.Fatalf("check with the policy fixed: %s", out)
	}

	// The maintenance key cannot read what the gateway key wrote: B003, with the ownership hint.
	e2 := newEnvWith(t, envOpts{MaintPolicy: `{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["s3:ListBucket","s3:GetBucketLocation","s3:GetBucketVersioning","s3:PutObject","s3:DeleteObject"],"Resource":["arn:aws:s3:::*"]}]}`})
	out, err = e2.cliErr("host", "add", fqdn)
	if err == nil || !strings.Contains(string(out), "B003") || !strings.Contains(string(out), "docs/storage.md") {
		t.Fatalf("a set-up where the maintenance key cannot read the gateway's objects: %v\n%s", err, out)
	}
}
