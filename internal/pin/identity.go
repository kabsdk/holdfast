package pin

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"time"
)

// An agent's identity (decision 041): a key the agent makes itself at enrollment, and a self-signed certificate the
// server pins, as SSH pins a host key. The private key never leaves the host. There is no CA and nothing to renew:
// the pin is what counts, not the certificate's dates. A new enrollment makes a new key.

// NewIdentity makes a key and its self-signed certificate for name, as PEM: the certificate, then the key. restic
// reads the same file for the gateway (--tls-client-cert).
func NewIdentity(name string) ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: name, Organization: []string{"holdfast-agent"}},
		NotBefore: now.Add(-24 * time.Hour), NotAfter: now.AddDate(100, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return append(out, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb})...), nil
}

// ParseIdentity reads what NewIdentity made. Its Leaf is set, so Fingerprint(id.Leaf) is what a server pins.
func ParseIdentity(b []byte) (*tls.Certificate, error) {
	c, err := tls.X509KeyPair(b, b)
	if err != nil {
		return nil, err
	}
	if c.Leaf == nil {
		if c.Leaf, err = x509.ParseCertificate(c.Certificate[0]); err != nil {
			return nil, err
		}
	}
	return &c, nil
}

// LoadIdentity reads an identity file.
func LoadIdentity(path string) (*tls.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseIdentity(b)
}
