package e2e

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// The checks on config saves (spec §4.5), through the CLI:
//   - an unaccepted warning refuses job set, and says which --accept would save it;
//   - --accept saves it, and the revision and the audit log record the accepted code;
//   - an error cannot be accepted, and job check says so without saving;
//   - R104 is decided by restic on the real repository: keeping fewer of the job's snapshots is a cut, and
//     keeping more is not.
func TestChecks(t *testing.T) {
	src := smallSource(t)
	e := newEnv(t)
	const fqdn = "chk01.example.com"
	job := []string{"--path", src, "--every", "1m", "--keep-last", "10"}

	var host struct {
		Enrollment struct{ Code string } `json:"enrollment"`
	}
	must(t, json.Unmarshal(e.cli("host", "add", fqdn), &host))
	type rev struct {
		Number   int64    `json:"number"`
		Accepted []string `json:"accepted_codes"`
	}
	var revs []rev
	out, err := e.cliErr(append([]string{"job", "set", fqdn, "data"}, job...)...)
	if err == nil || !strings.Contains(string(out), "R101") || !strings.Contains(string(out), "--accept R101") {
		t.Fatalf("an unaccepted R101 must refuse job set, with the --accept that saves it: %v %s", err, out)
	}
	if must(t, json.Unmarshal(e.cli("revision", "list"), &revs)); len(revs) != 1 {
		t.Fatalf("a refused job was saved: %+v", revs)
	}

	var res struct {
		Revision int64    `json:"revision"`
		Accepted []string `json:"accepted_codes"`
	}
	must(t, json.Unmarshal(e.cli(append([]string{"job", "set", fqdn, "data", "--accept", "R101"}, job...)...), &res))
	if !slices.Equal(res.Accepted, []string{"R101"}) {
		t.Fatalf("accepted codes: %+v", res)
	}
	must(t, json.Unmarshal(e.cli("revision", "list"), &revs))
	if i := slices.IndexFunc(revs, func(r rev) bool { return r.Number == res.Revision }); len(revs) != 2 || i < 0 || !slices.Equal(revs[i].Accepted, []string{"R101"}) {
		t.Fatalf("the job's revision must record R101: %+v", revs)
	}
	var audit []auditRow
	must(t, json.Unmarshal(e.cli("audit"), &audit))
	if i := slices.IndexFunc(audit, func(a auditRow) bool { return a.Action == "job.add" }); i < 0 ||
		!slices.Contains(anyStrings(audit[i].Detail["accepted_codes"]), "R101") {
		t.Fatalf("the audit log must record R101: %+v", audit)
	}

	// An error cannot be accepted; job check says so and saves nothing.
	if out, err := e.cliErr("job", "check", fqdn, "data", "--path", src, "--daily", "02:00", "--keep-hourly", "48h", "--keep-daily", "14d", "--keep-monthly", "12mo", "--accept", "R001"); err == nil || !strings.Contains(string(out), "R001") {
		t.Fatalf("job check must report R001 and fail: %v %s", err, out)
	}
	if out, err := e.cliErr("job", "set", fqdn, "data", "--path", src, "--daily", "02:00", "--keep-hourly", "48h", "--keep-daily", "14d", "--keep-monthly", "12mo", "--accept", "R001"); err == nil || !strings.Contains(string(out), "R001") {
		t.Fatalf("an error must refuse job set, accepted or not: %v %s", err, out)
	}

	// R104, from restic: with three snapshots, keeping the newest one only is a cut.
	e.startAgent(fqdn, host.Enrollment.Code)
	waitFor(t, "three snapshots in the catalogue", 4*time.Minute, func() bool { return e.host(fqdn).Snapshots >= 3 })
	cut := []string{"job", "set", fqdn, "data", "--path", src, "--every", "1m", "--keep-last", "1", testAccept}
	if out, err := e.cliErr(cut...); err == nil || !strings.Contains(string(out), "R104") {
		t.Fatalf("keeping fewer snapshots must need R104 accepted: %v %s", err, out)
	}
	var saved struct {
		Revision int64    `json:"revision"`
		Accepted []string `json:"accepted_codes"`
	}
	must(t, json.Unmarshal(e.cli(append(cut, "--accept", "R104")...), &saved))
	if !slices.Contains(saved.Accepted, "R104") {
		t.Fatalf("R104 not recorded as accepted: %+v", saved)
	}
	// Keeping more again is not a cut.
	must(t, json.Unmarshal(e.cli("job", "set", fqdn, "data", "--path", src, "--every", "1m", "--keep-last", "10", testAccept), &saved))
	if slices.Contains(saved.Accepted, "R104") {
		t.Fatalf("keeping more is not a cut: %+v", saved)
	}
}

