// Package freshness is the storage witness (decision 008). It lists the bucket directly and reports
// when each host's newest snapshot object was written. It needs list access only: it holds no
// repository password and never talks to the server or the agents. Because it cannot decrypt, it
// judges per host, not per job (plan-m1 L6).
package freshness

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// RepoState is what storage says about one host's repository.
type RepoState struct {
	Host            string
	SnapshotObjects int
	NewestSnapshot  time.Time
}

// Lister lists every host's repository state.
type Lister interface {
	List(ctx context.Context) ([]RepoState, error)
}

// S3Lister lists repos/<fqdn>/snapshots/ in a bucket. In a versioned bucket it sees current
// objects only, so a deleted snapshot no longer counts.
type S3Lister struct {
	Client *minio.Client
	Bucket string
}

// NewS3Lister connects to an S3 endpoint such as https://rgw.example.internal.
func NewS3Lister(endpoint, bucket, region, accessKey, secretKey string) (*S3Lister, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("endpoint %q is not a URL", endpoint)
	}
	c, err := minio.New(u.Host, &minio.Options{Creds: credentials.NewStaticV4(accessKey, secretKey, ""), Secure: u.Scheme == "https", Region: region})
	if err != nil {
		return nil, err
	}
	return &S3Lister{Client: c, Bucket: bucket}, nil
}

// List implements Lister.
func (l *S3Lister) List(ctx context.Context) ([]RepoState, error) {
	var hosts []string
	for obj := range l.Client.ListObjects(ctx, l.Bucket, minio.ListObjectsOptions{Prefix: "repos/"}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		if strings.HasSuffix(obj.Key, "/") {
			hosts = append(hosts, strings.TrimSuffix(strings.TrimPrefix(obj.Key, "repos/"), "/"))
		}
	}
	var out []RepoState
	for _, h := range hosts {
		rs := RepoState{Host: h}
		for obj := range l.Client.ListObjects(ctx, l.Bucket, minio.ListObjectsOptions{Prefix: "repos/" + h + "/snapshots/", Recursive: true}) {
			if obj.Err != nil {
				return nil, obj.Err
			}
			rs.SnapshotObjects++
			if obj.LastModified.After(rs.NewestSnapshot) {
				rs.NewestSnapshot = obj.LastModified
			}
		}
		out = append(out, rs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out, nil
}

// Metrics renders Prometheus text format. staleAfter is the witness's own threshold, exported for each
// host. The stale alert uses it when the server's schedules are not available, so the witness can still
// raise the alarm while the server is down (decision 030).
func Metrics(states []RepoState, scrapeErr error, staleAfter time.Duration) string {
	var b strings.Builder
	ok := 1
	if scrapeErr != nil {
		ok = 0
	}
	fmt.Fprintf(&b, "# HELP holdfast_storage_scrape_success 1 if storage could be listed.\n# TYPE holdfast_storage_scrape_success gauge\nholdfast_storage_scrape_success %d\n", ok)
	b.WriteString("# HELP holdfast_storage_stale_after_seconds The witness's own stale threshold, used when the server's schedules are missing.\n# TYPE holdfast_storage_stale_after_seconds gauge\n")
	for _, s := range states {
		fmt.Fprintf(&b, "holdfast_storage_stale_after_seconds{host=%q} %.0f\n", s.Host, staleAfter.Seconds())
	}
	b.WriteString("# HELP holdfast_storage_newest_snapshot_timestamp_seconds Write time of the newest snapshot object, read from storage.\n# TYPE holdfast_storage_newest_snapshot_timestamp_seconds gauge\n")
	for _, s := range states {
		v := int64(0)
		if !s.NewestSnapshot.IsZero() {
			v = s.NewestSnapshot.Unix()
		}
		fmt.Fprintf(&b, "holdfast_storage_newest_snapshot_timestamp_seconds{host=%q} %d\n", s.Host, v)
	}
	b.WriteString("# HELP holdfast_storage_snapshot_objects Number of snapshot objects in storage.\n# TYPE holdfast_storage_snapshot_objects gauge\n")
	for _, s := range states {
		fmt.Fprintf(&b, "holdfast_storage_snapshot_objects{host=%q} %d\n", s.Host, s.SnapshotObjects)
	}
	return b.String()
}
