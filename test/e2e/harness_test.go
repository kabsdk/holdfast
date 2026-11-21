// Package e2e runs the real binaries against real restic, a real PostgreSQL and a real S3 server
// (MinIO), with real process kills. `make e2e` provides all of it and runs the suite as root in a
// privileged container, which the metadata tests need (ownership, trusted.* xattrs, ACLs).
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/kabsdk/holdfast/internal/restic"
	"github.com/kabsdk/holdfast/internal/testpg"
)

var (
	binDir     string
	repoRoot   string
	resticBin  = os.Getenv("HOLDFAST_RESTIC")
	mcBin      = os.Getenv("HOLDFAST_MC")
	s3Endpoint = os.Getenv("HOLDFAST_S3_ENDPOINT")
	s3User     = os.Getenv("HOLDFAST_S3_ROOT_USER")
	s3Pass     = os.Getenv("HOLDFAST_S3_ROOT_PASSWORD")
)

func TestMain(m *testing.M) {
	for name, v := range map[string]string{"HOLDFAST_RESTIC": resticBin, "HOLDFAST_MC": mcBin, "HOLDFAST_S3_ENDPOINT": s3Endpoint,
		"HOLDFAST_S3_ROOT_USER": s3User, "HOLDFAST_S3_ROOT_PASSWORD": s3Pass, "HOLDFAST_TEST_PG": os.Getenv("HOLDFAST_TEST_PG")} {
		if v == "" {
			fmt.Printf("SKIP: the e2e tests need %s (run `make e2e`)\n", name)
			os.Exit(0)
		}
	}
	repoRoot, _ = filepath.Abs("../..")
	dir, err := os.MkdirTemp("", "holdfast-bin-")
	if err != nil {
		panic(err)
	}
	binDir = dir
	build := exec.Command("go", "build", "-o", binDir+"/", "./cmd/...")
	build.Dir = repoRoot
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(binDir)
	os.Exit(code)
}

/* ---------------- processes ---------------- */

type proc struct {
	cmd *exec.Cmd
	log string
}

func start(t *testing.T, logPath string, env []string, name string, args ...string) *proc {
	t.Helper()
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(binDir, name), args...)
	cmd.Stdout, cmd.Stderr = f, f
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &proc{cmd: cmd, log: logPath}
	t.Cleanup(func() { p.kill(syscall.SIGKILL) })
	return p
}

// kill signals the whole process group, so restic dies with the agent.
func (p *proc) kill(sig syscall.Signal) {
	if p.cmd.Process == nil || p.cmd.ProcessState != nil {
		return
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, sig)
	_, _ = p.cmd.Process.Wait()
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for: %s", timeout, what)
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

/* ---------------- S3 ---------------- */

func mcHost() string {
	u, _ := url.Parse(s3Endpoint)
	return fmt.Sprintf("%s://%s:%s@%s", u.Scheme, s3User, s3Pass, u.Host)
}

func mc(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(mcBin, args...)
	cmd.Env = append(os.Environ(), "MC_HOST_hf="+mcHost())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mc %v: %v\n%s", args, err, out)
	}
	return string(out)
}

/* ---------------- one test environment: database, bucket, server ---------------- */

type env struct {
	t       *testing.T
	dir     string
	port    int
	url     string
	sock    string
	bucket  string
	dsn     string
	cfgPath string
	srv     *proc
	srvEnv  []string
	logs    []string
	client  *http.Client
	// provisioner: Holdfast generates host keys and runs provision-minio.sh. Otherwise each test makes
	// the keys itself, as an admin would (decision 029).
	provisioner bool
}

// envOpts change one test environment.
type envOpts struct {
	S3Endpoint  string    // the endpoint the server and agents use (a proxy in front of MinIO); default: MinIO
	TLS         *tlsFiles // serve the agent listener over TLS with this certificate
	Provisioner bool      // configure storage.provisioner_command, and add hosts without a key
	MaintPolicy string    // give the server a maintenance key with this policy instead of MinIO's root
}

func newEnv(t *testing.T) *env { return newEnvWith(t, envOpts{}) }

