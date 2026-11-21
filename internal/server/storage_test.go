package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/server/storage"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// testProber fails the given storage checks, and remembers the key it was asked to test.
type testProber struct {
	mu      sync.Mutex
	codes   []string
	lastKey storage.Key
	calls   int
}

func (p *testProber) called() int { p.mu.Lock(); defer p.mu.Unlock(); return p.calls }

func (p *testProber) Probe(_ context.Context, _ string, k storage.Key) storage.Report {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastKey, p.calls = k, p.calls+1
	r := storage.Report{Findings: []storage.Finding{}}
	for _, c := range p.codes {
		r.Findings = append(r.Findings, storage.Finding{Code: c, Message: "test finding " + c})
	}
	return r
}

func (p *testProber) fail(codes ...string) { p.mu.Lock(); defer p.mu.Unlock(); p.codes = codes }

// TestStorageContractGatesHosts: the admin gives each host's S3 key, and a key that fails the storage
// contract is refused at host add and at set-key. Enrollment is refused while the storage fails, the
// daily check tests it again, and the secret never shows (decision 029).
func TestStorageContractGatesHosts(t *testing.T) {
	p := &testProber{}
	s := newTestServerWith(t, func(d *Deps) { d.Provisioner, d.Prober = nil, p })
	ctx := context.Background()
	const fqdn = "web01.example.com"
	const secret, secret2 = "host-secret-0123456789abcdef", "host-secret-2-fedcba9876543210"
	key := &S3KeyInput{AccessKeyID: "HOSTKEY01", SecretAccessKey: secret}
	add := func(k *S3KeyInput) (AddHostResult, error) {
		return s.AddHost(ctx, root, AddHostInput{FQDN: fqdn, JobID: "system", Job: job("/etc"), S3Key: k})
	}

	var ve *ValidationError
	if _, err := add(nil); !errors.As(err, &ve) || ve.Problems[0].Path != "s3_key" {
		t.Fatalf("without a provisioner the key is required: %v", err)
	}
	if _, err := add(&S3KeyInput{AccessKeyID: "HOSTKEY01", SecretAccessKey: "has a space"}); !errors.As(err, &ve) {
		t.Fatalf("a malformed key: %v", err)
	}

	// A key that fails the contract is refused, and nothing is saved.
	p.fail(storage.CodeHostDelete)
	var se *StorageError
	if _, err := add(key); !errors.As(err, &se) || !slices.Equal(se.Report.Codes(), []string{"B005"}) {
		t.Fatalf("a key that fails B005: %v", err)
	}
	if _, err := s.st.Q().GetHost(ctx, fqdn); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the host was saved with a key that failed: %v", err)
	}
	if _, err := s.st.Q().Head(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a revision was saved with a key that failed: %v", err)
	}

	// A key that passes: the host is added with exactly that key.
	p.fail()
	res, err := add(key)
	if err != nil || !res.Storage.OK() {
		t.Fatalf("add host with a good key: %+v %v", res, err)
	}
	if p.lastKey.SecretAccessKey != secret {
		t.Fatal("the contract was tested with another key than the one given")
	}
	c, _ := s.st.Q().GetCredentials(ctx, fqdn)
	if c.S3AccessKeyID != "HOSTKEY01" || c.S3KeySource != store.KeyByAdmin {
		t.Fatalf("stored key: %s, made by %s", c.S3AccessKeyID, c.S3KeySource)
	}

	// A stranger with the FQDN but not the code causes no S3 traffic.
	before := p.called()
	if _, err := s.Enroll(ctx, proto.EnrollRequest{FQDN: fqdn, Code: "WRONG-CODE-0000"}); err == nil {
		t.Fatal("enrolled with a wrong code")
	}
	if p.called() != before {
		t.Fatal("a wrong code made the server test the storage")
	}

	// The storage changed before enrollment: refused, the reason audited, and the code stays valid.
	p.fail(storage.CodeMaintRead)
	if _, err := s.Enroll(ctx, proto.EnrollRequest{FQDN: fqdn, Code: res.Enrollment.Code}); err == nil {
		t.Fatal("enrolled while the maintenance key cannot read the host's objects")
	}
	if h, _ := s.Host(ctx, fqdn); h.Storage == nil || !slices.Equal(h.Storage.Codes(), []string{"B003"}) {
		t.Fatalf("the host view does not show the failed check: %+v", h.Storage)
	}
	audit, _ := s.st.Q().ListAudit(ctx, fqdn, 100)
	refused := false
	for _, a := range audit {
		if a.Action == "agent.enroll_refused" && strings.Contains(a.Detail["reason"].(string), "B003") {
			refused = true
		}
	}
	if !refused {
		t.Fatalf("the refusal is not audited with its code: %+v", audit)
	}
	p.fail()
	if _, err := s.Enroll(ctx, proto.EnrollRequest{FQDN: fqdn, Code: res.Enrollment.Code}); err != nil {
		t.Fatalf("enroll once the storage passes: %v", err)
	}
	poll, err := s.Poll(ctx, fqdn, proto.PollRequest{}, 0)
	if err != nil || poll.Credentials == nil || poll.Credentials.S3AccessKeyID != "HOSTKEY01" || poll.Credentials.S3SecretAccessKey != secret {
		t.Fatalf("the agent does not get the admin's key: %+v %v", poll.Credentials, err)
	}

	// Replacing the key: one that fails is refused and the old one stays; one that passes reaches the agent.
	newKey := S3KeyInput{AccessKeyID: "HOSTKEY02", SecretAccessKey: secret2}
	p.fail(storage.CodeHostOtherPrefix)
	if _, err := s.SetS3Key(ctx, root, fqdn, SetS3KeyInput{S3Key: newKey}); !errors.As(err, &se) {
		t.Fatalf("set-key with a key that fails B007: %v", err)
	}
	if c, _ := s.st.Q().GetCredentials(ctx, fqdn); c.Version != 1 || c.S3AccessKeyID != "HOSTKEY01" {
		t.Fatalf("a refused key replaced the old one: version %d, %s", c.Version, c.S3AccessKeyID)
	}
	p.fail()
	set, err := s.SetS3Key(ctx, root, fqdn, SetS3KeyInput{S3Key: newKey, Message: "rotation"})
	if err != nil || set.CredentialsVersion != 2 {
		t.Fatalf("set-key: %+v %v", set, err)
	}
	poll, err = s.Poll(ctx, fqdn, proto.PollRequest{CredentialsVersion: 1}, 0)
	if err != nil || poll.Credentials == nil || poll.Credentials.Version != 2 || poll.Credentials.S3AccessKeyID != "HOSTKEY02" {
		t.Fatalf("the agent does not get the new key: %+v %v", poll.Credentials, err)
	}

	// The daily check tests the storage again, and the metrics name the failed check.
	p.fail(storage.CodeHostDelete)
	if out := s.RunChecks(ctx, root); len(out) != 1 || out[0].Status != "failed" || !slices.Equal(out[0].Storage, []string{"B005"}) {
		t.Fatalf("a check while B005 fails: %+v", out)
	}
	rec := httptest.NewRecorder()
	s.AgentHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	metrics := rec.Body.String()
	for _, want := range []string{`holdfast_storage_contract_ok{host="web01.example.com"} 0`, `holdfast_storage_contract_failed{host="web01.example.com",code="B005"} 1`} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %s", want)
		}
	}

	// The secrets show nowhere a person or a scraper can see.
	h, _ := s.Host(ctx, fqdn)
	hostJSON, _ := json.Marshal(h)
	audit, _ = s.st.Q().ListAudit(ctx, "", 1000)
	auditJSON, _ := json.Marshal(audit)
	revs, _ := s.st.Q().ListRevisions(ctx, 100)
	revJSON, _ := json.Marshal(revs)
	for where, text := range map[string]string{"host view": string(hostJSON), "audit log": string(auditJSON), "metrics": metrics, "revisions": string(revJSON)} {
		if strings.Contains(text, secret) || strings.Contains(text, secret2) {
			t.Errorf("a host's S3 secret is in the %s", where)
		}
	}
}
