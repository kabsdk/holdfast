package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Decision 037: the dump runs with a clean environment, as its user, and agent.yaml is checked.
func TestDumpCommand(t *testing.T) {
	got := dumpCommand(Dump{Command: []string{"/usr/bin/pg_dumpall", "--clean"}, User: "postgres", Env: map[string]string{"PGPORT": "5433", "PGHOST": "/run/pg"}})
	want := []string{"/usr/bin/env", "-i", "PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8", "PGHOST=/run/pg", "PGPORT=5433",
		"runuser", "-u", "postgres", "--", "/usr/bin/pg_dumpall", "--clean"}
	if !slices.Equal(got, want) {
		t.Fatalf("dump command:\n got %v\nwant %v", got, want)
	}
	if got := dumpCommand(Dump{Command: []string{"/usr/bin/mariadb-dump"}, User: "root"}); slices.Contains(got, "runuser") {
		t.Fatalf("root needs no runuser: %v", got)
	}

	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "agent.yaml")
		if err := os.WriteFile(p, []byte("server: https://h.example.test\nfqdn: web01.example.com\n"+s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	c, err := LoadConfig(write("dumps:\n  postgresql:\n    command: [/usr/bin/pg_dumpall]\n    user: postgres\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Dumps["postgresql"].Filename != "postgresql.dump" || !slices.Equal(c.DumpNames(), []string{"postgresql"}) {
		t.Fatalf("defaults: %+v", c.Dumps)
	}
	for _, bad := range []string{
		"dumps:\n  pg:\n    command: [pg_dumpall]\n",                           // not an absolute path
		"dumps:\n  Bad_Name:\n    command: [/usr/bin/true]\n",                  // not a name
		"dumps:\n  pg:\n    command: [/usr/bin/true]\n    filename: ../x\n",    // not a file name
		"dumps:\n  pg:\n    command: [/usr/bin/true]\n    env: {\"A B\": x}\n", // not a variable
	} {
		if _, err := LoadConfig(write(bad)); err == nil || !strings.Contains(err.Error(), "dumps.") {
			t.Errorf("accepted %q: %v", bad, err)
		}
	}
}
