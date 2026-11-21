package creds

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestSealOpen(t *testing.T) {
	s, err := NewSealer(Random(KeySize))
	if err != nil {
		t.Fatal(err)
	}
	c := s.Seal("repo-password", "web01.example.com")
	if got, err := s.Open(c, "web01.example.com"); err != nil || got != "repo-password" {
		t.Fatalf("open = %q, %v", got, err)
	}
	if _, err := s.Open(c, "db01.example.com"); err == nil {
		t.Fatal("a secret sealed for one host opened for another")
	}
	other, _ := NewSealer(Random(KeySize))
	if _, err := other.Open(c, "web01.example.com"); err == nil {
		t.Fatal("a secret opened with the wrong key")
	}
	if c2 := s.Seal("repo-password", "web01.example.com"); string(c2) == string(c) {
		t.Fatal("sealing twice gave the same ciphertext (nonce reuse)")
	}
}

func TestKeyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "key")
	if err := WriteNewKeyFile(p); err != nil {
		t.Fatal(err)
	}
	if err := WriteNewKeyFile(p); err == nil {
		t.Fatal("WriteNewKeyFile overwrote an existing key")
	}
	if _, err := LoadKeyFile(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyFile(p); err == nil {
		t.Fatal("a world-readable key file was accepted")
	}
}

func TestCodesAndTokens(t *testing.T) {
	code := NewEnrollmentCode()
	if !regexp.MustCompile(`^[2-9A-HJ-KM-NP-Z]{4}-[2-9A-HJ-KM-NP-Z]{4}-[2-9A-HJ-KM-NP-Z]{4}$`).MatchString(code) {
		t.Fatalf("unexpected code format %q", code)
	}
	if NormaliseCode(" 7kq2-m9tx-4hrw ") != "7KQ2-M9TX-4HRW" {
		t.Fatal("codes must compare case-insensitively")
	}
	if NewToken() == NewToken() || len(NewPassword()) != 64 {
		t.Fatal("tokens must be random and passwords 32 bytes hex")
	}
	if HashToken("a") == HashToken("b") || len(HashToken("a")) != 64 {
		t.Fatal("hash looks wrong")
	}
}
