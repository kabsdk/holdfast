// Package freshness is the independent witness: it reads backup storage directly and
// reports when each host's newest snapshot object was written. It needs list access
// only; it holds no repository password and never talks to the server or agents.
package freshness

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RepoState is what storage says about one host's repository.
type RepoState struct {
	Host           string
	SnapshotObjs   int
	NewestSnapshot time.Time
}

// Lister lists per-host snapshot objects. LocalLister covers filesystem repositories;
// an S3 lister (ListObjectsV2 on repos/<host>/snapshots/, LastModified) is the
// production implementation for Ceph RGW.
type Lister interface {
	List() ([]RepoState, error)
}

// LocalLister scans <root>/<host>/snapshots/.
type LocalLister struct{ Root string }

// List implements Lister.
func (l LocalLister) List() ([]RepoState, error) {
	hosts, err := os.ReadDir(l.Root)
	if err != nil {
		return nil, err
	}
	var out []RepoState
	for _, h := range hosts {
		if !h.IsDir() {
			continue
		}
		rs := RepoState{Host: h.Name()}
		entries, err := os.ReadDir(filepath.Join(l.Root, h.Name(), "snapshots"))
		if err != nil {
			continue // not a restic repository
		}
		for _, e := range entries {
			info, err := e.Info()
			if err != nil || info.IsDir() {
				continue
			}
			rs.SnapshotObjs++
			if info.ModTime().After(rs.NewestSnapshot) {
				rs.NewestSnapshot = info.ModTime()
			}
		}
		out = append(out, rs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out, nil
}

// Metrics renders Prometheus text format.
func Metrics(states []RepoState, scrapeErr error) string {
	var b strings.Builder
	ok := 1
	if scrapeErr != nil {
		ok = 0
	}
	fmt.Fprintf(&b, "# HELP holdfast_storage_scrape_success 1 if storage could be listed.\n# TYPE holdfast_storage_scrape_success gauge\nholdfast_storage_scrape_success %d\n", ok)
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
		fmt.Fprintf(&b, "holdfast_storage_snapshot_objects{host=%q} %d\n", s.Host, s.SnapshotObjs)
	}
	return b.String()
}
