package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// M5 (plan-m5 §3):
//   - the escrow file decrypts with the age key and opens the repository with plain restic;
//   - the repository's size, with S3's old versions;
//   - rotate, then a backup with the new password, after enrolling again;
//   - decommission, with its wait and its cancellation.

type escrowFile struct {
	Format             string `json:"format"`
	FQDN               string `json:"fqdn"`
	Repository         string `json:"repository"`
	RepoPassword       string `json:"repo_password"`
	CredentialsVersion int    `json:"credentials_version"`
}

// s3 is a client with MinIO's root, as the storage admin has.
func s3(t *testing.T) *minio.Client {
	t.Helper()
	u, _ := url.Parse(s3Endpoint)
	c, err := minio.New(u.Host, &minio.Options{Creds: credentials.NewStaticV4(s3User, s3Pass, ""), Secure: u.Scheme == "https", Region: "us-east-1"})
	must(t, err)
	return c
}

// escrowFile reads escrow/<fqdn>.age from the bucket and decrypts it with the offline key, as someone
// recovering would.
func (e *env) escrowFile(fqdn string) escrowFile {
	e.t.Helper()
	o, err := s3(e.t).GetObject(context.Background(), e.bucket, "escrow/"+fqdn+".age", minio.GetObjectOptions{})
	must(e.t, err)
	defer o.Close()
	raw, err := io.ReadAll(o)
	must(e.t, err)
	r, err := age.Decrypt(bytes.NewReader(raw), e.escrowKey)
	if err != nil {
		e.t.Fatalf("the escrow file does not decrypt with the age key: %v", err)
	}
	var f escrowFile
	b, _ := io.ReadAll(r)
	must(e.t, json.Unmarshal(b, &f))
	return f
}

// objects counts the current objects and all versions under a prefix.
func (e *env) objects(prefix string) (current, versions int) {
	e.t.Helper()
	c := s3(e.t)
	for o := range c.ListObjects(context.Background(), e.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		must(e.t, o.Err)
		current++
	}
	for o := range c.ListObjects(context.Background(), e.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true, WithVersions: true}) {
		must(e.t, o.Err)
		if !o.IsDeleteMarker {
			versions++
		}
	}
	return current, versions
}

func hasProblem(h hostView, code string) bool {
	return slices.ContainsFunc(h.Problems, func(p problem) bool { return p.Code == code })
}

func TestEscrowSizeAndRotate(t *testing.T) {
	ctx := context.Background()
	src := smallSource(t)
	e := newEnv(t)
	const fqdn = "esc01.example.com"
	a := e.startAgent(fqdn, e.addHost(fqdn, "data", "--path", src, "--daily", "02:00"))
	e.waitProgress(fqdn, "first_backup_done", 2*time.Minute)

	// The escrow file, written at host add, opens the repository with plain restic and the offline key.
	f := e.escrowFile(fqdn)
	if f.FQDN != fqdn || f.Format != "holdfast-escrow-1" || f.RepoPassword == "" || !strings.HasSuffix(f.Repository, "/repos/"+fqdn) {
		t.Fatalf("escrow file: %+v", f)
	}
	old := e.resticFor(escrowHost{Repository: f.Repository, RepoPassword: f.RepoPassword}, false)
	if n := snapshots(t, old); n != 1 {
		t.Fatalf("the escrowed password opens the repository with %d snapshots, want 1", n)
	}
	if h := e.host(fqdn); !h.Escrow.Current || hasProblem(h, "escrow") {
		t.Fatalf("escrow on the host page: %+v %+v", h.Escrow, h.Problems)
	}
	if m := metrics(t, e.url+"/metrics"); m[`holdfast_escrow_current{host="`+fqdn+`"}`] != 1 || m["holdfast_escrow_configured"] != 1 {
		t.Fatal("the escrow metrics are missing")
	}

	// The daily listing measures the repository at the first maintenance pass.
	waitFor(t, "the repository's size", 3*time.Minute, func() bool {
		s := e.host(fqdn).Size
		return s != nil && s.Current > 0 && s.Objects > 0
	})
	if m := metrics(t, e.url+"/metrics"); m[`holdfast_repository_bytes{host="`+fqdn+`",versions="current"}`] <= 0 {
		t.Fatal("the size metric is missing")
	}

	// Rotate: a new password, the old one gone, the token revoked, a new code.
	var rot struct {
		CredentialsVersion int      `json:"credentials_version"`
		NewKey             string   `json:"new_key"`
		RemovedKeys        []string `json:"removed_keys"`
		Escrowed           bool     `json:"escrowed"`
		Warning            string   `json:"warning"`
		Enrollment         struct {
			Code string `json:"code"`
		} `json:"enrollment"`
	}
	must(t, json.Unmarshal(e.cli("host", "rotate", fqdn, "-m", "e2e: suspected compromise"), &rot))
	if rot.Warning != "" || len(rot.RemovedKeys) != 1 || !rot.Escrowed || rot.Enrollment.Code == "" || rot.NewKey == "" {
		t.Fatalf("rotate: %+v", rot)
	}
	if _, err := old.Snapshots(ctx); err == nil {
		t.Fatal("the old password still opens the repository")
	}
	f2 := e.escrowFile(fqdn)
	if f2.RepoPassword == f.RepoPassword || f2.CredentialsVersion != rot.CredentialsVersion {
		t.Fatalf("the escrow file must hold the new password: %+v", f2)
	}
	next := e.resticFor(escrowHost{Repository: f2.Repository, RepoPassword: f2.RepoPassword}, false)
	keys, err := next.Keys(ctx)
	if err != nil || len(keys) != 1 || keys[0].ID != rot.NewKey {
		t.Fatalf("the repository's keys after the rotation: %+v %v", keys, err)
	}
	h := e.host(fqdn)
	if h.State != "awaiting_enrollment" || !hasProblem(h, "enroll") {
		t.Fatalf("the agent's token must be revoked: %s %+v", h.State, h.Problems)
	}

	// Enrolled again with the new code, the agent backs up with the new password.
	a.kill(syscall.SIGTERM)
	e.startAgent(fqdn, rot.Enrollment.Code)
	waitFor(t, "the agent to be back", time.Minute, func() bool { return e.host(fqdn).State == "enrolled" })
	before := countStatus(e.events(fqdn), "data", "success")
	e.cli("job", "run", fqdn, "data")
	waitFor(t, "a backup with the new password", 2*time.Minute, func() bool { return countStatus(e.events(fqdn), "data", "success") > before })
	if n := snapshots(t, next); n != 2 {
		t.Fatalf("snapshots after the rotation: %d, want 2", n)
	}
	// The new key is Holdfast's own: no alert.
	if raw := e.hostRaw(fqdn); raw["foreign_keys"] != nil {
		t.Fatalf("the rotated key is reported as added outside Holdfast: %v", raw["foreign_keys"])
	}
	var actions []string
	for _, x := range e.audit(fqdn) {
		actions = append(actions, x.Action)
	}
	for _, want := range []string{"host.rotated", "repository.key_removed", "escrow.written"} {
		if !slices.Contains(actions, want) {
			t.Errorf("no %s in the audit log: %v", want, actions)
		}
	}
}

