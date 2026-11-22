package storage

import (
	"bytes"
	"context"
	"fmt"
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

// MemBucket is a Bucket in memory, for the tests.
type MemBucket struct {
	mu      sync.Mutex
	Sizes   map[string]Size
	Escrow  map[string][]byte
	Deleted map[string]bool
}

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
