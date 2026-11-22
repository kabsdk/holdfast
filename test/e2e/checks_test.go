package e2e

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// The checks on config saves (spec §4.5), through the CLI:
//   - an unaccepted warning refuses host add, and says which --accept would save it;
//   - --accept saves it, and the revision and the audit log record the accepted code;
//   - an error cannot be accepted, and job check says so without saving;
//   - R104 is decided by restic on the real repository: keeping fewer of the job's snapshots is a cut, and
//     keeping more is not.
func TestChecks(t *testing.T) {
	src := smallSource(t)
	e := newEnv(t)
	const fqdn = "chk01.example.com"
	kf, _ := e.makeKey(fqdn, "")
	job := []string{"--job", "data", "--path", src, "--every", "1m", "--keep-last", "10"}

	out, err := e.cliErr(append([]string{"host", "add", fqdn, "--s3-key-file", kf}, job...)...)
	if err == nil || !strings.Contains(string(out), "R101") || !strings.Contains(string(out), "--accept R101") {
		t.Fatalf("an unaccepted R101 must refuse host add, with the --accept that saves it: %v %s", err, out)
	}
	if out, err := e.cliErr("host", "show", fqdn); err == nil {
		t.Fatalf("a refused host was saved: %s", out)
	}

	var res struct {
		Revision   int64                             `json:"revision"`
		Accepted   []string                          `json:"accepted_codes"`
		Enrollment struct{ Code string }             `json:"enrollment"`
		Findings   []struct{ Code, Severity string } `json:"findings"`
	}
	must(t, json.Unmarshal(e.cli(append([]string{"host", "add", fqdn, "--s3-key-file", kf, "--accept", "R101"}, job...)...), &res))
	if !slices.Equal(res.Accepted, []string{"R101"}) {
		t.Fatalf("accepted codes: %+v", res)
	}
	var revs []struct {
		Number   int64    `json:"number"`
		Accepted []string `json:"accepted_codes"`
	}
	must(t, json.Unmarshal(e.cli("revision", "list"), &revs))
	if len(revs) != 1 || !slices.Equal(revs[0].Accepted, []string{"R101"}) {
		t.Fatalf("the revision must record R101: %+v", revs)
	}
	var audit []auditRow
	must(t, json.Unmarshal(e.cli("audit"), &audit))
	if i := slices.IndexFunc(audit, func(a auditRow) bool { return a.Action == "host.add" }); i < 0 ||
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
	e.startAgent(fqdn, res.Enrollment.Code)
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
