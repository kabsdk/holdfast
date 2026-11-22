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

func (p *testProber) ProbeMaintenance(context.Context) storage.Report {
	return storage.Report{Findings: []storage.Finding{}}
}

// TestStorageContractGatesHosts: hosts have no S3 key (decision 041). The storage contract is tested with the
// gateway key on the host's prefix, and a host whose storage fails it is refused at host add. Enrollment is refused
// while the storage fails, the daily check tests it again, and neither the gateway's secret nor any S3 key shows.
func TestStorageContractGatesHosts(t *testing.T) {
	p := &testProber{}
	const gwSecret = "gateway-secret-0123456789abcdef"
	s := newTestServerWith(t, func(d *Deps) {
		d.Prober, d.Gateway = p, storage.Key{AccessKeyID: "GATEWAY01", SecretAccessKey: gwSecret}
	})
	ctx := context.Background()
	const fqdn = "web01.example.com"
	add := func() (AddHostResult, error) {
		return s.AddHost(ctx, root, AddHostInput{FQDN: fqdn, JobID: "system", Job: job("/etc")})
	}

	// Storage that fails the contract: the host is refused, and nothing is saved.
	p.fail(storage.CodeHostDelete)
	var se *StorageError
	if _, err := add(); !errors.As(err, &se) || !slices.Equal(se.Report.Codes(), []string{"B005"}) {
		t.Fatalf("storage that fails B005: %v", err)
	}
	if _, err := s.st.Q().GetHost(ctx, fqdn); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the host was saved while its storage failed: %v", err)
	}
	if _, err := s.st.Q().Head(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a revision was saved while the storage failed: %v", err)
	}

	// Storage that passes: the host is added, tested with the gateway key, and holds no S3 key.
	p.fail()
	res, err := add()
	if err != nil || !res.Storage.OK() {
		t.Fatalf("add host: %+v %v", res, err)
	}
	if p.lastKey.AccessKeyID != "GATEWAY01" || p.lastKey.SecretAccessKey != gwSecret {
		t.Fatal("the contract was not tested with the gateway key")
	}

	// A stranger with the FQDN but not the code causes no S3 traffic.
	before := p.called()
	if err := s.Enroll(ctx, proto.EnrollRequest{FQDN: fqdn, Code: "WRONG-CODE-0000"}, certOf(fqdn)); err == nil {
		t.Fatal("enrolled with a wrong code")
	}
	if p.called() != before {
		t.Fatal("a wrong code made the server test the storage")
	}

	// The storage changed before enrollment: refused, the reason audited, and the code stays valid.
	p.fail(storage.CodeMaintRead)
	if err := s.Enroll(ctx, proto.EnrollRequest{FQDN: fqdn, Code: res.Enrollment.Code}, certOf(fqdn)); err == nil {
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
	if err := s.Enroll(ctx, proto.EnrollRequest{FQDN: fqdn, Code: res.Enrollment.Code}, certOf(fqdn)); err != nil {
		t.Fatalf("enroll once the storage passes: %v", err)
	}
	poll, err := s.Poll(ctx, fqdn, proto.PollRequest{}, 0)
	if err != nil || poll.Credentials == nil || poll.Credentials.RepoPassword == "" {
		t.Fatalf("the agent does not get its password: %+v %v", poll.Credentials, err)
	}
	if !strings.HasPrefix(poll.Config.Repository, "rest:https://holdfast.example.test/gateway/v1/"+fqdn+"/") {
		t.Fatalf("the agent's repository is not at the gateway: %s", poll.Config.Repository)
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

	// The gateway's secret shows nowhere a person, an agent or a scraper can see.
	h, _ := s.Host(ctx, fqdn)
	hostJSON, _ := json.Marshal(h)
	pollJSON, _ := json.Marshal(poll)
	audit, _ = s.st.Q().ListAudit(ctx, "", 1000)
	auditJSON, _ := json.Marshal(audit)
	revs, _ := s.st.Q().ListRevisions(ctx, 100)
	revJSON, _ := json.Marshal(revs)
	for where, text := range map[string]string{"host view": string(hostJSON), "poll": string(pollJSON), "audit log": string(auditJSON), "metrics": metrics, "revisions": string(revJSON)} {
		if strings.Contains(text, gwSecret) {
			t.Errorf("the gateway's S3 secret is in the %s", where)
		}
	}
}
