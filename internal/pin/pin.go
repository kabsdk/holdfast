// Package pin is the HTTPS client that trusts one pinned certificate: agents use it for their server, and a vault for
// its source server (decisions 030 and 039).
package pin

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Client pins the server when a fingerprint is given. Without one, the system's roots apply.
func Client(server, fingerprint string) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if fingerprint != "" {
		u, err := url.Parse(server)
		if err != nil || u.Hostname() == "" {
			return nil, fmt.Errorf("server %q is not a URL", server)
		}
		host := u.Hostname()
		fp := strings.ToLower(strings.ReplaceAll(fingerprint, ":", ""))
		tr.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true, // the standard verification is replaced by Verify
			VerifyConnection: func(cs tls.ConnectionState) error {
				return Verify(cs.PeerCertificates, fp, host)
			},
		}
	}
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
		sum := sha256.Sum256(c.Raw)
		if hex.EncodeToString(sum[:]) == fingerprint {
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
