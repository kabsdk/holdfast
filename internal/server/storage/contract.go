package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// The storage checks (spec §4.5, docs/storage.md). Each is an error: while any fails, the host cannot
// enroll. The codes are stable. B001, B002 and B005–B007 were the host key's until hosts had none (decision 041):
// they are the gateway key's now, which the server's gateway reads and writes every host's repository with.
const (
	CodeHostWrite         = "B001" // the gateway key cannot write under the host's prefix, or delete under its locks/
	CodeHostRead          = "B002" // the gateway key cannot read back or list the host's prefix
	CodeMaintRead         = "B003" // the maintenance key cannot read what the gateway key wrote
	CodeMaintWrite        = "B004" // the maintenance key cannot write, list or delete under repos/
	CodeHostDelete        = "B005" // the gateway key can delete outside locks/
	CodeHostVersionDelete = "B006" // the gateway key can delete a version for good
	CodeHostOtherPrefix   = "B007" // the gateway key can use the bucket outside repos/, such as the escrow files
	CodeVersioning        = "B008" // bucket versioning is not enabled
	CodeMaintVersionDel   = "B009" // the maintenance key can delete an object version for good
	CodeMaintSettings     = "B010" // the maintenance key can change the bucket's settings
	CodeLifecycle         = "B011" // no lifecycle rule expires noncurrent versions under repos/
	CodeEscrowWrite       = "B012" // the maintenance key cannot write and delete under escrow/ (decision 036)
	// B014 and B015 tested the read-only keys of M6 and M7, which decision 041 removed. They are not used again.
)

// Finding is one storage check that failed.
type Finding struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Report is the outcome of one contract test.
type Report struct {
	CheckedAt time.Time `json:"checked_at"`
	Findings  []Finding `json:"findings"` // empty when every check passed
	// NoncurrentDays is how long the bucket's lifecycle rule keeps a deleted file's old version, 0 if no
	// rule was found. It dates when deleted data is really gone (decision 031).
	NoncurrentDays int `json:"noncurrent_days,omitempty"`
	// ObjectLock describes the bucket's Object Lock, when it has one and the maintenance key may read the
	// setting. It is reported, not required, and Holdfast never extends a lock (decision 041).
	ObjectLock string `json:"object_lock,omitempty"`
}

// OK is true when every check passed.
func (r Report) OK() bool { return len(r.Findings) == 0 }

// Codes lists the failed checks' codes.
func (r Report) Codes() []string {
	out := make([]string, 0, len(r.Findings))
	for _, f := range r.Findings {
		out = append(out, f.Code)
	}
	return out
}

// Summary is the findings on one line, for errors and the audit log.
func (r Report) Summary() string {
	var b bytes.Buffer
	for i, f := range r.Findings {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(f.Code + ": " + f.Message)
	}
	return b.String()
}

// Prober tests the storage contract on one host's prefix: the gateway key's and the maintenance key's (decision 041).
type Prober interface {
	Probe(ctx context.Context, fqdn string, gateway Key) Report
	// ProbeMaintenance tests the bucket's side alone, with the maintenance key: the off-site bucket, which only the
	// server writes (decision 041).
	ProbeMaintenance(ctx context.Context) Report
}

// ProbeMaintenance implements Prober: it returns the same findings as Probe.
func (p StaticProber) ProbeMaintenance(context.Context) Report {
	return Report{Findings: append([]Finding{}, p.Findings...)}
}