func newEnvWith(t *testing.T, o envOpts) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{t: t, dir: dir, port: freePort(t), sock: filepath.Join(dir, "run", "api.sock"), bucket: "hf-" + randHex(6), client: http.DefaultClient,
		provisioner: o.Provisioner}
	scheme := "http"
	if o.TLS != nil {
		scheme = "https"
		e.client = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	}
	e.url = fmt.Sprintf("%s://127.0.0.1:%d", scheme, e.port)
	endpoint := s3Endpoint
	if o.S3Endpoint != "" {
		endpoint = o.S3Endpoint
	}
	mc(t, "mb", "--quiet", "hf/"+e.bucket)
	mc(t, "version", "enable", "hf/"+e.bucket)
	mc(t, "ilm", "rule", "add", "--noncurrent-expire-days", "30", "hf/"+e.bucket) // docs/storage.md, B011
	dsn := testpg.DSN(t)
	e.dsn = dsn
	keygen := exec.Command(filepath.Join(binDir, "holdfast-server"), "keygen", filepath.Join(dir, "credentials.key"))
	if out, err := keygen.CombinedOutput(); err != nil {
		t.Fatalf("keygen: %v %s", err, out)
	}
	// The server's maintenance key is a user with docs/storage.md's policy, not MinIO's root, which could
	// change the bucket's settings (B010).
	maint := filepath.Join(dir, "maintenance.env")
	policy := o.MaintPolicy
	if policy == "" {
		policy = e.maintPolicy()
	}
	mak, msk := e.makeUser(policy)
	must(t, os.WriteFile(maint, []byte("AWS_ACCESS_KEY_ID="+mak+"\nAWS_SECRET_ACCESS_KEY="+msk+"\n"), 0o600))
	stor := map[string]any{"endpoint": endpoint, "bucket": e.bucket, "region": "us-east-1", "maintenance_credentials_file": maint}
	if o.Provisioner {
		stor["provisioner_command"] = []string{"bash", filepath.Join(repoRoot, "test", "e2e", "provision-minio.sh")}
	}
	set := map[string]any{
		"listen": fmt.Sprintf("127.0.0.1:%d", e.port), "api_socket": e.sock, "public_url": e.url,
		"database_dsn": dsn, "credentials_key_file": filepath.Join(dir, "credentials.key"), "state_dir": filepath.Join(dir, "server"),
		"restic_bin": resticBin, "default_zone": "UTC", "node_name": "e2e",
		"storage": stor,
		// Tests run `holdfast check` themselves; a background check would race their lock counts.
		"maintenance": map[string]any{"check": "off", "check_cycle_days": 30},
	}
	if o.TLS != nil {
		set["tls"] = map[string]any{"cert_file": o.TLS.cert, "key_file": o.TLS.key}
		set["ca_sha256"] = o.TLS.pin
	}
	b, _ := yaml.Marshal(set)
	e.cfgPath = filepath.Join(dir, "server.yaml")
	must(t, os.WriteFile(e.cfgPath, b, 0o600))
	e.srvEnv = append(os.Environ(), "MC="+mcBin, "MC_HOST_hf="+mcHost(), "TZ=UTC")
	t.Cleanup(func() {
		if t.Failed() {
			for _, l := range e.logs {
				b, _ := os.ReadFile(l)
				t.Logf("==== %s ====\n%s", filepath.Base(l), tail(string(b), 6000))
			}
		}
	})
	e.startServer()
	return e
}

