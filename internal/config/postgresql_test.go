package config

import (
	"strings"
	"testing"

	"github.com/kabsdk/holdfast/internal/schedule"
)

func pgDoc(conns map[string]PGConnection, jobs map[string]Job) Document {
	d := New()
	d.Hosts["k3s-postgres-main"] = Host{Jobs: jobs, PostgreSQL: conns}
	return d
}

func pgJob(conn, db string, globals bool) Job {
	j := DefaultJob()
	j.Engine = EnginePostgreSQL
	j.Schedule = schedule.Schedule{Every: &schedule.Every{N: 6, Unit: "hours", From: "00:30"}}
	j.PostgreSQL = &PGJob{Connection: conn, Database: db, Globals: globals}
	return j
}

func TestPostgreSQLConnectionValidation(t *testing.T) {
	ok := []PGConnection{
		{URL: "postgresql://postgres-main-rw.postgres.svc:5432/?sslmode=verify-full&sslrootcert=/etc/holdfast/pg/ca.crt", User: "holdfast_backup", RestoreUser: "holdfast_restore"},
		{URL: "postgresql:///?host=/var/run/postgresql", User: "holdfast_backup"},
		{URL: "postgres://db.example.internal/", User: "backup"},
	}
	for _, c := range ok {
		if p := c.Validate(); len(p) > 0 {
			t.Errorf("%s: %v", c.URL, p)
		}
	}
	bad := map[string]PGConnection{
		"have fields of their own":  {URL: "postgresql://holdfast:secret@db.example.internal/", User: "holdfast"},
		"holds no user, password":   {URL: "postgresql://db.example.internal/?password=secret", User: "holdfast"},
		"names no database":         {URL: "postgresql://db.example.internal/authelia", User: "holdfast"},
		"starts with postgresql://": {URL: "mysql://db.example.internal/", User: "holdfast"},
		"names a host":              {URL: "postgresql:///", User: "holdfast"},
		"not a plain role name":     {URL: "postgresql://db/", User: "bad user"},
	}
	for want, c := range bad {
		if p := strings.Join(c.Validate(), "; "); !strings.Contains(p, want) {
			t.Errorf("%s: want a problem saying %q, got %q", c.URL, want, p)
		}
	}
}

func TestPostgreSQLJobs(t *testing.T) {
	conns := map[string]PGConnection{"postgres-main": {URL: "postgresql://postgres-main-rw.postgres.svc:5432/", User: "holdfast_backup"}}
	good := pgDoc(conns, map[string]Job{"authelia": pgJob("postgres-main", "authelia", false), "roles": pgJob("postgres-main", "", true),
		"policy-reporter": pgJob("postgres-main", "policy-reporter", false)})
	if p := good.Validate(); len(p) > 0 {
		t.Fatalf("valid PostgreSQL jobs refused: %v", p)
	}
	cases := map[string]Job{
		"no PostgreSQL connection named": pgJob("elsewhere", "authelia", false),
		"one database, or the roles":     pgJob("postgres-main", "authelia", true),
		"not one Holdfast backs up":      pgJob("postgres-main", "a b", false),
	}
	for want, j := range cases {
		d := pgDoc(conns, map[string]Job{"x": j})
		if msg := d.Validate().Error(); !strings.Contains(msg, want) {
			t.Errorf("want %q in %q", want, msg)
		}
	}
	withPaths := pgJob("postgres-main", "authelia", false)
	withPaths.Paths = []string{"/etc"}
	if msg := pgDoc(conns, map[string]Job{"x": withPaths}).Validate().Error(); !strings.Contains(msg, "not paths") {
		t.Errorf("a postgresql job with paths: %q", msg)
	}
	twice := pgDoc(conns, map[string]Job{"a": pgJob("postgres-main", "authelia", false), "b": pgJob("postgres-main", "authelia", false)})
	if msg := twice.Validate().Error(); !strings.Contains(msg, "backs up the same already") {
		t.Errorf("two jobs of one database: %q", msg)
	}
	files := DefaultJob()
	files.Paths = []string{"/etc"}
	files.PostgreSQL = &PGJob{Connection: "postgres-main", Database: "authelia"}
	if msg := pgDoc(conns, map[string]Job{"x": files}).Validate().Error(); !strings.Contains(msg, "a job of the postgresql engine") {
		t.Errorf("a restic job naming a database: %q", msg)
	}
	// A document round-trips: the connections are part of it, and parse back.
	b, err := good.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if back.Hosts["k3s-postgres-main"].PostgreSQL["postgres-main"].User != "holdfast_backup" || back.Hosts["k3s-postgres-main"].Jobs["roles"].PostgreSQL.Globals != true {
		t.Errorf("the PostgreSQL settings did not round-trip: %+v", back.Hosts["k3s-postgres-main"])
	}
}
