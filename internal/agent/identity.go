package agent

import (
	"os"
	"path/filepath"
)

// identityFile holds the agent's own certificate and then its private key, as PEM, root-only (pin.NewIdentity). The
// server pinned the certificate at enrollment (decision 041), and restic reads the same file for the gateway.
const identityFile = "agent.pem"

// Enrolled says whether the agent has an identity yet: it enrolled once, in this state directory.
func Enrolled(c Config) bool {
	c.Defaults()
	_, err := os.Stat(filepath.Join(c.StateDir, identityFile))
	return err == nil
}
