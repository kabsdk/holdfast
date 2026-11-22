package storage

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
)

// Bucket is what the server does in the bucket itself, with the maintenance key, besides what restic does
// (plan-m5): measure a host's repository, write its escrow file, and delete a decommissioned prefix.

// Size is a host's repository as the bucket holds it.
type Size struct {
	Current    int64     `json:"current_bytes"`    // what restic sees
	Noncurrent int64     `json:"noncurrent_bytes"` // old versions, kept until the lifecycle rule expires them
	Objects    int       `json:"objects"`
	At         time.Time `json:"measured_at"`
}

// Bucket does those things.
type Bucket interface {
	Size(ctx context.Context, fqdn string) (Size, error)
	PutEscrow(ctx context.Context, fqdn string, data []byte) error
	// DeleteRepository deletes every current object under the host's prefix, and its escrow file: S3 keeps
	// their old versions until the lifecycle rule expires them. It returns how many it deleted.
	DeleteRepository(ctx context.Context, fqdn string) (int, error)

	// For a rollback (decision 038): every version under the host's prefix, delete markers included; a
	// version made current again by copying it; and an object set aside by a delete marker. No version is
	// ever deleted.
	Versions(ctx context.Context, fqdn string) ([]ObjectVersion, error)
	CopyVersion(ctx context.Context, key, versionID string) error
	SetAside(ctx context.Context, key string) error
	// CanReadVersion reads one version's metadata: it needs s3:GetObjectVersion, which copying a version does.
	CanReadVersion(ctx context.Context, key, versionID string) error
}

// ObjectVersion is one version of one object, or a delete marker.
type ObjectVersion struct {
	Key            string    `json:"key"`
	VersionID      string    `json:"version_id"`
	LastModified   time.Time `json:"last_modified"`
	Size           int64     `json:"size"`
	IsLatest       bool      `json:"is_latest"`
	IsDeleteMarker bool      `json:"is_delete_marker"`
}

// RollbackStep is one object a rollback changes.
type RollbackStep struct {
	Key       string `json:"key"`
	Kind      string `json:"kind"`                 // data, index, snapshots, keys, config, or other
	VersionID string `json:"version_id,omitempty"` // for one brought back: the version made current again
}

// RollbackPlan is what rolling a prefix back to To changes: what comes back, and what is set aside.
type RollbackPlan struct {
	To         time.Time      `json:"to"`
	BringBack  []RollbackStep `json:"bring_back"`
	SetAside   []RollbackStep `json:"set_aside"`
	Unchanged  int            `json:"unchanged"`
	Deletions  []time.Time    `json:"snapshot_deletions"` // when snapshot files were deleted: the delete markers under snapshots/
	NewestSeen time.Time      `json:"newest_change"`      // the newest version or marker: rolling back to after it changes nothing
}

// SnapshotIDs returns the snapshot IDs among steps: the file names under snapshots/.
func SnapshotIDs(steps []RollbackStep) []string {
	out := []string{}
	for _, s := range steps {
		if s.Kind == "snapshots" {
			out = append(out, path.Base(s.Key))
		}
	}
	sort.Strings(out)
	return out
}

// PlanRollback works out, from a prefix's versions, what rolling it back to to changes. For each object it takes
// the version current at that time, and compares it with today's: one there then and gone or different now comes
// back; one there now and not then is set aside. locks/ is left alone: restic's locks are not part of the
// repository's state.
func PlanRollback(prefix string, versions []ObjectVersion, to time.Time) RollbackPlan {
	plan := RollbackPlan{To: to, BringBack: []RollbackStep{}, SetAside: []RollbackStep{}, Deletions: []time.Time{}}
	byKey := map[string][]ObjectVersion{}
	for _, v := range versions {
		rel := strings.TrimPrefix(v.Key, prefix)
		if strings.HasPrefix(rel, "locks/") {
			continue
		}
		byKey[v.Key] = append(byKey[v.Key], v)
		if v.LastModified.After(plan.NewestSeen) {
			plan.NewestSeen = v.LastModified
		}
		if v.IsDeleteMarker && strings.HasPrefix(rel, "snapshots/") {
			plan.Deletions = append(plan.Deletions, v.LastModified)
		}
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		vs := byKey[k]
		var now, then *ObjectVersion
		for i := range vs {
			v := &vs[i]
			if v.IsLatest {
				now = v
			}
			if !v.LastModified.After(to) && (then == nil || v.LastModified.After(then.LastModified)) {
				then = v
			}
		}
		kind := kindOf(strings.TrimPrefix(k, prefix))
		existsNow := now != nil && !now.IsDeleteMarker
		existedThen := then != nil && !then.IsDeleteMarker
		switch {
		case existedThen && (!existsNow || now.VersionID != then.VersionID):
			plan.BringBack = append(plan.BringBack, RollbackStep{Key: k, Kind: kind, VersionID: then.VersionID})
		case !existedThen && existsNow:
			plan.SetAside = append(plan.SetAside, RollbackStep{Key: k, Kind: kind})
		default:
			plan.Unchanged++
		}
	}
	sort.Slice(plan.Deletions, func(i, j int) bool { return plan.Deletions[i].Before(plan.Deletions[j]) })
	return plan
}