func tail(s string, n int) string {
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

func (e *env) logFile(name string) string {
	p := filepath.Join(e.dir, name+".log")
	for _, l := range e.logs {
		if l == p {
			return p
		}
	}
	e.logs = append(e.logs, p)
	return p
}

func (e *env) startServer() {
	e.srv = start(e.t, e.logFile("server"), e.srvEnv, "holdfast-server", "-config", e.cfgPath)
	waitFor(e.t, "server healthz", 20*time.Second, func() bool {
		r, err := e.client.Get(e.url + "/healthz")
		if err != nil {
			return false
		}
		r.Body.Close()
		_, serr := os.Stat(e.sock)
		return r.StatusCode == 200 && serr == nil
	})
}

// cli runs the holdfast CLI against this environment's API socket and returns its JSON output.
func (e *env) cli(args ...string) []byte {
	e.t.Helper()
	out, err := e.cliErr(args...)
	if err != nil {
		e.t.Fatalf("holdfast %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (e *env) cliErr(args ...string) ([]byte, error) {
	cmd := exec.Command(filepath.Join(binDir, "holdfast"), append([]string{"--socket", e.sock, "--json"}, args...)...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.Bytes(), err
}

// hostPolicy is the example host policy of docs/storage.md, for this environment's bucket.
func (e *env) hostPolicy(fqdn string) string {
	b, p := e.bucket, "repos/"+fqdn
	return `{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["s3:GetBucketLocation"],"Resource":["arn:aws:s3:::` + b + `"]},
 {"Effect":"Allow","Action":["s3:ListBucket"],"Resource":["arn:aws:s3:::` + b + `"],"Condition":{"StringLike":{"s3:prefix":["` + p + `","` + p + `/*"]}}},
 {"Effect":"Allow","Action":["s3:GetObject","s3:PutObject"],"Resource":["arn:aws:s3:::` + b + `/` + p + `/*"]},
 {"Effect":"Allow","Action":["s3:DeleteObject"],"Resource":["arn:aws:s3:::` + b + `/` + p + `/locks/*"]}]}`
}

// maintPolicy is the example maintenance policy of docs/storage.md, for this environment's bucket.
func (e *env) maintPolicy() string {
	b := e.bucket
	return `{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["s3:ListBucket","s3:GetBucketLocation","s3:GetBucketVersioning","s3:GetLifecycleConfiguration"],"Resource":["arn:aws:s3:::` + b + `"]},
 {"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:DeleteObject"],"Resource":["arn:aws:s3:::` + b + `/repos/*"]},
 {"Effect":"Deny","Action":["s3:DeleteObjectVersion"],"Resource":["arn:aws:s3:::` + b + `/*"]}]}`
}

// makeUser makes a MinIO user with its own policy, as an admin would with the S3 server's own tools.
// Test-only: mc takes the secret as an argument.
func (e *env) makeUser(policy string) (accessKey, secret string) {
	e.t.Helper()
	accessKey, secret = "HK"+strings.ToUpper(randHex(8)), randHex(20)
	name := "hf-" + randHex(6)
	pf := filepath.Join(e.dir, name+".json")
	must(e.t, os.WriteFile(pf, []byte(policy), 0o600))
	mc(e.t, "admin", "user", "add", "hf", accessKey, secret)
	mc(e.t, "admin", "policy", "create", "hf", name, pf)
	mc(e.t, "admin", "policy", "attach", "hf", name, "--user", accessKey)
	return accessKey, secret
}

// makeKey makes a host key with the given policy (docs/storage.md's when empty) and returns a root-only
// key file for --s3-key-file, and the access key.
func (e *env) makeKey(fqdn, policy string) (file, accessKey string) {
	e.t.Helper()
	if policy == "" {
		policy = e.hostPolicy(fqdn)
	}
	ak, sk := e.makeUser(policy)
	file = filepath.Join(e.dir, "key-"+randHex(4)+".env")
	must(e.t, os.WriteFile(file, []byte("AWS_ACCESS_KEY_ID="+ak+"\nAWS_SECRET_ACCESS_KEY="+sk+"\n"), 0o600))
	return file, ak
}

// addHost adds a host with one job through the CLI and returns the enrollment code. Unless the
// environment has a provisioner, it first makes the host's key, as an admin would.
func (e *env) addHost(fqdn, jobID string, jobArgs ...string) string {
	e.t.Helper()
	var res struct {
		Enrollment struct{ Code string } `json:"enrollment"`
		Warning    string                `json:"warning"`
	}
	args := []string{"host", "add", fqdn, "--job", jobID}
	if !e.provisioner {
		kf, _ := e.makeKey(fqdn, "")
		args = append(args, "--s3-key-file", kf)
	}
	out := e.cli(append(args, jobArgs...)...)
	if err := json.Unmarshal(out, &res); err != nil || res.Enrollment.Code == "" {
		e.t.Fatalf("host add: %v %s", err, out)
	}
	if res.Warning != "" {
		e.t.Fatalf("host add warned: %s", res.Warning)
	}
	return res.Enrollment.Code
}

type agentProc struct {
	*proc
	dir     string
	cfgPath string
}

func (a *agentProc) spooled() int {
	entries, _ := os.ReadDir(filepath.Join(a.dir, "state", "spool"))
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

// enrollAgent enrolls an agent for the host; runAgent then runs it.
func (e *env) enrollAgent(fqdn, code string) *agentProc {
	e.t.Helper()
	a, out, err := e.enrollAgentWith(fqdn, code, e.url)
	if err != nil {
		e.t.Fatalf("agent enroll: %v\n%s", err, out)
	}
	return a
}

// enrollAgentWith enrolls through the given server URL (a proxy, say) with extra flags, and
// returns the error instead of failing, for tests of refused enrollments.
func (e *env) enrollAgentWith(fqdn, code, server string, extra ...string) (*agentProc, []byte, error) {
	dir := filepath.Join(e.dir, "agent-"+strings.Split(fqdn, ".")[0])
	must(e.t, os.MkdirAll(dir, 0o700))
	a := &agentProc{dir: dir, cfgPath: filepath.Join(dir, "agent.yaml")}
	args := append([]string{"enroll", "--server", server, "--fqdn", fqdn, "--code", code,
		"--config", a.cfgPath, "--state-dir", filepath.Join(dir, "state"), "--credentials-dir", filepath.Join(dir, "credentials"),
		"--restic", resticBin, "--poll-timeout", "2s", "--tick", "250ms"}, extra...)
	out, err := exec.Command(filepath.Join(binDir, "holdfast-agent"), args...).CombinedOutput()
	return a, out, err
}

// event is one run as the API returns it.
type event struct {
	ID           string     `json:"id"`
	Kind         string     `json:"kind"`
	JobID        string     `json:"job_id"`
	CommandID    string     `json:"command_id"`
	Status       string     `json:"status"`
	ScheduledFor *time.Time `json:"scheduled_for"`
	Started      time.Time  `json:"started"`
	Finished     time.Time  `json:"finished"`
	Revision     int64      `json:"revision"`
	SnapshotID   string     `json:"snapshot_id"`
	Error        string     `json:"error"`
}

// events returns a host's runs, newest first, through the CLI.
func (e *env) events(fqdn string) []event {
	e.t.Helper()
	var evs []event
	out, err := e.cliErr("host", "events", fqdn)
	if err != nil {
		return nil
	}
	_ = json.Unmarshal(out, &evs)
	return evs
}

type auditEntry struct {
	Actor  string         `json:"actor"`
	Action string         `json:"action"`
	FQDN   string         `json:"fqdn"`
	Detail map[string]any `json:"detail"`
}

func (e *env) audit(fqdn string) []auditEntry {
	e.t.Helper()
	var list []auditEntry
	if err := json.Unmarshal(e.cli("audit", "--host", fqdn), &list); err != nil {
		e.t.Fatal(err)
	}
	return list
}

/* ---------------- TLS ---------------- */

type tlsFiles struct {
	cert, key, pin string
	caDER          []byte // the CA's certificate, which is public
}

// newCert makes a certificate. A CA when isCA; otherwise a server certificate for 127.0.0.1, signed by
// parent's key (self-signed when parent is nil).
func newCert(t *testing.T, isCA bool, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) ([]byte, *x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "holdfast-e2e"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	if isCA {
		tmpl.IsCA, tmpl.BasicConstraintsValid, tmpl.KeyUsage = true, true, x509.KeyUsageCertSign|x509.KeyUsageDigitalSignature
	} else {
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		tmpl.KeyUsage, tmpl.ExtKeyUsage = x509.KeyUsageDigitalSignature, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	if parent == nil {
		parent, parentKey = tmpl, k
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &k.PublicKey, parentKey)
	must(t, err)
	c, err := x509.ParseCertificate(der)
	must(t, err)
	return der, c, k
}

// serverCert makes a CA and a server certificate for 127.0.0.1 signed by it, as in production. The
// certificate file holds the chain (server, then CA); pin is the CA's sha256.
func serverCert(t *testing.T) *tlsFiles {
	t.Helper()
	caDER, ca, caKey := newCert(t, true, nil, nil)
	der, _, k := newCert(t, false, ca, caKey)
	kb, err := x509.MarshalECPrivateKey(k)
	must(t, err)
	dir := t.TempDir()
	f := &tlsFiles{cert: filepath.Join(dir, "server.crt"), key: filepath.Join(dir, "server.key"), caDER: caDER}
	chain := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...)
	must(t, os.WriteFile(f.cert, chain, 0o644))
	must(t, os.WriteFile(f.key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600))
	sum := sha256.Sum256(caDER)
	f.pin = hex.EncodeToString(sum[:])
	return f
}

// impostor serves HTTPS on 127.0.0.1 with a certificate of its own, sent with the real CA's certificate
// after it, so the pinned fingerprint is in the chain. It is a man in the middle who knows the public CA.
func impostor(t *testing.T, real *tlsFiles) string {
	t.Helper()
	der, _, k := newCert(t, false, nil, nil)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the impostor was trusted: the agent sent %s %s", r.Method, r.URL.Path)
		http.Error(w, "impostor", http.StatusTeapot)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der, real.caDER}, PrivateKey: k}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.URL
}

/* ---------------- proxies ---------------- */

// throttle is a TCP proxy whose upload direction (towards the target) can be slowed down, so a
// backup is still uploading when the test kills the agent.
type throttle struct {
	l      net.Listener
	target string
	rate   atomic.Int64 // bytes per second towards the target; 0 means unlimited
}

func newThrottle(t *testing.T, target string) *throttle {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	p := &throttle{l: l, target: target}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go p.handle(c)
		}
	}()
	return p
}

func (p *throttle) addr() string { return p.l.Addr().String() }

func (p *throttle) handle(c net.Conn) {
	defer c.Close()
	up, err := net.Dial("tcp", p.target)
	if err != nil {
		return
	}
	defer up.Close()
	go func() { _, _ = io.Copy(c, up); c.Close() }()
	buf := make([]byte, 4096)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			if _, werr := up.Write(buf[:n]); werr != nil {
				return
			}
			if r := p.rate.Load(); r > 0 {
				time.Sleep(time.Duration(float64(n) / float64(r) * float64(time.Second)))
			}
		}
		if err != nil {
			return
		}
	}
}

