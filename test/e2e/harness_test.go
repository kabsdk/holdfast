// Package e2e runs the real binaries against real restic, a real PostgreSQL and a real S3 server
// (MinIO), with real process kills. `make e2e` provides all of it and runs the suite as root in a
// privileged container, which the metadata tests need (ownership, trusted.* xattrs, ACLs).
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	cfgPath string
	srv     *proc
	srvEnv  []string
	logs    []string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{t: t, dir: dir, port: freePort(t), sock: filepath.Join(dir, "run", "api.sock"), bucket: "hf-" + randHex(6)}
	e.url = fmt.Sprintf("http://127.0.0.1:%d", e.port)
	mc(t, "mb", "--quiet", "hf/"+e.bucket)
	mc(t, "version", "enable", "hf/"+e.bucket)
	dsn := testpg.DSN(t)
	keygen := exec.Command(filepath.Join(binDir, "holdfast-server"), "keygen", filepath.Join(dir, "credentials.key"))
	if out, err := keygen.CombinedOutput(); err != nil {
		t.Fatalf("keygen: %v %s", err, out)
	}
	maint := filepath.Join(dir, "maintenance.env")
	must(t, os.WriteFile(maint, []byte("AWS_ACCESS_KEY_ID="+s3User+"\nAWS_SECRET_ACCESS_KEY="+s3Pass+"\n"), 0o600))
	set := map[string]any{
		"listen": fmt.Sprintf("127.0.0.1:%d", e.port), "api_socket": e.sock, "public_url": e.url,
		"database_dsn": dsn, "credentials_key_file": filepath.Join(dir, "credentials.key"), "state_dir": filepath.Join(dir, "server"),
		"restic_bin": resticBin, "default_zone": "UTC", "node_name": "e2e",
		"storage": map[string]any{"endpoint": s3Endpoint, "bucket": e.bucket, "region": "us-east-1", "maintenance_credentials_file": maint,
			"provisioner_command": []string{"bash", filepath.Join(repoRoot, "test", "e2e", "provision-minio.sh")}},
		"maintenance": map[string]any{"check_cycle_days": 30},
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
		r, err := http.Get(e.url + "/healthz")
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

// addHost adds a host with one job through the CLI and returns the enrollment code.
func (e *env) addHost(fqdn, jobID string, jobArgs ...string) string {
	e.t.Helper()
	var res struct {
		Enrollment struct{ Code string } `json:"enrollment"`
		Warning    string                `json:"warning"`
	}
	out := e.cli(append([]string{"host", "add", fqdn, "--job", jobID}, jobArgs...)...)
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

// enrollAgent enrolls an agent for the host; startAgent then runs it.
func (e *env) enrollAgent(fqdn, code string) *agentProc {
	e.t.Helper()
	dir := filepath.Join(e.dir, "agent-"+strings.Split(fqdn, ".")[0])
	must(e.t, os.MkdirAll(dir, 0o700))
	a := &agentProc{dir: dir, cfgPath: filepath.Join(dir, "agent.yaml")}
	cmd := exec.Command(filepath.Join(binDir, "holdfast-agent"), "enroll", "--server", e.url, "--fqdn", fqdn, "--code", code,
		"--config", a.cfgPath, "--state-dir", filepath.Join(dir, "state"), "--credentials-dir", filepath.Join(dir, "credentials"),
		"--restic", resticBin, "--poll-timeout", "2s", "--tick", "250ms")
	if out, err := cmd.CombinedOutput(); err != nil {
		e.t.Fatalf("agent enroll: %v\n%s", err, out)
	}
	return a
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
		AppliedRevision int64  `json:"applied_revision"`
		ResticVersion   string `json:"restic_version"`
	} `json:"agent"`
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
