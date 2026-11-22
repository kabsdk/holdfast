package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Plan-m5 §3: an upgrade from the previous agent to this one, and an agent too old for the server.
//
// The previous agent is built from HOLDFAST_PREV_AGENT_REF, by default M4's last commit: the agent before
// M5, which sends no protocol version.
const defaultPrevAgentRef = "c69b595"

// buildPrevAgent builds the previous agent from git into binDir, as holdfast-agent-prev.
func buildPrevAgent(t *testing.T) string {
	t.Helper()
	ref := os.Getenv("HOLDFAST_PREV_AGENT_REF")
	if ref == "" {
		ref = defaultPrevAgentRef
	}
	src := t.TempDir()
	// The checkout belongs to another user in the e2e container; safe.directory lets git read it.
	archive := exec.Command("sh", "-c", `git -c safe.directory='*' -C "$1" archive "$2" | tar -x -C "$3"`, "sh", repoRoot, ref, src)
	if out, err := archive.CombinedOutput(); err != nil {
		t.Fatalf("git archive %s: %v\n%s", ref, err, out)
	}
	bin := filepath.Join(binDir, "holdfast-agent-prev")
	build := exec.Command("go", "build", "-ldflags", "-X github.com/kabsdk/holdfast/internal/agent.Version=prev", "-o", bin, "./cmd/holdfast-agent")
	build.Dir = src
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=-buildvcs=false")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the agent at %s: %v\n%s", ref, err, out)
	}
	return bin
}

func (e *env) agentFacts(fqdn string) (version string, protocol int) {
	e.t.Helper()
	raw := e.hostRaw(fqdn)
	a, _ := raw["agent"].(map[string]any)
	v, _ := a["agent_version"].(string)
	p, _ := a["protocol"].(float64)
	return v, int(p)
}

func TestAgentUpgradeAndTooOld(t *testing.T) {
	prev := buildPrevAgent(t)
	src := smallSource(t)
	e := newEnv(t)
	const fqdn = "upg01.example.com"
	code := e.addHost(fqdn, "data", "--path", src, "--every", "1m")

	// The previous agent enrolls and backs up.
	dir := filepath.Join(e.dir, "agent-upg01")
	must(t, os.MkdirAll(dir, 0o700))
	a := &agentProc{dir: dir, cfgPath: filepath.Join(dir, "agent.yaml")}
	out, err := exec.Command(prev, "enroll", "--server", e.url, "--fqdn", fqdn, "--code", code,
		"--config", a.cfgPath, "--state-dir", filepath.Join(dir, "state"), "--credentials-dir", filepath.Join(dir, "credentials"),
		"--restic", resticBin, "--poll-timeout", "2s", "--tick", "250ms").CombinedOutput()
	if err != nil {
		t.Fatalf("the previous agent's enroll: %v\n%s", err, out)
	}
	old := start(t, e.logFile("agent-upg01-prev"), append(os.Environ(), "TZ=UTC"), "holdfast-agent-prev", "run", "--config", a.cfgPath)
	e.waitProgress(fqdn, "first_backup_done", 2*time.Minute)
	if v, p := e.agentFacts(fqdn); v != "prev" || p != 0 {
		t.Fatalf("the previous agent reports %q, protocol %d", v, p)
	}

	// Upgraded in place, as the package's restart does: the same config, credentials and state.
	old.kill(syscall.SIGTERM)
	e.runAgent(a)
	waitFor(t, "the new agent to report", time.Minute, func() bool { v, p := e.agentFacts(fqdn); return v == "dev" && p == 1 })
	before := countStatus(e.events(fqdn), "data", "success")
	waitFor(t, "a backup by the new agent", 3*time.Minute, func() bool { return countStatus(e.events(fqdn), "data", "success") > before })
	if n := countStatus(e.events(fqdn), "data", "interrupted"); n != 0 {
		t.Fatalf("the upgrade interrupted %d runs", n)
	}

	// The server now needs protocol 2, so this agent is too old: it is told so, and the host says so. It
	// keeps backing up on its cached config, and the server still takes its reports.
	e.restartServer([]string{"HOLDFAST_TEST_MIN_PROTOCOL=2"}, "-test")
	waitFor(t, "the too-old problem", time.Minute, func() bool { return hasProblem(e.host(fqdn), "agent_too_old") })
	waitFor(t, "the agent to log it", time.Minute, func() bool {
		b, _ := os.ReadFile(e.logFile(filepath.Base(a.dir)))
		return strings.Contains(string(b), "the server needs a newer agent")
	})
	if m := metrics(t, e.url+"/metrics"); m[`holdfast_agent_too_old{host="`+fqdn+`"}`] != 1 {
		t.Fatal("the too-old metric is missing")
	}
	before = countStatus(e.events(fqdn), "data", "success")
	waitFor(t, "a backup while too old", 3*time.Minute, func() bool { return countStatus(e.events(fqdn), "data", "success") > before })

	// Back on a server that accepts it, it is fine again.
	e.restartServer(nil)
	waitFor(t, "the problem to go", time.Minute, func() bool { return !hasProblem(e.host(fqdn), "agent_too_old") })
}