// dropProxy forwards everything to the server, but for the first n event deliveries it throws the
// server's answer away and tells the agent the delivery failed. The server has stored the events;
// the agent sends them again.
type dropProxy struct {
	url       string
	remaining atomic.Int32
	dropped   atomic.Int32
}

func newDropProxy(t *testing.T, target string, n int32) *dropProxy {
	u, _ := url.Parse(target)
	rp := httputil.NewSingleHostReverseProxy(u)
	p := &dropProxy{}
	p.remaining.Store(n)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/agent/v1/events" && p.remaining.Load() > 0 {
			rec := httptest.NewRecorder()
			rp.ServeHTTP(rec, r)
			if rec.Code == http.StatusOK {
				p.remaining.Add(-1)
				p.dropped.Add(1)
			}
			http.Error(w, "dropped by the test proxy", http.StatusBadGateway)
			return
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	p.url = srv.URL
	return p
}

func (e *env) runAgent(a *agentProc) {
	a.proc = start(e.t, e.logFile(filepath.Base(a.dir)), append(os.Environ(), "TZ=UTC"), "holdfast-agent", "run", "--config", a.cfgPath)
}

func (e *env) startAgent(fqdn, code string) *agentProc {
	a := e.enrollAgent(fqdn, code)
	e.runAgent(a)
	return a
}