// A job needs only its paths: the rest comes from the default job, and a later save changes only what it gives.
// host add takes no job.
func TestJobDefaults(t *testing.T) {
	e := newEnv(t)
	const fqdn = "def01.example.com"
	if out, err := e.cliErr("host", "add", fqdn, "--job", "system"); err == nil {
		t.Fatalf("host add took a job: %s", out)
	}
	e.cli("host", "add", fqdn)
	e.cli("job", "set", fqdn, "system", "--path", "/etc")
	type job struct {
		Paths    []string       `json:"paths"`
		Schedule map[string]any `json:"schedule"`
		Rule     map[string]any `json:"retention"`
	}
	read := func() job {
		var h struct {
			Config struct {
				Hosts map[string]struct {
					Jobs map[string]job `json:"jobs"`
				} `json:"hosts"`
			} `json:"config"`
		}
		var revs []struct {
			Number int64 `json:"number"`
		}
		must(t, json.Unmarshal(e.cli("revision", "list"), &revs))
		head := int64(0)
		for _, r := range revs {
			head = max(head, r.Number)
		}
		must(t, json.Unmarshal(e.cli("revision", "show", fmt.Sprint(head)), &h))
		return h.Config.Hosts[fqdn].Jobs["system"]
	}
	j := read()
	if fmt.Sprint(j.Schedule["daily"]) != "map[times:[02:00]]" || fmt.Sprint(j.Schedule["spread"]) != "map[enabled:true minutes:20]" ||
		j.Rule["daily"] == nil || j.Rule["weekly"] == nil || j.Rule["monthly"] == nil || j.Rule["yearly"] == nil || fmt.Sprint(j.Rule["keep_last"]) != "3" {
		t.Fatalf("a new job must take the default schedule and retention: %+v", j)
	}
	// A later save changes only what it gives: the retention here; the paths and the schedule stay.
	e.cli("job", "set", fqdn, "system", "--keep-daily", "30d", "--keep-monthly", "24mo")
	j = read()
	if !slices.Equal(j.Paths, []string{"/etc"}) || fmt.Sprint(j.Schedule["daily"]) != "map[times:[02:00]]" ||
		fmt.Sprint(j.Rule["daily"]) != "map[n:30 unit:days]" || j.Rule["weekly"] != nil || fmt.Sprint(j.Rule["monthly"]) != "map[n:24 unit:months]" {
		t.Fatalf("a save with retention flags must change the retention only: %+v", j)
	}
	// A new schedule form on an existing job replaces the form, and keeps the job's spread.
	e.cli("job", "set", fqdn, "system", "--every", "12h")
	j = read()
	if j.Schedule["daily"] != nil || j.Schedule["every"] == nil || fmt.Sprint(j.Schedule["spread"]) != "map[enabled:true minutes:20]" ||
		fmt.Sprint(j.Rule["daily"]) != "map[n:30 unit:days]" {
		t.Fatalf("a new schedule form must replace the form only: %+v", j)
	}
	// A new job given a schedule takes it whole: the default's 20-minute spread is not laid over every 1m (S001).
	e.cli("job", "set", fqdn, "often", "--path", "/srv", "--every", "1m", testAccept)
}

type auditRow struct {
	Actor  string         `json:"actor"`
	Action string         `json:"action"`
	Detail map[string]any `json:"detail"`
}

func anyStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
