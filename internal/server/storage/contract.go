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
// enroll. The codes are stable.
const (
	CodeHostWrite         = "B001" // the host key cannot write under its prefix, or delete under its locks/
	CodeHostRead          = "B002" // the host key cannot read back or list its prefix
	CodeMaintRead         = "B003" // the maintenance key cannot read what the host key wrote
	CodeMaintWrite        = "B004" // the maintenance key cannot write, list or delete under repos/
	CodeHostDelete        = "B005" // the host key can delete outside its locks/
	CodeHostVersionDelete = "B006" // the host key can delete a version of a non-lock object for good
	CodeHostOtherPrefix   = "B007" // the host key can use another prefix, or list repos/
	CodeVersioning        = "B008" // bucket versioning is not enabled
	CodeMaintVersionDel   = "B009" // the maintenance key can delete an object version for good
	CodeMaintSettings     = "B010" // the maintenance key can change the bucket's settings
	CodeLifecycle         = "B011" // no lifecycle rule expires noncurrent versions under repos/
	CodeEscrowWrite       = "B012" // the maintenance key cannot write and delete under escrow/ (decision 036)
	// A read-only key, for a restore onto another host (plan-m6 §2.2).
	CodeReadOnlyRead  = "B014" // it cannot read or list the host's prefix
	CodeReadOnlyWrite = "B015" // it can write or delete there
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

// Prober tests the storage contract for one host's key (decision 029), and a read-only key for a restore onto
// another host (plan-m6 §2.2).
type Prober interface {
	Probe(ctx context.Context, fqdn string, host Key) Report
	ProbeReadOnly(ctx context.Context, fqdn string, key Key) Report
}

// StaticProber returns the same findings every time. Tests use it where there is no S3 server.
type StaticProber struct{ Findings []Finding }

// Probe implements Prober.
func (p StaticProber) Probe(context.Context, string, Key) Report {
	return Report{Findings: append([]Finding{}, p.Findings...)}
}

// ProbeReadOnly implements Prober: every read-only key passes.
func (p StaticProber) ProbeReadOnly(context.Context, string, Key) Report {
	return Report{Findings: []Finding{}}
}

// ProbeReadOnly tests a key for a restore onto another host: it must read and list the host's prefix (B014), and
// must not write or delete anything there, its locks included (B015). restic then restores with --no-lock.
func (p S3Prober) ProbeReadOnly(ctx context.Context, fqdn string, key Key) Report {
	rep := Report{Findings: []Finding{}}
	add := func(code, format string, a ...any) {
		rep.Findings = append(rep.Findings, Finding{Code: code, Message: fmt.Sprintf(format, a...)})
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	c, err1 := p.client(key, "")
	mc, err2 := p.client(p.Maint, p.Location.Region)
	if err := errors.Join(err1, err2); err != nil {
		add(CodeReadOnlyRead, "cannot use the S3 endpoint %s: %v", p.Location.Endpoint, err)
		return rep
	}
	b, prefix := p.Location.Bucket, Prefix(fqdn)+"/"
	if err := list(ctx, c, b, prefix); err != nil {
		add(CodeReadOnlyRead, "the key cannot list %s: %s", prefix, s3err(err))
	}
	if err := get(ctx, c, b, prefix+"config"); err != nil {
		add(CodeReadOnlyRead, "the key cannot read %sconfig, the repository's own file: %s", prefix, s3err(err))
	}
	tag := "holdfast-probe-" + randHex(6)
	for _, k := range []string{prefix + tag, prefix + "locks/" + tag} {
		if _, err := put(ctx, c, b, k); err == nil {
			add(CodeReadOnlyWrite, "the key wrote %s: a key for a restore onto another host must only read", k)
			_ = mc.RemoveObject(ctx, b, k, minio.RemoveObjectOptions{})
		}
	}
	if err := c.RemoveObject(ctx, b, prefix+tag+"-absent", minio.RemoveObjectOptions{}); err == nil {
		add(CodeReadOnlyWrite, "the key may delete under %s: a key for a restore onto another host must only read", prefix)
	}
	return rep
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

// Probe implements Prober.
func (p S3Prober) Probe(ctx context.Context, fqdn string, host Key) Report {
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
	// The host client has no region, as restic on an agent has none: it asks the bucket, like restic.
	hc, err1 := p.client(host, "")
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

	// The host's own prefix.
	if info, err := put(ctx, hc, b, own); err != nil {
		add(CodeHostWrite, "the host key cannot write %s: %s", own, s3err(err))
	} else {
		if err := get(ctx, hc, b, own); err != nil {
			add(CodeHostRead, "the host key cannot read back %s: %s", own, s3err(err))
		}
		if err := list(ctx, hc, b, prefix); err != nil {
			add(CodeHostRead, "the host key cannot list %s: %s", prefix, s3err(err))
		}
		if err := get(ctx, mc, b, own); err != nil {
			add(CodeMaintRead, "the maintenance key cannot read %s, which the host key wrote: %s. "+
				"On some S3 servers an object belongs to the user who uploaded it; see docs/storage.md", own, s3err(err))
		}
		if err := hc.RemoveObject(ctx, b, own, minio.RemoveObjectOptions{}); err == nil {
			add(CodeHostDelete, "the host key deleted %s: it may delete only under %slocks/", own, prefix)
		}
		if versioned && info.VersionID != "" {
			if err := hc.RemoveObject(ctx, b, own, minio.RemoveObjectOptions{VersionID: info.VersionID}); err == nil {
				add(CodeHostVersionDelete, "the host key deleted a version of %s for good", own)
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
	if _, err := put(ctx, hc, b, lock); err != nil {
		add(CodeHostWrite, "the host key cannot write %s: %s", lock, s3err(err))
	} else if err := hc.RemoveObject(ctx, b, lock, minio.RemoveObjectOptions{}); err != nil {
		add(CodeHostWrite, "the host key cannot delete %s: %s", lock, s3err(err))
		_ = mc.RemoveObject(ctx, b, lock, minio.RemoveObjectOptions{})
	}

	// Everyone else's.
	if info, err := put(ctx, mc, b, other); err != nil {
		add(CodeMaintWrite, "the maintenance key cannot write %s: %s", other, s3err(err))
	} else {
		if err := get(ctx, hc, b, other); err == nil {
			add(CodeHostOtherPrefix, "the host key read %s, outside its prefix", other)
		}
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
	if _, err := put(ctx, hc, b, other+"-host"); err == nil {
		add(CodeHostOtherPrefix, "the host key wrote %s-host, outside its prefix", other)
		_ = mc.RemoveObject(ctx, b, other+"-host", minio.RemoveObjectOptions{})
	}
	if err := list(ctx, hc, b, "repos/"); err == nil {
		add(CodeHostOtherPrefix, "the host key can list repos/, so it sees every host's name")
	}

	// The escrow files (decision 036).
	if p.Escrow {
		esc := "escrow/" + tag
		if _, err := put(ctx, mc, b, esc); err != nil {
			add(CodeEscrowWrite, "the maintenance key cannot write %s: %s; it needs s3:PutObject and s3:DeleteObject on escrow/* (docs/storage.md)", esc, s3err(err))
		} else {
			if err := get(ctx, hc, b, esc); err == nil {
				add(CodeHostOtherPrefix, "the host key read %s, outside its prefix", esc)
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