type jobStatus struct {
	LastStatus     string     `json:"last_status"`
	LastError      string     `json:"last_error"`
	LastSuccessAt  *time.Time `json:"last_success_at"`
	LastSnapshotID string     `json:"last_snapshot_id"`
}

type hostView struct {
	FQDN            string `json:"fqdn"`
	State           string `json:"state"`
	Progress        string `json:"progress"`
	Online          bool   `json:"online"`
	InSync          bool   `json:"in_sync"`
	DesiredRevision int64  `json:"desired_revision"`
	Agent           *struct {
		AppliedRevision    int64  `json:"applied_revision"`
		ResticVersion      string `json:"restic_version"`
		CredentialsVersion int    `json:"credentials_version"`
	} `json:"agent"`
	Storage *struct {
		Findings []struct {
			Code string `json:"code"`
		} `json:"findings"`
	} `json:"storage"`
	Jobs []struct {
		ID     string     `json:"id"`
		Status *jobStatus `json:"status"`
	} `json:"jobs"`
}

func (e *env) host(fqdn string) hostView {
	e.t.Helper()
	var h hostView
	out, err := e.cliErr("host", "show", fqdn)
	if err != nil {
		return h // the server may be down in some tests
	}
	_ = json.Unmarshal(out, &h)
	return h
}