func kindOf(rel string) string {
	switch first, _, _ := strings.Cut(rel, "/"); first {
	case "data", "index", "snapshots", "keys":
		return first
	case "config":
		return "config"
	}
	return "other"
}

// EscrowKey is where a host's escrow file goes (decision 036).
func EscrowKey(fqdn string) string { return "escrow/" + fqdn + ".age" }

// S3Bucket is Bucket on the S3 server, with the maintenance key.
type S3Bucket struct {
	Location Location
	Maint    Key
}

func (b S3Bucket) client() (*minio.Client, error) {
	return S3Prober{Location: b.Location}.client(b.Maint, b.Location.Region)
}

// Size lists the prefix with its versions: one listing gives both what is current and what is old.
func (b S3Bucket) Size(ctx context.Context, fqdn string) (Size, error) {
	c, err := b.client()
	if err != nil {
		return Size{}, err
	}
	var s Size
	for o := range c.ListObjects(ctx, b.Location.Bucket, minio.ListObjectsOptions{Prefix: Prefix(fqdn) + "/", Recursive: true, WithVersions: true}) {
		if o.Err != nil {
			msg := s3err(o.Err)
			if msg == "AccessDenied" {
				msg += "; the maintenance key needs s3:ListBucketVersions on the bucket (docs/storage.md)"
			}
			return Size{}, fmt.Errorf("listing the versions under %s/: %s", Prefix(fqdn), msg)
		}
		switch {
		case o.IsDeleteMarker:
		case o.IsLatest:
			s.Current += o.Size
			s.Objects++
		default:
			s.Noncurrent += o.Size
		}
	}
	return s, nil
}

// PutEscrow writes the host's escrow file.
func (b S3Bucket) PutEscrow(ctx context.Context, fqdn string, data []byte) error {
	c, err := b.client()
	if err != nil {
		return err
	}
	_, err = c.PutObject(ctx, b.Location.Bucket, EscrowKey(fqdn), bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		return fmt.Errorf("writing %s: %s", EscrowKey(fqdn), s3err(err))
	}
	return nil
}

// DeleteRepository deletes the current objects, never a version: the maintenance key may not (B009).
func (b S3Bucket) DeleteRepository(ctx context.Context, fqdn string) (int, error) {
	c, err := b.client()
	if err != nil {
		return 0, err
	}
	objects := make(chan minio.ObjectInfo)
	go func() {
		defer close(objects)
		for o := range c.ListObjects(ctx, b.Location.Bucket, minio.ListObjectsOptions{Prefix: Prefix(fqdn) + "/", Recursive: true}) {
			if o.Err == nil {
				objects <- minio.ObjectInfo{Key: o.Key}
			}
		}
		objects <- minio.ObjectInfo{Key: EscrowKey(fqdn)}
	}()
	n := 0
	for r := range c.RemoveObjectsWithResult(ctx, b.Location.Bucket, objects, minio.RemoveObjectsOptions{}) {
		if r.Err != nil {
			return n, fmt.Errorf("deleting %s: %s", r.ObjectName, s3err(r.Err))
		}
		if r.ObjectName != EscrowKey(fqdn) {
			n++
		}
	}
	return n, nil
}

// Versions lists every version under the prefix, delete markers included.
func (b S3Bucket) Versions(ctx context.Context, fqdn string) ([]ObjectVersion, error) {
	c, err := b.client()
	if err != nil {
		return nil, err
	}
	var out []ObjectVersion
	for o := range c.ListObjects(ctx, b.Location.Bucket, minio.ListObjectsOptions{Prefix: Prefix(fqdn) + "/", Recursive: true, WithVersions: true}) {
		if o.Err != nil {
			msg := s3err(o.Err)
			if msg == "AccessDenied" {
				msg += "; the maintenance key needs s3:ListBucketVersions on the bucket (docs/storage.md)"
			}
			return nil, fmt.Errorf("listing the versions under %s/: %s", Prefix(fqdn), msg)
		}
		out = append(out, ObjectVersion{Key: o.Key, VersionID: o.VersionID, LastModified: o.LastModified, Size: o.Size,
			IsLatest: o.IsLatest, IsDeleteMarker: o.IsDeleteMarker})
	}
	return out, nil
}

