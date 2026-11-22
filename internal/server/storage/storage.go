// Package storage knows where repositories live, what each S3 key must be allowed to do, and how to
// test that (decisions 029 and 041, docs/storage.md).
//
// Holdfast never creates S3 identities itself: the admin gives it two keys per bucket. Hosts hold none: they back
// up through the server's gateway (decision 041). The gateway key may:
//   - read, write and list under repos/;
//   - delete only lock files, under repos/<fqdn>/locks/;
//   - never delete a version, and never use the bucket outside repos/.
//
// The maintenance key may do everything under repos/, including read what the gateway key wrote, but never delete a
// version. S3Prober tests all of it with plain S3 calls.
package storage

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
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
	f, err := os.Open(path)
	if err != nil {
		return Key{}, err
	}
	defer f.Close()
	k, err := ParseKey(f)
	if err != nil {
		return Key{}, fmt.Errorf("%s: %w", path, err)
	}
	return k, nil
}

// ParseKey reads AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY from KEY=VALUE lines. Other lines are
// ignored, so a file made for the AWS CLI's environment works as it is.
func ParseKey(r io.Reader) (Key, error) {
	var k Key
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		name, value, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch strings.TrimSpace(strings.TrimPrefix(name, "export ")) {
		case "AWS_ACCESS_KEY_ID":
			k.AccessKeyID = value
		case "AWS_SECRET_ACCESS_KEY":
			k.SecretAccessKey = value
		}
	}
	if err := sc.Err(); err != nil {
		return Key{}, err
	}
	return k, k.Validate()
}

// Validate checks that both parts are present and look like an S3 key: printable, without spaces,
// and of a sane length. It never puts the secret in its message.
func (k Key) Validate() error {
	check := func(name, v string) error {
		if v == "" {
			return fmt.Errorf("%s is missing", name)
		}
		if len(v) > 256 {
			return fmt.Errorf("%s is longer than 256 characters", name)
		}
		for _, c := range v {
			if c <= ' ' || c > '~' {
				return fmt.Errorf("%s contains a space or a character that is not printable ASCII", name)
			}
		}
		return nil
	}
	return errors.Join(check("AWS_ACCESS_KEY_ID", k.AccessKeyID), check("AWS_SECRET_ACCESS_KEY", k.SecretAccessKey))
}