// hostRaw is the API's host view as a plain map, for fields hostView does not carry.
func (e *env) hostRaw(fqdn string) map[string]any {
	e.t.Helper()
	var m map[string]any
	must(e.t, json.Unmarshal(e.cli("host", "show", fqdn), &m))
	return m
}

func (e *env) waitProgress(fqdn, progress string, d time.Duration) {
	e.t.Helper()
	waitFor(e.t, fqdn+" "+progress, d, func() bool { return e.host(fqdn).Progress == progress })
}

type escrowHost struct {
	FQDN              string `json:"fqdn"`
	Repository        string `json:"repository"`
	RepoPassword      string `json:"repo_password"`
	S3AccessKeyID     string `json:"s3_access_key_id"`
	S3SecretAccessKey string `json:"s3_secret_access_key"`
}

// escrow exports the escrow bundle through the CLI (root on the server only).
func (e *env) escrow() map[string]escrowHost {
	e.t.Helper()
	p := filepath.Join(e.dir, "escrow-"+randHex(4)+".json")
	e.cli("escrow", "export", "--to", p)
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		e.t.Fatalf("escrow bundle must be 0600: %v %v", st, err)
	}
	var b struct{ Hosts []escrowHost }
	raw, _ := os.ReadFile(p)
	if err := json.Unmarshal(raw, &b); err != nil {
		e.t.Fatal(err)
	}
	out := map[string]escrowHost{}
	for _, h := range b.Hosts {
		out[h.FQDN] = h
	}
	return out
}

// resticFor opens a host's repository with plain restic, as someone recovering from the escrow
// bundle would. withHostKey uses the host's own S3 key instead of the storage admin's.
func (e *env) resticFor(h escrowHost, withHostKey bool) restic.Runner {
	pf := filepath.Join(e.dir, "pass-"+randHex(4))
	must(e.t, os.WriteFile(pf, []byte(h.RepoPassword), 0o600))
	ak, sk := s3User, s3Pass
	if withHostKey {
		ak, sk = h.S3AccessKeyID, h.S3SecretAccessKey
	}
	return restic.Runner{Bin: resticBin, Repo: h.Repository, PasswordFile: pf, CacheDir: filepath.Join(e.dir, "restic-cache"),
		Env: []string{"AWS_ACCESS_KEY_ID=" + ak, "AWS_SECRET_ACCESS_KEY=" + sk, "AWS_DEFAULT_REGION=us-east-1"}}
}

// metrics scrapes a Prometheus text endpoint into "name{labels}" → value.
func metrics(t *testing.T, u string) map[string]float64 {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("scrape %s: %v", u, err)
	}
	defer resp.Body.Close()
	return parseMetrics(resp.Body)
}

func parseMetrics(r io.Reader) map[string]float64 {
	out := map[string]float64{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndex(line, " ")
		if v, err := strconv.ParseFloat(line[i+1:], 64); err == nil {
			out[line[:i]] = v
		}
	}
	return out
}

func snapshots(t *testing.T, r restic.Runner) int {
	t.Helper()
	s, err := r.Snapshots(context.Background())
	if err != nil {
		t.Fatalf("restic snapshots: %v", err)
	}
	return len(s)
}

func requireRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: ownership, trusted.* xattrs and ACLs (make e2e runs as root in a container)")
	}
}
