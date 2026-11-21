package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kabsdk/holdfast/internal/schedule"
)

func sampleJob() Job {
	return Job{
		Engine:   EngineRestic,
		Paths:    []string{"/etc", "/home"},
		Excludes: []string{"/home/*/.cache"},
		Schedule: schedule.Schedule{Daily: &schedule.Daily{Times: []schedule.ClockTime{"02:00"}}, Spread: schedule.Spread{Enabled: true, Minutes: 20}},
		Retention: Retention{
			Within: &Duration{7, Days}, Daily: &Duration{14, Days}, Weekly: &Duration{8, Weeks},
			Monthly: &Duration{12, Months}, Yearly: &Duration{3, Years}, KeepLast: DefaultKeepLast,
		},
	}
}

func sampleDoc() Document {
	d := New()
	d.Hosts["web01.example.com"] = Host{Jobs: map[string]Job{"system": sampleJob(), "www": sampleJob()}}
	d.Hosts["db01.example.com"] = Host{Jobs: map[string]Job{"system": sampleJob()}}
	return d
}

func TestCanonicalIsDeterministic(t *testing.T) {
	a, err := sampleDoc().Canonical()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ { // map iteration order varies between runs; the encoding must not
		b, _ := sampleDoc().Canonical()
		if string(a) != string(b) {
			t.Fatal("canonical encoding differs between two encodings of the same document")
		}
	}
	// A missing map and an empty map encode the same.
	empty := Document{Schema: SchemaVersion, Access: DefaultAccess()}
	ea, _ := empty.Canonical()
	eb, _ := New().Canonical()
	if string(ea) != string(eb) {
		t.Fatalf("nil and empty maps encode differently:\n%s\n%s", ea, eb)
	}
	if strings.Contains(string(a), "password") || strings.Contains(string(a), "secret") {
		t.Fatal("a config document must never contain credentials")
	}
}

func TestParseRoundTripAndUnknownFields(t *testing.T) {
	b, _ := sampleDoc().Canonical()
	d, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := d.Canonical()
	if string(b) != string(b2) {
		t.Fatal("document does not round-trip through Parse")
	}
	var raw map[string]any
	_ = json.Unmarshal(b, &raw)
	raw["future_field"] = true
	withUnknown, _ := json.Marshal(raw)
	if _, err := Parse(withUnknown); err == nil {
		t.Fatal("Parse accepted a field it does not know")
	}
}

func TestValidate(t *testing.T) {
	if p := sampleDoc().Validate(); len(p) != 0 {
		t.Fatalf("valid document rejected: %v", p)
	}
	d := sampleDoc()
	bad := sampleJob()
	bad.Engine = "borg"
	bad.Paths = []string{"etc", "/var/../tmp"}
	bad.Schedule = schedule.Schedule{}
	bad.Retention.Daily = &Duration{0, Days}
	d.Hosts["Bad_Host"] = Host{Jobs: map[string]Job{"Not A Slug": bad}}
	d.Access.Bindings = append(d.Access.Bindings, Binding{Group: "x", Role: "root"})
	msgs := d.Validate().Error()
	for _, want := range []string{"must be lower case", "job ID", "engine \"borg\"", "not absolute", "not clean", "exactly one of every", "the number must be at least 1", "role \"root\"", "hosts is empty"} {
		if !strings.Contains(msgs, want) {
			t.Errorf("expected a problem mentioning %q in: %s", want, msgs)
		}
	}
}

func TestFQDNAndJobID(t *testing.T) {
	for _, ok := range []string{"web01.example.com", "a.b", "x-1.dc1.example.com"} {
		if err := ValidFQDN(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "web01", "Web01.example.com", "web01.example.com.", "-a.example.com", "a_b.example.com", strings.Repeat("a", 64) + ".com"} {
		if ValidFQDN(bad) == nil {
			t.Errorf("%q accepted as an FQDN", bad)
		}
	}
	for _, ok := range []string{"system", "www", "db-dumps", "1"} {
		if err := ValidJobID(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "System", "a,b", "-x", "a b", "job=1"} {
		if ValidJobID(bad) == nil {
			t.Errorf("%q accepted as a job ID", bad)
		}
	}
}

func TestForgetArgs(t *testing.T) {
	got := strings.Join(sampleJob().Retention.ForgetArgs(), " ")
	want := "--keep-within 7d --keep-within-daily 14d --keep-within-weekly 56d --keep-within-monthly 12m --keep-within-yearly 3y --keep-last 3"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if args := (Retention{Hourly: &Duration{48, Hours}}).ForgetArgs(); strings.Join(args, " ") != "--keep-within-hourly 48h" {
		t.Fatalf("hourly only: %v", args)
	}
	for _, a := range sampleJob().Retention.ForgetArgs() {
		for _, countFlag := range []string{"--keep-daily", "--keep-weekly", "--keep-monthly", "--keep-yearly", "--keep-hourly"} {
			if a == countFlag {
				t.Fatalf("count-based flag %s used (decision 018)", a)
			}
		}
	}
}
