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
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/minio/minio-go/v7"
	"go.yaml.in/yaml/v3"

	"github.com/kabsdk/holdfast/internal/pin"
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
	// The login and UI tests (M4) need a real OIDC provider and a browser: test/e2e/authelia/setup.sh's
	// state directory, and a Chrome DevTools URL. Without them they skip.
	oidcDir    = os.Getenv("HOLDFAST_OIDC_DIR")
	browserURL = os.Getenv("HOLDFAST_BROWSER")
)

// The UI listener's fixed address: Authelia's client has its redirect URI.
const (
	uiAddr    = "127.0.0.1:58443"
	uiURL     = "https://ui.holdfast.test:58443"
	issuerURL = "https://auth.holdfast.test:59091"
)

// oidcPassword returns a test user's password from Authelia's state.
func oidcPassword(t *testing.T, user string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(oidcDir, "users.env"))
	must(t, err)
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "HOLDFAST_E2E_PASS_"+strings.ToUpper(user)+"="); ok {
			return strings.TrimSpace(v)
		}
	}
	t.Fatalf("no password for %s", user)
	return ""
}

func TestMain(m *testing.M) {
	for name, v := range map[string]string{"HOLDFAST_RESTIC": resticBin, "HOLDFAST_MC": mcBin, "HOLDFAST_S3_ENDPOINT": s3Endpoint,
		"HOLDFAST_S3_ROOT_USER": s3User, "HOLDFAST_S3_ROOT_PASSWORD": s3Pass, "HOLDFAST_TEST_PG": os.Getenv("HOLDFAST_TEST_PG")} {
		if v == "" {
			fmt.Printf("SKIP: the e2e tests need %s (run `make e2e`)\n", name)
			os.Exit(0)
		}
	}
	repoRoot, _ = filepath.Abs("../..")
	if oidcDir != "" {
		// The login tests use real names, as browsers and Authelia's cookies need: both point here.
		if b, _ := os.ReadFile("/etc/hosts"); !strings.Contains(string(b), "ui.holdfast.test") {
			f, err := os.OpenFile("/etc/hosts", os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				panic(err)
			}
			_, _ = f.WriteString("127.0.0.1 ui.holdfast.test auth.holdfast.test\n")
			f.Close()
		}
	}
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
	// escrowKey is the offline age key: server.yaml names its recipient, so the server writes each host's
	// escrow file (decision 036), and only this key reads them.
	escrowKey *age.X25519Identity
	maintKey  string // a vault's maintenance key's access key ID (vault_test.go)
	// gwKey is the gateway key's access key ID, and gwPolicy the name of its policy (decision 041): hosts hold no
	// S3 key, the server's gateway uses this one for all of them.
	gwKey, gwPolicy string
	// offBucket and offKey are the off-site bucket and its key's access key ID, with envOpts.Offsite.
	offBucket, offKey string
	// tls is the agent listener's certificate: always TLS, since agents are known by their own certificates
	// (decision 041). Agents pin tls.pin.
	tls *tlsFiles
}

// envOpts change one test environment.
type envOpts struct {
	S3Endpoint    string    // the endpoint the server uses (a proxy in front of MinIO); default: MinIO
	TLS           *tlsFiles // the agent listener's certificate; default: a new one (serverCert)
	MaintPolicy   string    // give the server a maintenance key with this policy instead of docs/storage.md's
	GatewayPolicy string    // give the server a gateway key with this policy instead of docs/storage.md's
	OIDC          bool      // serve the UI listener on uiAddr, with login through the e2e Authelia (M4)
	UI            bool      // serve the UI listener on uiAddr with no identity provider: root logs in with ui-login
	Webhook       string    // notify.webhook_url and heartbeat_url: the server's alerts go there
	Offsite       bool      // a second bucket, and offsite: in server.yaml (decision 041)
	DailyChecks   bool      // maintenance.check daily: agents check their repository after a backup (decision 030)
}

