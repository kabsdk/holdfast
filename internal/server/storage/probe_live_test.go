package storage

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
)

// TestS3ProberLive runs the contract test against a real S3 server, such as the lab's Ceph RGW
// (lab/README.md). It is skipped unless HOLDFAST_PROBE_ENDPOINT is set. The other variables:
// HOLDFAST_PROBE_BUCKET, HOLDFAST_PROBE_REGION, HOLDFAST_PROBE_FQDN, HOLDFAST_PROBE_MAINT_KEY and
// HOLDFAST_PROBE_HOST_KEY (key files), and HOLDFAST_PROBE_EXPECT: the codes that must fail, comma
// separated ("" for none). Without HOLDFAST_PROBE_EXPECT the findings are only logged.
func TestS3ProberLive(t *testing.T) {
	ep := os.Getenv("HOLDFAST_PROBE_ENDPOINT")
	if ep == "" {
		t.Skip("HOLDFAST_PROBE_ENDPOINT is not set")
	}
	maint, err := ReadKeyFile(os.Getenv("HOLDFAST_PROBE_MAINT_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	host, err := ReadKeyFile(os.Getenv("HOLDFAST_PROBE_HOST_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	p := S3Prober{Location: Location{Endpoint: ep, Bucket: os.Getenv("HOLDFAST_PROBE_BUCKET"), Region: os.Getenv("HOLDFAST_PROBE_REGION")}, Maint: maint}
	rep := p.Probe(context.Background(), os.Getenv("HOLDFAST_PROBE_FQDN"), host)
	for _, f := range rep.Findings {
		t.Logf("%s  %s", f.Code, f.Message)
	}
	got := slices.Compact(slices.Sorted(slices.Values(rep.Codes())))
	t.Logf("failed checks: %v", got)
	expect, set := os.LookupEnv("HOLDFAST_PROBE_EXPECT")
	if !set {
		return
	}
	want := []string{}
	for _, c := range strings.Split(expect, ",") {
		if c = strings.TrimSpace(c); c != "" {
			want = append(want, c)
		}
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("failed checks %v, want %v", got, want)
	}
}
