package agent

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

type testCert struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

// issue makes a certificate signed by parent, or self-signed when parent is nil.
func issue(t *testing.T, name string, ca bool, parent *testCert, ip string) *testCert {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	if ca {
		tmpl.IsCA, tmpl.BasicConstraintsValid, tmpl.KeyUsage = true, true, x509.KeyUsageCertSign|x509.KeyUsageDigitalSignature
	} else {
		tmpl.KeyUsage, tmpl.ExtKeyUsage = x509.KeyUsageDigitalSignature, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP(ip)}
	}
	signer, signerKey := tmpl, k
	if parent != nil {
		signer, signerKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &k.PublicKey, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCert{cert: c, key: k}
}

func pinOf(c *testCert) string {
	sum := sha256.Sum256(c.cert.Raw)
	return hex.EncodeToString(sum[:])
}

// TestPinnedServerVerification: the pinned certificate is the only root, and the server's own certificate
// must chain to it and name the host. A real CA certificate sent next to a stranger's certificate fails.
func TestPinnedServerVerification(t *testing.T) {
	ca := issue(t, "holdfast CA", true, nil, "")
	server := issue(t, "holdfast", false, ca, "127.0.0.1")
	attacker := issue(t, "attacker", false, nil, "127.0.0.1")
	otherCA := issue(t, "other CA", true, nil, "")
	signedElsewhere := issue(t, "holdfast", false, otherCA, "127.0.0.1")
	selfSigned := issue(t, "holdfast self-signed", false, nil, "127.0.0.1")

	for _, tc := range []struct {
		name  string
		chain []*x509.Certificate
		pin   string
		host  string
		want  string // "" for accepted, else part of the error
	}{
		{"the server's chain, pinned to its CA", []*x509.Certificate{server.cert, ca.cert}, pinOf(ca), "127.0.0.1", ""},
		{"a self-signed server, pinned to itself", []*x509.Certificate{selfSigned.cert}, pinOf(selfSigned), "127.0.0.1", ""},
		{"a man in the middle sends the real CA next to his own certificate", []*x509.Certificate{attacker.cert, ca.cert}, pinOf(ca), "127.0.0.1", "does not chain"},
		{"a certificate from another CA, with that CA in the chain", []*x509.Certificate{signedElsewhere.cert, otherCA.cert, ca.cert}, pinOf(ca), "127.0.0.1", "does not chain"},
		{"the right chain for another host", []*x509.Certificate{server.cert, ca.cert}, pinOf(ca), "10.0.0.9", "does not chain"},
		{"a chain without the pinned certificate", []*x509.Certificate{server.cert}, pinOf(ca), "127.0.0.1", "does not match the pinned fingerprint"},
		{"no certificate", nil, pinOf(ca), "127.0.0.1", "no certificate"},
	} {
		err := verifyPinned(tc.chain, tc.pin, tc.host)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: got %v, want an error with %q", tc.name, err, tc.want)
		}
	}
}
