package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// The first backup runs right after enrollment. Its snapshot, restored with plain restic from the
// escrow bundle alone (as after losing Holdfast), keeps every metadata class (PoC test 1). Restore
// through the agent is M2 (plan-m1 Q1).
func TestBackupFidelityAndEscrow(t *testing.T) {
	requireRoot(t)
	src := filepath.Join(t.TempDir(), "data")
	buildFixture(t, src)
	e := newEnv(t)
	code := e.addHost("web01.example.com", "system", "--path", src, "--daily", "02:00")
	e.startAgent("web01.example.com", code)
	e.waitProgress("web01.example.com", "first_backup_done", 2*time.Minute)

	h := e.escrow()["web01.example.com"]
	r := e.resticFor(h, false)
	snaps, err := r.Snapshots(context.Background(), "job=system")
	if err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots: %d %v", len(snaps), err)
	}
	if snaps[0].Hostname != "web01.example.com" {
		t.Errorf("snapshot hostname %q, want the FQDN", snaps[0].Hostname)
	}
	target := t.TempDir()
	must(t, r.Restore(context.Background(), snaps[0].ID, target, nil))
	for _, res := range compareTrees(scan(t, src), scan(t, filepath.Join(target, src))) {
		t.Logf("%-50s %-5v %s", res.Property, res.Pass, res.Detail)
		if !res.Pass {
			t.Errorf("metadata lost: %s: %s", res.Property, res.Detail)
		}
	}
}

// SIGKILL the server: the agent keeps backing up on its own schedule and spools the results; when
// the server is back, every result arrives, and each is counted once (PoC test 2). The schedule's
// smallest unit is a minute, so the outage lasts long enough to span two slots.
func TestAgentKeepsBackingUpWhileServerIsDown(t *testing.T) {
	src := filepath.Join(t.TempDir(), "data")
	must(t, os.MkdirAll(src, 0o755))
	must(t, os.WriteFile(filepath.Join(src, "f.txt"), []byte("x"), 0o644))
	e := newEnv(t)
	code := e.addHost("db01.example.com", "etc", "--path", src, "--every", "1m")
	a := e.startAgent("db01.example.com", code)
	e.waitProgress("db01.example.com", "first_backup_done", 2*time.Minute)
	r := e.resticFor(e.escrow()["db01.example.com"], false)
	before := snapshots(t, r)

	e.srv.kill(syscall.SIGKILL)
	outage := 150 * time.Second
	time.Sleep(outage)
	made := snapshots(t, r) - before
	if made < 2 {
		t.Fatalf("the agent made %d snapshots during a %s server outage, want at least 2", made, outage)
	}
	spooled := a.spooled()
	if spooled < made {
		t.Fatalf("%d snapshots made but only %d events spooled", made, spooled)
	}

	e.startServer()
	key := `holdfast_runs_total{host="db01.example.com",kind="backup",job_id="etc",status="success"}`
	waitFor(t, "spool drained and every snapshot counted once", 90*time.Second, func() bool {
		total := snapshots(t, r)
		m := metrics(t, e.url+"/metrics")
		return a.spooled() == 0 && m[key] == float64(total)
	})
	t.Logf("outage %s: %d snapshots made while the server was dead, %d events spooled, all delivered and counted once", outage, made, spooled)
}

// Each host's S3 key works only on its own prefix and can delete only locks; the bucket is
// versioned (spec §2.3).
func TestStorageIsolation(t *testing.T) {
	e := newEnv(t)
	e.addHost("web01.example.com", "system", "--path", "/etc", "--daily", "02:00")
	e.addHost("db01.example.com", "system", "--path", "/etc", "--daily", "02:00")
	esc := e.escrow()
	web := esc["web01.example.com"]
	u, _ := url.Parse(s3Endpoint)
	ctx := context.Background()
	asHost, err := minio.New(u.Host, &minio.Options{Creds: credentials.NewStaticV4(web.S3AccessKeyID, web.S3SecretAccessKey, ""), Region: "us-east-1"})
	must(t, err)
	put := func(key string) error {
		_, err := asHost.PutObject(ctx, e.bucket, key, strings.NewReader("x"), 1, minio.PutObjectOptions{})
		return err
	}
	if err := put("repos/web01.example.com/data/zz/test"); err != nil {
		t.Errorf("a host cannot write its own prefix: %v", err)
	}
	if err := put("repos/db01.example.com/data/zz/test"); err == nil {
		t.Error("a host wrote into another host's prefix")
	}
	if _, err := asHost.StatObject(ctx, e.bucket, "repos/db01.example.com/config", minio.StatObjectOptions{}); err == nil {
		t.Error("a host read another host's repository")
	}
	if err := asHost.RemoveObject(ctx, e.bucket, "repos/web01.example.com/config", minio.RemoveObjectOptions{}); err == nil {
		t.Error("a host deleted its repository config: deletes must be limited to locks/")
	}
	if err := put("repos/web01.example.com/locks/test"); err != nil {
		t.Fatal(err)
	}
	if err := asHost.RemoveObject(ctx, e.bucket, "repos/web01.example.com/locks/test", minio.RemoveObjectOptions{}); err != nil {
		t.Errorf("a host cannot remove its own lock: %v", err)
	}
	for obj := range asHost.ListObjects(ctx, e.bucket, minio.ListObjectsOptions{Prefix: "repos/"}) {
		if obj.Err == nil {
			t.Errorf("a host listed other hosts' prefixes: %s", obj.Key)
		}
		break
	}
	if out := mc(t, "version", "info", "hf/"+e.bucket); !strings.Contains(out, "versioning is enabled") {
		t.Errorf("bucket versioning is not enabled: %s", out)
	}
}

