package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"holdfast/internal/server"
)

var (
	binDir    string
	resticBin string
)

func TestMain(m *testing.M) {
	resticBin = os.Getenv("HOLDFAST_RESTIC")
	if resticBin == "" {
		if p, err := exec.LookPath("restic"); err == nil {
			resticBin = p
		}
	}
	if resticBin == "" {
		fmt.Println("SKIP: restic not found (set HOLDFAST_RESTIC)")
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "holdfast-bin-")
	if err != nil {
		panic(err)
	}
	binDir = dir
	root, _ := filepath.Abs("../..")
	build := exec.Command("go", "build", "-o", binDir+"/", "./cmd/...")
	build.Dir = root
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(binDir)
	os.Exit(code)
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// proc is a child process with its log captured to a file.
type proc struct {
	cmd *exec.Cmd
	log string
}

func start(t *testing.T, name, logPath string, args ...string) *proc {
	t.Helper()
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(binDir, name), args...)
	cmd.Stdout, cmd.Stderr = f, f
	cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(resticBin)+":"+os.Getenv("PATH"), "TZ=UTC")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &proc{cmd: cmd, log: logPath}
	t.Cleanup(func() { p.kill(syscall.SIGKILL) })
	return p
}

// kill signals the whole process group, so restic children die with SIGKILL too.
func (p *proc) kill(sig syscall.Signal) {
	if p.cmd.Process == nil || p.cmd.ProcessState != nil {
		return
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, sig)
	_, _ = p.cmd.Process.Wait()
}

func dumpLogs(t *testing.T, paths ...string) {
	for _, p := range paths {
		b, _ := os.ReadFile(p)
		t.Logf("==== %s ====\n%s", filepath.Base(p), b)
	}
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

// env bundles one server + its config for a test.
type env struct {
	t         *testing.T
	dir       string
	port      int
	url       string
	cfgPath   string
	cfg       server.Config
	srv       *proc
	adminTok  string
	logs      []string
}

func newEnv(t *testing.T, hosts []server.HostSpec) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{t: t, dir: dir, port: freePort(t), adminTok: "admin-test-token"}
	e.url = fmt.Sprintf("http://127.0.0.1:%d", e.port)
	e.cfg = server.Config{
		Listen: fmt.Sprintf("127.0.0.1:%d", e.port), StateDir: filepath.Join(dir, "server"),
		RepoBase: filepath.Join(dir, "repos"), ResticBin: resticBin, AdminToken: e.adminTok,
		CheckCycleDays: 30, Hosts: hosts,
	}
	e.cfgPath = filepath.Join(dir, "server.json")
	e.writeConfig()
	t.Cleanup(func() {
		if t.Failed() {
			dumpLogs(t, e.logs...)
		}
	})
	return e
}

func (e *env) writeConfig() {
	b, _ := json.MarshalIndent(e.cfg, "", "  ")
	if err := os.WriteFile(e.cfgPath, b, 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) startServer() {
	log := filepath.Join(e.dir, "server.log")
	e.logs = appendUnique(e.logs, log)
	e.srv = start(e.t, "holdfast-server", log, "-config", e.cfgPath)
	waitFor(e.t, "server healthz", 10*time.Second, func() bool {
		r, err := http.Get(e.url + "/healthz")
		if err != nil {
			return false
		}
		r.Body.Close()
		return r.StatusCode == 200
	})
}

func (e *env) startAgent(host, code string) *proc {
	adir := filepath.Join(e.dir, "agent-"+host)
	_ = os.MkdirAll(adir, 0o700)
	cfg := map[string]string{"server": e.url, "host": host, "enroll_code": code,
		"state_dir": filepath.Join(adir, "state"), "restic_bin": resticBin, "poll_timeout": "2s", "tick_every": "250ms"}
	b, _ := json.Marshal(cfg)
	cp := filepath.Join(adir, "agent.json")
	_ = os.WriteFile(cp, b, 0o600)
	log := filepath.Join(e.dir, "agent-"+host+".log")
	e.logs = appendUnique(e.logs, log)
	return start(e.t, "holdfast-agent", log, "-config", cp)
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

func (e *env) do(method, path string, body any) (int, []byte) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.url+path, rd)
	req.Header.Set("Authorization", "Bearer "+e.adminTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (e *env) hosts() []server.HostView {
	code, b := e.do("GET", "/api/v1/hosts", nil)
	if code != 200 {
		return nil
	}
	var hv []server.HostView
	_ = json.Unmarshal(b, &hv)
	return hv
}

func (e *env) hostView(name string) (server.HostView, bool) {
	for _, h := range e.hosts() {
		if h.Name == name {
			return h, true
		}
	}
	return server.HostView{}, false
}

// metrics scrapes a Prometheus text endpoint into "name{labels}" -> value.
func metrics(t *testing.T, url string) map[string]float64 {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("scrape %s: %v", url, err)
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
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err == nil {
			out[line[:i]] = v
		}
	}
	return out
}

func readFile(t *testing.T, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
