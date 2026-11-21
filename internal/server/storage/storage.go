// Package storage knows where repositories live and how a host's S3 identity is made.
//
// Holdfast generates each host's S3 access key and secret itself and stores them sealed, then asks
// a Provisioner to make that identity exist with the right policy (plan-m1 Q4):
//   - read, write and list under repos/<fqdn>/ only;
//   - delete only under repos/<fqdn>/locks/ (spec §2.3).
//
// Provisioning must be idempotent: calling it again with the same values changes nothing. That is
// what makes adding a host and enrolling it safe to retry.
package storage

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Location is the S3 endpoint and bucket that hold the repositories.
type Location struct {
	Endpoint string // https://rgw.example.internal (http:// only in tests)
	Bucket   string
	Region   string
}

// Prefix is a host's prefix in the bucket.
func Prefix(fqdn string) string { return "repos/" + fqdn }

// RepoURL is the restic repository URL for a host.
func (l Location) RepoURL(fqdn string) string {
	return "s3:" + strings.TrimSuffix(l.Endpoint, "/") + "/" + l.Bucket + "/" + Prefix(fqdn)
}

// Key is an S3 identity.
type Key struct {
	AccessKeyID     string
	SecretAccessKey string
}

// Env returns the variables restic reads for S3.
func (l Location) Env(k Key) []string {
	env := []string{"AWS_ACCESS_KEY_ID=" + k.AccessKeyID, "AWS_SECRET_ACCESS_KEY=" + k.SecretAccessKey}
	if l.Region != "" {
		env = append(env, "AWS_DEFAULT_REGION="+l.Region)
	}
	return env
}

// Provisioner makes a host's S3 identity exist with the host policy.
type Provisioner interface {
	EnsureHostKey(ctx context.Context, fqdn string, key Key) error
}

// ExecProvisioner runs a site-specific command. The command gets the FQDN, bucket and prefix in its
// environment, and the access key and secret on stdin as KEY=VALUE lines (never in argv). It must
// exit 0 once the identity exists with the policy, and be safe to run again.
type ExecProvisioner struct {
	Command []string
	Bucket  string
}

// EnsureHostKey implements Provisioner.
func (p ExecProvisioner) EnsureHostKey(ctx context.Context, fqdn string, key Key) error {
	if len(p.Command) == 0 {
		return errors.New("storage.provisioner.command is not set")
	}
	cmd := exec.CommandContext(ctx, p.Command[0], p.Command[1:]...)
	cmd.Env = append(os.Environ(), "HOLDFAST_FQDN="+fqdn, "HOLDFAST_BUCKET="+p.Bucket, "HOLDFAST_PREFIX="+Prefix(fqdn))
	cmd.Stdin = strings.NewReader("ACCESS_KEY_ID=" + key.AccessKeyID + "\nSECRET_ACCESS_KEY=" + key.SecretAccessKey + "\n")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("provisioning the S3 identity of %s failed: %v: %s", fqdn, err, tail(out.String(), 2000))
	}
	return nil
}

// NoopProvisioner is for setups where every host uses identities made outside Holdfast. It is
// used by unit tests.
type NoopProvisioner struct{}

// EnsureHostKey implements Provisioner.
func (NoopProvisioner) EnsureHostKey(context.Context, string, Key) error { return nil }

// ReadKeyFile reads an S3 identity from a root-only file of KEY=VALUE lines with
// AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY.
func ReadKeyFile(path string) (Key, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return Key{}, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return Key{}, fmt.Errorf("%s has mode %04o: it must not be readable by group or others", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Key{}, err
	}
	var k Key
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		name, value, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(name) {
		case "AWS_ACCESS_KEY_ID":
			k.AccessKeyID = strings.TrimSpace(value)
		case "AWS_SECRET_ACCESS_KEY":
			k.SecretAccessKey = strings.TrimSpace(value)
		}
	}
	if k.AccessKeyID == "" || k.SecretAccessKey == "" {
		return Key{}, fmt.Errorf("%s needs AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY", path)
	}
	return k, nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}