// CopyVersion copies a version of an object over its current one: that version's content is current again,
// and the version that was current stays, as an old version.
func (b S3Bucket) CopyVersion(ctx context.Context, key, versionID string) error {
	c, err := b.client()
	if err != nil {
		return err
	}
	_, err = c.CopyObject(ctx, minio.CopyDestOptions{Bucket: b.Location.Bucket, Object: key},
		minio.CopySrcOptions{Bucket: b.Location.Bucket, Object: key, VersionID: versionID})
	if err != nil {
		return fmt.Errorf("copying %s's version %s: %s", key, versionID, s3err(err))
	}
	return nil
}

// SetAside puts a delete marker in front of an object; its versions stay.
func (b S3Bucket) SetAside(ctx context.Context, key string) error {
	c, err := b.client()
	if err != nil {
		return err
	}
	if err := c.RemoveObject(ctx, b.Location.Bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("setting %s aside: %s", key, s3err(err))
	}
	return nil
}

// CanReadVersion reads one version's metadata.
func (b S3Bucket) CanReadVersion(ctx context.Context, key, versionID string) error {
	c, err := b.client()
	if err != nil {
		return err
	}
	if _, err := c.StatObject(ctx, b.Location.Bucket, key, minio.StatObjectOptions{VersionID: versionID}); err != nil {
		msg := s3err(err)
		if msg == "AccessDenied" || msg == "Forbidden" {
			msg += "; the maintenance key needs s3:GetObjectVersion under repos/ to bring old versions back (docs/storage.md)"
		}
		return fmt.Errorf("reading %s's version %s: %s", key, versionID, msg)
	}
	return nil
}

// MemBucket is a Bucket in memory, for the tests.
type MemBucket struct {
	mu      sync.Mutex
	Sizes   map[string]Size
	Escrow  map[string][]byte
	Deleted map[string]bool
	// Objects holds versions for rollbacks, oldest first per key; Copies and Markers record what was done.
	Objects map[string][]ObjectVersion
	Copies  []string
	Markers []string
	Clock   func() time.Time
}

func (m *MemBucket) Versions(_ context.Context, fqdn string) ([]ObjectVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ObjectVersion
	for k, vs := range m.Objects {
		if !strings.HasPrefix(k, Prefix(fqdn)+"/") {
			continue
		}
		for i, v := range vs {
			v.IsLatest = i == len(vs)-1
			out = append(out, v)
		}
	}
	return out, nil
}

func (m *MemBucket) add(key string, v ObjectVersion) {
	if m.Objects == nil {
		m.Objects = map[string][]ObjectVersion{}
	}
	v.Key = key
	if v.VersionID == "" {
		v.VersionID = fmt.Sprintf("v%d", len(m.Objects[key])+1)
	}
	if m.Clock != nil {
		v.LastModified = m.Clock()
	}
	m.Objects[key] = append(m.Objects[key], v)
}

// Put adds a version, as a write would.
func (m *MemBucket) Put(key string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.add(key, ObjectVersion{LastModified: at, Size: 1})
}

// Delete adds a delete marker, as a delete would.
func (m *MemBucket) Delete(key string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.add(key, ObjectVersion{LastModified: at, IsDeleteMarker: true})
}

func (m *MemBucket) CopyVersion(_ context.Context, key, versionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Copies = append(m.Copies, key+"@"+versionID)
	m.add(key, ObjectVersion{Size: 1})
	return nil
}

func (m *MemBucket) SetAside(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Markers = append(m.Markers, key)
	m.add(key, ObjectVersion{IsDeleteMarker: true})
	return nil
}

func (m *MemBucket) CanReadVersion(context.Context, string, string) error { return nil }

func (m *MemBucket) Size(_ context.Context, fqdn string) (Size, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Sizes[fqdn], nil
}

func (m *MemBucket) PutEscrow(_ context.Context, fqdn string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Escrow == nil {
		m.Escrow = map[string][]byte{}
	}
	m.Escrow[fqdn] = append([]byte(nil), data...)
	return nil
}

func (m *MemBucket) DeleteRepository(_ context.Context, fqdn string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Deleted == nil {
		m.Deleted = map[string]bool{}
	}
	m.Deleted[fqdn] = true
	delete(m.Escrow, fqdn)
	return 1, nil
}
