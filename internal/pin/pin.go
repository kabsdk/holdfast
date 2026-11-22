// Package pin is Holdfast's pinned TLS (decisions 030 and 041): a client that trusts one pinned server certificate, and
// the fingerprint by which the server knows an agent's own certificate.
package pin

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Fingerprint is how a certificate is pinned: the sha256 of its DER, in lower-case hex.
func Fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return hex.EncodeToString(sum[:])
}

func normalise(fingerprint string) string {
	return strings.ToLower(strings.ReplaceAll(fingerprint, ":", ""))
}

// Client pins the server when a fingerprint is given. Without one, the system's roots apply. With cert, the client
// identifies itself by that certificate: an agent's own (decision 041).
func Client(server, fingerprint string, cert *tls.Certificate) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if fingerprint != "" {
		u, err := url.Parse(server)
		if err != nil || u.Hostname() == "" {
			return nil, fmt.Errorf("server %q is not a URL", server)
		}
		host, fp := u.Hostname(), normalise(fingerprint)
		cfg.InsecureSkipVerify = true // the standard verification is replaced by Verify
		cfg.VerifyConnection = func(cs tls.ConnectionState) error { return Verify(cs.PeerCertificates, fp, host) }
	}
	if cert != nil {
		cfg.Certificates = []tls.Certificate{*cert}
	}
	tr.TLSClientConfig = cfg
	return &http.Client{Transport: tr}, nil
}

// Verify accepts a server whose own certificate chains to the pinned certificate and names host.
//
// The pin is the sha256 of one certificate in the chain the server sends: normally its CA's, or its own if it is
// self-signed. That certificate becomes the only trusted root. Finding the pin somewhere in the chain is not enough
// on its own. A CA's certificate is public, so a man in the middle could send it along with a certificate of his own
// (decision 030).
func Verify(chain []*x509.Certificate, fingerprint, host string) error {
	if len(chain) == 0 {
		return errors.New("the server sent no certificate")
	}
	roots, intermediates := x509.NewCertPool(), x509.NewCertPool()
	found := false
	for _, c := range chain {
		if Fingerprint(c) == fingerprint {
			roots.AddCert(c)
			found = true
		} else {
			intermediates.AddCert(c)
		}
	}
	if !found {
		return errors.New("the server's certificate chain does not match the pinned fingerprint")
	}
	if _, err := chain[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, DNSName: host}); err != nil {
		return fmt.Errorf("the server's certificate does not chain to the pinned certificate for %s: %w", host, err)
	}
	return nil
}

// PinnedPEM connects to the server and returns the pinned certificate itself, as PEM: the root that restic is given
// to verify the gateway with (--cacert). The connection is verified as Client verifies it.
func PinnedPEM(server, fingerprint string) ([]byte, error) {
	u, err := url.Parse(server)
	if err != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("server %q is not a URL", server)
	}
	addr := u.Host
	if u.Port() == "" {
		addr = net.JoinHostPort(u.Hostname(), "443")
	}
	fp := normalise(fingerprint)
	var pinned *x509.Certificate
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 15 * time.Second}, "tcp", addr, &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if err := Verify(cs.PeerCertificates, fp, u.Hostname()); err != nil {
				return err
			}
			for _, c := range cs.PeerCertificates {
				if Fingerprint(c) == fp {
					pinned = c
				}
			}
			return nil
		}})
	if err != nil {
		return nil, err
	}
	conn.Close()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pinned.Raw}), nil
}
