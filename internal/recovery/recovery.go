// Package recovery is the recovery file: with the bucket and its two S3 keys, it is all a lost server
// is rebuilt from. It holds the age key that the escrow files and the nightly exports are encrypted to, and the server's
// credentials key, which seals the repository passwords in its database. The installer makes it on the server; the
// admin copies it offline and removes it from the server.
//
// It is text, so it can live in a password manager. Lines that start with # are comments. An age key file, as
// age-keygen writes it, is a recovery file too, without a credentials key. age itself refuses the credentials key's
// line, so Holdfast reads the file itself; `age -d -i` needs the AGE-SECRET-KEY line on its own.
package recovery

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"filippo.io/age"
)

// credentialsPrefix starts the line that holds the credentials key, in hex.
const credentialsPrefix = "HOLDFAST-CREDENTIALS-KEY-"

// DefaultPath is where the installer writes it.
const DefaultPath = "/root/holdfast-recovery.txt"

// File is a recovery file's contents.
type File struct {
	Identities     []age.Identity
	Recipients     []string // the age public keys of Identities
	CredentialsKey []byte   // 32 bytes, or nil when the file has none
}

// Parse reads a recovery file, or an age key file.
func Parse(r io.Reader) (File, error) {
	var f File
	sc := bufio.NewScanner(r)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "AGE-SECRET-KEY-"):
			id, err := age.ParseX25519Identity(line)
			if err != nil {
				return File{}, fmt.Errorf("line %d: not an age key: %w", n, err)
			}
			f.Identities = append(f.Identities, id)
			f.Recipients = append(f.Recipients, id.Recipient().String())
		case strings.HasPrefix(line, credentialsPrefix):
			k, err := hex.DecodeString(strings.TrimPrefix(line, credentialsPrefix))
			if err != nil || len(k) != 32 {
				return File{}, fmt.Errorf("line %d: not a Holdfast credentials key", n)
			}
			if f.CredentialsKey != nil && !bytes.Equal(f.CredentialsKey, k) {
				return File{}, fmt.Errorf("line %d: a second, different credentials key", n)
			}
			f.CredentialsKey = k
		default:
			return File{}, fmt.Errorf("line %d is neither a comment, an age key (AGE-SECRET-KEY-…) nor a Holdfast credentials key (%s…)", n, credentialsPrefix)
		}
	}
	if err := sc.Err(); err != nil {
		return File{}, err
	}
	if len(f.Identities) == 0 && f.CredentialsKey == nil {
		return File{}, fmt.Errorf("it holds neither an age key nor a credentials key")
	}
	return f, nil
}

// ReadFile reads a recovery file once.
func ReadFile(path string) (File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return File{}, err
	}
	f, err := Parse(bytes.NewReader(b))
	if err != nil {
		return File{}, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// Render writes a recovery file: the age key, when there is one, and the credentials key.
func Render(node string, made time.Time, id *age.X25519Identity, credentialsKey []byte) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# Holdfast recovery file for %s, made %s.\n", node, made.UTC().Format("2006-01-02 15:04 UTC"))
	b.WriteString("# Keep it offline, such as in a password manager, and delete it from the server. With the bucket and its\n")
	b.WriteString("# two S3 keys it rebuilds a lost server, and it opens every backup: guard it as you guard root.\n")
	if id != nil {
		b.WriteString("#\n# The age key: each host's escrow file and the nightly exports are encrypted to its public key.\n")
		fmt.Fprintf(&b, "# public key: %s\n%s\n", id.Recipient(), id)
	} else {
		b.WriteString("#\n# This server's escrow uses an age key you made yourself. Add its AGE-SECRET-KEY-… line here, so this file\n")
		b.WriteString("# is all a rebuild needs.\n")
	}
	b.WriteString("#\n# The credentials key: the repository passwords in the server's database are sealed with it.\n")
	fmt.Fprintf(&b, "%s%s\n", credentialsPrefix, strings.ToUpper(hex.EncodeToString(credentialsKey)))
	return b.Bytes()
}

// WriteFile writes data to path with mode 0600, refusing to overwrite a file.
func WriteFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