// ProbeMaintenance tests what the maintenance key must and must not do: B004, B008–B012.
func (p S3Prober) ProbeMaintenance(ctx context.Context) Report {
	rep := Report{Findings: []Finding{}}
	add := func(code, format string, a ...any) {
		rep.Findings = append(rep.Findings, Finding{Code: code, Message: fmt.Sprintf(format, a...)})
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	mc, err := p.client(p.Maint, p.Location.Region)
	if err != nil {
		add(CodeMaintWrite, "cannot use the S3 endpoint %s: %v", p.Location.Endpoint, err)
		return rep
	}
	b := p.Location.Bucket
	versioned := p.bucketSettings(ctx, mc, &rep, add)
	other := "repos/holdfast-probe.invalid/holdfast-probe-" + randHex(6)
	if info, err := put(ctx, mc, b, other); err != nil {
		add(CodeMaintWrite, "the maintenance key cannot write %s: %s", other, s3err(err))
	} else {
		if err := get(ctx, mc, b, other); err != nil {
			add(CodeMaintWrite, "the maintenance key cannot read back %s: %s", other, s3err(err))
		}
		if versioned && info.VersionID != "" {
			if err := mc.RemoveObject(ctx, b, other, minio.RemoveObjectOptions{VersionID: info.VersionID}); err == nil {
				add(CodeMaintVersionDel, "the maintenance key deleted a version of %s for good, so a compromised server could "+
					"destroy the copies for good; deny it s3:DeleteObjectVersion (docs/storage.md)", other)
			}
		}
		if err := mc.RemoveObject(ctx, b, other, minio.RemoveObjectOptions{}); err != nil {
			add(CodeMaintWrite, "the maintenance key cannot delete %s: %s", other, s3err(err))
		}
	}
	if err := list(ctx, mc, b, "repos/"); err != nil {
		add(CodeMaintWrite, "the maintenance key cannot list repos/: %s", s3err(err))
	}
	if p.Escrow {
		esc := "escrow/holdfast-probe-" + randHex(6)
		if _, err := put(ctx, mc, b, esc); err != nil {
			add(CodeEscrowWrite, "the maintenance key cannot write %s: %s; it needs s3:PutObject and s3:DeleteObject on escrow/* (docs/storage.md)", esc, s3err(err))
		} else if err := mc.RemoveObject(ctx, b, esc, minio.RemoveObjectOptions{}); err != nil {
			add(CodeEscrowWrite, "the maintenance key cannot delete %s: %s", esc, s3err(err))
		}
	}
	return rep
}

// bucketSettings tests versioning (B008), that the maintenance key cannot change the bucket's settings (B010) and
// the lifecycle rule (B011). It says whether versioning is on.
func (p S3Prober) bucketSettings(ctx context.Context, mc *minio.Client, rep *Report, add func(code, format string, a ...any)) bool {
	b := p.Location.Bucket
	versioned := false
	if v, err := mc.GetBucketVersioning(ctx, b); err != nil {
		add(CodeVersioning, "the maintenance key cannot read the versioning setting of bucket %s: %s", b, s3err(err))
	} else if !v.Enabled() {
		add(CodeVersioning, "versioning is not enabled on bucket %s", b)
	} else {
		versioned = true
		// Setting it to what it already is changes nothing, and shows whether the key could change it.
		if err := mc.SetBucketVersioning(ctx, b, minio.BucketVersioningConfiguration{Status: "Enabled"}); err == nil {
			add(CodeMaintSettings, "the maintenance key may change bucket %s's settings (it was allowed to set versioning; "+
				"the setting was left as it was), so a compromised server could switch off the floor", b)
		}
	}
	if days, err := lifecycleExpires(ctx, mc, b); err != nil {
		add(CodeLifecycle, "%v", err)
	} else {
		rep.NoncurrentDays = days
	}
	// Object Lock is reported, not required (decision 041): a key that may not read the setting reports nothing.
	if on, mode, validity, unit, err := mc.GetObjectLockConfig(ctx, b); err == nil && on == "Enabled" {
		rep.ObjectLock = "enabled"
		if mode != nil && validity != nil && unit != nil {
			rep.ObjectLock = fmt.Sprintf("enabled, %s for %d %s by default", strings.ToLower(string(*mode)), *validity, strings.ToLower(string(*unit)))
		}
	}
	return versioned
}

// StaticProber returns the same findings every time. Tests use it where there is no S3 server.
type StaticProber struct{ Findings []Finding }

// Probe implements Prober.
func (p StaticProber) Probe(context.Context, string, Key) Report {
	return Report{Findings: append([]Finding{}, p.Findings...)}
}

// S3Prober tests the contract with plain S3 calls, the same calls restic makes, so it works with any S3
// server. It writes a few tiny probe objects and deletes them again. restic ignores them: their names are
// not object IDs.
type S3Prober struct {
	Location Location
	Maint    Key
	Timeout  time.Duration // for the whole test; 60s when zero
	// Escrow tests B012 too: the server writes escrow files when server.yaml names an age recipient.
	Escrow bool
}

var probeBody = []byte("holdfast storage probe; safe to delete\n")

// Probe implements Prober: it tests the gateway key on the host's prefix (decision 041), and the maintenance key
// beside it.
func (p S3Prober) Probe(ctx context.Context, fqdn string, gateway Key) Report {
	rep := Report{Findings: []Finding{}}
	add := func(code, format string, a ...any) {
		rep.Findings = append(rep.Findings, Finding{Code: code, Message: fmt.Sprintf(format, a...)})
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	gc, err1 := p.client(gateway, p.Location.Region)
	mc, err2 := p.client(p.Maint, p.Location.Region)
	if err := errors.Join(err1, err2); err != nil {
		add(CodeHostWrite, "cannot use the S3 endpoint %s: %v", p.Location.Endpoint, err)
		return rep
	}
	b := p.Location.Bucket
	prefix := Prefix(fqdn) + "/"
	tag := "holdfast-probe-" + randHex(6)
	own, lock := prefix+tag, prefix+"locks/"+tag
	other := "repos/holdfast-probe.invalid/" + tag // a reserved name that is no host's prefix

	versioned := p.bucketSettings(ctx, mc, &rep, add)

	// The host's prefix.
	if info, err := put(ctx, gc, b, own); err != nil {
		add(CodeHostWrite, "the gateway key cannot write %s: %s", own, s3err(err))
	} else {
		if err := get(ctx, gc, b, own); err != nil {
			add(CodeHostRead, "the gateway key cannot read back %s: %s", own, s3err(err))
		}
		if err := list(ctx, gc, b, prefix); err != nil {
			add(CodeHostRead, "the gateway key cannot list %s: %s", prefix, s3err(err))
		}
		if err := get(ctx, mc, b, own); err != nil {
			add(CodeMaintRead, "the maintenance key cannot read %s, which the gateway key wrote: %s. "+
				"On some S3 servers an object belongs to the user who uploaded it; see docs/storage.md", own, s3err(err))
		}
		if err := gc.RemoveObject(ctx, b, own, minio.RemoveObjectOptions{}); err == nil {
			add(CodeHostDelete, "the gateway key deleted %s: it may delete only lock files, under locks/", own)
		}
		if versioned && info.VersionID != "" {
			if err := gc.RemoveObject(ctx, b, own, minio.RemoveObjectOptions{VersionID: info.VersionID}); err == nil {
				add(CodeHostVersionDelete, "the gateway key deleted a version of %s for good; deny it s3:DeleteObjectVersion (docs/storage.md)", own)
			}
		}
		if err := mc.RemoveObject(ctx, b, own, minio.RemoveObjectOptions{}); err != nil {
			add(CodeMaintWrite, "the maintenance key cannot delete %s: %s", own, s3err(err))
		}
	}
	if err := list(ctx, mc, b, prefix); err != nil {
		add(CodeMaintWrite, "the maintenance key cannot list %s: %s", prefix, s3err(err))
	}

	// Its locks.
	if _, err := put(ctx, gc, b, lock); err != nil {
		add(CodeHostWrite, "the gateway key cannot write %s: %s", lock, s3err(err))
	} else if err := gc.RemoveObject(ctx, b, lock, minio.RemoveObjectOptions{}); err != nil {
		add(CodeHostWrite, "the gateway key cannot delete %s: %s", lock, s3err(err))
		_ = mc.RemoveObject(ctx, b, lock, minio.RemoveObjectOptions{})
	}

	// The maintenance key never deletes a version either.
	if info, err := put(ctx, mc, b, other); err != nil {
		add(CodeMaintWrite, "the maintenance key cannot write %s: %s", other, s3err(err))
	} else {
		if versioned && info.VersionID != "" {
			if err := mc.RemoveObject(ctx, b, other, minio.RemoveObjectOptions{VersionID: info.VersionID}); err == nil {
				add(CodeMaintVersionDel, "the maintenance key deleted a version of %s for good, so a compromised server could "+
					"destroy backups for good; deny it s3:DeleteObjectVersion (docs/storage.md)", other)
			}
		}
		if err := mc.RemoveObject(ctx, b, other, minio.RemoveObjectOptions{}); err != nil {
			add(CodeMaintWrite, "the maintenance key cannot delete %s: %s", other, s3err(err))
		}
	}

	// Outside repos/: the escrow files (decision 036) are the maintenance key's alone.
	esc := "escrow/" + tag
	if _, err := put(ctx, gc, b, esc+"-gateway"); err == nil {
		add(CodeHostOtherPrefix, "the gateway key wrote %s-gateway, outside repos/: it could replace an escrow file", esc)
		_ = mc.RemoveObject(ctx, b, esc+"-gateway", minio.RemoveObjectOptions{})
	}
	if p.Escrow {
		if _, err := put(ctx, mc, b, esc); err != nil {
			add(CodeEscrowWrite, "the maintenance key cannot write %s: %s; it needs s3:PutObject and s3:DeleteObject on escrow/* (docs/storage.md)", esc, s3err(err))
		} else {
			if err := get(ctx, gc, b, esc); err == nil {
				add(CodeHostOtherPrefix, "the gateway key read %s, outside repos/", esc)
			}
			if err := mc.RemoveObject(ctx, b, esc, minio.RemoveObjectOptions{}); err != nil {
				add(CodeEscrowWrite, "the maintenance key cannot delete %s: %s", esc, s3err(err))
			}
		}
	}
	return rep
}

func (p S3Prober) client(k Key, region string) (*minio.Client, error) {
	u, err := url.Parse(p.Location.Endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("storage.endpoint %q is not a URL", p.Location.Endpoint)
	}
	return minio.New(u.Host, &minio.Options{Creds: credentials.NewStaticV4(k.AccessKeyID, k.SecretAccessKey, ""),
		Secure: u.Scheme == "https", Region: region})
}

// lifecycleExpires checks that an enabled lifecycle rule covering repos/ expires noncurrent versions, and
// returns after how many days; with several such rules, the longest. Without one, the bucket only grows:
// nothing a prune deletes is ever freed. How long the versions are kept is the admin's choice; it is also
// how long deleted data can be recovered.
func lifecycleExpires(ctx context.Context, c *minio.Client, bucket string) (int, error) {
	lc, err := c.GetBucketLifecycle(ctx, bucket)
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchLifecycleConfiguration" {
			return 0, fmt.Errorf("bucket %s has no lifecycle rule, so old versions are never removed and the bucket only grows", bucket)
		}
		return 0, fmt.Errorf("the maintenance key cannot read bucket %s's lifecycle rule: %s", bucket, s3err(err))
	}
	days := 0
	for _, r := range lc.Rules {
		prefix := r.RuleFilter.Prefix
		if prefix == "" {
			prefix = r.Prefix
		}
		if n := int(r.NoncurrentVersionExpiration.NoncurrentDays); r.Status == "Enabled" && n > 0 && strings.HasPrefix("repos/", prefix) {
			days = max(days, n)
		}
	}
	if days == 0 {
		return 0, fmt.Errorf("no enabled lifecycle rule on bucket %s expires noncurrent versions under repos/, so the bucket only grows", bucket)
	}
	return days, nil
}

func put(ctx context.Context, c *minio.Client, bucket, key string) (minio.UploadInfo, error) {
	return c.PutObject(ctx, bucket, key, bytes.NewReader(probeBody), int64(len(probeBody)), minio.PutObjectOptions{ContentType: "text/plain"})
}

func get(ctx context.Context, c *minio.Client, bucket, key string) error {
	o, err := c.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return err
	}
	defer o.Close()
	_, err = io.ReadAll(o)
	return err
}

// list reports whether a listing of prefix is allowed: the first answer decides.
func list(ctx context.Context, c *minio.Client, bucket, prefix string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for o := range c.ListObjects(ctx, bucket, minio.ListObjectsOptions{Prefix: prefix, MaxKeys: 1}) {
		return o.Err
	}
	return nil
}

func s3err(err error) string {
	if r := minio.ToErrorResponse(err); r.Code != "" {
		return r.Code
	}
	return err.Error()
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