// The witness lists the bucket with a list-only key: it agrees with the server while healthy, and
// shows the gap when the newest snapshot object disappears from storage (PoC test 5). The alert
// thresholds themselves are checked by the promtool rule tests.
func TestStalenessAndIndependentStorageWitness(t *testing.T) {
	src := filepath.Join(t.TempDir(), "data")
	must(t, os.MkdirAll(src, 0o755))
	must(t, os.WriteFile(filepath.Join(src, "f.txt"), []byte("x"), 0o644))
	e := newEnv(t)
	code := e.addHost("file01.example.com", "home", "--path", src, "--every", "1m")
	a := e.startAgent("file01.example.com", code)
	key := `holdfast_runs_total{host="file01.example.com",kind="backup",job_id="home",status="success"}`
	// Three backups: the first runs at enrollment and the next at the coming minute slot, which can be
	// seconds later; the third is a full slot after the second.
	waitFor(t, "three backups", 4*time.Minute, func() bool { return metrics(t, e.url+"/metrics")[key] >= 3 })
	a.kill(syscall.SIGTERM)

	// A list-only identity for the witness.
	witnessKey, witnessSecret := "HFWITNESS"+strings.ToUpper(randHex(4)), randHex(16)
	pol := filepath.Join(e.dir, "witness-policy.json")
	must(t, os.WriteFile(pol, []byte(fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:ListBucket"],"Resource":["arn:aws:s3:::%s"],"Condition":{"StringLike":{"s3:prefix":["repos/","repos/*"]}}}]}`, e.bucket)), 0o600))
	mc(t, "admin", "user", "add", "hf", witnessKey, witnessSecret)
	mc(t, "admin", "policy", "create", "hf", "witness-"+e.bucket, pol)
	mc(t, "admin", "policy", "attach", "hf", "witness-"+e.bucket, "--user", witnessKey)
	wenv := filepath.Join(e.dir, "witness.env")
	must(t, os.WriteFile(wenv, []byte("AWS_ACCESS_KEY_ID="+witnessKey+"\nAWS_SECRET_ACCESS_KEY="+witnessSecret+"\n"), 0o600))
	witness := func() map[string]float64 {
		out, err := exec.Command(filepath.Join(binDir, "holdfast-freshness"), "-endpoint", s3Endpoint, "-bucket", e.bucket, "-region", "us-east-1",
			"-credentials-file", wenv, "-once").CombinedOutput()
		if err != nil {
			t.Fatalf("witness: %v %s", err, out)
		}
		return parseMetrics(strings.NewReader(string(out)))
	}
	w := witness()
	if w["holdfast_storage_scrape_success"] != 1 {
		t.Fatal("the witness could not list storage with its list-only key")
	}
	server := metrics(t, e.url+"/metrics")[`holdfast_job_last_success_timestamp_seconds{host="file01.example.com",job_id="home"}`]
	newest := w[`holdfast_storage_newest_snapshot_timestamp_seconds{host="file01.example.com"}`]
	if gap := server - newest; gap > 10 || gap < -10 {
		t.Fatalf("witnesses should agree while healthy: server %v, storage %v", server, newest)
	}

	// Storage loses the newest snapshot (a delete marker, as the bucket is versioned). The witness must
	// fall back to the second-newest object, and the server now leads it by at least their gap.
	type obj struct {
		Key          string    `json:"key"`
		LastModified time.Time `json:"lastModified"`
	}
	var objs []obj
	for _, line := range strings.Split(mc(t, "ls", "--json", "hf/"+e.bucket+"/repos/file01.example.com/snapshots/"), "\n") {
		var o obj
		if json.Unmarshal([]byte(line), &o) == nil && o.Key != "" {
			objs = append(objs, o)
		}
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].LastModified.After(objs[j].LastModified) })
	if len(objs) < 3 {
		t.Fatalf("expected at least 3 snapshot objects, got %d", len(objs))
	}
	mc(t, "rm", "hf/"+e.bucket+"/repos/file01.example.com/snapshots/"+objs[0].Key)
	w = witness()
	after := w[`holdfast_storage_newest_snapshot_timestamp_seconds{host="file01.example.com"}`]
	if want := float64(objs[1].LastModified.Unix()); after != want {
		t.Fatalf("after the deletion the witness reports newest %v, want the second-newest object's %v", after, want)
	}
	gap := server - after
	if min := objs[0].LastModified.Sub(objs[1].LastModified).Seconds() - 2; gap < min || gap < 30 {
		t.Fatalf("the witness gap is %.0fs; want at least %.0fs (the deleted snapshot's lead) and 30s", gap, min)
	}
	if lg := metrics(t, e.url+"/metrics")[`holdfast_job_longest_gap_seconds{host="file01.example.com",job_id="home"}`]; lg != 60 {
		t.Errorf("longest gap metric = %v, want 60 (every 1 minute)", lg)
	}
	t.Logf("healthy witness gap %.0fs; after deleting the newest snapshot object the witness lags the server by %.0fs", server-newest, gap)
}