func newEnv(t *testing.T) *env { return newEnvWith(t, envOpts{}) }

// insecure is for what needs no client certificate and checks nothing of the server: /healthz and /metrics.
var insecure = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}

func newEnvWith(t *testing.T, o envOpts) *env {
	t.Helper()
	dir := t.TempDir()
	if o.TLS == nil {
		o.TLS = serverCert(t)
	}
	e := &env{t: t, dir: dir, port: freePort(t), sock: filepath.Join(dir, "run", "api.sock"), bucket: "hf-" + randHex(6), client: insecure, tls: o.TLS}
	e.url = fmt.Sprintf("https://127.0.0.1:%d", e.port)
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
		policy = maintPolicy(e.bucket)
	}
	mak, msk := e.makeUser(policy)
	must(t, os.WriteFile(maint, []byte("AWS_ACCESS_KEY_ID="+mak+"\nAWS_SECRET_ACCESS_KEY="+msk+"\n"), 0o600))
	// The gateway key, with docs/storage.md's policy unless the test gives another.
	gwFile := filepath.Join(dir, "gateway.env")
	if o.GatewayPolicy == "" {
		o.GatewayPolicy = e.gatewayPolicy()
	}
	gak, gsk, gpol := e.makeUserWithPolicy(o.GatewayPolicy)
	e.gwKey, e.gwPolicy = gak, gpol
	must(t, os.WriteFile(gwFile, []byte("AWS_ACCESS_KEY_ID="+gak+"\nAWS_SECRET_ACCESS_KEY="+gsk+"\n"), 0o600))
	id, err := age.GenerateX25519Identity()
	must(t, err)
	e.escrowKey = id
	stor := map[string]any{"endpoint": endpoint, "bucket": e.bucket, "region": "us-east-1", "maintenance_credentials_file": maint,
		"gateway_credentials_file": gwFile}
	set := map[string]any{
		"listen": fmt.Sprintf("127.0.0.1:%d", e.port), "api_socket": e.sock, "public_url": e.url,
		"database_dsn": dsn, "credentials_key_file": filepath.Join(dir, "credentials.key"), "state_dir": filepath.Join(dir, "server"),
		"restic_bin": resticBin, "default_zone": "UTC", "node_name": "e2e",
		"storage": stor,
		// Tests run `holdfast check` and `holdfast retention run` themselves; a background check would race
		// their lock counts, and a daily retention run their snapshot counts.
		"maintenance": map[string]any{"check": "off", "check_cycle_days": 30, "retention": "off"},
		"escrow":      map[string]any{"age_recipient": id.Recipient().String()},
		"tls":         map[string]any{"cert_file": o.TLS.cert, "key_file": o.TLS.key},
		"ca_sha256":   o.TLS.pin,
	}
	if o.DailyChecks {
		set["maintenance"].(map[string]any)["check"] = "daily"
	}
	if o.Offsite {
		// The off-site bucket, made as the primary one is, with a key of its own.
		e.offBucket = e.bucket + "-off"
		mc(t, "mb", "--quiet", "hf/"+e.offBucket)
		mc(t, "version", "enable", "hf/"+e.offBucket)
		mc(t, "ilm", "rule", "add", "--noncurrent-expire-days", "30", "hf/"+e.offBucket)
		oak, osk := e.makeUser(maintPolicy(e.offBucket))
		e.offKey = oak
		offFile := filepath.Join(dir, "offsite-maintenance.env")
		must(t, os.WriteFile(offFile, []byte("AWS_ACCESS_KEY_ID="+oak+"\nAWS_SECRET_ACCESS_KEY="+osk+"\n"), 0o600))
		set["offsite"] = map[string]any{"endpoint": s3Endpoint, "bucket": e.offBucket, "region": "us-east-1", "maintenance_credentials_file": offFile, "poll": "2s"}
	}
	if o.OIDC || o.UI {
		if oidcDir == "" { // the UI's certificate is the one made for the e2e Authelia, for *.holdfast.test
			t.Skip("needs the e2e OIDC provider (HOLDFAST_OIDC_DIR; make e2e starts it)")
		}
		set["ui"] = map[string]any{"listen": uiAddr, "public_url": uiURL,
			"tls": map[string]any{"cert_file": filepath.Join(oidcDir, "tls", "public.crt"), "key_file": filepath.Join(oidcDir, "tls", "private.pem")}}
	}
	if o.Webhook != "" {
		set["notify"] = map[string]any{"webhook_url": o.Webhook, "heartbeat_url": o.Webhook}
	}
	if o.OIDC {
		// The client secret goes into a root-only file, as in production.
		secret := filepath.Join(dir, "oidc-client-secret")
		b, err := os.ReadFile(filepath.Join(oidcDir, "client-secret"))
		must(t, err)
		must(t, os.WriteFile(secret, b, 0o600))
		set["auth"] = map[string]any{"oidc": map[string]any{"issuer": issuerURL, "client_id": "holdfast", "client_secret_file": secret,
			"ca_file": filepath.Join(oidcDir, "ca", "ca.public.crt")}}
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

func (e *env) startServer() { e.startServerWith(nil) }

// restartServer stops the server and starts it again, with extra environment variables and flags.
func (e *env) restartServer(extraEnv []string, flags ...string) {
	e.t.Helper()
	e.srv.kill(syscall.SIGTERM)
	e.startServerWith(extraEnv, flags...)
}

func (e *env) startServerWith(extraEnv []string, flags ...string) {
	e.srv = start(e.t, e.logFile("server"), append(slices.Clone(e.srvEnv), extraEnv...), "holdfast-server", append(flags, "-config", e.cfgPath)...)
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

// offsite runs the holdfast CLI on the off-site copies (holdfast --offsite).
func (e *env) offsite(args ...string) []byte {
	e.t.Helper()
	return e.cli(append([]string{"--offsite"}, args...)...)
}

func (e *env) cliErr(args ...string) ([]byte, error) {
	cmd := exec.Command(filepath.Join(binDir, "holdfast"), append([]string{"--socket", e.sock, "--json"}, args...)...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.Bytes(), err
}

// cliText runs the CLI for what a person reads: its tables, not the API's JSON.
func (e *env) cliText(args ...string) []byte {
	e.t.Helper()
	cmd := exec.Command(filepath.Join(binDir, "holdfast"), append([]string{"--socket", e.sock}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("holdfast %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// gatewayPolicy is the example gateway policy of docs/storage.md, for this environment's bucket (decision 041): read
// and write under repos/, delete only lock files, never a version.
func (e *env) gatewayPolicy() string {
	b := e.bucket
	return `{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["s3:ListBucket","s3:GetBucketLocation"],"Resource":["arn:aws:s3:::` + b + `"]},
 {"Effect":"Allow","Action":["s3:GetObject","s3:PutObject"],"Resource":["arn:aws:s3:::` + b + `/repos/*"]},
 {"Effect":"Allow","Action":["s3:DeleteObject"],"Resource":["arn:aws:s3:::` + b + `/repos/*/locks/*"]},
 {"Effect":"Deny","Action":["s3:DeleteObjectVersion"],"Resource":["arn:aws:s3:::` + b + `/*"]}]}`
}

// setGatewayPolicy gives the gateway key another policy, as an admin would at the S3 server.
func (e *env) setGatewayPolicy(policy string) {
	e.t.Helper()
	name := "hf-" + randHex(6)
	pf := filepath.Join(e.dir, name+".json")
	must(e.t, os.WriteFile(pf, []byte(policy), 0o600))
	mc(e.t, "admin", "policy", "create", "hf", name, pf)
	mc(e.t, "admin", "policy", "attach", "hf", name, "--user", e.gwKey)
	mc(e.t, "admin", "policy", "detach", "hf", e.gwPolicy, "--user", e.gwKey)
	e.gwPolicy = name
}

// maintPolicy is the example maintenance policy of docs/storage.md, for a bucket.
func maintPolicy(b string) string {
	return `{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["s3:ListBucket","s3:ListBucketVersions","s3:GetBucketLocation","s3:GetBucketVersioning","s3:GetLifecycleConfiguration"],"Resource":["arn:aws:s3:::` + b + `"]},
 {"Effect":"Allow","Action":["s3:GetObject","s3:GetObjectVersion","s3:PutObject","s3:DeleteObject"],"Resource":["arn:aws:s3:::` + b + `/repos/*"]},
 {"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:DeleteObject"],"Resource":["arn:aws:s3:::` + b + `/escrow/*"]},
 {"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:DeleteObject"],"Resource":["arn:aws:s3:::` + b + `/meta/*"]},
 {"Effect":"Deny","Action":["s3:DeleteObjectVersion"],"Resource":["arn:aws:s3:::` + b + `/*"]}]}`
}

// makeUser makes a MinIO user with its own policy, as an admin would with the S3 server's own tools.
// Test-only: mc takes the secret as an argument.
func (e *env) makeUser(policy string) (accessKey, secret string) {
	accessKey, secret, _ = e.makeUserWithPolicy(policy)
	return accessKey, secret
}

func (e *env) makeUserWithPolicy(policy string) (accessKey, secret, policyName string) {
	e.t.Helper()
	accessKey, secret = "HK"+strings.ToUpper(randHex(8)), randHex(20)
	name := "hf-" + randHex(6)
	pf := filepath.Join(e.dir, name+".json")
	must(e.t, os.WriteFile(pf, []byte(policy), 0o600))
	mc(e.t, "admin", "user", "add", "hf", accessKey, secret)
	mc(e.t, "admin", "policy", "create", "hf", name, pf)
	mc(e.t, "admin", "policy", "attach", "hf", name, "--user", accessKey)
	return accessKey, secret, name
}

// testAccept accepts the check warnings that test jobs trip: they keep a few snapshots on short schedules,
// which the checks rightly warn about for real jobs (spec §4.5). TestChecks tests the checks themselves.
const testAccept = "--accept=R006,R101,R102,R103,S002"

// addHost adds a host, then its one job, through the CLI, as people do, and returns the enrollment
// code. A host needs no S3 key (decision 041). Unless jobArgs say otherwise, the job accepts testAccept.
func (e *env) addHost(fqdn, jobID string, jobArgs ...string) string {
	e.t.Helper()
	var res struct {
		Enrollment struct{ Code string } `json:"enrollment"`
		Warning    string                `json:"warning"`
	}
	out := e.cli("agent", "add", fqdn)
	if err := json.Unmarshal(out, &res); err != nil || res.Enrollment.Code == "" {
		e.t.Fatalf("host add: %v %s", err, out)
	}
	if res.Warning != "" {
		e.t.Fatalf("host add warned: %s", res.Warning)
	}
	args := []string{"job", "set", fqdn, jobID}
	if !slices.ContainsFunc(jobArgs, func(a string) bool { return strings.HasPrefix(a, "--accept") }) {
		args = append(args, testAccept)
	}
	e.cli(append(args, jobArgs...)...)
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

// enrollAgentWith enrolls through the given server URL (an impostor, say) with extra flags, and
// returns the error instead of failing, for tests of refused enrollments. The agent pins this
// environment's server unless extra gives --ca-sha256.
func (e *env) enrollAgentWith(fqdn, code, server string, extra ...string) (*agentProc, []byte, error) {
	return e.enrollWithBinary(filepath.Join(binDir, "holdfast-agent"), fqdn, code, server, extra...)
}

func (e *env) enrollWithBinary(bin, fqdn, code, server string, extra ...string) (*agentProc, []byte, error) {
	dir := filepath.Join(e.dir, "agent-"+strings.Split(fqdn, ".")[0])
	must(e.t, os.MkdirAll(dir, 0o700))
	a := &agentProc{dir: dir, cfgPath: filepath.Join(dir, "agent.yaml")}
	args := []string{"enroll", "--server", server, "--name", fqdn, "--code", code,
		"--config", a.cfgPath, "--state-dir", filepath.Join(dir, "state"), "--credentials-dir", filepath.Join(dir, "credentials"),
		"--restic", resticBin, "--poll-timeout", "2s", "--tick", "250ms"}
	if !slices.Contains(extra, "--ca-sha256") {
		args = append(args, "--ca-sha256", e.tls.pin)
	}
	out, err := exec.Command(bin, append(args, extra...)...).CombinedOutput()
	return a, out, err
}

// identity is an enrolled agent's own key and certificate.
func (a *agentProc) identity(t *testing.T) *tls.Certificate {
	t.Helper()
	id, err := pin.LoadIdentity(filepath.Join(a.dir, "state", "agent.pem"))
	must(t, err)
	return id
}

// agentClient is a client that connects as an enrolled agent: with its certificate, pinning the server.
func (e *env) agentClient(a *agentProc) *http.Client {
	e.t.Helper()
	c, err := pin.Client(e.url, e.tls.pin, a.identity(e.t))
	must(e.t, err)
	return c
}

// pointAgentAt changes the server an enrolled agent talks to, as editing its agent.yaml would.
func pointAgentAt(t *testing.T, a *agentProc, from, to string) {
	t.Helper()
	b, err := os.ReadFile(a.cfgPath)
	must(t, err)
	if !strings.Contains(string(b), from) {
		t.Fatalf("%s does not name %s:\n%s", a.cfgPath, from, b)
	}
	must(t, os.WriteFile(a.cfgPath, []byte(strings.ReplaceAll(string(b), from, to)), 0o600))
}

// event is one run as the API returns it.
type event struct {
	ID           string            `json:"id"`
	Kind         string            `json:"kind"`
	JobID        string            `json:"job_id"`
	CommandID    string            `json:"command_id"`
	Status       string            `json:"status"`
	ScheduledFor *time.Time        `json:"scheduled_for"`
	Started      time.Time         `json:"started"`
	Finished     time.Time         `json:"finished"`
	Revision     int64             `json:"revision"`
	SnapshotID   string            `json:"snapshot_id"`
	Error        string            `json:"error"`
	Detail       map[string]string `json:"detail"`
}

// events returns a host's runs, newest first, through the CLI.
func (e *env) events(fqdn string) []event {
	e.t.Helper()
	var evs []event
	out, err := e.cliErr("agent", "events", fqdn)
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
//
// The server knows an agent by its certificate (decision 041), which a proxy cannot pass on. So this one
// serves the server's own certificate, which the agent pins, and connects to the server as the agent,
// with the agent's own key: the agent must be enrolled before it is pointed here (pointAgentAt).
type dropProxy struct {
	url       string
	remaining atomic.Int32
	dropped   atomic.Int32
}

func newDropProxy(t *testing.T, e *env, a *agentProc, n int32) *dropProxy {
	u, _ := url.Parse(e.url)
	rp := httputil.NewSingleHostReverseProxy(u)
	rp.Transport = e.agentClient(a).Transport
	p := &dropProxy{}
	p.remaining.Store(n)
	cert, err := tls.LoadX509KeyPair(e.tls.cert, e.tls.key)
	must(t, err)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
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
	Snapshots       int    `json:"snapshots"` // in the catalogue
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
		ID       string     `json:"id"`
		Status   *jobStatus `json:"status"`
		InBucket *bool      `json:"in_bucket"`
		Stored   *struct {
			Bytes      int64     `json:"bytes"`
			MeasuredAt time.Time `json:"measured_at"`
		} `json:"stored"`
	} `json:"jobs"`
	// M5: the escrow file, the repository's size, a rotation not finished, decommission.
	Escrow struct {
		Current bool   `json:"current"`
		Error   string `json:"error"`
	} `json:"escrow"`
	Size *struct {
		Current    int64 `json:"current_bytes"`
		Noncurrent int64 `json:"noncurrent_bytes"`
		Objects    int   `json:"objects"`
	} `json:"size"`
	RepoKeyRetiring string `json:"repo_key_retiring"`
	Decommission    *struct {
		DueAt   time.Time  `json:"due_at"`
		DoneAt  *time.Time `json:"done_at"`
		Objects int        `json:"objects_deleted"`
		GoneAt  *time.Time `json:"really_gone_at"`
	} `json:"decommission"`
	Problems []problem `json:"problems"`
}

type problem struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
}

func (e *env) host(fqdn string) hostView {
	e.t.Helper()
	var h hostView
	out, err := e.cliErr("agent", "show", fqdn)
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
	must(e.t, json.Unmarshal(e.cli("agent", "show", fqdn), &m))
	return m
}

func (e *env) waitProgress(fqdn, progress string, d time.Duration) {
	e.t.Helper()
	waitFor(e.t, fqdn+" "+progress, d, func() bool { return e.host(fqdn).Progress == progress })
}

type escrowHost struct {
	FQDN         string `json:"fqdn"`
	Repository   string `json:"repository"`
	RepoPassword string `json:"repo_password"`
}

// escrow reads every host's escrow file from the bucket, escrow/<fqdn>.age, and decrypts it with the recovery key, as
// someone recovering without Holdfast would (decision 036: there is no plaintext bundle).
func (e *env) escrow() map[string]escrowHost {
	e.t.Helper()
	out := map[string]escrowHost{}
	for o := range s3(e.t).ListObjects(context.Background(), e.bucket, minio.ListObjectsOptions{Prefix: "escrow/"}) {
		must(e.t, o.Err)
		fqdn, ok := strings.CutSuffix(strings.TrimPrefix(o.Key, "escrow/"), ".age")
		if !ok {
			continue
		}
		f := e.escrowFile(fqdn)
		out[f.FQDN] = escrowHost{FQDN: f.FQDN, Repository: f.Repository, RepoPassword: f.RepoPassword}
	}
	return out
}

// resticFor opens a host's repository with plain restic, straight from S3 with the storage admin's key, as
// someone recovering from the escrow files would.
func (e *env) resticFor(h escrowHost) restic.Runner {
	pf := filepath.Join(e.dir, "pass-"+randHex(4))
	must(e.t, os.WriteFile(pf, []byte(h.RepoPassword), 0o600))
	return restic.Runner{Bin: resticBin, Repo: h.Repository, PasswordFile: pf, CacheDir: filepath.Join(e.dir, "restic-cache"),
		Env: []string{"AWS_ACCESS_KEY_ID=" + s3User, "AWS_SECRET_ACCESS_KEY=" + s3Pass, "AWS_DEFAULT_REGION=us-east-1"}}
}

// resticAsAgent is restic as root on an enrolled host would run it by hand: through the gateway, with the agent's
// own certificate and the pinned server certificate the agent saved (decision 041).
func (e *env) resticAsAgent(a *agentProc, h escrowHost) restic.Runner {
	pf := filepath.Join(e.dir, "pass-"+randHex(4))
	must(e.t, os.WriteFile(pf, []byte(h.RepoPassword), 0o600))
	return restic.Runner{Bin: resticBin, Repo: "rest:" + e.url + "/gateway/v1/" + h.FQDN + "/", PasswordFile: pf,
		CacheDir: filepath.Join(e.dir, "restic-cache-agent"), CACert: filepath.Join(a.dir, "state", "server-ca.pem"),
		TLSClientCert: filepath.Join(a.dir, "state", "agent.pem")}
}

// metrics scrapes a Prometheus text endpoint into "name{labels}" → value. /metrics needs no client
// certificate.
func metrics(t *testing.T, u string) map[string]float64 {
	t.Helper()
	resp, err := insecure.Get(u)
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
