package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Decision 052: Holdfast runs no command on a host. An agent.yaml from before 0.6.0 with dumps still loads, so the
// host's other jobs keep running, and the agent warns that dumps are ignored.
func TestLegacyDumpsAreIgnored(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "agent.yaml")
	body := "server: https://h.example.test\nfqdn: web01.example.com\ndumps:\n  postgresql:\n    command: [/usr/bin/pg_dumpall]\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("an agent.yaml with dumps must still load: %v", err)
	}
	if w := c.Warnings(); len(w) != 1 || !strings.Contains(w[0], "dumps is ignored") {
		t.Fatalf("warnings: %v", w)
	}

	if err := os.WriteFile(p, []byte("server: https://h.example.test\nfqdn: web01.example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := LoadConfig(p); err != nil || len(c.Warnings()) != 0 {
		t.Fatalf("without dumps, no warning: %v %v", c.Warnings(), err)
	}
}
