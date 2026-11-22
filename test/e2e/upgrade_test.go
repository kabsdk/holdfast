package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kabsdk/holdfast/internal/proto"
)

// Plan-m5 §3: an upgrade from the previous agent to this one, and an agent too old for the server.
//
// The previous agent is built from HOLDFAST_PREV_AGENT_REF, by default M4's last commit: an agent with a bearer
// token and no certificate of its own. From M8 on (decision 041) such an agent is not upgraded in place but enrolled
// again, so that is what this test checks. Once a release with agent certificates exists, the default becomes it,
// and the test upgrades in place again.
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

	// An agent from before decision 041 has no certificate of its own, so this server cannot know it: its
	// enrollment is refused, with a reason, and the code stays valid. Every agent enrolls again on M8.
	if _, out, err := e.enrollWithBinary(prev, fqdn, code, e.url); err == nil || !strings.Contains(string(out), "certificate") {
		t.Fatalf("an agent without a certificate of its own must be refused, and told why: %v\n%s", err, out)
	}

	// The new agent, in the same directories, enrolls with the same code and backs up.
	a := e.startAgent(fqdn, code)
	e.waitProgress(fqdn, "first_backup_done", 2*time.Minute)
	waitFor(t, "the new agent to report", time.Minute, func() bool { v, p := e.agentFacts(fqdn); return v == "dev" && p == proto.Protocol })
	before := countStatus(e.events(fqdn), "data", "success")
	waitFor(t, "a scheduled backup", 3*time.Minute, func() bool { return countStatus(e.events(fqdn), "data", "success") > before })

	// The server now needs a newer protocol, so this agent is too old: it is told so, and the host says so. It
	// keeps backing up on its cached config, and the server still takes its reports.
	e.restartServer([]string{fmt.Sprintf("HOLDFAST_TEST_MIN_PROTOCOL=%d", proto.Protocol+1)}, "-test")
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
