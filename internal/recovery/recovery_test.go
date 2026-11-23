package recovery

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
)

func TestRecoveryFile(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	key := bytes.Repeat([]byte{7}, 32)
	f, err := Parse(bytes.NewReader(Render("hfsrv", time.Now(), id, key)))
	if err != nil || len(f.Identities) != 1 || f.Recipients[0] != id.Recipient().String() || !bytes.Equal(f.CredentialsKey, key) {
		t.Fatalf("round trip: %+v %v", f, err)
	}

	// An age key file, as age-keygen writes it, is one too.
	keygen := "# created: 2026-11-23T10:00:00Z\n# public key: " + id.Recipient().String() + "\n" + id.String() + "\n"
	if f, err := Parse(strings.NewReader(keygen)); err != nil || len(f.Identities) != 1 || f.CredentialsKey != nil {
		t.Fatalf("an age key file: %+v %v", f, err)
	}

	// A server set up with the admin's own age key: the credentials key alone, then their key added below it.
	own := Render("hfsrv", time.Now(), nil, key)
	if f, err := Parse(bytes.NewReader(own)); err != nil || len(f.Identities) != 0 || f.CredentialsKey == nil {
		t.Fatalf("the credentials key alone: %+v %v", f, err)
	}
	if f, err := Parse(bytes.NewReader(append(own, keygen...))); err != nil || len(f.Identities) != 1 {
		t.Fatalf("with the admin's key added: %+v %v", f, err)
	}

	for _, bad := range []string{"", "# only comments\n", "password=hunter2\n", "HOLDFAST-CREDENTIALS-KEY-ZZ\n"} {
		if _, err := Parse(strings.NewReader(bad)); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
}
