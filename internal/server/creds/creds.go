// Package creds generates and protects secrets: repository passwords, session tokens and enrollment
// codes. Secrets at rest are sealed with AES-256-GCM under a key read from a root-only file.
// Nothing here logs or returns a secret except to its one caller.
package creds

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Sealer encrypts secrets at rest.
type Sealer struct{ aead cipher.AEAD }

// KeySize is the size of the sealing key in bytes.
const KeySize = 32

// LoadKeyFile reads a hex-encoded 32-byte key. The file must not be readable by group or others.
func LoadKeyFile(path string) (*Sealer, error) {
	key, err := ReadKeyFile(path)
	if err != nil {
		return nil, err
	}
	return NewSealer(key)
}

// ReadKeyFile reads the key's bytes, for the recovery file. The file must not be readable by group
// or others.
func ReadKeyFile(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("credentials key %s has mode %04o: it must not be readable by group or others", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, fmt.Errorf("credentials key %s: not hex: %w", path, err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("credentials key %s must be %d bytes, got %d", path, KeySize, len(key))
	}
	return key, nil
}

// WriteNewKeyFile creates a new random key at path with mode 0600. It refuses to overwrite.
func WriteNewKeyFile(path string) error {
	return WriteKeyFile(path, Random(KeySize))
}

// WriteKeyFile writes a key at path with mode 0600, as LoadKeyFile reads it. It refuses to overwrite.
func WriteKeyFile(path string, key []byte) error {
	if len(key) != KeySize {
		return fmt.Errorf("credentials key must be %d bytes, got %d", KeySize, len(key))
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(hex.EncodeToString(key) + "\n")
	return err
}

// NewSealer returns a Sealer for a 32-byte key.
func NewSealer(key []byte) (*Sealer, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("credentials key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// Seal encrypts plaintext; the nonce is prepended. aad binds the ciphertext to its row (the FQDN),
// so a ciphertext copied to another host's row does not open.
func (s *Sealer) Seal(plaintext, aad string) []byte {
	nonce := Random(s.aead.NonceSize())
	return s.aead.Seal(nonce, nonce, []byte(plaintext), []byte(aad))
}

// Open decrypts what Seal produced with the same aad.
func (s *Sealer) Open(sealed []byte, aad string) (string, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n {
		return "", errors.New("sealed secret is too short")
	}
	pt, err := s.aead.Open(nil, sealed[:n], sealed[n:], []byte(aad))
	if err != nil {
		return "", errors.New("sealed secret does not open with this key")
	}
	return string(pt), nil
}

// Random returns n bytes from crypto/rand.
func Random(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return b
}

// NewPassword returns a repository password: 32 random bytes, hex-encoded.
func NewPassword() string { return hex.EncodeToString(Random(32)) }

// NewToken returns a secret token, such as a login session's: 32 random bytes, hex-encoded.
func NewToken() string { return hex.EncodeToString(Random(32)) }

// HashToken is how tokens and enrollment codes are stored and looked up.
func HashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// codeAlphabet avoids look-alike characters (no 0/O, 1/I/L).
const codeAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

// NewEnrollmentCode returns a code that is easy to read aloud and type, such as "7KQ2-M9TX-4HRW".
// 12 characters from 31 give about 59 bits; a code is single-use and valid for 15 minutes.
func NewEnrollmentCode() string {
	b := Random(12)
	var sb strings.Builder
	for i, x := range b {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(codeAlphabet[int(x)%len(codeAlphabet)])
	}
	return sb.String()
}

// NormaliseCode makes a typed code comparable: upper case, no spaces.
func NormaliseCode(c string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(c), " ", ""))
}