func TestDecommission(t *testing.T) {
	src := smallSource(t)
	e := newEnv(t)
	const fqdn = "dec01.example.com"
	e.startAgent(fqdn, e.addHost(fqdn, "data", "--path", src, "--daily", "02:00"))
	e.waitProgress(fqdn, "first_backup_done", 2*time.Minute)
	prefix := "repos/" + fqdn + "/"
	if cur, _ := e.objects(prefix); cur == 0 {
		t.Fatal("no repository")
	}

	// Only a removed host can be decommissioned.
	if out, err := e.cliErr("host", "decommission", fqdn, "--confirm", fqdn); err == nil || !strings.Contains(string(out), "remove") {
		t.Fatalf("a host in the config was decommissioned: %v %s", err, out)
	}
	e.cli("host", "remove", fqdn)
	waitFor(t, "the host to be retired", time.Minute, func() bool { return e.host(fqdn).State == "retired" })
	if out, err := e.cliErr("host", "decommission", fqdn, "--confirm", "dec01"); err == nil || !strings.Contains(string(out), "FQDN") {
		t.Fatalf("decommission without the FQDN typed: %v %s", err, out)
	}

	// Asked for, cancelled, asked for again.
	e.cli("host", "decommission", fqdn, "--confirm", fqdn, "-m", "e2e")
	if h := e.host(fqdn); h.Decommission == nil || time.Until(h.Decommission.DueAt) < 6*24*time.Hour {
		t.Fatalf("decommission waits 7 days: %+v", h.Decommission)
	}
	e.cli("host", "decommission", fqdn, "--cancel")
	if h := e.host(fqdn); h.Decommission != nil {
		t.Fatalf("cancelled: %+v", h.Decommission)
	}
	e.cli("host", "decommission", fqdn, "--confirm", fqdn)
	if h := e.host(fqdn); !hasProblem(h, "decommission") {
		t.Fatalf("a waiting decommission is shown: %+v", h.Problems)
	}

	// 8 days later it runs (the unit test checks the exact end of the wait): the current objects go; S3 keeps the versions; the page says when they are gone.
	e.restartServer([]string{"HOLDFAST_TEST_NOW_OFFSET=192h"}, "-test")
	waitFor(t, "the decommission", 3*time.Minute, func() bool {
		d := e.host(fqdn).Decommission
		return d != nil && d.DoneAt != nil
	})
	h := e.host(fqdn)
	cur, versions := e.objects(prefix)
	if cur != 0 || versions == 0 || h.Decommission.Objects == 0 {
		t.Fatalf("after decommission: %d current objects, %d versions, %d deleted", cur, versions, h.Decommission.Objects)
	}
	if h.Decommission.GoneAt == nil || h.Decommission.GoneAt.Sub(*h.Decommission.DoneAt) != 30*24*time.Hour {
		t.Fatalf("really gone date: %+v", h.Decommission)
	}
	if cur, _ := e.objects("escrow/" + fqdn + ".age"); cur != 0 {
		t.Fatal("the escrow file must be deleted too")
	}
	audited := false
	for _, x := range e.audit(fqdn) {
		audited = audited || x.Action == "host.decommissioned"
	}
	if !audited {
		t.Fatal("the decommission is not audited")
	}
	t.Logf("decommissioned: %d objects deleted; S3 keeps %d versions", h.Decommission.Objects, versions)
}
