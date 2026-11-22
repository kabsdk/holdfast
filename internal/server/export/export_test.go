package export

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/kabsdk/holdfast/internal/server/store"
)

func TestSealAndOpen(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 22, 0, 30, 0, 0, time.UTC)
	in := Contents{
		Manifest: Manifest{HoldfastVersion: "0.9.0", CreatedAt: now, Node: "hfsrv",
			Schemas: []Schema{{Name: "public", Version: 15}, {Name: "offsite", Version: 15}}},
		Tables: map[string][]store.TableData{
			"public":  {{Name: "hosts", CSV: []byte("fqdn,state\nweb01.example.com,enrolled\n"), Rows: 1}, {Name: "audit", CSV: []byte("id,action\n"), Rows: 0}},
			"offsite": {{Name: "hosts", CSV: []byte("fqdn,state\nweb01.example.com,enrolled\n"), Rows: 1}},
		},
	}
	sealed, err := Seal(in, id.Recipient().String())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("web01")) {
		t.Fatal("the export is not encrypted")
	}
	out, err := Open(sealed, id)
	if err != nil {
		t.Fatal(err)
	}
	if out.Manifest.Format != Format || len(out.Manifest.Schemas) != 2 || out.Manifest.Schemas[0].Tables[0].File != "public/hosts.csv" {
		t.Fatalf("manifest: %+v", out.Manifest)
	}
	if got := out.Tables["public"]; len(got) != 2 || string(got[0].CSV) != string(in.Tables["public"][0].CSV) || got[0].Rows != 1 {
		t.Fatalf("public tables: %+v", got)
	}
	other, _ := age.GenerateX25519Identity()
	if _, err := Open(sealed, other); err == nil || !strings.Contains(err.Error(), "age key") {
		t.Fatalf("another key: %v", err)
	}
	if _, err := Seal(in, "not-a-recipient"); err == nil {
		t.Fatal("a bad recipient was accepted")
	}
}

func TestOpenRefusesADamagedExport(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	in := Contents{Manifest: Manifest{CreatedAt: time.Now(), Schemas: []Schema{{Name: "public", Version: 1}}},
		Tables: map[string][]store.TableData{"public": {{Name: "hosts", CSV: []byte("fqdn\nweb01\n"), Rows: 1}}}}
	sealed, err := Seal(in, id.Recipient().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(sealed[:len(sealed)-20], id); err == nil {
		t.Fatal("a truncated export was opened")
	}
	bad := Contents{Manifest: Manifest{CreatedAt: time.Now(), Schemas: []Schema{{Name: "Public; DROP", Version: 1}}}}
	if _, err := Seal(bad, id.Recipient().String()); err == nil {
		t.Fatal("a schema name Holdfast does not make was sealed")
	}
}
